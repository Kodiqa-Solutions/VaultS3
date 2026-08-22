package cluster

import (
	"fmt"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
	"github.com/hashicorp/raft"
)

// The control group has to keep working exactly as it does today when its
// traffic arrives through the mux, because that is what the shard runtime shares
// a port with. This runs a real three-node control group over real sockets, with
// shard layers registered alongside, and checks that consensus is undisturbed.
func TestControlGroupRunsOverTheMux(t *testing.T) {
	const n = 3
	type muxNode struct {
		id   string
		addr string
		node *Node
	}

	nodes := make([]*muxNode, n)
	muxes := make([]*TransportMux, n)
	for i := 0; i < n; i++ {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("listen: %v", err)
		}
		mux := NewTransportMux(ln)
		t.Cleanup(func() { mux.Close() })
		muxes[i] = mux

		// Shard layers exist on the same port, so any bleed between them and the
		// control group would show up here.
		if _, err := mux.ShardLayer(0); err != nil {
			t.Fatal(err)
		}
		if _, err := mux.ShardLayer(1); err != nil {
			t.Fatal(err)
		}

		store, err := metadata.NewStore(filepath.Join(t.TempDir(), fmt.Sprintf("meta-%d.db", i)))
		if err != nil {
			t.Fatalf("store: %v", err)
		}
		t.Cleanup(func() { store.Close() })

		trans := raft.NewNetworkTransportWithConfig(&raft.NetworkTransportConfig{
			Stream:  mux.ControlLayer(),
			MaxPool: 3,
			Timeout: raftTimeout,
		})
		rstore := raft.NewInmemStore()
		node, err := newNodeWithDeps(
			ClusterConfig{NodeID: fmt.Sprintf("node-%d", i), Bootstrap: i == 0},
			store,
			raftDeps{transport: trans, logStore: rstore, stable: rstore, snapshots: raft.NewInmemSnapshotStore()},
		)
		if err != nil {
			t.Fatalf("node %d: %v", i, err)
		}
		t.Cleanup(func() { node.Shutdown() })
		nodes[i] = &muxNode{id: fmt.Sprintf("node-%d", i), addr: ln.Addr().String(), node: node}
	}

	eventually(t, 20*time.Second, "node-0 bootstraps", func() bool { return nodes[0].node.IsLeader() })
	for i := 1; i < n; i++ {
		if err := nodes[0].node.Join(nodes[i].id, nodes[i].addr); err != nil {
			t.Fatalf("join %s: %v", nodes[i].id, err)
		}
	}
	eventually(t, 20*time.Second, "every node sees the full membership", func() bool {
		for _, nd := range nodes {
			servers, err := nd.node.Servers()
			if err != nil || len(servers) != n {
				return false
			}
		}
		return true
	})

	if err := createBucketOn(t, nodes[0].node, "over-the-mux"); err != nil {
		t.Fatalf("apply through the muxed transport: %v", err)
	}
	for _, nd := range nodes {
		nd := nd
		eventually(t, 15*time.Second, fmt.Sprintf("%s replicates the write", nd.id), func() bool {
			b, err := nd.node.store.GetBucket("over-the-mux")
			return err == nil && b != nil
		})
	}
}
