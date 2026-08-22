package reclaim

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// unknownLookup answers Unknown for the named buckets, standing in for a metadata
// store that errors or a metadata shard this node cannot reach.
type unknownLookup struct {
	fakeLookup
	unknownBuckets map[string]bool
	unknownUploads bool
}

func (u unknownLookup) HasObject(b, k string) Presence {
	if u.unknownBuckets[b] {
		return Unknown
	}
	return u.fakeLookup.HasObject(b, k)
}

func (u unknownLookup) HasVersion(b, k, v string) Presence {
	if u.unknownBuckets[b] {
		return Unknown
	}
	return u.fakeLookup.HasVersion(b, k, v)
}

func (u unknownLookup) HasUpload(id string) Presence {
	if u.unknownUploads {
		return Unknown
	}
	return u.fakeLookup.HasUpload(id)
}

func writeOld(t *testing.T, path string, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
}

// The rule that matters: data whose metadata could not be read is never deleted
// and never counted as an orphan. This is the shape of the failure that would
// otherwise erase live data on every node that does not hold a bucket's metadata
// shard, and the same rule that issue #47 was fixed under.
func TestUnreadableMetadataDeletesNothing(t *testing.T) {
	dir := t.TempDir()
	for _, k := range []string{"a.parquet", "nested/b.parquet", "nested/deep/c.parquet"} {
		writeOld(t, filepath.Join(dir, "live", k), "payload")
	}

	look := unknownLookup{
		fakeLookup:     fakeLookup{objects: map[string]bool{}},
		unknownBuckets: map[string]bool{"live": true},
	}
	rep, err := Run(Options{DataDir: dir, MinAge: time.Hour, DryRun: false}, look)
	if err != nil {
		t.Fatal(err)
	}

	if rep.Deleted != 0 || rep.DeletedBytes != 0 {
		t.Fatalf("deleted %d files (%d bytes) although no lookup could be answered", rep.Deleted, rep.DeletedBytes)
	}
	if rep.Orphans != 0 {
		t.Fatalf("reported %d orphans from unanswerable lookups", rep.Orphans)
	}
	if rep.SkippedUnknown != 3 {
		t.Fatalf("SkippedUnknown = %d, want 3", rep.SkippedUnknown)
	}
	if len(rep.Incomplete) != 1 || rep.Incomplete[0] != "live" {
		t.Fatalf("Incomplete = %v, want [live]", rep.Incomplete)
	}
	for _, k := range []string{"a.parquet", "nested/b.parquet", "nested/deep/c.parquet"} {
		if _, err := os.Stat(filepath.Join(dir, "live", k)); err != nil {
			t.Fatalf("live data was destroyed: %s: %v", k, err)
		}
	}
}

// A single unanswerable lookup must protect the WHOLE bucket, including files
// the scan had already decided were orphans before it hit the unknown. Walk order
// must not decide whether data survives.
func TestOneUnknownProtectsTheWholeBucket(t *testing.T) {
	dir := t.TempDir()
	// "aaa" sorts before "zzz", so the orphan is visited before the unknown one.
	writeOld(t, filepath.Join(dir, "mixed", "aaa-orphan.parquet"), "junk")
	writeOld(t, filepath.Join(dir, "mixed", "zzz-unknown.parquet"), "live")

	// partialUnknown answers Absent for the first key and Unknown for the second.
	rep, err := Run(Options{DataDir: dir, MinAge: time.Hour, DryRun: false}, partialUnknown{})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Deleted != 0 {
		t.Fatalf("deleted %d files in a bucket that could not be fully read", rep.Deleted)
	}
	if !contains(rep.Incomplete, "mixed") {
		t.Fatalf("bucket not reported incomplete: %v", rep.Incomplete)
	}
	if _, err := os.Stat(filepath.Join(dir, "mixed", "aaa-orphan.parquet")); err != nil {
		t.Fatalf("a file deleted before the unknown was discovered: %v", err)
	}
}

// partialUnknown answers Absent for one key and Unknown for another in the same
// bucket, so the order of discovery is what the test exercises.
type partialUnknown struct{}

func (partialUnknown) HasObject(b, k string) Presence {
	if k == "zzz-unknown.parquet" {
		return Unknown
	}
	return Absent
}
func (partialUnknown) HasVersion(b, k, v string) Presence { return Absent }
func (partialUnknown) HasUpload(id string) Presence       { return Absent }

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// A bucket that answers cleanly must still be reclaimed, or the guard would make
// the feature useless.
func TestAnsweredBucketsStillReclaim(t *testing.T) {
	dir := t.TempDir()
	writeOld(t, filepath.Join(dir, "clean", "orphan.parquet"), "junk")
	writeOld(t, filepath.Join(dir, "murky", "maybe.parquet"), "live")

	look := unknownLookup{
		fakeLookup:     fakeLookup{objects: map[string]bool{}},
		unknownBuckets: map[string]bool{"murky": true},
	}
	rep, err := Run(Options{DataDir: dir, MinAge: time.Hour, DryRun: false}, look)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Deleted != 1 {
		t.Fatalf("deleted %d, want exactly the one orphan in the readable bucket", rep.Deleted)
	}
	if _, err := os.Stat(filepath.Join(dir, "clean", "orphan.parquet")); !os.IsNotExist(err) {
		t.Fatal("the genuine orphan survived, the guard is too broad")
	}
	if _, err := os.Stat(filepath.Join(dir, "murky", "maybe.parquet")); err != nil {
		t.Fatal("data in the unreadable bucket was deleted")
	}
}
