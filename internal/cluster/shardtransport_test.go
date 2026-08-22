package cluster

import (
	"encoding/binary"
	"io"
	"net"
	"testing"
	"time"

	"github.com/hashicorp/raft"
)

func newTestMux(t *testing.T) *TransportMux {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	m := NewTransportMux(ln)
	m.handshakeTimeout = 2 * time.Second
	t.Cleanup(func() { m.Close() })
	return m
}

func acceptWithin(t *testing.T, l raft.StreamLayer, d time.Duration) net.Conn {
	t.Helper()
	type res struct {
		c   net.Conn
		err error
	}
	ch := make(chan res, 1)
	go func() {
		c, err := l.Accept()
		ch <- res{c, err}
	}()
	select {
	case r := <-ch:
		if r.err != nil {
			t.Fatalf("accept: %v", r.err)
		}
		return r.c
	case <-time.After(d):
		t.Fatal("no connection was delivered")
		return nil
	}
}

func mustNotAccept(t *testing.T, l raft.StreamLayer, d time.Duration) {
	t.Helper()
	ch := make(chan net.Conn, 1)
	go func() {
		c, err := l.Accept()
		if err == nil {
			ch <- c
		}
	}()
	select {
	case c := <-ch:
		c.Close()
		t.Fatal("a connection was delivered to the wrong group")
	case <-time.After(d):
	}
}

// A control connection carries no header at all, which is what lets a node
// running an older build keep talking to an upgraded one. The first byte it
// sends is Raft's rpcType and must reach Raft unread.
func TestMuxDeliversUnframedTrafficToTheControlGroup(t *testing.T) {
	m := newTestMux(t)
	control := m.ControlLayer()
	shard, err := m.ShardLayer(0)
	if err != nil {
		t.Fatal(err)
	}

	c, err := control.Dial(raft.ServerAddress(m.ln.Addr().String()), time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()
	// rpcType 1 (AppendEntries) followed by a payload, as Raft would write it.
	if _, err := c.Write([]byte{1, 'h', 'i'}); err != nil {
		t.Fatalf("write: %v", err)
	}

	got := acceptWithin(t, control, 3*time.Second)
	defer got.Close()
	buf := make([]byte, 3)
	if _, err := io.ReadFull(got, buf); err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(buf) != "\x01hi" {
		t.Fatalf("control group received %q, want the unmodified stream", buf)
	}
	mustNotAccept(t, shard, 200*time.Millisecond)
}

func TestMuxRoutesEachShardToItsOwnGroup(t *testing.T) {
	m := newTestMux(t)
	control := m.ControlLayer()
	zero, _ := m.ShardLayer(0)
	seven, _ := m.ShardLayer(7)

	c, err := seven.Dial(raft.ServerAddress(m.ln.Addr().String()), time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()
	if _, err := c.Write([]byte{2, 'x'}); err != nil {
		t.Fatalf("write: %v", err)
	}

	got := acceptWithin(t, seven, 3*time.Second)
	defer got.Close()
	buf := make([]byte, 2)
	if _, err := io.ReadFull(got, buf); err != nil {
		t.Fatalf("read: %v", err)
	}
	if buf[0] != 2 || buf[1] != 'x' {
		t.Fatalf("shard 7 received %q, want the stream after the header", buf)
	}
	// The header must be consumed, and no other group may see the connection.
	mustNotAccept(t, zero, 200*time.Millisecond)
	mustNotAccept(t, control, 200*time.Millisecond)
}

// A peer that believes this node serves a shard it does not must be refused, not
// answered: an accepted connection would tell the peer the group is here.
func TestMuxRefusesAnUnservedShard(t *testing.T) {
	m := newTestMux(t)
	addr := m.ln.Addr().String()

	conn, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	header := append(append([]byte{}, shardStreamMagic[:]...), 0, 42)
	if _, err := conn.Write(header); err != nil {
		t.Fatalf("write: %v", err)
	}
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := conn.Read(make([]byte, 1)); err == nil {
		t.Fatal("a connection for an unserved shard was kept open")
	}
}

// Raft's rpcType is 0..4, so a control connection can never begin with 'V'.
// Something that starts like the magic but is not it is neither, and guessing
// would hand a corrupt stream to a Raft group.
func TestMuxRefusesAMalformedHeader(t *testing.T) {
	m := newTestMux(t)
	control := m.ControlLayer()

	conn, err := net.DialTimeout("tcp", m.ln.Addr().String(), time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte{'V', 'S', '3', 'Z', 0, 0}); err != nil {
		t.Fatalf("write: %v", err)
	}
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := conn.Read(make([]byte, 1)); err == nil {
		t.Fatal("a malformed header was accepted")
	}
	mustNotAccept(t, control, 200*time.Millisecond)
}

// The magic cannot collide with Raft's own framing. This pins the property the
// whole scheme rests on, so a future protocol change cannot break it silently.
func TestShardMagicCannotCollideWithARaftRPCType(t *testing.T) {
	for b := byte(0); b <= 10; b++ {
		if b == shardStreamMagic[0] {
			t.Fatalf("rpcType %d collides with the shard magic byte", b)
		}
	}
	if shardStreamMagic[0] < 32 {
		t.Fatal("the magic byte must be outside the range Raft uses for rpcType")
	}
}

func TestShardLayerReturnsTheSameLayerForTheSameShard(t *testing.T) {
	m := newTestMux(t)
	a, err := m.ShardLayer(3)
	if err != nil {
		t.Fatal(err)
	}
	b, err := m.ShardLayer(3)
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Fatal("registering a shard twice produced two layers, so one would never be served")
	}
	if _, err := m.ShardLayer(-1); err == nil {
		t.Fatal("a negative shard id was accepted")
	}
	if _, err := m.ShardLayer(1 << 17); err == nil {
		t.Fatal("an out-of-range shard id was accepted")
	}
}

// Closing one group must not take the shared port down for the others.
func TestClosingOneLayerLeavesTheOthersServing(t *testing.T) {
	m := newTestMux(t)
	one, _ := m.ShardLayer(1)
	two, _ := m.ShardLayer(2)

	if err := one.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, err := one.Accept(); err != ErrTransportClosed {
		t.Fatalf("accept on a closed layer returned %v, want ErrTransportClosed", err)
	}

	c, err := two.Dial(raft.ServerAddress(m.ln.Addr().String()), time.Second)
	if err != nil {
		t.Fatalf("the shared listener stopped serving after one layer closed: %v", err)
	}
	defer c.Close()
	if _, err := c.Write([]byte{1}); err != nil {
		t.Fatalf("write: %v", err)
	}
	got := acceptWithin(t, two, 3*time.Second)
	got.Close()
}

func TestMuxCloseStopsEveryLayer(t *testing.T) {
	m := newTestMux(t)
	control := m.ControlLayer()
	shard, _ := m.ShardLayer(0)
	addr := m.ln.Addr().String()

	if err := m.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, err := control.Accept(); err != ErrTransportClosed {
		t.Fatalf("control accept returned %v, want ErrTransportClosed", err)
	}
	if _, err := shard.Accept(); err != ErrTransportClosed {
		t.Fatalf("shard accept returned %v, want ErrTransportClosed", err)
	}
	if c, err := net.DialTimeout("tcp", addr, 500*time.Millisecond); err == nil {
		c.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		if _, rerr := c.Read(make([]byte, 1)); rerr == nil {
			t.Fatal("the port still serves after Close")
		}
		c.Close()
	}
}

func TestShardHeaderEncodesTheShardID(t *testing.T) {
	m := newTestMux(t)
	layer, _ := m.ShardLayer(513)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	done := make(chan []byte, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			done <- nil
			return
		}
		defer c.Close()
		buf := make([]byte, shardHeaderLen)
		io.ReadFull(c, buf)
		done <- buf
	}()

	c, err := layer.Dial(raft.ServerAddress(ln.Addr().String()), time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()

	select {
	case hdr := <-done:
		if hdr == nil {
			t.Fatal("no header received")
		}
		if string(hdr[:4]) != string(shardStreamMagic[:]) {
			t.Fatalf("header magic %q", hdr[:4])
		}
		if got := binary.BigEndian.Uint16(hdr[4:]); got != 513 {
			t.Fatalf("header names shard %d, want 513", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("dial wrote no header")
	}
}

// A shard group can stop and start again on the same node, which is what happens
// when a node is removed from a shard and later added back. Handing the restarted
// group the closed layer would leave it unable to accept a single connection,
// while still able to dial out, so it would look alive and never replicate.
func TestShardLayerIsReplacedAfterItIsClosed(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	mux := NewTransportMux(ln)
	defer mux.Close()

	first, err := mux.ShardLayer(1)
	if err != nil {
		t.Fatal(err)
	}
	first.Close()

	second, err := mux.ShardLayer(1)
	if err != nil {
		t.Fatalf("shard layer not available after a restart: %v", err)
	}
	if second == first {
		t.Fatal("the restarted group was handed the closed layer")
	}

	// It really accepts: dial it the way a peer would.
	accepted := make(chan net.Conn, 1)
	go func() {
		conn, err := second.Accept()
		if err == nil {
			accepted <- conn
		}
	}()
	conn, err := second.Dial(raft.ServerAddress(ln.Addr().String()), 2*time.Second)
	if err != nil {
		t.Fatalf("dial the restarted layer: %v", err)
	}
	defer conn.Close()
	select {
	case got := <-accepted:
		got.Close()
	case <-time.After(5 * time.Second):
		t.Fatal("the restarted layer never accepted the connection")
	}
}
