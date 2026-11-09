package metadata

import (
	"encoding/json"
	"path/filepath"
	"testing"

	bolt "go.etcd.io/bbolt"
)

func usageStore(t *testing.T) *Store {
	t.Helper()
	s, err := NewStore(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	s.CreateBucket("b")
	return s
}

// measured adds up the bucket from its records, as a fresh baseline would.
func measured(t *testing.T, s *Store, bucket string) BucketStat {
	t.Helper()
	var st BucketStat
	s.db.View(func(tx *bolt.Tx) error { st = measureStoredTx(tx, bucket); return nil })
	return st
}

func checkUsage(t *testing.T, s *Store, want BucketStat, step string) {
	t.Helper()
	got, err := s.BucketStoredUsage("b")
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("%s: stored usage %+v, want %+v", step, got, want)
	}
	if m := measured(t, s, "b"); m != got {
		t.Fatalf("%s: kept figure %+v, records add up to %+v", step, got, m)
	}
}

// A cluster's quota is held to the stored usage: every version, and every
// object written without versioning, counted once by recorded size. The
// figure kept with the writes must always match the records.
func TestStoredUsageFollowsEveryWrite(t *testing.T) {
	s := usageStore(t)
	checkUsage(t, s, BucketStat{}, "empty")

	// Unversioned objects: put, overwrite, delete.
	s.PutObjectMeta(ObjectMeta{Bucket: "b", Key: "u", Size: 10})
	checkUsage(t, s, BucketStat{10, 1}, "unversioned put")
	s.PutObjectMeta(ObjectMeta{Bucket: "b", Key: "u", Size: 4})
	checkUsage(t, s, BucketStat{4, 1}, "unversioned overwrite")

	// Versioning on: the plain object is adopted as the null version, then a
	// new version takes the key. It is still stored once, plus the new one.
	null := ObjectMeta{Bucket: "b", Key: "u", VersionID: "null", Size: 4}
	s.PutObjectVersion(null)
	v1 := ObjectMeta{Bucket: "b", Key: "u", VersionID: "0000000000000001aaaaaaaa", Size: 100, IsLatest: true}
	s.PutObjectVersion(v1)
	s.PutObjectMeta(v1)
	checkUsage(t, s, BucketStat{104, 2}, "null kept, version added")

	// A delete marker holds nothing.
	dm := ObjectMeta{Bucket: "b", Key: "u", VersionID: "0000000000000002bbbbbbbb", DeleteMarker: true, IsLatest: true}
	s.PutObjectVersion(dm)
	s.PutObjectMeta(dm)
	checkUsage(t, s, BucketStat{104, 2}, "delete marker")

	// Removing the marker promotes v1; removing v1 promotes null.
	s.DeleteObjectVersion("b", "u", dm.VersionID)
	checkUsage(t, s, BucketStat{104, 2}, "marker removed")
	s.DeleteObjectVersion("b", "u", v1.VersionID)
	checkUsage(t, s, BucketStat{4, 1}, "version removed")

	// A lock update rewrites the record, size unchanged.
	n, _ := s.GetObjectVersion("b", "u", "null")
	n.LegalHold = true
	s.UpdateObjectVersionMeta(*n)
	checkUsage(t, s, BucketStat{4, 1}, "lock update")

	// Rolling the pointer to a version (snapshot restore).
	s.SetLatestVersion("b", "u", "null")
	checkUsage(t, s, BucketStat{4, 1}, "set latest")

	// Another unversioned key, then deleted.
	s.PutObjectMeta(ObjectMeta{Bucket: "b", Key: "x", Size: 7})
	checkUsage(t, s, BucketStat{11, 2}, "second key")
	s.DeleteObjectMeta("b", "x")
	checkUsage(t, s, BucketStat{4, 1}, "second key deleted")

	// Batches (the Raft state machine) adjust as single writes do.
	s.PutObjectMetaBatch([]ObjectMeta{{Bucket: "b", Key: "y", Size: 1}, {Bucket: "b", Key: "y", Size: 3}, {Bucket: "b", Key: "z", Size: 5}})
	checkUsage(t, s, BucketStat{12, 3}, "batch")

	// Another bucket is separate.
	s.CreateBucket("other")
	s.PutObjectMeta(ObjectMeta{Bucket: "other", Key: "o", Size: 1000})
	checkUsage(t, s, BucketStat{12, 3}, "other bucket untouched")
}

// A bucket from before 5.0.13 has no figure. It is measured at its first
// write after the upgrade, in that write's transaction, so it includes it.
func TestStoredUsageOfABucketWrittenBeforeTheCounter(t *testing.T) {
	s := usageStore(t)
	s.PutObjectMeta(ObjectMeta{Bucket: "b", Key: "a", Size: 10})
	s.PutObjectVersion(ObjectMeta{Bucket: "b", Key: "v", VersionID: "0000000000000001aaaaaaaa", Size: 20})
	// Forget the figure, as a store from an earlier version has none.
	s.db.Update(func(tx *bolt.Tx) error { return tx.Bucket(bucketStoredBucket).Delete([]byte("b")) })
	checkUsage(t, s, BucketStat{30, 2}, "no figure: measured on read")
	s.PutObjectMeta(ObjectMeta{Bucket: "b", Key: "c", Size: 5})
	checkUsage(t, s, BucketStat{35, 3}, "first write after the upgrade")
	s.PutObjectMeta(ObjectMeta{Bucket: "b", Key: "d", Size: 1})
	checkUsage(t, s, BucketStat{36, 4}, "later writes adjust")

	// Deleting a bucket's objects, and the bucket, forget the figure.
	s.DeleteBucketObjectMeta("b")
	checkUsage(t, s, BucketStat{20, 1}, "objects deleted, the version remains")
}

// Regression (5.0.13 review): a node rolled back to a version that does not
// keep the figures, written to, and rolled forward kept its old figures, so
// the quota did not count what was written meanwhile, for good. A figure
// survives a reopen only after this version's own clean close.
func TestStoredFiguresSurviveOnlyACleanClose(t *testing.T) {
	path := filepath.Join(t.TempDir(), "m.db")
	open := func() *Store {
		s, err := NewStore(path)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	kept := func(s *Store) bool {
		found := false
		s.db.View(func(tx *bolt.Tx) error { found = tx.Bucket(bucketStoredBucket).Get([]byte("b")) != nil; return nil })
		return found
	}
	s := open()
	s.CreateBucket("b")
	s.PutObjectMeta(ObjectMeta{Bucket: "b", Key: "a", Size: 10})
	s.Close()
	s = open()
	if !kept(s) {
		t.Fatal("a clean close and reopen dropped the figure")
	}
	// What an earlier version does: write records without the figure, and
	// close without the marker.
	s.db.Update(func(tx *bolt.Tx) error {
		d, _ := json.Marshal(ObjectMeta{Bucket: "b", Key: "old-binary", Size: 5000})
		return tx.Bucket(objectsBucket).Put(objectMetaKey("b", "old-binary"), d)
	})
	s.db.Close()
	s = open()
	if kept(s) {
		t.Fatal("a figure survived writes by another binary")
	}
	if st, _ := s.BucketStoredUsage("b"); st != (BucketStat{5010, 2}) {
		t.Fatalf("measured again %+v, want 5010/2", st)
	}
	if !kept(s) {
		t.Fatal("a measured figure was not kept")
	}
	s.Close()
}
