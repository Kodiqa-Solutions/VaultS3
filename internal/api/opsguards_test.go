package api

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
	"github.com/Kodiqa-Solutions/VaultS3/internal/storage"
)

// blockingEngine holds the first speedtest write until released, and records
// every key a speedtest writes.
type blockingEngine struct {
	storage.Engine
	entered chan struct{}
	release chan struct{}
	once    sync.Once
	mu      sync.Mutex
	keys    []string
	failDir bool
}

func (b *blockingEngine) PutObject(bucket, key string, r io.Reader, size int64) (int64, string, error) {
	if bucket == speedtestBucket {
		b.mu.Lock()
		b.keys = append(b.keys, key)
		b.mu.Unlock()
		b.once.Do(func() {
			close(b.entered)
			<-b.release
		})
	}
	return b.Engine.PutObject(bucket, key, r, size)
}

func (b *blockingEngine) CreateBucketDir(bucket string) error {
	if b.failDir && bucket == speedtestBucket {
		return errors.New("read-only file system")
	}
	return b.Engine.CreateBucketDir(bucket)
}

// requestWithin fails the test when a request that should be refused at once
// instead waits on the run in progress, which is what an unguarded second run
// does here.
func requestWithin(t *testing.T, do func() *httptest.ResponseRecorder) *httptest.ResponseRecorder {
	t.Helper()
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- do() }()
	select {
	case rr := <-done:
		return rr
	case <-time.After(5 * time.Second):
		t.Fatal("a second run was not refused: it started and is waiting on the first")
		return nil
	}
}

func TestSpeedtestRunsOneAtATimeWithItsOwnKey(t *testing.T) {
	h, _ := newTestAPI(t)
	eng := &blockingEngine{Engine: h.engine, entered: make(chan struct{}), release: make(chan struct{})}
	h.engine = eng
	tok := getToken(t, h)

	first := make(chan int)
	go func() { first <- doRequest(h, "POST", "/speedtest", nil, tok).Code }()
	<-eng.entered
	rr := requestWithin(t, func() *httptest.ResponseRecorder { return doRequest(h, "POST", "/speedtest", nil, tok) })
	if rr.Code != http.StatusConflict || !strings.Contains(rr.Body.String(), "already running") {
		t.Fatalf("a second run while one runs: %d %s, want 409", rr.Code, rr.Body.String())
	}
	close(eng.release)
	if code := <-first; code != http.StatusOK {
		t.Fatalf("first run: %d", code)
	}
	if rr := doRequest(h, "POST", "/speedtest", nil, tok); rr.Code != http.StatusOK {
		t.Fatalf("a run after the first finished: %d %s", rr.Code, rr.Body.String())
	}
	if len(eng.keys) != 2 || eng.keys[0] == eng.keys[1] {
		t.Fatalf("speedtest keys %v, want two distinct keys", eng.keys)
	}

	// A failure to create the scratch directory is reported, not ignored.
	eng.failDir = true
	rr = doRequest(h, "POST", "/speedtest", nil, tok)
	if rr.Code != http.StatusInternalServerError || !strings.Contains(rr.Body.String(), "read-only file system") {
		t.Fatalf("directory failure: %d %s", rr.Code, rr.Body.String())
	}
}

// legacyEngine reports one legacy object and holds its first rewrite.
type legacyEngine struct {
	storage.Engine
	entered  chan struct{}
	release  chan struct{}
	once     sync.Once
	rewrites atomic.Int32
}

func (l *legacyEngine) IsLegacyObject(bucket, key string) (bool, error) { return true, nil }
func (l *legacyEngine) RewriteObject(bucket, key string) error {
	l.rewrites.Add(1)
	l.once.Do(func() {
		close(l.entered)
		<-l.release
	})
	return nil
}

func TestReencryptApplyRunsOneAtATime(t *testing.T) {
	h, store := newTestAPI(t)
	store.CreateBucket("b")
	store.PutObjectMeta(metadata.ObjectMeta{Bucket: "b", Key: "old.bin", Size: 1})
	eng := &legacyEngine{Engine: h.engine, entered: make(chan struct{}), release: make(chan struct{})}
	h.engine = eng
	tok := getToken(t, h)

	first := make(chan int)
	go func() { first <- doRequest(h, "POST", "/reencrypt?apply=true", nil, tok).Code }()
	<-eng.entered
	rr := requestWithin(t, func() *httptest.ResponseRecorder { return doRequest(h, "POST", "/reencrypt?apply=true", nil, tok) })
	if rr.Code != http.StatusConflict || !strings.Contains(rr.Body.String(), "already running") {
		t.Fatalf("a second apply while one runs: %d %s, want 409", rr.Code, rr.Body.String())
	}
	// A report-only run changes nothing and is not held back.
	if rr := doRequest(h, "POST", "/reencrypt", nil, tok); rr.Code != http.StatusOK {
		t.Fatalf("a dry run while an apply runs: %d", rr.Code)
	}
	close(eng.release)
	if code := <-first; code != http.StatusOK {
		t.Fatalf("first apply: %d", code)
	}
	if n := eng.rewrites.Load(); n != 1 {
		t.Fatalf("%d rewrites, want 1", n)
	}
}

// A drain body that does not parse used to be ignored, draining whichever node
// received it.
func TestDrainRefusesAMalformedBody(t *testing.T) {
	h, _ := newTestAPI(t)
	writable := &atomic.Bool{}
	writable.Store(true)
	h.SetWritable(writable)
	req := httptest.NewRequest("POST", "/api/v1/cluster/drain", strings.NewReader(`{"nodeId": "node-2"`))
	req.Header.Set("Authorization", "Bearer "+getToken(t, h))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "invalid request body") {
		t.Fatalf("malformed drain body: %d %s, want 400", rr.Code, rr.Body.String())
	}
	if !writable.Load() {
		t.Fatal("this node was drained by a request naming another one")
	}
	// No body at all still means this node.
	req = httptest.NewRequest("POST", "/api/v1/cluster/drain", nil)
	req.Header.Set("Authorization", "Bearer "+getToken(t, h))
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK || writable.Load() {
		t.Fatalf("empty drain body: %d writable=%v, want this node drained", rr.Code, writable.Load())
	}
}

// forwardUpload spoke plain http to the owner even on a TLS cluster.
func TestForwardUploadUsesTLSWhenEnabled(t *testing.T) {
	h, _ := newTestAPI(t)
	owner := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Write([]byte(`[{"key":"k","size":1}]`))
	}))
	defer owner.Close()
	addr := strings.TrimPrefix(owner.URL, "http://")

	if _, err := h.forwardUpload(addr, "b", "", "k", "text/plain", bytes.NewReader([]byte("x"))); err != nil {
		t.Fatalf("plain cluster: %v", err)
	}
	h.cfg.Server.TLS.Enabled = true
	_, err := h.forwardUpload(addr, "b", "", "k", "text/plain", bytes.NewReader([]byte("x")))
	if err == nil || !strings.Contains(err.Error(), "HTTPS client") {
		t.Fatalf("TLS cluster talking to a plain owner: %v, want the https attempt to be refused", err)
	}
}

// A function URL whose host RESOLVED to an internal address passed, since only
// literal IPs were checked.
func TestWebhookURLChecksResolvedAddresses(t *testing.T) {
	orig := lookupWebhookHost
	t.Cleanup(func() { lookupWebhookHost = orig })
	answers := map[string][]string{
		"internal.example.com": {"10.0.0.5"},
		"metadata.example.com": {"169.254.169.254"},
		"loop.example.com":     {"93.184.216.34", "127.0.0.1"},
		"public.example.com":   {"93.184.216.34"},
	}
	lookupWebhookHost = func(_ context.Context, host string) ([]net.IP, error) {
		a, ok := answers[host]
		if !ok {
			return nil, errors.New("no such host")
		}
		var ips []net.IP
		for _, s := range a {
			ips = append(ips, net.ParseIP(s))
		}
		return ips, nil
	}
	for _, u := range []string{
		"http://internal.example.com/fn",
		"https://metadata.example.com/latest",
		"http://loop.example.com/",
		"http://nowhere.example.com/",
		"http://[::ffff:127.0.0.1]/",
		"http://192.168.1.1/",
	} {
		if err := ValidateWebhookURL(u); err == nil {
			t.Errorf("%s was accepted", u)
		}
	}
	if err := ValidateWebhookURL("http://internal.example.com/fn"); err == nil || !strings.Contains(err.Error(), "resolves to 10.0.0.5") {
		t.Errorf("refusal does not name the resolved address: %v", err)
	}
	if err := ValidateWebhookURL("https://public.example.com/fn"); err != nil {
		t.Errorf("a public host was refused: %v", err)
	}
}

// failBucketDirEngine cannot create bucket directories.
type failBucketDirEngine struct{ storage.Engine }

func (failBucketDirEngine) CreateBucketDir(string) error { return errors.New("disk full") }

// The bucket record stayed when its storage could not be created, so the
// bucket listed, had nothing behind it, and a retry answered 409.
func TestCreateBucketRollsBackWhenStorageFails(t *testing.T) {
	h, store := newTestAPI(t)
	h.engine = failBucketDirEngine{h.engine}
	rr := doRequest(h, "POST", "/buckets", map[string]string{"name": "fresh"}, getToken(t, h))
	if rr.Code != http.StatusInternalServerError || !strings.Contains(rr.Body.String(), "failed to create bucket storage") {
		t.Fatalf("got %d %s", rr.Code, rr.Body.String())
	}
	if store.BucketExists("fresh") {
		t.Fatal("the bucket record outlived its failed storage")
	}
}
