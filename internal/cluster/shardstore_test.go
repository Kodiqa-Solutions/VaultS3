package cluster

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
)

// addrBook is the live node-id to API-address map the router resolves through.
type addrBook struct {
	mu sync.RWMutex
	m  map[string]string
}

func (a *addrBook) set(id, addr string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.m[id] = addr
}

func (a *addrBook) get(id string) (string, bool) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	addr, ok := a.m[id]
	return addr, ok && addr != ""
}

// noopApplier stands in for the control group. The sharded store must never send
// an object write here, so any command that reaches it is a routing bug; the
// tests below assert on the shard stores, which is where the records must land.
type noopApplier struct{}

func (noopApplier) Apply([]byte) error           { return nil }
func (noopApplier) IsLeader() bool               { return true }
func (noopApplier) ForwardToLeader([]byte) error { return nil }

type routedNode struct {
	*testShardNode
	router  *ShardRouter
	api     *httptest.Server
	control *metadata.Store
	store   *metadata.ShardedStore
}

// newRoutedCluster gives every node of a shard cluster the store-level RPC
// endpoints and a ShardedStore that routes through them.
func newRoutedCluster(t *testing.T, n int, m *ShardMap) []*routedNode {
	t.Helper()
	base := newShardCluster(t, n, m)
	book := &addrBook{m: make(map[string]string, n)}

	out := make([]*routedNode, n)
	for i, sn := range base {
		router := NewShardRouter(sn.id, sn.rt, book.get, "shard-secret")
		router.SetMap(m)

		mux := http.NewServeMux()
		mux.HandleFunc(shardCallPath, router.CallHandler())
		mux.HandleFunc(shardApplyPath, router.ApplyHandler())
		api := httptest.NewServer(mux)
		t.Cleanup(api.Close)
		book.set(sn.id, strings.TrimPrefix(api.URL, "http://"))

		control, err := metadata.NewStore(filepath.Join(t.TempDir(), fmt.Sprintf("control-%d.db", i)))
		if err != nil {
			t.Fatalf("control store: %v", err)
		}
		t.Cleanup(func() { control.Close() })

		out[i] = &routedNode{
			testShardNode: sn,
			router:        router,
			api:           api,
			control:       control,
			store:         metadata.NewShardedStore(metadata.NewDistributedStore(control, noopApplier{}), router),
		}
	}
	return out
}

// bucketInShard finds a bucket name that hashes to the wanted shard, so a test
// can aim a write at a specific group without hardcoding a hash.
func bucketInShard(t *testing.T, shard, shards int) string {
	t.Helper()
	for i := 0; i < 1000; i++ {
		name := fmt.Sprintf("bucket-%d", i)
		if ShardForBucket(name, shards) == shard {
			return name
		}
	}
	t.Fatalf("no bucket name hashes to shard %d of %d", shard, shards)
	return ""
}

func nonMember(t *testing.T, nodes []*routedNode, m *ShardMap, shard int) *routedNode {
	t.Helper()
	for _, n := range nodes {
		if !m.HoldsShard(n.id, shard) {
			return n
		}
	}
	t.Fatalf("every node holds shard %d, so the RPC hop cannot be exercised", shard)
	return nil
}

// The point of P3: a node that holds no copy of a bucket's metadata shard can
// still read and write that bucket's objects, and the records land in the owning
// group rather than on the node that took the request.
func TestObjectMetadataRoutesToTheOwningShard(t *testing.T) {
	m, err := BuildShardMap(2, 2, ringOf("node-0", "node-1", "node-2"), 1)
	if err != nil {
		t.Fatal(err)
	}
	nodes := newRoutedCluster(t, 3, m)
	shardLeader(t, nodesOf(nodes), 0)
	shardLeader(t, nodesOf(nodes), 1)

	bucket := bucketInShard(t, 0, m.Shards)
	writer := nonMember(t, nodes, m, 0)

	meta := metadata.ObjectMeta{Bucket: bucket, Key: "routed.txt", Size: 42, ETag: "etag-1"}
	if err := writer.store.PutObjectMeta(meta); err != nil {
		t.Fatalf("write from a node outside the shard: %v", err)
	}

	// Every member of shard 0 converges on the record.
	for _, n := range nodes {
		if !m.HoldsShard(n.id, 0) {
			continue
		}
		n := n
		eventually(t, 15*time.Second, fmt.Sprintf("%s holds the routed record", n.id), func() bool {
			g, err := n.rt.Group(0)
			if err != nil {
				return false
			}
			got, err := g.Store().GetObjectMeta(bucket, "routed.txt")
			return err == nil && got != nil && got.Size == 42
		})
	}

	// It is nowhere else: not in the writer's control store, not in shard 1.
	if got, _ := writer.control.GetObjectMeta(bucket, "routed.txt"); got != nil {
		t.Fatal("an object write reached the control store, which no longer indexes objects")
	}
	for _, n := range nodes {
		if !m.HoldsShard(n.id, 1) {
			continue
		}
		g, err := n.rt.Group(1)
		if err != nil {
			continue
		}
		if got, _ := g.Store().GetObjectMeta(bucket, "routed.txt"); got != nil {
			t.Fatalf("%s: a shard 0 record leaked into shard 1", n.id)
		}
	}

	// Read it back through the store on a node that holds nothing, which is the
	// hop the whole design turns on.
	got, err := writer.store.GetObjectMeta(bucket, "routed.txt")
	if err != nil {
		t.Fatalf("read from a node outside the shard: %v", err)
	}
	if got == nil || got.ETag != "etag-1" {
		t.Fatalf("routed read returned %+v", got)
	}

	// And on a member, where it is served locally.
	for _, n := range nodes {
		if !m.HoldsShard(n.id, 0) {
			continue
		}
		n := n
		eventually(t, 15*time.Second, fmt.Sprintf("%s serves the record locally", n.id), func() bool {
			got, err := n.store.GetObjectMeta(bucket, "routed.txt")
			return err == nil && got != nil
		})
		break
	}

	// Listing routes the same way.
	listed, _, err := writer.store.ListLatestObjects(bucket, "", "", 100)
	if err != nil {
		t.Fatalf("routed listing: %v", err)
	}
	if len(listed) != 1 || listed[0].Key != "routed.txt" {
		t.Fatalf("routed listing returned %d records: %+v", len(listed), listed)
	}
}

// The rule the whole feature depends on: a shard that cannot be asked reports
// that it cannot be asked. Answering "no such object" would be indistinguishable
// from an empty shard, and the reclaimer deletes data it believes is orphaned.
func TestUnreachableShardIsUnavailableNotEmpty(t *testing.T) {
	m, err := BuildShardMap(2, 2, ringOf("node-0", "node-1", "node-2"), 1)
	if err != nil {
		t.Fatal(err)
	}
	nodes := newRoutedCluster(t, 3, m)
	shardLeader(t, nodesOf(nodes), 0)

	bucket := bucketInShard(t, 0, m.Shards)
	reader := nonMember(t, nodes, m, 0)

	// Take every member of the shard off the network.
	for _, n := range nodes {
		if m.HoldsShard(n.id, 0) {
			n.api.Close()
		}
	}

	meta, err := reader.store.GetObjectMeta(bucket, "anything")
	if err == nil {
		t.Fatal("an unreachable shard reported success")
	}
	if meta != nil {
		t.Fatalf("an unreachable shard returned a record: %+v", meta)
	}
	if !errors.Is(err, metadata.ErrShardUnavailable) {
		t.Fatalf("error does not say the shard was unavailable: %v", err)
	}

	// A write must fail the same way rather than being dropped.
	if err := reader.store.PutObjectMeta(metadata.ObjectMeta{Bucket: bucket, Key: "k"}); err == nil {
		t.Fatal("a write to an unreachable shard reported success")
	} else if !errors.Is(err, metadata.ErrShardUnavailable) {
		t.Fatalf("write error does not say the shard was unavailable: %v", err)
	}
}

// A caller routing by a different assignment must be refused, not served. Writing
// a bucket's records into the wrong group would hide them from every reader that
// routes correctly.
func TestShardRPCRefusesAMismatchedAssignment(t *testing.T) {
	m, err := BuildShardMap(4, 1, ringOf("node-0"), 1)
	if err != nil {
		t.Fatal(err)
	}
	nodes := newRoutedCluster(t, 1, m)
	receiver := nodes[0]

	bucket := bucketInShard(t, 0, m.Shards)
	wrong := (ShardForBucket(bucket, m.Shards) + 1) % m.Shards

	body := fmt.Sprintf(`{"shard":%d,"request":{"op":"get_object_meta","bucket":%q,"key":"k"}}`, wrong, bucket)
	req, _ := http.NewRequest(http.MethodPost, receiver.api.URL+shardCallPath, strings.NewReader(body))
	req.Header.Set(clusterSecretHeader, "shard-secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("a request addressed to the wrong shard returned %d, want 409", resp.StatusCode)
	}
}

// A write that reaches a member which is not the leader must name the leader
// rather than apply anything, so the caller retries in one hop.
func TestShardWriteToAFollowerNamesTheLeader(t *testing.T) {
	m, err := BuildShardMap(1, 3, ringOf("node-0", "node-1", "node-2"), 1)
	if err != nil {
		t.Fatal(err)
	}
	nodes := newRoutedCluster(t, 3, m)
	leader := shardLeader(t, nodesOf(nodes), 0)

	var follower *routedNode
	for _, n := range nodes {
		if n.id != leader.id {
			follower = n
			break
		}
	}
	cmd, err := marshalCommand(CmdPutObjectMeta, metadata.ObjectMeta{Bucket: "b", Key: "k"})
	if err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf(`{"shard":0,"bucket":"b","data":%q}`, base64.StdEncoding.EncodeToString(cmd))
	req, _ := http.NewRequest(http.MethodPost, follower.api.URL+shardApplyPath, strings.NewReader(body))
	req.Header.Set(clusterSecretHeader, "shard-secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("a write to a shard follower returned %d, want 503", resp.StatusCode)
	}
	if got := resp.Header.Get(shardLeaderHeader); got != leader.id {
		t.Fatalf("follower named %q as the shard leader, want %q", got, leader.id)
	}
	// And it applied nothing of its own.
	g, err := follower.rt.Group(0)
	if err != nil {
		t.Fatal(err)
	}
	if meta, _ := g.Store().GetObjectMeta("b", "k"); meta != nil {
		t.Fatal("a shard follower applied a write it should have redirected")
	}
}

func nodesOf(routed []*routedNode) []*testShardNode {
	out := make([]*testShardNode, len(routed))
	for i, n := range routed {
		out[i] = n.testShardNode
	}
	return out
}

// A listing must come from the shard's leader. A follower can be an entry
// behind, and a listing served from one omits a key the client was told was
// stored, which is the failure issue #37 was about on the control group.
func TestShardListingsComeFromTheShardLeader(t *testing.T) {
	m, err := BuildShardMap(1, 3, ringOf("node-0", "node-1", "node-2"), 1)
	if err != nil {
		t.Fatal(err)
	}
	nodes := newRoutedCluster(t, 3, m)
	leader := shardLeader(t, nodesOf(nodes), 0)

	var follower *routedNode
	for _, n := range nodes {
		if n.id != leader.id {
			follower = n
			break
		}
	}
	bucket := bucketInShard(t, 0, m.Shards)

	// One record committed through the group: every member has it.
	if err := follower.store.PutObjectMeta(metadata.ObjectMeta{Bucket: bucket, Key: "committed"}); err != nil {
		t.Fatalf("write: %v", err)
	}
	eventually(t, 15*time.Second, "the follower applies the committed write", func() bool {
		g, err := follower.rt.Group(0)
		if err != nil {
			return false
		}
		meta, _ := g.Store().GetObjectMeta(bucket, "committed")
		return meta != nil
	})

	// And one written straight into the follower's copy, which the group never
	// ordered. A listing that shows it was served from the follower.
	g, err := follower.rt.Group(0)
	if err != nil {
		t.Fatal(err)
	}
	if err := g.Store().PutObjectMeta(metadata.ObjectMeta{Bucket: bucket, Key: "follower-only"}); err != nil {
		t.Fatal(err)
	}

	listed, _, err := follower.store.ListLatestObjects(bucket, "", "", 100)
	if err != nil {
		t.Fatalf("listing from a follower: %v", err)
	}
	for _, meta := range listed {
		if meta.Key == "follower-only" {
			t.Fatal("the listing was served from a shard follower, not its leader")
		}
	}
	if len(listed) != 1 || listed[0].Key != "committed" {
		t.Fatalf("listing returned %d records: %+v", len(listed), listed)
	}
}
