package storage

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newColdTierRig(t *testing.T) (*ColdTier, *FileSystem, *FileSystem) {
	t.Helper()
	dir := t.TempDir()
	hot, err := NewFileSystem(filepath.Join(dir, "hot"))
	if err != nil {
		t.Fatal(err)
	}
	cold, err := NewFileSystem(filepath.Join(dir, "cold"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range []*FileSystem{hot, cold} {
		if err := e.CreateBucketDir("b"); err != nil {
			t.Fatal(err)
		}
	}
	return NewColdTier(hot, cold), hot, cold
}

func readAll(t *testing.T, e Engine, key string) string {
	t.Helper()
	r, _, err := e.GetObject("b", key)
	if err != nil {
		t.Fatalf("read %s: %v", key, err)
	}
	defer r.Close()
	b, _ := io.ReadAll(r)
	return string(b)
}

// Regression (5.0.12): nothing read the cold tier, so an object the tiering
// manager moved there answered NoSuchKey until it was moved back. A read that
// finds no hot file now reads the cold one.
func TestColdTierReadsAnObjectThatIsOnlyCold(t *testing.T) {
	ct, _, cold := newColdTierRig(t)
	cold.PutObject("b", "k", bytes.NewReader([]byte("cold bytes")), 10)
	if got := readAll(t, ct, "k"); got != "cold bytes" {
		t.Fatalf("read %q", got)
	}
	if !ct.ObjectExists("b", "k") {
		t.Error("ObjectExists: false for a cold object")
	}
	if n, err := ct.ObjectSize("b", "k"); err != nil || n != 10 {
		t.Errorf("ObjectSize: %d %v", n, err)
	}
	if _, _, err := ct.GetObject("b", "missing"); err == nil || !os.IsNotExist(unwrapAll(err)) {
		t.Errorf("a key in neither tier: %v, want a not-exist error", err)
	}
}

func unwrapAll(err error) error {
	for {
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return err
		}
		next := u.Unwrap()
		if next == nil {
			return err
		}
		err = next
	}
}

// The hot file wins, and a write replaces a cold copy, so a stale cold file is
// never read in place of newer data.
func TestColdTierPrefersHotAndAWriteDropsTheColdCopy(t *testing.T) {
	ct, hot, cold := newColdTierRig(t)
	cold.PutObject("b", "k", bytes.NewReader([]byte("old")), 3)
	hot.PutObject("b", "k", bytes.NewReader([]byte("new")), 3)
	if got := readAll(t, ct, "k"); got != "new" {
		t.Fatalf("read %q, want the hot file", got)
	}
	if _, _, err := ct.PutObject("b", "k", bytes.NewReader([]byte("newest")), 6); err != nil {
		t.Fatal(err)
	}
	if cold.ObjectExists("b", "k") {
		t.Error("a write left the cold copy behind")
	}
	if err := ct.DeleteObject("b", "k"); err != nil {
		t.Fatal(err)
	}
	if ct.ObjectExists("b", "k") {
		t.Error("a delete left the object in a tier")
	}
	cold.PutObject("b", "c", bytes.NewReader([]byte("x")), 1)
	if err := ct.DeleteObject("b", "c"); err != nil || cold.ObjectExists("b", "c") {
		t.Errorf("a delete of a cold-only object: %v, still there %v", err, cold.ObjectExists("b", "c"))
	}
}

// Both tiers count toward the bucket's size, so moving objects cold does not
// make room under a quota.
func TestColdTierBucketSizeCountsBothTiers(t *testing.T) {
	ct, hot, cold := newColdTierRig(t)
	hot.PutObject("b", "h", bytes.NewReader([]byte("12345")), 5)
	cold.PutObject("b", "c", bytes.NewReader([]byte("123")), 3)
	size, count, err := ct.BucketSize("b")
	if err != nil || size != 8 || count != 2 {
		t.Fatalf("BucketSize: %d bytes, %d objects, %v; want 8 and 2", size, count, err)
	}
}

func TestColdTierListsBothTiersInKeyOrder(t *testing.T) {
	ct, hot, cold := newColdTierRig(t)
	for _, k := range []string{"a", "c", "e"} {
		putFile(t, hot, "b", k, "hot "+k)
	}
	for _, k := range []string{"b", "c", "d"} {
		putFile(t, cold, "b", k, "cold "+k)
	}
	objs, more, err := ct.ListObjects("b", "", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for _, o := range objs {
		keys = append(keys, o.Key)
	}
	if got := strings.Join(keys, ","); got != "a,b,c,d,e" || more {
		t.Fatalf("listing %q more=%v, want a,b,c,d,e", got, more)
	}
	if objs[2].Size != int64(len("hot c")) {
		t.Errorf("a key in both tiers was listed as the cold file")
	}
	objs, more, _ = ct.ListObjects("b", "", "a", 2)
	if len(objs) != 2 || objs[0].Key != "b" || objs[1].Key != "c" || !more {
		t.Fatalf("page after a, 2 keys: %+v more=%v", objs, more)
	}
	// A bucket with only cold objects is not empty.
	hot.CreateBucketDir("onlycold")
	putFile(t, cold, "onlycold", "x", "x")
	if objs, _, _ := ct.ListObjects("onlycold", "", "", 1); len(objs) != 1 {
		t.Fatalf("a bucket with only a cold object listed %d keys", len(objs))
	}
	// No cold directory for the bucket is not an error.
	hot.CreateBucketDir("hotonly")
	putFile(t, hot, "hotonly", "y", "y")
	if objs, _, err := ct.ListObjects("hotonly", "", "", 0); err != nil || len(objs) != 1 {
		t.Fatalf("hot-only bucket: %v %d", err, len(objs))
	}
}

func putFile(t *testing.T, e *FileSystem, bucket, key, data string) {
	t.Helper()
	e.CreateBucketDir(bucket)
	if _, _, err := e.PutObject(bucket, key, strings.NewReader(data), int64(len(data))); err != nil {
		t.Fatal(err)
	}
}

// Regression (5.0.12 review): a cold "a" and a hot "a/b". The hot folder "a"
// hid the cold file: GET failed and ObjectSize gave the folder's size.
func TestColdTierReadsAColdFileUnderAHotFolder(t *testing.T) {
	ct, _, cold := newColdTierRig(t)
	cold.PutObject("b", "a", bytes.NewReader([]byte("cold a")), 6)
	if _, _, err := ct.PutObject("b", "a/b", bytes.NewReader([]byte("hot ab")), 6); err != nil {
		t.Fatalf("put a/b: %v", err)
	}
	if got := readAll(t, ct, "a"); got != "cold a" {
		t.Errorf("GET a = %q", got)
	}
	if n, err := ct.ObjectSize("b", "a"); err != nil || n != 6 {
		t.Errorf("ObjectSize a: %d %v, want 6", n, err)
	}
	if got := readAll(t, ct, "a/b"); got != "hot ab" {
		t.Errorf("GET a/b = %q", got)
	}
}

// Regression (5.0.12 review): a cold "a/b" and a hot file "a". The hot path
// a/b is ENOTDIR: GET failed, and DELETE removed the cold copy and then
// failed, leaving the record without data.
func TestColdTierReadsAndDeletesAColdFileUnderAHotFile(t *testing.T) {
	ct, _, cold := newColdTierRig(t)
	cold.PutObject("b", "a/b", bytes.NewReader([]byte("cold ab")), 7)
	if _, _, err := ct.PutObject("b", "a", bytes.NewReader([]byte("hot a")), 5); err != nil {
		t.Fatalf("put a: %v", err)
	}
	if got := readAll(t, ct, "a/b"); got != "cold ab" {
		t.Errorf("GET a/b = %q", got)
	}
	if n, err := ct.ObjectSize("b", "a/b"); err != nil || n != 7 {
		t.Errorf("ObjectSize a/b: %d %v", n, err)
	}
	if err := ct.DeleteObject("b", "a/b"); err != nil {
		t.Errorf("DELETE a/b: %v", err)
	}
	if got := readAll(t, ct, "a"); got != "hot a" {
		t.Errorf("hot a after deleting a/b: %q", got)
	}
}
