package metadata

import (
	"fmt"
	"path/filepath"
	"testing"
)

// commitCount reads the database's transaction id, which BoltDB increments by
// exactly one per committed write transaction. The difference across an
// operation is therefore the number of commits, i.e. the number of fsyncs.
func commitCount(t *testing.T, s *Store) int {
	t.Helper()
	tx, err := s.db.Begin(false)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	return tx.ID()
}

func newBatchStore(t *testing.T) *Store {
	t.Helper()
	s, err := NewStore(filepath.Join(t.TempDir(), "meta.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if err := s.CreateBucket("b"); err != nil {
		t.Fatal(err)
	}
	return s
}

// The point of the batch is that it is ONE transaction, i.e. one fsync for the
// whole set instead of one per object (issue #50). Counting write transactions
// is the only way to notice if that ever regresses to a loop of single writes.
func TestPutObjectMetaBatchUsesOneTransaction(t *testing.T) {
	s := newBatchStore(t)
	metas := make([]ObjectMeta, 64)
	for i := range metas {
		metas[i] = ObjectMeta{Bucket: "b", Key: fmt.Sprintf("k-%03d", i), Size: int64(i)}
	}

	before := commitCount(t, s)
	if err := s.PutObjectMetaBatch(metas); err != nil {
		t.Fatal(err)
	}
	if got := commitCount(t, s) - before; got != 1 {
		t.Fatalf("writing %d records took %d commits, want 1", len(metas), got)
	}

	before = commitCount(t, s)
	for _, m := range metas {
		if err := s.PutObjectMeta(m); err != nil {
			t.Fatal(err)
		}
	}
	if got := commitCount(t, s) - before; got != len(metas) {
		t.Fatalf("the single-record path took %d commits for %d records", got, len(metas))
	}
}

func TestPutObjectMetaBatchMatchesIndividualWrites(t *testing.T) {
	// Includes a repeated key so the in-batch ordering and the incremental bucket
	// counters are both exercised.
	metas := []ObjectMeta{
		{Bucket: "b", Key: "a", Size: 10},
		{Bucket: "b", Key: "c", Size: 30},
		{Bucket: "b", Key: "a", Size: 7},
	}

	batched := newBatchStore(t)
	if err := batched.PutObjectMetaBatch(metas); err != nil {
		t.Fatal(err)
	}

	single := newBatchStore(t)
	for _, m := range metas {
		if err := single.PutObjectMeta(m); err != nil {
			t.Fatal(err)
		}
	}

	bs, _, err := batched.BucketStats("b")
	if err != nil {
		t.Fatal(err)
	}
	ss, _, err := single.BucketStats("b")
	if err != nil {
		t.Fatal(err)
	}
	if bs != ss {
		t.Fatalf("bucket stats diverged: batched %+v, individual %+v", bs, ss)
	}

	got, err := batched.GetObjectMeta("b", "a")
	if err != nil || got == nil {
		t.Fatalf("key a missing: %v", err)
	}
	if got.Size != 7 {
		t.Fatalf("size = %d, want 7 (the later write in the batch wins)", got.Size)
	}
}

func TestPutObjectMetaBatchEmptyIsNoop(t *testing.T) {
	s := newBatchStore(t)
	before := commitCount(t, s)
	if err := s.PutObjectMetaBatch(nil); err != nil {
		t.Fatal(err)
	}
	if got := commitCount(t, s) - before; got != 0 {
		t.Fatalf("an empty batch committed %d transactions, want 0", got)
	}
}
