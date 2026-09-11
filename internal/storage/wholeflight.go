package storage

import (
	"bytes"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
)

// Shared materialisation of whole-object ciphertext.
//
// Three at-rest formats predate the chunked VS3S stream (issue #49) and cannot
// be streamed: the original whole-object AES-GCM blob of the single-key engine,
// the per-bucket VS3X blob, and the KMS-wrapped blob. One GCM tag covers the
// entire object, so nothing may be released before all of it has been read and
// verified, and the reader has to hold the plaintext. That cost is unavoidable
// per object; what was avoidable is paying it per request. N concurrent GETs of
// the same legacy object (a video player issuing Range requests, a parquet reader
// probing footers) each read and decrypted their own copy, so peak memory was
// object size times concurrency, the shape that OOM-killed nodes in #49 and was
// left for the legacy formats after #53.
//
// wholeFlight is a per-engine singleflight over that work: the first request to
// arrive for a stored blob decrypts it, every request that arrives while that
// decryption is still running waits for it and shares the same []byte through
// its own bytes.Reader (reads only, so sharing is safe), and the entry is
// forgotten the moment the decryption finishes. It is dedup of in-flight work,
// not a cache: a request arriving after that decrypts afresh and sees whatever
// is on disk then, so an overwrite is never masked by a buffer of the previous
// contents, and the plaintext becomes garbage as soon as its readers drop it.
// Objects rewritten to the streaming format never come here.
type wholeFlight struct {
	mu      sync.Mutex
	entries map[string]*wholeEntry
	// runs counts materialisations that actually ran, for tests and diagnostics.
	runs atomic.Int64
}

// wholeEntry is one in-flight materialisation.
type wholeEntry struct {
	done chan struct{} // closed when data/err are set
	data []byte
	err  error
	// joined counts the requests attached to this entry, including the one
	// decrypting. Tests use it to know every concurrent reader has arrived.
	joined int
}

// wholeFlightKey names a stored blob for dedup. The stored size is included so
// two requests can only ever share work on the same bytes.
func wholeFlightKey(bucket, key, versionID string, stored int64) string {
	return fmt.Sprintf("%s\x00%s\x00%s\x00%d", bucket, key, versionID, stored)
}

// open returns a reader over the plaintext for key, running materialise only if
// no other request is currently doing so. materialise owns src and must close
// it; when the work is shared, src is closed here unread.
func (f *wholeFlight) open(key string, src io.Closer, materialise func() ([]byte, error)) (ReadSeekCloser, int64, error) {
	f.mu.Lock()
	if f.entries == nil {
		f.entries = make(map[string]*wholeEntry)
	}
	e, shared := f.entries[key]
	if !shared {
		e = &wholeEntry{done: make(chan struct{})}
		f.entries[key] = e
	}
	e.joined++
	f.mu.Unlock()

	if shared {
		src.Close()
		<-e.done
	} else {
		f.runs.Add(1)
		e.data, e.err = materialise()
		// Forget the entry before waking the waiters: anyone arriving from here
		// on starts a fresh read rather than joining a finished one.
		f.mu.Lock()
		if f.entries[key] == e {
			delete(f.entries, key)
		}
		f.mu.Unlock()
		close(e.done)
	}
	if e.err != nil {
		return nil, 0, e.err
	}
	return &bytesReadSeekCloser{Reader: bytes.NewReader(e.data)}, int64(len(e.data)), nil
}
