package cluster

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/hashicorp/raft"
)

// Running metadata shards means running several Raft groups on one node, and
// each Raft group needs a transport. Giving every group its own port would make
// the port count depend on the shard count, which no Kubernetes Service or
// firewall rule could follow. Instead all groups share the existing Raft port
// and connections are demultiplexed by what the dialer says it wants.
//
// A dialer that wants a SHARD group announces it: four magic bytes and the shard
// id, written immediately after connect. A dialer that wants the CONTROL group
// announces nothing and speaks Raft directly, exactly as every existing node
// already does.
//
// That asymmetry is deliberate and permanent. If control traffic also carried a
// header, a node running an older build would send none, and an upgraded peer
// would reject it: a rolling upgrade would split the control group, which is the
// group that holds the shard map. Leaving control traffic unframed means old and
// new nodes keep talking throughout.
//
// The framing is unambiguous because Raft's first byte on a connection is an
// rpcType, which hashicorp/raft defines in the range 0 to 4. It can never be
// 'V', so a control connection can never be mistaken for a shard connection.
var shardStreamMagic = [4]byte{'V', 'S', '3', 'R'}

const (
	// shardHeaderLen is the magic plus a uint16 shard id.
	shardHeaderLen = len(shardStreamMagic) + 2
	// controlShard marks the stream layer that serves the control group.
	controlShard = -1
	// defaultHandshakeTimeout bounds how long an accepted connection may take to
	// declare itself. A connection that stalls here is holding a goroutine, not
	// the accept loop, but it still must not hold it forever.
	defaultHandshakeTimeout = 10 * time.Second
	// connBacklog is how many accepted connections may wait for a group that has
	// not called Accept yet, e.g. while its Raft instance is still starting.
	connBacklog = 16
)

// ErrTransportClosed is returned by Accept once the mux is shut down.
var ErrTransportClosed = errors.New("cluster: transport closed")

// TransportMux serves every Raft group on this node from one listener.
type TransportMux struct {
	ln               net.Listener
	advertise        net.Addr
	handshakeTimeout time.Duration

	mu      sync.RWMutex
	control *muxStreamLayer
	shards  map[uint16]*muxStreamLayer

	closeOnce sync.Once
	closed    chan struct{}
	wg        sync.WaitGroup
}

// NewTransportMux starts demultiplexing connections from ln. The control group's
// stream layer exists immediately; shard layers are registered as their groups
// start.
func NewTransportMux(ln net.Listener) *TransportMux {
	return NewTransportMuxWithAdvertise(ln, ln.Addr())
}

// NewTransportMuxWithAdvertise is NewTransportMux for a listener whose own
// address is not the one peers should dial. Raft records the advertised address
// in the cluster configuration, so a node bound to a wildcard must advertise a
// routable address instead or every peer would try to dial 0.0.0.0.
func NewTransportMuxWithAdvertise(ln net.Listener, advertise net.Addr) *TransportMux {
	if advertise == nil {
		advertise = ln.Addr()
	}
	m := &TransportMux{
		ln:               ln,
		advertise:        advertise,
		handshakeTimeout: defaultHandshakeTimeout,
		shards:           make(map[uint16]*muxStreamLayer),
		closed:           make(chan struct{}),
	}
	m.control = m.newLayer(controlShard)
	m.wg.Add(1)
	go m.acceptLoop()
	return m
}

// ControlLayer is the stream layer for the control group: it dials plainly and
// accepts connections that carry no header.
func (m *TransportMux) ControlLayer() raft.StreamLayer { return m.control }

// ShardLayer registers and returns the stream layer for one shard. Calling it
// twice for the same shard returns the same layer, so a restarting group reuses
// its registration rather than orphaning it.
func (m *TransportMux) ShardLayer(shard int) (raft.StreamLayer, error) {
	if shard < 0 || shard > int(^uint16(0)) {
		return nil, fmt.Errorf("cluster: shard %d is out of range", shard)
	}
	select {
	case <-m.closed:
		return nil, ErrTransportClosed
	default:
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	// A layer that has been closed is not reusable: its Accept returns
	// immediately forever. A node removed from a shard and later added back would
	// otherwise start a group whose transport can never accept a connection, and
	// it would look alive because it can still dial out. Replace it instead.
	if l, ok := m.shards[uint16(shard)]; ok && !l.isClosed() {
		return l, nil
	}
	l := m.newLayer(shard)
	m.shards[uint16(shard)] = l
	return l, nil
}

func (m *TransportMux) newLayer(shard int) *muxStreamLayer {
	return &muxStreamLayer{
		shard:  shard,
		addr:   m.advertise,
		conns:  make(chan net.Conn, connBacklog),
		closed: make(chan struct{}),
	}
}

// Close stops accepting and releases every layer. The listener is closed too:
// the mux owns it, and a group closing its own layer must not take the port
// down for the others.
func (m *TransportMux) Close() error {
	err := error(nil)
	m.closeOnce.Do(func() {
		close(m.closed)
		err = m.ln.Close()
		m.mu.Lock()
		m.control.close()
		for _, l := range m.shards {
			l.close()
		}
		m.mu.Unlock()
		m.wg.Wait()
	})
	return err
}

func (m *TransportMux) acceptLoop() {
	defer m.wg.Done()
	for {
		conn, err := m.ln.Accept()
		if err != nil {
			select {
			case <-m.closed:
				return
			default:
			}
			// A transient accept error must not kill the loop, or the node stops
			// being reachable for every group at once.
			slog.Warn("cluster: transport accept failed", "error", err)
			time.Sleep(50 * time.Millisecond)
			continue
		}
		m.wg.Add(1)
		// The handshake reads from the connection, so it cannot run on the accept
		// loop: one slow peer would stall every other group's connections.
		go func() {
			defer m.wg.Done()
			m.route(conn)
		}()
	}
}

// route reads the optional shard header and hands the connection to the group it
// belongs to, with the header consumed and any over-read bytes replayed.
func (m *TransportMux) route(conn net.Conn) {
	if err := conn.SetReadDeadline(time.Now().Add(m.handshakeTimeout)); err != nil {
		conn.Close()
		return
	}
	var first [1]byte
	if _, err := io.ReadFull(conn, first[:]); err != nil {
		conn.Close()
		return
	}

	if first[0] != shardStreamMagic[0] {
		// Control traffic. The byte just read is the Raft rpcType, so it has to be
		// handed back unread.
		m.deliver(m.control, &prefixConn{Conn: conn, prefix: first[:1]}, controlShard)
		return
	}

	rest := make([]byte, shardHeaderLen-1)
	if _, err := io.ReadFull(conn, rest); err != nil {
		conn.Close()
		return
	}
	if string(rest[:3]) != string(shardStreamMagic[1:]) {
		// Started like the magic but is not: not a control connection either,
		// since Raft never opens with 'V'. Refuse it rather than guess.
		slog.Warn("cluster: rejected a connection with an unrecognized header", "remote", conn.RemoteAddr())
		conn.Close()
		return
	}
	shard := binary.BigEndian.Uint16(rest[3:])

	m.mu.RLock()
	layer, ok := m.shards[shard]
	m.mu.RUnlock()
	if !ok {
		// A peer thinks this node serves a shard it does not. Closing is correct:
		// answering would make the peer believe the group is here.
		slog.Warn("cluster: connection for a shard this node does not serve",
			"shard", shard, "remote", conn.RemoteAddr())
		conn.Close()
		return
	}
	m.deliver(layer, conn, int(shard))
}

// deliver hands a connection to a layer, clearing the handshake deadline first
// so Raft's own timeouts govern from here on.
func (m *TransportMux) deliver(l *muxStreamLayer, conn net.Conn, shard int) {
	if err := conn.SetReadDeadline(time.Time{}); err != nil {
		conn.Close()
		return
	}
	select {
	case l.conns <- conn:
	case <-l.closed:
		conn.Close()
	case <-m.closed:
		conn.Close()
	case <-time.After(m.handshakeTimeout):
		// The group is not accepting. Dropping is better than blocking forever:
		// the peer retries, and a stuck group must not leak connections.
		slog.Warn("cluster: timed out handing a connection to its group", "shard", shard)
		conn.Close()
	}
}

// muxStreamLayer is one group's view of the shared listener.
type muxStreamLayer struct {
	shard int // controlShard for the control group
	addr  net.Addr
	conns chan net.Conn

	closeOnce sync.Once
	closed    chan struct{}
}

func (l *muxStreamLayer) Accept() (net.Conn, error) {
	select {
	case c := <-l.conns:
		return c, nil
	case <-l.closed:
		return nil, ErrTransportClosed
	}
}

func (l *muxStreamLayer) Addr() net.Addr { return l.addr }

func (l *muxStreamLayer) Close() error {
	l.close()
	return nil
}

// isClosed reports whether this layer has been shut down.
func (l *muxStreamLayer) isClosed() bool {
	select {
	case <-l.closed:
		return true
	default:
		return false
	}
}

func (l *muxStreamLayer) close() {
	l.closeOnce.Do(func() {
		close(l.closed)
		// Drain so connections handed over but never accepted are not leaked.
		for {
			select {
			case c := <-l.conns:
				c.Close()
			default:
				return
			}
		}
	})
}

// Dial opens a connection to a peer's Raft port and, for a shard layer, declares
// which group it is for. The control layer writes nothing at all, which is what
// keeps a node running an older build reachable.
func (l *muxStreamLayer) Dial(address raft.ServerAddress, timeout time.Duration) (net.Conn, error) {
	conn, err := net.DialTimeout("tcp", string(address), timeout)
	if err != nil {
		return nil, err
	}
	if l.shard == controlShard {
		return conn, nil
	}
	header := make([]byte, 0, shardHeaderLen)
	header = append(header, shardStreamMagic[:]...)
	header = binary.BigEndian.AppendUint16(header, uint16(l.shard))
	if timeout > 0 {
		if err := conn.SetWriteDeadline(time.Now().Add(timeout)); err != nil {
			conn.Close()
			return nil, err
		}
	}
	if _, err := conn.Write(header); err != nil {
		conn.Close()
		return nil, fmt.Errorf("cluster: announce shard %d: %w", l.shard, err)
	}
	if err := conn.SetWriteDeadline(time.Time{}); err != nil {
		conn.Close()
		return nil, err
	}
	return conn, nil
}

// prefixConn replays bytes that were read to classify the connection before the
// rest of the stream is handed to Raft.
type prefixConn struct {
	net.Conn
	prefix []byte
}

func (c *prefixConn) Read(b []byte) (int, error) {
	if len(c.prefix) > 0 {
		n := copy(b, c.prefix)
		c.prefix = c.prefix[n:]
		return n, nil
	}
	return c.Conn.Read(b)
}
