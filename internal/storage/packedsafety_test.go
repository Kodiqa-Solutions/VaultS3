package storage

import (
	"bytes"
	"errors"
	"fmt"
	"testing"
)

// cutReader yields some bytes and then fails, the way a dropped client
// connection looks to the engine.
type cutReader struct {
	data []byte
	off  int
}

func (r *cutReader) Read(p []byte) (int, error) {
	if r.off >= len(r.data) {
		return 0, errors.New("connection reset")
	}
	n := copy(p, r.data[r.off:])
	r.off += n
	return n, nil
}

// A large overwrite that fails part way must leave the previous small object
// readable. The packed index entry used to be dropped before the inner write
// ran, so a failed upload destroyed data that the metadata still described.
func TestPackedFailedLargeOverwriteKeepsOldObject(t *testing.T) {
	for _, sized := range []bool{true, false} {
		t.Run(fmt.Sprintf("sized=%v", sized), func(t *testing.T) {
			p, _ := newPacked(t, 1024, 1<<20)
			old := []byte("the previous small object")
			if _, _, err := p.PutObject("b", "k", bytes.NewReader(old), int64(len(old))); err != nil {
				t.Fatalf("PutObject: %v", err)
			}
			big := bytes.Repeat([]byte("x"), 4096)
			size := int64(len(big)) * 2
			if !sized {
				// A size hint below the threshold that turns out to be wrong
				// takes the second delegation path.
				size = 10
			}
			if _, _, err := p.PutObject("b", "k", &cutReader{data: big}, size); err == nil {
				t.Fatal("PutObject with a failing reader returned nil")
			}
			if got := get(t, p, "b", "k"); !bytes.Equal(got, old) {
				t.Fatalf("after a failed overwrite got %q, want the previous object", got)
			}
		})
	}
}

// A successful large overwrite of a packed object must serve the new bytes.
func TestPackedLargeOverwriteReplacesPacked(t *testing.T) {
	p, _ := newPacked(t, 1024, 1<<20)
	old := []byte("small")
	if _, _, err := p.PutObject("b", "k", bytes.NewReader(old), int64(len(old))); err != nil {
		t.Fatalf("PutObject: %v", err)
	}
	big := bytes.Repeat([]byte("y"), 4096)
	if _, _, err := p.PutObject("b", "k", bytes.NewReader(big), int64(len(big))); err != nil {
		t.Fatalf("PutObject: %v", err)
	}
	if got := get(t, p, "b", "k"); !bytes.Equal(got, big) {
		t.Fatalf("got %d bytes, want the new large object", len(got))
	}
}

// Dropping a bucket must drop every packed index entry, or a recreated bucket
// of the same name lists and serves the deleted objects.
func TestPackedDeleteBucketDropsAllEntries(t *testing.T) {
	p, _ := newPacked(t, 1024, 1<<30)
	for i := 0; i < 1000; i++ {
		d := []byte(fmt.Sprintf("v%d", i))
		if _, _, err := p.PutObject("b", fmt.Sprintf("k%05d", i), bytes.NewReader(d), int64(len(d))); err != nil {
			t.Fatalf("PutObject: %v", err)
		}
	}
	if err := p.DeleteBucketDir("b"); err != nil {
		t.Fatalf("DeleteBucketDir: %v", err)
	}
	if err := p.CreateBucketDir("b"); err != nil {
		t.Fatalf("CreateBucketDir: %v", err)
	}
	objs, _, err := p.ListObjects("b", "", "", 0)
	if err != nil {
		t.Fatalf("ListObjects: %v", err)
	}
	if len(objs) != 0 {
		t.Fatalf("recreated bucket lists %d deleted objects", len(objs))
	}
}
