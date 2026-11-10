package metadata

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCopyIndexRecordsAndForgets(t *testing.T) {
	path := filepath.Join(t.TempDir(), "local-copies.db")
	c, err := OpenCopyIndex(path)
	if err != nil {
		t.Fatal(err)
	}
	c.Record("b", "k", "", `"e1"`, 4)
	c.Record("b", "k", "v1", `"e2"`, 4)
	if e, ok := c.Lookup("b", "k", "", 4); !ok || e != `"e1"` {
		t.Errorf("plain: %q %v", e, ok)
	}
	if e, ok := c.Lookup("b", "k", "v1", 4); !ok || e != `"e2"` {
		t.Errorf("version: %q %v", e, ok)
	}
	c.Record("b", "k", "", "", 4) // a copy of no known object
	if _, ok := c.Lookup("b", "k", "", 4); ok {
		t.Error("an empty ETag kept the old record")
	}
	_ = c.Close()

	// A file that cannot be opened as an index is replaced, not fatal.
	if err := os.WriteFile(path, []byte("not a database"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err = OpenCopyIndex(path)
	if err != nil {
		t.Fatalf("a damaged index stopped the start: %v", err)
	}
	defer func() { _ = c.Close() }()
	if _, ok := c.Lookup("b", "k", "v1", 4); ok {
		t.Error("a recreated index kept records")
	}
	var nilIndex *CopyIndex
	nilIndex.Record("b", "k", "", "x", 4) // single node: no index, no-op
	if _, ok := nilIndex.Lookup("b", "k", "", 4); ok {
		t.Error("a nil index found a record")
	}
}

// A database that fails in use switches the index off instead of failing
// writes: every file is then judged as before.
func TestABrokenCopyIndexSwitchesOff(t *testing.T) {
	c, err := OpenCopyIndex(filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	c.Record("b", "k", "", `"e"`, 4)
	_ = c.db.Close() // in use after this, bbolt fails
	c.Record("b", "k", "", `"f"`, 4)
	if _, ok := c.Lookup("b", "k", "", 4); ok {
		t.Error("a failed index still answered")
	}
	if c.Working() {
		t.Error("a failed write left the index working")
	}
}

// Regression (5.0.15 review): an index that switched itself off stopped
// recording while files changed, and its records were trusted again at the
// next start. It now deletes its file; records otherwise survive a restart.
func TestRecordsOfASwitchedOffIndexAreNotTrustedAfterARestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "local-copies.db")
	c, err := OpenCopyIndex(path)
	if err != nil {
		t.Fatal(err)
	}
	c.Record("b", "k", "", `"e1"`, 4)
	_ = c.db.Close() // a stop without Close: the record is synced
	c, err = OpenCopyIndex(path)
	if err != nil {
		t.Fatal(err)
	}
	if e, ok := c.Lookup("b", "k", "", 4); !ok || e != `"e1"` {
		t.Fatalf("after a crash: %q %v", e, ok)
	}
	c.switchOff(os.ErrClosed)
	_ = c.Close()
	c, err = OpenCopyIndex(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	if _, ok := c.Lookup("b", "k", "", 4); ok {
		t.Error("records of an index that switched itself off were trusted after a restart")
	}
}

// A record counts only while the file still has the recorded size. Records
// are not synced, so after a machine crash one can outlive the overwrite it
// should have been dropped for; the new file's other size disowns it.
func TestARecordCountsOnlyAtTheRecordedSize(t *testing.T) {
	c, err := OpenCopyIndex(filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	c.Record("b", "k", "", `"e1"`, 4)
	if _, ok := c.Lookup("b", "k", "", 5); ok {
		t.Error("a record was trusted for a file of another size")
	}
	if e, ok := c.Lookup("b", "k", "", 4); !ok || e != `"e1"` {
		t.Errorf("at the recorded size: %q %v", e, ok)
	}
	if e, n, ok := c.Entry("b", "k", ""); !ok || e != `"e1"` || n != 4 {
		t.Errorf("entry %q %d %v", e, n, ok)
	}
	c.Record("b", "k", "", `"e2"`, -1) // no file to measure
	if _, _, ok := c.Entry("b", "k", ""); ok {
		t.Error("a record without a size was kept")
	}
}
