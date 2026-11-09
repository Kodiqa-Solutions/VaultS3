package s3

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

var snowballHeader = map[string]string{"X-Amz-Meta-Snowball-Auto-Extract": "true"}

// A signed streaming archive was staged on disk in full before any entry was
// stored. It is now extracted as it arrives: the first entry is readable
// while the rest of the body is still held back by the client.
func TestSignedChunkedSnowballIsExtractedWhileItStreams(t *testing.T) {
	ts, _ := fullServer(t)
	call(t, ts, http.MethodPut, "/snows", "").must(t, "create bucket")
	archive := snowballArchive(t, "first entry")

	release := make(chan struct{})
	seen := make(chan int, 1)
	go func() {
		// Poll for a.txt while the body is held; tell the main goroutine.
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			if r := call(t, ts, http.MethodGet, "/snows/a.txt", ""); r.code == http.StatusOK {
				seen <- r.code
				close(release)
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		seen <- 0
		close(release)
	}()
	r := signedStreamPut(t, ts, "/snows/archive.tar", archive, streamOpts{chunk: 64 << 10, headers: snowballHeader,
		bodyFrom: func(framed []byte) io.Reader {
			half := len(framed) / 2
			return io.MultiReader(bytes.NewReader(framed[:half]), &waitReader{release, bytes.NewReader(framed[half:])})
		}})
	if r.code != http.StatusOK {
		t.Fatalf("honest archive: %d %s", r.code, r.body)
	}
	if code := <-seen; code != http.StatusOK {
		t.Fatal("no entry was stored before the body ended: the archive was staged, not streamed")
	}
	if got := call(t, ts, http.MethodGet, "/snows/big.bin", ""); len(got.body) != 2<<20 {
		t.Fatalf("big.bin: %d bytes", len(got.body))
	}
}

type waitReader struct {
	wait <-chan struct{}
	r    io.Reader
}

func (w *waitReader) Read(p []byte) (int, error) {
	<-w.wait
	return w.r.Read(p)
}

// While streaming, a chunk is handed to extraction only once its signature
// checks. A chunk altered after signing ends the import with 403, and no byte
// of it is stored: an entry it carries is not imported.
func TestTamperedChunkOfAStreamedSnowballIsNotStored(t *testing.T) {
	ts, _ := fullServer(t)
	call(t, ts, http.MethodPut, "/snowt", "").must(t, "create bucket")
	archive := snowballArchive(t, "small")
	// 64 KiB chunks: a.txt is in chunk 1, big.bin spans the rest. Chunk 5
	// falls inside big.bin.
	r := signedStreamPut(t, ts, "/snowt/archive.tar", archive, streamOpts{chunk: 64 << 10, tamperChunk: 5, headers: snowballHeader})
	if r.code != http.StatusForbidden || !strings.Contains(r.body, "SignatureDoesNotMatch") {
		t.Fatalf("%d %s, want 403 SignatureDoesNotMatch", r.code, r.body)
	}
	if got := call(t, ts, http.MethodGet, "/snowt/big.bin", ""); got.code != http.StatusNotFound {
		t.Fatalf("the entry with the tampered chunk was stored: %d, %d bytes", got.code, len(got.body))
	}
	// The first chunk was verified on its own, so its entry is the client's.
	if got := call(t, ts, http.MethodGet, "/snowt/a.txt", ""); got.code == http.StatusOK && got.body != "small" {
		t.Fatalf("a.txt holds %q", got.body)
	}
	// Tampering the very first chunk stores nothing at all.
	call(t, ts, http.MethodDelete, "/snowt/a.txt", "")
	r = signedStreamPut(t, ts, "/snowt/archive.tar", archive, streamOpts{chunk: 64 << 10, tamperChunk: 1, headers: snowballHeader})
	if r.code != http.StatusForbidden {
		t.Fatalf("first chunk tampered: %d %s", r.code, r.body)
	}
	if got := call(t, ts, http.MethodGet, "/snowt/a.txt", ""); got.code != http.StatusNotFound {
		t.Fatalf("an entry of a tampered first chunk was stored: %d", got.code)
	}
}

// An archive in chunks larger than can be held is staged as before, and a
// trailing checksum, checked only at the body's end, keeps it staged too.
func TestSnowballFallsBackToStagingForLargeChunksAndTrailers(t *testing.T) {
	ts, _ := fullServer(t)
	call(t, ts, http.MethodPut, "/snowf", "").must(t, "create bucket")
	archive := snowballArchive(t, "staged")
	prev := maxHeldChunk
	maxHeldChunk = 32 << 10
	defer func() { maxHeldChunk = prev }()

	r := signedStreamPut(t, ts, "/snowf/archive.tar", archive, streamOpts{chunk: 64 << 10, headers: snowballHeader})
	if r.code != http.StatusOK {
		t.Fatalf("large chunks, staged: %d %s", r.code, r.body)
	}
	if got := call(t, ts, http.MethodGet, "/snowf/a.txt", ""); got.body != "staged" {
		t.Fatalf("a.txt = %d %q", got.code, got.body)
	}
	// Large chunks with one tampered: staged, so nothing is stored.
	call(t, ts, http.MethodDelete, "/snowf/a.txt", "")
	r = signedStreamPut(t, ts, "/snowf/archive.tar", archive, streamOpts{chunk: 64 << 10, tamperChunk: 20, headers: snowballHeader})
	if r.code != http.StatusForbidden {
		t.Fatalf("tampered staged archive: %d %s", r.code, r.body)
	}
	if got := call(t, ts, http.MethodGet, "/snowf/a.txt", ""); got.code != http.StatusNotFound {
		t.Fatalf("a staged archive with a tampered chunk had an entry stored: %d", got.code)
	}
	maxHeldChunk = prev
	// A trailer with a bad signature: staged, nothing stored.
	r = signedStreamPut(t, ts, "/snowf/archive.tar", archive, streamOpts{chunk: 64 << 10, headers: snowballHeader,
		trailerName: "x-amz-checksum-crc32", trailerValue: "AAAAAA==", trailerSig: strings.Repeat("0", 64)})
	if r.code == http.StatusOK {
		t.Fatalf("an archive with a bad trailer signature was accepted")
	}
	if got := call(t, ts, http.MethodGet, "/snowf/a.txt", ""); got.code != http.StatusNotFound {
		t.Fatalf("an entry was stored from an archive whose trailer failed: %d", got.code)
	}
}
