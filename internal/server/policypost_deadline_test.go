package server

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
	"github.com/Kodiqa-Solutions/VaultS3/internal/middleware"
	"github.com/Kodiqa-Solutions/VaultS3/internal/replication"
	"github.com/Kodiqa-Solutions/VaultS3/internal/s3"
	"github.com/Kodiqa-Solutions/VaultS3/internal/storage"
)

// policyPostListener serves a real S3 handler behind the middleware the main
// listener uses, wrapped as the main listener is.
func policyPostListener(t *testing.T, servesAPI ...bool) string {
	t.Helper()
	dir := t.TempDir()
	store, err := metadata.NewStore(filepath.Join(dir, "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	dataDir := filepath.Join(dir, "data")
	_ = os.MkdirAll(dataDir, 0o755)
	engine, err := storage.NewFileSystem(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	auth := s3.NewAuthenticator("ppadmin", "pp-secret-key", store, nil, nil)
	var h http.Handler = s3.NewHandler(store, engine, auth, false, "", nil)
	h = middleware.RequestID(h)
	h = middleware.SecurityHeaders(h)
	h = middleware.PanicRecovery(h)
	return serve(t, policyPostDeadline(len(servesAPI) > 0 && servesAPI[0], h))
}

// Regression (5.0.11 review): a presigned POST's fields are read before
// anyone is authenticated, with no deadline, so an anonymous client that sent
// the headers and stalled the body held the connection for ever (5.0.10
// refused such a request at authentication before reading the body).
func TestStalledPolicyPostFieldsAreCutOff(t *testing.T) {
	shrinkTimeouts(t, 300*time.Millisecond, time.Second)
	addr := policyPostListener(t)
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_, _ = conn.Write([]byte("POST /anybucket HTTP/1.1\r\nHost: x\r\nContent-Type: multipart/form-data; boundary=XX\r\nContent-Length: 100000\r\n\r\n--XX\r\nContent-Disposition: form-data; name=\"key\"\r\n\r\nab"))
	if !closedWithin(conn, 3*time.Second) {
		t.Fatal("an anonymous presigned POST with a stalled body still holds the connection after 3s (header limit 300ms)")
	}
}

// The limit covers the fields only: once the form's signature and permission
// check out, the file streams with no deadline, however slowly.
func TestSignedPolicyPostFileStreamsWithoutTheLimit(t *testing.T) {
	shrinkTimeouts(t, 300*time.Millisecond, time.Minute)
	addr := policyPostListener(t)
	base := "http://" + addr
	mk, _ := http.NewRequest(http.MethodPut, base+"/ppslow", nil)
	replication.SignPeerRequest(mk, "ppadmin", "pp-secret-key")
	if resp, err := http.DefaultClient.Do(mk); err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("create bucket: %v %v", resp, err)
	}

	now := time.Now().UTC()
	date, amzDate := now.Format("20060102"), now.Format("20060102T150405Z")
	cred := fmt.Sprintf("ppadmin/%s/us-east-1/s3/aws4_request", date)
	doc, _ := json.Marshal(map[string]any{"expiration": now.Add(time.Hour).Format(time.RFC3339), "conditions": []any{
		map[string]string{"bucket": "ppslow"}, map[string]string{"key": "slow.bin"},
		map[string]string{"x-amz-algorithm": "AWS4-HMAC-SHA256"}, map[string]string{"x-amz-credential": cred},
		map[string]string{"x-amz-date": amzDate},
	}})
	policy := base64.StdEncoding.EncodeToString(doc)
	key := []byte("AWS4pp-secret-key")
	for _, s := range []string{date, "us-east-1", "s3", "aws4_request"} {
		m := hmac.New(sha256.New, key)
		m.Write([]byte(s))
		key = m.Sum(nil)
	}
	m := hmac.New(sha256.New, key)
	m.Write([]byte(policy))

	var head bytes.Buffer
	mw := multipart.NewWriter(&head)
	for _, kv := range [][2]string{{"key", "slow.bin"}, {"x-amz-algorithm", "AWS4-HMAC-SHA256"}, {"x-amz-credential", cred},
		{"x-amz-date", amzDate}, {"policy", policy}, {"x-amz-signature", hex.EncodeToString(m.Sum(nil))}} {
		_ = mw.WriteField(kv[0], kv[1])
	}
	_, _ = mw.CreateFormFile("file", "slow.bin")
	tail := "\r\n--" + mw.Boundary() + "--\r\n"

	// The file arrives in 10 pieces 150 ms apart: 1.5 s, five times the limit.
	pr, pw := io.Pipe()
	go func() {
		_, _ = pw.Write(head.Bytes())
		for i := 0; i < 10; i++ {
			time.Sleep(150 * time.Millisecond)
			_, _ = pw.Write([]byte("0123456789"))
		}
		_, _ = pw.Write([]byte(tail))
		pw.Close()
	}()
	req, _ := http.NewRequest(http.MethodPost, base+"/ppslow", pr)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("a signed form whose file streams slowly was cut off: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("a signed form whose file streams slowly: %d %s, want 204", resp.StatusCode, body)
	}
	get, _ := http.NewRequest(http.MethodGet, base+"/ppslow/slow.bin", nil)
	replication.SignPeerRequest(get, "ppadmin", "pp-secret-key")
	gr, err := http.DefaultClient.Do(get)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(gr.Body)
	gr.Body.Close()
	if want := strings.Repeat("0123456789", 10); string(got) != want {
		t.Fatalf("stored %q, want %q", got, want)
	}
}

// Regression (5.0.11 round 3): on a listener that also serves the dashboard,
// every path under /api/ was left without the limit, but only /api/v1/ is the
// dashboard's. POST /api/ is an S3 form upload to the bucket "api", read
// before authentication, and a stalled one held the connection for ever.
func TestStalledPolicyPostToBucketAPIIsCutOff(t *testing.T) {
	shrinkTimeouts(t, 300*time.Millisecond, time.Second)
	addr := policyPostListener(t, true)
	stall := func(path string) net.Conn {
		conn, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = conn.Write([]byte("POST " + path + " HTTP/1.1\r\nHost: x\r\nContent-Type: multipart/form-data; boundary=XX\r\nContent-Length: 100000\r\n\r\n--XX\r\nContent-Disposition: form-data; name=\"key\"\r\n\r\nab"))
		return conn
	}
	for _, path := range []string{"/api/", "/api/?x=1", "/api"} {
		conn := stall(path)
		if !closedWithin(conn, 3*time.Second) {
			t.Errorf("POST %s with a stalled body still holds the connection after 3s", path)
		}
		conn.Close()
	}
	// The dashboard's own prefix is left alone: a handler there reading a
	// slow body is not cut off.
	slowRead := serve(t, policyPostDeadline(true, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "cut", http.StatusRequestTimeout)
			return
		}
		w.WriteHeader(http.StatusOK)
	})))
	pr, pw := io.Pipe()
	go func() {
		_, _ = pw.Write([]byte("--XX\r\n"))
		time.Sleep(time.Second) // three times the limit
		_, _ = pw.Write([]byte("--XX--\r\n"))
		pw.Close()
	}()
	req, _ := http.NewRequest(http.MethodPost, "http://"+slowRead+"/api/v1/buckets/b/upload", pr)
	req.Header.Set("Content-Type", "multipart/form-data; boundary=XX")
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("a slow dashboard upload on /api/v1/: %v %v, want 200 (not cut off)", resp, err)
	}
	resp.Body.Close()
}
