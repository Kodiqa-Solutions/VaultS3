package storage

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"io"
	"runtime"
	"sync"
	"testing"
	"time"
)

// gatedEngine wraps an Engine so that the readers it hands out serve the small
// header peeks freely but do not yield the object body until the test opens the
// gate. That holds N concurrent GETs at the point where each would start
// materialising, which is the race the singleflight has to collapse.
type gatedEngine struct {
	Engine
	gate chan struct{}
}

func (g *gatedEngine) GetObject(bucket, key string) (ReadSeekCloser, int64, error) {
	r, n, err := g.Engine.GetObject(bucket, key)
	if err != nil {
		return nil, 0, err
	}
	return &gatedReader{ReadSeekCloser: r, gate: g.gate}, n, nil
}

type gatedReader struct {
	ReadSeekCloser
	gate <-chan struct{}
}

func (r *gatedReader) Read(p []byte) (int, error) {
	if len(p) > streamHeaderLen { // the format peeks are shorter than this
		<-r.gate
	}
	return r.ReadSeekCloser.Read(p)
}

// waitForRefs blocks until n requests have joined the one entry in f.
func waitForRefs(t *testing.T, f *wholeFlight, n int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		f.mu.Lock()
		refs := 0
		for _, e := range f.entries {
			refs += e.joined
		}
		f.mu.Unlock()
		if refs == n {
			return
		}
		runtime.Gosched()
	}
	t.Fatalf("%d readers never all attached to the shared entry", n)
}

// Twenty concurrent GETs of one legacy whole-object blob must decrypt it once
// and serve every request the full plaintext. Before this the encrypted engine
// made twenty copies.
func TestConcurrentLegacyReadsShareOneMaterialisation(t *testing.T) {
	const readers = 20
	fs, err := NewFileSystem(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := fs.CreateBucketDir("b"); err != nil {
		t.Fatal(err)
	}
	key := testKey(t)
	plain := bytes.Repeat([]byte("whole-object ciphertext, sealed before streaming existed\n"), 2000)

	// Write the blob exactly as the old engine did: nonce || GCM seal of everything.
	block, _ := aes.NewCipher(key)
	gcm, _ := cipher.NewGCM(block)
	nonce := make([]byte, gcm.NonceSize())
	rand.Read(nonce)
	legacy := gcm.Seal(nonce, nonce, plain, nil)
	if _, _, err := fs.PutObject("b", "old.bin", bytes.NewReader(legacy), int64(len(legacy))); err != nil {
		t.Fatal(err)
	}

	gated := &gatedEngine{Engine: fs, gate: make(chan struct{})}
	enc, err := NewEncryptedEngine(gated, key)
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	results := make([][]byte, readers)
	errs := make([]error, readers)
	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r, size, err := enc.GetObject("b", "old.bin")
			if err != nil {
				errs[i] = err
				return
			}
			defer r.Close()
			if size != int64(len(plain)) {
				t.Errorf("reader %d: size %d, want %d", i, size, len(plain))
			}
			// Half the readers take a Range, the rest stream the whole object.
			if i%2 == 1 {
				if _, err := r.Seek(int64(i*100), io.SeekStart); err != nil {
					errs[i] = err
					return
				}
				got := make([]byte, 64)
				_, errs[i] = io.ReadFull(r, got)
				if !bytes.Equal(got, plain[i*100:i*100+64]) {
					t.Errorf("reader %d: range read returned the wrong bytes", i)
				}
				results[i] = plain
				return
			}
			results[i], errs[i] = io.ReadAll(r)
		}(i)
	}
	waitForRefs(t, &enc.whole, readers)
	close(gated.gate)
	wg.Wait()

	for i := range results {
		if errs[i] != nil {
			t.Fatalf("reader %d: %v", i, errs[i])
		}
		if !bytes.Equal(results[i], plain) {
			t.Fatalf("reader %d did not get the full plaintext", i)
		}
	}
	if runs := enc.whole.runs.Load(); runs != 1 {
		t.Fatalf("legacy blob was decrypted %d times for %d concurrent readers, want once", runs, readers)
	}

	// The decryption is over, so nothing may be retained: a fresh GET
	// materialises again (this is dedup of in-flight work, not a cache).
	enc.whole.mu.Lock()
	held := len(enc.whole.entries)
	enc.whole.mu.Unlock()
	if held != 0 {
		t.Fatalf("%d entries still held after the decryption finished", held)
	}
	r, _, err := enc.GetObject("b", "old.bin")
	if err != nil {
		t.Fatal(err)
	}
	r.Close()
	if runs := enc.whole.runs.Load(); runs != 2 {
		t.Fatalf("a GET after the shared decryption finished ran %d materialisations in total, want 2", runs)
	}
}
