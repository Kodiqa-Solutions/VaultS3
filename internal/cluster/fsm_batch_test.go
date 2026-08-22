package cluster

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
	"github.com/hashicorp/raft"
)

// The FSM must satisfy raft.BatchingFSM, or Raft silently falls back to applying
// one entry per transaction and the whole point of issue #50's fix is lost.
var _ raft.BatchingFSM = (*FSM)(nil)

func newBatchTestFSM(t *testing.T) (*FSM, *metadata.Store) {
	t.Helper()
	store, err := metadata.NewStore(filepath.Join(t.TempDir(), "meta.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	if err := store.CreateBucket("b"); err != nil {
		t.Fatal(err)
	}
	return NewFSM(store), store
}

func cmdLog(t *testing.T, index uint64, typ CommandType, payload interface{}) *raft.Log {
	t.Helper()
	data, err := marshalCommand(typ, payload)
	if err != nil {
		t.Fatal(err)
	}
	return &raft.Log{Index: index, Type: raft.LogCommand, Data: data}
}

func meta(key string, size int64) metadata.ObjectMeta {
	return metadata.ObjectMeta{Bucket: "b", Key: key, Size: size, ETag: "e"}
}

func TestApplyBatchAppliesEveryEntryInOrder(t *testing.T) {
	fsm, store := newBatchTestFSM(t)

	logs := []*raft.Log{
		cmdLog(t, 1, CmdPutObjectMeta, meta("a", 1)),
		cmdLog(t, 2, CmdPutObjectMeta, meta("b", 2)),
		// A non-object command in the middle must break the run without being
		// skipped or reordered.
		cmdLog(t, 3, CmdCreateBucket, struct{ Name string }{"other"}),
		cmdLog(t, 4, CmdPutObjectMeta, meta("c", 3)),
		// Two writes of the same key inside one run: the later one must win.
		cmdLog(t, 5, CmdPutObjectMeta, meta("a", 99)),
	}

	resps := fsm.ApplyBatch(logs)
	if len(resps) != len(logs) {
		t.Fatalf("raft panics unless responses match the batch: got %d want %d", len(resps), len(logs))
	}
	for i, r := range resps {
		if err, ok := r.(error); ok && err != nil {
			t.Fatalf("entry %d returned %v", i, err)
		}
	}
	if got := fsm.AppliedIndex(); got != 5 {
		t.Fatalf("applied index = %d, want 5", got)
	}

	for _, want := range []struct {
		key  string
		size int64
	}{{"a", 99}, {"b", 2}, {"c", 3}} {
		got, err := store.GetObjectMeta("b", want.key)
		if err != nil || got == nil {
			t.Fatalf("%s missing: %v", want.key, err)
		}
		if got.Size != want.size {
			t.Fatalf("%s size = %d, want %d (last write in the batch must win)", want.key, got.Size, want.size)
		}
	}
	if !store.BucketExists("other") {
		t.Fatal("the non-object command in the middle of the batch was not applied")
	}
}

// Batched application must be indistinguishable from one-at-a-time application,
// including the incrementally maintained per-bucket counters.
func TestApplyBatchMatchesOneAtATime(t *testing.T) {
	seq := []*raft.Log{
		cmdLog(t, 1, CmdPutObjectMeta, meta("x", 10)),
		cmdLog(t, 2, CmdPutObjectMeta, meta("y", 20)),
		cmdLog(t, 3, CmdPutObjectMeta, meta("x", 5)),
		cmdLog(t, 4, CmdDeleteObjectMeta, struct{ Bucket, Key string }{"b", "y"}),
		cmdLog(t, 5, CmdPutObjectMeta, meta("z", 7)),
	}

	batched, batchStore := newBatchTestFSM(t)
	batched.ApplyBatch(seq)

	single, singleStore := newBatchTestFSM(t)
	for _, l := range seq {
		single.Apply(l)
	}

	bs, _, err := batchStore.BucketStats("b")
	if err != nil {
		t.Fatal(err)
	}
	ss, _, err := singleStore.BucketStats("b")
	if err != nil {
		t.Fatal(err)
	}
	if bs != ss {
		t.Fatalf("bucket stats diverged: batched %+v, one-at-a-time %+v", bs, ss)
	}

	bl, _, err := batchStore.ListLatestObjects("b", "", "", 100)
	if err != nil {
		t.Fatal(err)
	}
	sl, _, err := singleStore.ListLatestObjects("b", "", "", 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(bl) != len(sl) {
		t.Fatalf("object count diverged: batched %d, one-at-a-time %d", len(bl), len(sl))
	}
	for i := range bl {
		if bl[i].Key != sl[i].Key || bl[i].Size != sl[i].Size {
			t.Fatalf("object %d diverged: batched %s/%d, one-at-a-time %s/%d",
				i, bl[i].Key, bl[i].Size, sl[i].Key, sl[i].Size)
		}
	}
}

// A corrupt entry must not stall the batch or take its neighbours down with it.
func TestApplyBatchSurvivesUndecodableEntry(t *testing.T) {
	fsm, store := newBatchTestFSM(t)

	bad, err := json.Marshal(Command{Type: CmdPutObjectMeta, Data: json.RawMessage(`"not-an-object"`)})
	if err != nil {
		t.Fatal(err)
	}
	logs := []*raft.Log{
		cmdLog(t, 1, CmdPutObjectMeta, meta("before", 1)),
		{Index: 2, Type: raft.LogCommand, Data: bad},
		cmdLog(t, 3, CmdPutObjectMeta, meta("after", 1)),
	}

	resps := fsm.ApplyBatch(logs)
	if len(resps) != 3 {
		t.Fatalf("responses = %d, want 3", len(resps))
	}
	if err, ok := resps[1].(error); !ok || err == nil {
		t.Fatalf("the undecodable entry should report an error, got %v", resps[1])
	}
	for _, key := range []string{"before", "after"} {
		got, err := store.GetObjectMeta("b", key)
		if err != nil || got == nil {
			t.Fatalf("%q was lost alongside the bad entry: %v", key, err)
		}
	}
}

// Raft sends configuration entries to a batching FSM too. They carry no command,
// so they must be passed over silently while still occupying a response slot.
func TestApplyBatchIgnoresNonCommandEntries(t *testing.T) {
	fsm, store := newBatchTestFSM(t)
	logs := []*raft.Log{
		{Index: 1, Type: raft.LogConfiguration, Data: []byte("whatever")},
		cmdLog(t, 2, CmdPutObjectMeta, meta("k", 1)),
	}
	resps := fsm.ApplyBatch(logs)
	if len(resps) != 2 {
		t.Fatalf("responses = %d, want 2", len(resps))
	}
	if resps[0] != nil {
		t.Fatalf("configuration entry produced %v, want nil", resps[0])
	}
	if got, err := store.GetObjectMeta("b", "k"); err != nil || got == nil {
		t.Fatalf("command after a configuration entry was not applied: %v", err)
	}
}
