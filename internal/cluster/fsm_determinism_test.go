package cluster

import (
	"bytes"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
	"github.com/hashicorp/raft"
)

// memSink is a raft.SnapshotSink that keeps the bytes in memory.
type memSink struct {
	bytes.Buffer
	cancelled bool
}

func (m *memSink) ID() string    { return "mem" }
func (m *memSink) Cancel() error { m.cancelled = true; return nil }
func (m *memSink) Close() error  { return nil }

// growStore makes room in the store's memory map, so a small write while a
// snapshot is open does not have to wait for it (bbolt makes a write that
// grows the file wait for every open read transaction).
func growStore(t *testing.T, store *metadata.Store) {
	t.Helper()
	pad := make([]metadata.ObjectMeta, 2000)
	for i := range pad {
		pad[i] = metadata.ObjectMeta{Bucket: "pad", Key: fmt.Sprintf("p%05d", i), ETag: strings.Repeat("x", 200)}
	}
	if err := store.PutObjectMetaBatch(pad); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteBucketObjectMeta("pad"); err != nil {
		t.Fatal(err)
	}
}

// Raft records a snapshot's index when it calls Snapshot and keeps applying
// entries while Persist writes the bytes. The snapshot must hold the state at
// that index, or the entries after it end up in the snapshot AND get replayed
// on top of it after a restore.
func TestFSMSnapshotIsTakenWhenRaftAsksForIt(t *testing.T) {
	fsm, store := newBatchTestFSM(t)
	growStore(t, store)
	fsm.Apply(cmdLog(t, 1, CmdPutObjectMeta, meta("before", 1)))

	snap, err := fsm.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	defer snap.Release()

	// Apply carries on, on its own goroutine as in Raft.
	done := make(chan interface{}, 1)
	go func() { done <- fsm.Apply(cmdLog(t, 2, CmdPutObjectMeta, meta("after", 2))) }()
	select {
	case resp := <-done:
		if resp != nil {
			t.Fatalf("apply: %v", resp)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the apply waited on the snapshot, so this test cannot tell the two apart")
	}

	sink := &memSink{}
	if err := snap.Persist(sink); err != nil {
		t.Fatal(err)
	}

	dst, err := metadata.NewStore(t.TempDir() + "/dst.db")
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close()
	if err := NewFSM(dst).Restore(nopCloser{bytes.NewReader(sink.Bytes())}); err != nil {
		t.Fatal(err)
	}
	if m, _ := dst.GetObjectMeta("b", "before"); m == nil {
		t.Fatal("entry applied before the snapshot is missing from it")
	}
	if m, _ := dst.GetObjectMeta("b", "after"); m != nil {
		t.Fatal("entry applied AFTER the snapshot was taken is in it, and would be replayed on top of it")
	}
}

type nopCloser struct{ *bytes.Reader }

func (nopCloser) Close() error { return nil }

// Replaying an enqueue, which happens after every restart for the entries past
// the last snapshot, must not add a second event, and the event's ID must be
// the same on every node so an acknowledgement removes the right one.
func TestFSMEnqueueReplayIsIdempotent(t *testing.T) {
	fsm, store := newBatchTestFSM(t)
	other, otherStore := newBatchTestFSM(t)

	// The other node has already used some IDs of its own.
	for i := 0; i < 3; i++ {
		if err := otherStore.EnqueueReplication(metadata.ReplicationEvent{Bucket: "x", Key: "local", Peer: "p"}); err != nil {
			t.Fatal(err)
		}
	}
	otherStore.AckReplication(1)
	otherStore.AckReplication(2)
	otherStore.AckReplication(3)

	entry := cmdLog(t, 17, CmdEnqueueReplication, metadata.ReplicationEvent{Bucket: "b", Key: "k", Peer: "p", CreatedAt: 5})
	fsm.Apply(entry)
	fsm.Apply(entry) // replay
	other.ApplyBatch([]*raft.Log{entry})

	q, _ := store.ListReplicationQueue(10)
	if len(q) != 1 {
		t.Fatalf("replaying one enqueue left %d events, want 1", len(q))
	}
	oq, _ := otherStore.ListReplicationQueue(10)
	if len(oq) != 1 || oq[0].ID != q[0].ID {
		t.Fatalf("the two nodes stored the event under different IDs: %+v vs %+v", q, oq)
	}

	fsm.Apply(cmdLog(t, 18, CmdAckReplication, struct{ ID uint64 }{q[0].ID}))
	if n, _ := store.ReplicationQueueDepth(); n != 0 {
		t.Fatalf("ack by ID left %d events", n)
	}
}

// Times recorded by an apply must come from the log entry, not from the clock
// of whichever node applies it, or every node holds a different value.
func TestFSMAppliesTheSameTimeOnEveryNode(t *testing.T) {
	a, storeA := newBatchTestFSM(t)
	b, storeB := newBatchTestFSM(t)

	proposed := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	withTime := cmdLog(t, 1, CmdCreateBucket, struct {
		Name      string
		CreatedAt int64
	}{"stamped", proposed.UnixNano()})
	// An entry from an older proposer carries no time: the leader's append
	// time is the next best thing that every node agrees on.
	appended := time.Date(2021, 6, 7, 8, 9, 10, 0, time.UTC)
	legacy := cmdLog(t, 2, CmdCreateBucket, struct{ Name string }{"legacy"})
	legacy.AppendedAt = appended

	a.Apply(withTime)
	a.Apply(legacy)
	time.Sleep(20 * time.Millisecond)
	b.Apply(withTime)
	b.Apply(legacy)

	for _, c := range []struct {
		name string
		want time.Time
	}{{"stamped", proposed}, {"legacy", appended}} {
		ia, _ := storeA.GetBucket(c.name)
		ib, _ := storeB.GetBucket(c.name)
		if ia == nil || ib == nil {
			t.Fatalf("bucket %s missing", c.name)
		}
		if !ia.CreatedAt.Equal(c.want) || !ib.CreatedAt.Equal(c.want) {
			t.Errorf("bucket %s CreationDate = %v and %v, want %v on both nodes", c.name, ia.CreatedAt, ib.CreatedAt, c.want)
		}
	}

	// Expiring keys: the cut-off travels with the command.
	if err := storeA.CreateAccessKey(metadata.AccessKey{AccessKey: "sts", SecretKey: "s", ExpiresAt: 2000}); err != nil {
		t.Fatal(err)
	}
	a.Apply(cmdLog(t, 3, CmdDeleteExpiredAccessKeys, struct{ Now int64 }{1000}))
	if k, _ := storeA.GetAccessKey("sts"); k == nil {
		t.Fatal("a key that had not expired at the proposed time was deleted by the applying node's clock")
	}
	a.Apply(cmdLog(t, 4, CmdDeleteExpiredAccessKeys, struct{ Now int64 }{3000}))
	if k, _ := storeA.GetAccessKey("sts"); k != nil {
		t.Fatal("a key that had expired at the proposed time survived")
	}
}

// lockedBuffer collects log output from any goroutine.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// An apply that fails on a follower is invisible unless the FSM says so: Raft
// gives the result only to the leader's future. Store failures must be logged
// at error level with the command and index, answers like "already exists" not.
func TestFSMLogsApplyFailures(t *testing.T) {
	var out lockedBuffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&out, &slog.HandlerOptions{Level: slog.LevelInfo})))
	defer slog.SetDefault(prev)

	fsm, store := newBatchTestFSM(t)

	// A semantic refusal is not a failure of this node.
	fsm.Apply(cmdLog(t, 5, CmdCreateBucket, struct{ Name string }{"b"}))
	if strings.Contains(out.String(), "level=ERROR") {
		t.Fatalf("an already-existing bucket was logged as an error:\n%s", out.String())
	}

	// A store that cannot write is.
	store.Close()
	fsm.Apply(cmdLog(t, 9, CmdPutBucketPolicy, struct {
		Bucket string
		Policy []byte
	}{"b", []byte("{}")}))
	fsm.ApplyBatch([]*raft.Log{cmdLog(t, 10, CmdDeleteBucketPolicy, struct{ Bucket string }{"b"})})

	got := out.String()
	for _, want := range []string{"index=9", "index=10"} {
		if !strings.Contains(got, want) {
			t.Errorf("failed apply at %s was not logged:\n%s", want, got)
		}
	}
	if !strings.Contains(got, "level=ERROR") {
		t.Errorf("failed apply was not logged at error level:\n%s", got)
	}
}
