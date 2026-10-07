package metadata

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

// Every bolt bucket must be classified as Raft-owned, node-local or mixed, so a
// new bucket forces a decision instead of quietly landing on one side. Both the
// declarations in the source and the buckets a live store actually creates are
// checked: the second catches a bucket named inline.
func TestEveryBoltBucketIsClassified(t *testing.T) {
	declared := regexp.MustCompile(`(?m)^\s*(?:var\s+)?(\w+Bucket)\s*=\s*\[\]byte\("([^"]+)"\)`)
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]string{}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range declared.FindAllStringSubmatch(string(src), -1) {
			names[m[2]] = f + ":" + m[1]
		}
	}
	if len(names) < 25 {
		t.Fatalf("found only %d bucket declarations, the scan is not seeing the store", len(names))
	}

	// Buckets a running store creates, including the lazily created ones.
	s := openStore(t)
	if err := s.PutBucketSnapshot(BucketSnapshot{ID: "x", Bucket: "b"}); err != nil {
		t.Fatal(err)
	}
	if err := s.beginRestore(); err != nil {
		t.Fatal(err)
	}
	if err := s.finishRestore(); err != nil {
		t.Fatal(err)
	}
	s.db.View(func(tx *bolt.Tx) error {
		return tx.ForEach(func(name []byte, _ *bolt.Bucket) error {
			if _, ok := names[string(name)]; !ok {
				names[string(name)] = "created at runtime"
			}
			return nil
		})
	})

	for name, where := range names {
		if bucketScopeOf([]byte(name)) == scopeUnknown {
			t.Errorf("bolt bucket %q (%s) is in neither raftOwnedBuckets, nodeLocalBuckets nor mixedBuckets: "+
				"decide whether Raft or this node owns it, in raftscope.go", name, where)
		}
	}
	// And no bucket may be claimed by two lists.
	for _, b := range raftOwnedBuckets {
		for _, l := range nodeLocalBuckets {
			if bytes.Equal(b, l) {
				t.Errorf("bucket %q is both Raft-owned and node-local", b)
			}
		}
	}
}

// snapshotBucketNames lists the buckets a current-format snapshot carries.
func snapshotBucketNames(t *testing.T, snap []byte) []string {
	t.Helper()
	if !bytes.HasPrefix(snap, snapshotMagic) {
		t.Errorf("snapshot does not start with the format header")
		return nil
	}
	r := bytes.NewReader(snap[len(snapshotMagic):])
	var names []string
	for {
		name, err := readBytes(r)
		if err != nil {
			t.Fatalf("read bucket name: %v", err)
		}
		if len(name) == 0 {
			return names
		}
		names = append(names, string(name))
		var seq uint64
		if err := binary.Read(r, binary.BigEndian, &seq); err != nil {
			t.Fatal(err)
		}
		for {
			k, err := readBytes(r)
			if err != nil {
				t.Fatal(err)
			}
			if len(k) == 0 {
				break
			}
			if _, err := readBytes(r); err != nil {
				t.Fatal(err)
			}
		}
	}
}

// seedNodeLocal writes one record into every node-local place a node keeps
// state, tagged so the test can tell whose copy survived.
func seedNodeLocal(t *testing.T, s *Store, tag string) {
	t.Helper()
	if err := s.CreateMultipartUpload(MultipartUpload{UploadID: "up-" + tag, Bucket: "b", Key: "k"}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutPart("up-"+tag, PartInfo{PartNumber: 1, ETag: "e-" + tag, Size: 5}); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendChangeLog([]byte("change-" + tag)); err != nil {
		t.Fatal(err)
	}
	if err := s.PutAuditEntry(AuditEntry{Time: time.Now().UnixNano(), Principal: tag}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetJWTSigningKey([]byte("jwt-" + tag)); err != nil {
		t.Fatal(err)
	}
	if err := s.SetAdminCredentials("ak-"+tag, "sk-"+tag); err != nil {
		t.Fatal(err)
	}
	if err := s.PutReplicationConfig("b", "repl-"+tag); err != nil {
		t.Fatal(err)
	}
	if err := s.PutBucketSnapshot(BucketSnapshot{ID: "snap-" + tag, Bucket: "b"}); err != nil {
		t.Fatal(err)
	}
}

// assertNodeLocal checks that s holds exactly the node-local records seeded with
// want, and none seeded with other.
func assertNodeLocal(t *testing.T, s *Store, want, other string) {
	t.Helper()
	if u, _ := s.GetMultipartUpload("up-" + want); u == nil {
		t.Errorf("this node's in-progress multipart upload was lost")
	}
	if parts, _ := s.ListParts("up-" + want); len(parts) != 1 {
		t.Errorf("this node's uploaded parts were lost: %d left", len(parts))
	}
	if u, _ := s.GetMultipartUpload("up-" + other); u != nil {
		t.Errorf("the snapshot's source node's multipart upload was copied onto this node")
	}
	if seq, _ := s.ChangeLogSeq(); seq != 1 {
		t.Errorf("change log sequence is %d after the restore, want this node's 1", seq)
	}
	entries, _ := s.ReadChangeLog(0, 10)
	if len(entries) != 1 || !strings.Contains(string(entries[0].Value), want) {
		t.Errorf("change log is %v, want only this node's entry", entries)
	}
	audit, _ := s.ListAuditEntries(10, 0, 0, "", "")
	if len(audit) != 1 || audit[0].Principal != want {
		t.Errorf("audit trail is %+v, want only this node's entry", audit)
	}
	if k, _ := s.GetJWTSigningKey(); string(k) != "jwt-"+want {
		t.Errorf("console signing key is %q, want this node's", k)
	}
	if ak, _, _ := s.GetAdminCredentials(); ak != "ak-"+want {
		t.Errorf("admin access key is %q, want this node's", ak)
	}
	if c, _ := s.GetReplicationConfig("b"); c != "repl-"+want {
		t.Errorf("replication config is %q, want this node's", c)
	}
	if sn, _ := s.GetBucketSnapshot("b", "snap-"+want); sn == nil {
		t.Errorf("this node's bucket snapshot was lost")
	}
}

// Restoring a snapshot, which hashicorp/raft does on every start once one exists,
// must leave everything the node keeps for itself exactly as it was, and must
// not copy the source node's records onto it.
func TestRestoreKeepsNodeLocalState(t *testing.T) {
	src := openStore(t)
	if err := src.CreateBucket("b"); err != nil {
		t.Fatal(err)
	}
	if err := src.PutObjectMeta(ObjectMeta{Bucket: "b", Key: "obj"}); err != nil {
		t.Fatal(err)
	}
	if err := src.PutShardMap([]byte(`{"v":1}`)); err != nil {
		t.Fatal(err)
	}
	seedNodeLocal(t, src, "leader")
	var buf bytes.Buffer
	if err := src.WriteSnapshot(&buf); err != nil {
		t.Fatal(err)
	}

	for _, name := range snapshotBucketNames(t, buf.Bytes()) {
		if scope := bucketScopeOf([]byte(name)); scope != scopeRaft && scope != scopeMixed {
			t.Errorf("snapshot carries node-local bucket %q", name)
		}
	}

	dst := openStore(t)
	seedNodeLocal(t, dst, "follower")
	if err := dst.RestoreSnapshot(bytes.NewReader(buf.Bytes())); err != nil {
		t.Fatalf("restore: %v", err)
	}

	if m, _ := dst.GetObjectMeta("b", "obj"); m == nil {
		t.Fatal("the Raft-owned object did not arrive")
	}
	if m, _ := dst.GetShardMap(); string(m) != `{"v":1}` {
		t.Fatalf("shard map = %q, want the snapshot's", m)
	}
	assertNodeLocal(t, dst, "follower", "leader")
}

// writeFirstFormatSnapshot writes a snapshot exactly as versions before the
// format header did: every bucket, each prefixed with its key count.
func writeFirstFormatSnapshot(t *testing.T, s *Store, w io.Writer) {
	t.Helper()
	err := s.db.View(func(tx *bolt.Tx) error {
		return tx.ForEach(func(name []byte, b *bolt.Bucket) error {
			if err := writeBytes(w, name); err != nil {
				return err
			}
			var count uint64
			b.ForEach(func(k, v []byte) error { count++; return nil })
			if err := binary.Write(w, binary.BigEndian, count); err != nil {
				return err
			}
			return b.ForEach(func(k, v []byte) error {
				if err := writeBytes(w, k); err != nil {
					return err
				}
				return writeBytes(w, v)
			})
		})
	})
	if err != nil {
		t.Fatal(err)
	}
}

// A snapshot written by an older version holds node-local buckets too. It must
// still install, and those buckets must be read past rather than restored.
func TestRestoreFirstFormatSnapshotSkipsNodeLocal(t *testing.T) {
	src := openStore(t)
	if err := src.CreateBucket("old"); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"a", "b", "c"} {
		if err := src.PutObjectMeta(ObjectMeta{Bucket: "old", Key: k}); err != nil {
			t.Fatal(err)
		}
	}
	if err := src.PutShardMap([]byte(`{"v":7}`)); err != nil {
		t.Fatal(err)
	}
	seedNodeLocal(t, src, "leader")
	var buf bytes.Buffer
	writeFirstFormatSnapshot(t, src, &buf)
	if bytes.HasPrefix(buf.Bytes(), snapshotMagic) {
		t.Fatal("fixture is not in the first format")
	}

	dst := openStore(t)
	seedNodeLocal(t, dst, "follower")
	if err := dst.RestoreSnapshot(bytes.NewReader(buf.Bytes())); err != nil {
		t.Fatalf("restore of a first-format snapshot: %v", err)
	}
	for _, k := range []string{"a", "b", "c"} {
		if m, _ := dst.GetObjectMeta("old", k); m == nil {
			t.Fatalf("object %s from the first-format snapshot is missing", k)
		}
	}
	if !dst.BucketExists("old") {
		t.Fatal("bucket from the first-format snapshot is missing")
	}
	if m, _ := dst.GetShardMap(); string(m) != `{"v":7}` {
		t.Fatalf("shard map = %q, want the snapshot's", m)
	}
	assertNodeLocal(t, dst, "follower", "leader")
}

// The current format must round-trip the Raft-owned state, leave the node-local
// push replication queue out, and refuse a stream that stops early instead of
// taking it for a complete snapshot.
func TestCurrentFormatRoundTripAndTruncation(t *testing.T) {
	src := openStore(t)
	if err := src.CreateBucket("b"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 50; i++ {
		if err := src.PutObjectMeta(ObjectMeta{Bucket: "b", Key: fmt.Sprintf("k%02d", i), Size: int64(i)}); err != nil {
			t.Fatal(err)
		}
		if err := src.EnqueueReplication(ReplicationEvent{Bucket: "b", Key: "k", Peer: "p"}); err != nil {
			t.Fatal(err)
		}
	}
	var buf bytes.Buffer
	if err := src.WriteSnapshot(&buf); err != nil {
		t.Fatal(err)
	}
	snap := buf.Bytes()

	dst := openStore(t)
	if err := dst.RestoreSnapshot(bytes.NewReader(snap)); err != nil {
		t.Fatalf("restore: %v", err)
	}
	objs, _, err := dst.ListLatestObjects("b", "", "", 0)
	if err != nil || len(objs) != 50 {
		t.Fatalf("%d objects after restore (%v), want 50", len(objs), err)
	}
	// Push replication is per node: the node that served a write sends it, so
	// another node's queue must not arrive with a snapshot.
	if n, _ := dst.ReplicationQueueDepth(); n != 0 {
		t.Fatalf("queue depth %d after restore, want 0: the node-local queue was carried", n)
	}

	for _, cut := range []int{len(snap) - 1, len(snap) - 4, len(snap) / 2} {
		fresh := openStore(t)
		if err := fresh.RestoreSnapshot(bytes.NewReader(snap[:cut])); err == nil {
			t.Errorf("a snapshot cut at %d of %d bytes restored without error", cut, len(snap))
		}
	}
}

// An interrupted restore is cleared on the next open, and that clearing must
// reach only the Raft-owned state the restore was replacing.
func TestInterruptedRestoreKeepsNodeLocalState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "meta.db")
	s, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	seedNodeLocal(t, s, "self")
	if err := s.beginRestore(); err != nil {
		t.Fatal(err)
	}
	s.Close()

	reopened, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if reopened.restoreWasInterrupted() {
		t.Fatal("marker survived the reopen")
	}
	assertNodeLocal(t, reopened, "self", "nobody")
}

// The snapshot is the store as it was when it was taken, not when it is written
// out: Raft records the index at the first moment and keeps applying entries
// while the second happens.
func TestRaftSnapshotIsPointInTime(t *testing.T) {
	s := openStore(t)
	if err := s.CreateBucket("b"); err != nil {
		t.Fatal(err)
	}
	if err := s.PutObjectMeta(ObjectMeta{Bucket: "b", Key: "before"}); err != nil {
		t.Fatal(err)
	}
	// Grow the file first, so the memory map has room for one more small write
	// and that write does not have to wait for the snapshot to be released.
	pad := make([]ObjectMeta, 2000)
	for i := range pad {
		pad[i] = ObjectMeta{Bucket: "pad", Key: fmt.Sprintf("p%05d", i), ETag: strings.Repeat("x", 200)}
	}
	if err := s.PutObjectMetaBatch(pad); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteBucketObjectMeta("pad"); err != nil {
		t.Fatal(err)
	}
	sn, err := s.BeginRaftSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	defer sn.Release()
	// The write runs on its own goroutine, as Raft's apply does. bbolt makes a
	// write that has to grow the file wait for open read transactions, so on
	// this goroutine it could wait on the snapshot forever.
	wrote := make(chan error, 1)
	go func() { wrote <- s.PutObjectMeta(ObjectMeta{Bucket: "b", Key: "after"}) }()
	select {
	case err := <-wrote:
		if err != nil {
			t.Fatal(err)
		}
		wrote <- nil
	case <-time.After(5 * time.Second):
		t.Fatal("the write waited on the snapshot's read transaction, so this test cannot tell a live view from a point in time")
	}
	var buf bytes.Buffer
	if err := sn.Encode(&buf); err != nil {
		t.Fatal(err)
	}
	sn.Release()
	if err := <-wrote; err != nil {
		t.Fatal(err)
	}

	dst := openStore(t)
	if err := dst.RestoreSnapshot(bytes.NewReader(buf.Bytes())); err != nil {
		t.Fatal(err)
	}
	if m, _ := dst.GetObjectMeta("b", "before"); m == nil {
		t.Fatal("object written before the snapshot is missing")
	}
	if m, _ := dst.GetObjectMeta("b", "after"); m != nil {
		t.Fatal("object written after the snapshot was taken is in it")
	}
}

// Replaying an enqueue with the same ID must not add a second event.
func TestEnqueueReplicationWithIDIsIdempotent(t *testing.T) {
	s := openStore(t)
	ev := ReplicationEvent{Bucket: "b", Key: "k", Peer: "p", CreatedAt: 1}
	for i := 0; i < 2; i++ {
		if err := s.EnqueueReplicationWithID(ev, 42); err != nil {
			t.Fatal(err)
		}
	}
	q, _ := s.ListReplicationQueue(10)
	if len(q) != 1 || q[0].ID != 42 {
		t.Fatalf("queue = %+v, want one event with ID 42", q)
	}
	// A plain enqueue afterwards must not collide with it.
	if err := s.EnqueueReplication(ReplicationEvent{Bucket: "b", Key: "k2", Peer: "p"}); err != nil {
		t.Fatal(err)
	}
	q, _ = s.ListReplicationQueue(10)
	ids := []int{}
	for _, e := range q {
		ids = append(ids, int(e.ID))
	}
	sort.Ints(ids)
	if len(ids) != 2 || ids[0] != 42 || ids[1] <= 42 {
		t.Fatalf("IDs %v, want 42 and a fresh one above it", ids)
	}
}
