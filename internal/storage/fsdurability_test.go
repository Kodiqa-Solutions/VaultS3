package storage

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A failed rewrite of an existing version must leave that version intact. The
// version file used to be opened with os.Create, which truncated it the moment
// the write began, and the error path then removed it, so a dropped upload to a
// versioning-suspended bucket destroyed the previous null version.
func TestPutObjectVersionFailureKeepsPreviousVersion(t *testing.T) {
	fs, err := NewFileSystem(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := fs.CreateBucketDir("b"); err != nil {
		t.Fatal(err)
	}
	old := []byte("previous null version")
	if _, _, err := fs.PutObjectVersion("b", "k", "null", bytes.NewReader(old), int64(len(old))); err != nil {
		t.Fatalf("PutObjectVersion: %v", err)
	}
	if _, _, err := fs.PutObjectVersion("b", "k", "null", &cutReader{data: []byte("partial new")}, 100); err == nil {
		t.Fatal("PutObjectVersion with a failing reader returned nil")
	}
	rc, _, err := fs.GetObjectVersion("b", "k", "null")
	if err != nil {
		t.Fatalf("previous version is gone: %v", err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if !bytes.Equal(got, old) {
		t.Fatalf("previous version reads %q, want %q", got, old)
	}
	// No temp file may be left beside the versions.
	entries, _ := os.ReadDir(filepath.Dir(fs.versionPath("b", "k", "null")))
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".vaults3-tmp-") {
			t.Fatalf("temp file %s left behind", e.Name())
		}
	}
}

// Every write path must sync the file before it is renamed into place and the
// directory after, or a power loss after the metadata commit (which bbolt does
// fsync) can leave the committed key pointing at an empty or missing file. The
// test counts the attempts, because the outcome of a missing fsync is invisible
// without pulling the power.
func TestFileSystemWritesAreSynced(t *testing.T) {
	var files, dirs int
	origF, origD := syncFile, syncDirFn
	syncFile = func(f *os.File) error { files++; return origF(f) }
	syncDirFn = func(d string) error { dirs++; return origD(d) }
	t.Cleanup(func() { syncFile, syncDirFn = origF, origD })

	fs, err := NewFileSystem(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := fs.CreateBucketDir("b"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := fs.PutObject("b", "a/k", strings.NewReader("x"), 1); err != nil {
		t.Fatal(err)
	}
	if _, _, err := fs.PutObjectVersion("b", "a/k", "v1", strings.NewReader("y"), 1); err != nil {
		t.Fatal(err)
	}
	if files != 2 || dirs != 2 {
		t.Fatalf("file syncs=%d dir syncs=%d, want 2 and 2", files, dirs)
	}

	fs.SetSyncDirs(false)
	if _, _, err := fs.PutObject("b", "a/k", strings.NewReader("z"), 1); err != nil {
		t.Fatal(err)
	}
	if files != 3 || dirs != 2 {
		t.Fatalf("with SetSyncDirs(false): file syncs=%d dir syncs=%d, want 3 and 2", files, dirs)
	}
}
