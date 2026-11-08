package erasure

import (
	"bytes"
	"crypto/rand"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/Kodiqa-Solutions/VaultS3/internal/storage"
)

// unknownLength hides the length of a reader, as a chunked upload does.
type unknownLength struct{ io.Reader }

func newSpillEngine(t *testing.T, blockSize int64) (*Engine, string) {
	t.Helper()
	dir := t.TempDir()
	inner, err := storage.NewFileSystem(dir)
	if err != nil {
		t.Fatal(err)
	}
	e, err := NewEngine(inner, Config{DataShards: 2, ParityShards: 1, BlockSize: blockSize})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.CreateBucketDir("b"); err != nil {
		t.Fatal(err)
	}
	return e, dir
}

// An upload of unknown length (size -1) is stored and read back intact at
// every size around the block boundary, and leaves nothing spilled behind.
func TestUnknownLengthUploadRoundTrips(t *testing.T) {
	const block = 4096
	e, dir := newSpillEngine(t, block)
	for _, n := range []int{0, 1, block - 1, block, block + 1, 5*block + 7} {
		data := make([]byte, n)
		_, _ = rand.Read(data)
		size, _, err := e.PutObject("b", "k", unknownLength{bytes.NewReader(data)}, -1)
		if err != nil {
			t.Fatalf("%d bytes: PutObject: %v", n, err)
		}
		if size != int64(n) {
			t.Fatalf("%d bytes: PutObject reported %d", n, size)
		}
		rc, gotSize, err := e.GetObject("b", "k")
		if err != nil {
			t.Fatalf("%d bytes: GetObject: %v", n, err)
		}
		got, _ := io.ReadAll(rc)
		rc.Close()
		if gotSize != int64(n) || !bytes.Equal(got, data) {
			t.Fatalf("%d bytes: read back %d bytes (size %d), want the same bytes", n, len(got), gotSize)
		}
	}
	if left, _ := os.ReadDir(filepath.Join(dir, spillDir)); len(left) > 0 {
		t.Fatalf("%d spilled uploads left behind", len(left))
	}
}

// Regression (5.0.11): an upload of unknown length was read whole into memory
// before it was erasure coded, so one such upload held up to the 5 GiB object
// limit in RAM. Memory now stays near one block whatever the object's size.
func TestUnknownLengthUploadDoesNotHoldTheObjectInMemory(t *testing.T) {
	const block = 1 << 20
	const size = 64 << 20
	e, _ := newSpillEngine(t, block)
	var before, after testingMemStats
	readMem(&before)
	src := io.LimitReader(rand.Reader, size)
	if n, _, err := e.PutObject("b", "big", unknownLength{src}, -1); err != nil || n != size {
		t.Fatalf("PutObject: %d %v", n, err)
	}
	readMem(&after)
	if alloc := after.TotalAlloc - before.TotalAlloc; alloc > size/2 {
		t.Fatalf("a %d MiB upload of unknown length allocated %d MiB, want far less than the object", size>>20, alloc>>20)
	}
}

// Uploads a stopped run left spilled are removed when the engine starts.
func TestSpilledUploadsAreClearedAtStartup(t *testing.T) {
	dir := t.TempDir()
	stale := filepath.Join(dir, spillDir, "put-123")
	if err := os.MkdirAll(filepath.Dir(stale), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stale, []byte("half an upload"), 0o600); err != nil {
		t.Fatal(err)
	}
	inner, err := storage.NewFileSystem(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewEngine(inner, Config{DataShards: 2, ParityShards: 1, BlockSize: 1024}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("a stale spilled upload survived startup: %v", err)
	}
}

// peekingReader serves data and, once most of it has been read, records
// whether any spill file on disk contains marker.
type peekingReader struct {
	r       io.Reader
	n, at   int
	dir     string
	marker  []byte
	checked bool
	found   bool
}

func (p *peekingReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	p.n += n
	if !p.checked && p.n >= p.at {
		p.checked = true
		files, _ := os.ReadDir(p.dir)
		for _, f := range files {
			data, _ := os.ReadFile(filepath.Join(p.dir, f.Name()))
			if bytes.Contains(data, p.marker) {
				p.found = true
			}
		}
	}
	return n, err
}

// Regression (5.0.11 review): erasure coding sits above encryption at rest,
// so a spilled upload was its plaintext, written to disk on a server set to
// encrypt everything, and readable from a copy a crash left behind. The spill
// file is sealed with a key that is never stored.
func TestSpilledUploadIsNotPlaintextOnDisk(t *testing.T) {
	const block = 4096
	e, dir := newSpillEngine(t, block)
	marker := []byte("CONFIDENTIAL-CUSTOMER-RECORD")
	data := bytes.Repeat(marker, 2000) // ~56 KiB, many blocks
	pr := &peekingReader{r: bytes.NewReader(data), at: len(data) * 3 / 4, dir: filepath.Join(dir, spillDir), marker: marker}
	if _, _, err := e.PutObject("b", "secret", unknownLength{pr}, -1); err != nil {
		t.Fatal(err)
	}
	if !pr.checked {
		t.Fatal("the spill directory was never inspected")
	}
	if pr.found {
		t.Fatal("the spilled upload was written to disk as plaintext")
	}
	rc, _, err := e.GetObject("b", "secret")
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if !bytes.Equal(got, data) {
		t.Fatal("the object read back differs from what was uploaded")
	}
}
