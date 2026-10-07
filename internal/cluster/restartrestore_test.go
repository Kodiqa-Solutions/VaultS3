package cluster

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
	"github.com/hashicorp/raft"
)

// hashicorp/raft installs the latest snapshot into the FSM every time a node
// starts once a snapshot exists. That used to rebuild the whole metadata file
// from the snapshot, so a plain restart lost every in-progress multipart upload
// (node-local since issue #32) and the rest of the node's own records. Here a
// node takes a snapshot, keeps working, restarts on the same stores, and must
// come back with its uploads and its committed state.
func TestRestartKeepsNodeLocalStateAcrossSnapshotRestore(t *testing.T) {
	store, err := metadata.NewStore(filepath.Join(t.TempDir(), "meta.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	logs := raft.NewInmemStore()
	snaps := raft.NewInmemSnapshotStore()

	start := func() *Node {
		_, trans := raft.NewInmemTransport(raft.ServerAddress("solo"))
		n, err := newNodeWithDeps(ClusterConfig{NodeID: "solo", Bootstrap: true}, store,
			raftDeps{transport: trans, logStore: logs, stable: logs, snapshots: snaps})
		if err != nil {
			t.Fatal(err)
		}
		eventually(t, 15*time.Second, "solo node leads", n.IsLeader)
		return n
	}

	n := start()
	if err := createBucketOn(t, n, "before-snapshot"); err != nil {
		t.Fatal(err)
	}
	if err := n.raft.Snapshot().Error(); err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	// Written after the snapshot: a committed bucket, and this node's own
	// multipart upload and change log entry.
	if err := createBucketOn(t, n, "after-snapshot"); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateMultipartUpload(metadata.MultipartUpload{UploadID: "u1", Bucket: "after-snapshot", Key: "big"}); err != nil {
		t.Fatal(err)
	}
	if err := store.PutPart("u1", metadata.PartInfo{PartNumber: 1, ETag: "e", Size: 5 << 20}); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendChangeLog([]byte("local change")); err != nil {
		t.Fatal(err)
	}
	if err := n.Shutdown(); err != nil {
		t.Fatal(err)
	}

	n = start()
	defer n.Shutdown()

	// The entries after the snapshot are replayed once the restarted node
	// commits again, which can trail its election by a moment.
	for _, b := range []string{"before-snapshot", "after-snapshot"} {
		eventually(t, 10*time.Second, "committed bucket "+b+" present after restart", func() bool {
			return store.BucketExists(b)
		})
	}
	if u, _ := store.GetMultipartUpload("u1"); u == nil {
		t.Error("the in-progress multipart upload was lost by the restart (NoSuchUpload for the client)")
	}
	if parts, _ := store.ListParts("u1"); len(parts) != 1 {
		t.Errorf("uploaded parts lost by the restart: %d left", len(parts))
	}
	if seq, _ := store.ChangeLogSeq(); seq != 1 {
		t.Errorf("change log sequence went from 1 to %d across the restart", seq)
	}
}
