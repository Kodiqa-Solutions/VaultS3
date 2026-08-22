package reclaim

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// fakeLookup answers from plain sets, so a test states exactly what metadata
// still knows about.
type fakeLookup struct {
	objects  map[string]bool // "bucket/key"
	versions map[string]bool // "bucket/key@version"
	uploads  map[string]bool
}

func known(found bool) Presence {
	if found {
		return Present
	}
	return Absent
}

func (f fakeLookup) HasObject(b, k string) Presence { return known(f.objects[b+"/"+k]) }
func (f fakeLookup) HasVersion(b, k, v string) Presence {
	return known(f.versions[b+"/"+k+"@"+v])
}
func (f fakeLookup) HasUpload(id string) Presence { return known(f.uploads[id]) }
func newLookup() fakeLookup {
	return fakeLookup{objects: map[string]bool{}, versions: map[string]bool{}, uploads: map[string]bool{}}
}

// write creates a file with the given content and backdates it so the age guard
// does not skip it. Tests that want a "new" file pass age 0.
func write(t *testing.T, path, content string, age time.Duration) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	when := time.Now().Add(-age)
	if err := os.Chtimes(path, when, when); err != nil {
		t.Fatal(err)
	}
}

func TestReclaimRemovesOnlyDataWithNoMetadata(t *testing.T) {
	dir := t.TempDir()
	look := newLookup()

	live := filepath.Join(dir, "b", "live.parquet")
	orphan := filepath.Join(dir, "b", "nested/deep/orphan.parquet")
	write(t, live, "still referenced", 48*time.Hour)
	write(t, orphan, "bulk-deleted on another node", 48*time.Hour)
	look.objects["b/live.parquet"] = true

	rep, err := Run(Options{DataDir: dir, MinAge: time.Hour}, look)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if rep.Orphans != 1 {
		t.Fatalf("found %d orphans, want 1 (samples %+v)", rep.Orphans, rep.Samples)
	}
	if rep.Deleted != 1 {
		t.Fatalf("deleted %d, want 1", rep.Deleted)
	}
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Error("orphan file survived the reclaim")
	}
	if _, err := os.Stat(live); err != nil {
		t.Errorf("reclaim deleted a file that metadata still refers to: %v", err)
	}
	if rep.ByBucket["b"] == 0 {
		t.Error("per-bucket breakdown is empty")
	}
}

// The most dangerous failure mode: deleting an object that was just written but
// whose metadata has not committed yet. The age guard is the only thing between
// this package and data loss on a busy cluster.
func TestReclaimNeverTouchesRecentlyWrittenData(t *testing.T) {
	dir := t.TempDir()
	look := newLookup()

	fresh := filepath.Join(dir, "b", "just-written.bin")
	write(t, fresh, "metadata still in flight", 0)

	rep, err := Run(Options{DataDir: dir, MinAge: time.Hour}, look)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if rep.Orphans != 0 || rep.Deleted != 0 {
		t.Fatalf("reclaimed a file inside the write-then-commit window: %+v", rep)
	}
	if rep.SkippedTooNew != 1 {
		t.Fatalf("SkippedTooNew = %d, want 1", rep.SkippedTooNew)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Fatalf("freshly written file was deleted: %v", err)
	}
}

func TestReclaimRefusesWithoutMinAge(t *testing.T) {
	if _, err := Run(Options{DataDir: t.TempDir()}, newLookup()); err != ErrNoMinAge {
		t.Fatalf("got %v, want ErrNoMinAge — an unset guard must never mean 'delete everything'", err)
	}
}

func TestDryRunReportsWithoutDeleting(t *testing.T) {
	dir := t.TempDir()
	orphan := filepath.Join(dir, "b", "orphan.bin")
	write(t, orphan, "0123456789", 48*time.Hour)

	rep, err := Run(Options{DataDir: dir, MinAge: time.Hour, DryRun: true}, newLookup())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if rep.Orphans != 1 || rep.OrphanBytes != 10 {
		t.Fatalf("orphans=%d bytes=%d, want 1/10", rep.Orphans, rep.OrphanBytes)
	}
	if rep.Deleted != 0 || rep.DeletedBytes != 0 {
		t.Fatalf("dry run deleted something: %+v", rep)
	}
	if !rep.DryRun {
		t.Error("report does not record that it was a dry run")
	}
	if _, err := os.Stat(orphan); err != nil {
		t.Fatalf("dry run removed the file: %v", err)
	}
}

// A half-written object is renamed into place on success; deleting the temp file
// under an in-flight write would corrupt it.
func TestReclaimIgnoresInFlightTempFiles(t *testing.T) {
	dir := t.TempDir()
	tmp := filepath.Join(dir, "b", tmpPrefix+"923847")
	write(t, tmp, "streaming right now", 48*time.Hour)

	rep, err := Run(Options{DataDir: dir, MinAge: time.Hour}, newLookup())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if rep.Orphans != 0 {
		t.Fatalf("temp file treated as an orphan: %+v", rep.Samples)
	}
	if _, err := os.Stat(tmp); err != nil {
		t.Fatalf("in-flight temp file was deleted: %v", err)
	}
}

func TestReclaimHandlesVersionsSeparatelyFromCurrentObjects(t *testing.T) {
	dir := t.TempDir()
	look := newLookup()

	keep := filepath.Join(dir, "b", versionsDir, "a/b/doc.txt", "v-live")
	drop := filepath.Join(dir, "b", versionsDir, "a/b/doc.txt", "v-dead")
	write(t, keep, "current", 48*time.Hour)
	write(t, drop, "expired noncurrent version", 48*time.Hour)
	look.versions["b/a/b/doc.txt@v-live"] = true

	rep, err := Run(Options{DataDir: dir, MinAge: time.Hour}, look)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if rep.Orphans != 1 {
		t.Fatalf("orphans=%d, want 1 (samples %+v)", rep.Orphans, rep.Samples)
	}
	if got := rep.Samples[0]; got.Version != "v-dead" || got.Key != "a/b/doc.txt" {
		t.Fatalf("parsed version path as key=%q version=%q, want a/b/doc.txt / v-dead", got.Key, got.Version)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Errorf("live version deleted: %v", err)
	}
}

// Parts of an upload no record refers to are unreachable: no ListParts, no abort,
// no lifecycle rule finds them. On a cluster these pile up when a ring change
// strands an upload on the node that created it (issue #47 bug B).
func TestReclaimRemovesPartsOfVanishedUploads(t *testing.T) {
	dir := t.TempDir()
	look := newLookup()

	live := filepath.Join(dir, multipartDir, "upload-live", "00001")
	dead := filepath.Join(dir, multipartDir, "upload-gone", "00001")
	write(t, live, "in progress", 48*time.Hour)
	write(t, dead, "stranded by a ring change", 48*time.Hour)
	look.uploads["upload-live"] = true

	rep, err := Run(Options{DataDir: dir, MinAge: time.Hour}, look)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if rep.Orphans != 1 {
		t.Fatalf("orphans=%d, want 1 (samples %+v)", rep.Orphans, rep.Samples)
	}
	if rep.Samples[0].Upload != "upload-gone" {
		t.Fatalf("reclaimed upload %q, want upload-gone", rep.Samples[0].Upload)
	}
	if _, err := os.Stat(live); err != nil {
		t.Errorf("parts of a live upload were deleted: %v", err)
	}
}

func TestReclaimCanBeLimitedToBuckets(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "wanted", "x.bin"), "aaa", 48*time.Hour)
	write(t, filepath.Join(dir, "other", "y.bin"), "bbb", 48*time.Hour)

	rep, err := Run(Options{DataDir: dir, MinAge: time.Hour, Buckets: []string{"wanted"}}, newLookup())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if rep.Orphans != 1 {
		t.Fatalf("orphans=%d, want 1", rep.Orphans)
	}
	if _, err := os.Stat(filepath.Join(dir, "other", "y.bin")); err != nil {
		t.Errorf("scan escaped the requested bucket: %v", err)
	}
}

// Small-object packing writes every packed object into <dataDir>/_volumes/vol-*.dat.
// That directory sits beside the buckets and does not start with a dot, so a scan
// that treated it as a bucket would delete live volumes holding many objects each.
func TestReclaimNeverTouchesPackedVolumes(t *testing.T) {
	dir := t.TempDir()
	vol := filepath.Join(dir, packedVolumesDir, "vol-0000000001.dat")
	idx := filepath.Join(dir, packedVolumesDir, "index.db")
	write(t, vol, "millions of packed objects", 48*time.Hour)
	write(t, idx, "the index that finds them", 48*time.Hour)

	rep, err := Run(Options{DataDir: dir, MinAge: time.Hour}, newLookup())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if rep.Orphans != 0 {
		t.Fatalf("packed volumes treated as orphans: %+v", rep.Samples)
	}
	for _, p := range []string{vol, idx} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("reclaim deleted packed storage %s: %v", p, err)
		}
	}
}

// An erasure-coded object has no plain data file, only shards under <bucket>/.ec/.
// Judged against the plain-object lookup every shard looks orphaned, so the whole
// subtree must be off limits.
func TestReclaimNeverTouchesErasureShards(t *testing.T) {
	dir := t.TempDir()
	shard := filepath.Join(dir, "b", erasureDir, "big.bin", "shard-0")
	meta := filepath.Join(dir, "b", erasureDir, "big.bin", "meta.json")
	write(t, shard, "reed-solomon data shard", 48*time.Hour)
	write(t, meta, "{}", 48*time.Hour)

	rep, err := Run(Options{DataDir: dir, MinAge: time.Hour}, newLookup())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if rep.Orphans != 0 {
		t.Fatalf("erasure shards treated as orphans: %+v", rep.Samples)
	}
	for _, p := range []string{shard, meta} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("reclaim destroyed erasure data %s: %v", p, err)
		}
	}
}

// Keys containing dots are ordinary and must still be reclaimable; only a
// leading-dot path SEGMENT marks an internal layout.
func TestReclaimStillHandlesKeysContainingDots(t *testing.T) {
	dir := t.TempDir()
	orphan := filepath.Join(dir, "b", "year=2026/part-00001.snappy.parquet")
	write(t, orphan, "spark output", 48*time.Hour)

	rep, err := Run(Options{DataDir: dir, MinAge: time.Hour, DryRun: true}, newLookup())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if rep.Orphans != 1 {
		t.Fatalf("orphans=%d, want 1 — dotted filenames must stay reclaimable", rep.Orphans)
	}
}
