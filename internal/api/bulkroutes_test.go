package api

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
)

// A user allowed everything on b/* except b/secret/*. The bulk routes checked
// only the bucket wildcard, so the Deny never applied to them.
const denySecretDoc = `{"Statement":[` +
	`{"Effect":"Allow","Action":"s3:*","Resource":["arn:aws:s3:::b","arn:aws:s3:::b/*"]},` +
	`{"Effect":"Deny","Action":"s3:*","Resource":"arn:aws:s3:::b/secret/*"}]}`

// putPlain stores an object the way a non-versioned PUT does.
func putPlain(t *testing.T, h *APIHandler, store *metadata.Store, bucket, key, body string) {
	t.Helper()
	_, etag, err := h.engine.PutObject(bucket, key, strings.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.PutObjectMeta(metadata.ObjectMeta{Bucket: bucket, Key: key, ETag: etag, Size: int64(len(body))}); err != nil {
		t.Fatal(err)
	}
}

type bulkResult struct {
	Key     string `json:"key"`
	Deleted bool   `json:"deleted"`
	Error   string `json:"error"`
}

func bulkDelete(t *testing.T, h *APIHandler, bucket, tok string, keys ...string) map[string]bulkResult {
	t.Helper()
	rr := doRequest(h, "POST", "/buckets/"+bucket+"/bulk-delete", map[string][]string{"keys": keys}, tok)
	if rr.Code != http.StatusOK {
		t.Fatalf("bulk delete: %d %s", rr.Code, rr.Body.String())
	}
	var res []bulkResult
	if err := json.Unmarshal(rr.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	out := map[string]bulkResult{}
	for _, r := range res {
		out[r.Key] = r
	}
	return out
}

func TestBulkDeleteChecksEachKey(t *testing.T) {
	h, store := newTestAPI(t)
	if err := store.CreateBucket("b"); err != nil {
		t.Fatal(err)
	}
	h.engine.CreateBucketDir("b")
	putPlain(t, h, store, "b", "secret/plan.txt", "classified")
	putPlain(t, h, store, "b", "public.txt", "hello")
	tok := sessionWithPolicy(t, h, store, "dave", denySecretDoc)

	res := bulkDelete(t, h, "b", tok, "secret/plan.txt", "public.txt")
	if r := res["secret/plan.txt"]; r.Deleted || !strings.Contains(r.Error, "access denied: s3:DeleteObject on arn:aws:s3:::b/secret/plan.txt") {
		t.Errorf("denied key: %+v, want a per-key access denied", r)
	}
	if !res["public.txt"].Deleted {
		t.Errorf("the allowed bystander was not deleted: %+v", res["public.txt"])
	}
	if m, err := store.GetObjectMeta("b", "secret/plan.txt"); err != nil || m == nil || !h.engine.ObjectExists("b", "secret/plan.txt") {
		t.Fatal("the denied object was destroyed")
	}
}

func TestUploadChecksEachKey(t *testing.T) {
	h, store := newTestAPI(t)
	if err := store.CreateBucket("b"); err != nil {
		t.Fatal(err)
	}
	tok := sessionWithPolicy(t, h, store, "dave", denySecretDoc)

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("file", "plan.txt")
	fw.Write([]byte("overwritten"))
	fw, _ = mw.CreateFormFile("file", "other.txt")
	fw.Write([]byte("fine"))
	mw.Close()
	// The prefix puts the first file under secret/, which the policy denies.
	req := httptest.NewRequest("POST", "/api/v1/buckets/b/upload?prefix="+url.QueryEscape("secret/"), &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+tok)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	var res []uploadResult
	json.Unmarshal(rr.Body.Bytes(), &res)
	if rr.Code != http.StatusForbidden || len(res) != 2 {
		t.Fatalf("upload under a denied prefix: %d %s", rr.Code, rr.Body.String())
	}
	for _, r := range res {
		if !strings.Contains(r.Error, "access denied: s3:PutObject on arn:aws:s3:::b/secret/") {
			t.Errorf("%s: %q, want access denied on its own key", r.Key, r.Error)
		}
	}
	if h.engine.ObjectExists("b", "secret/plan.txt") {
		t.Fatal("a file was written under the denied prefix")
	}

	// The same user can still upload outside it.
	var ok bytes.Buffer
	mw = multipart.NewWriter(&ok)
	fw, _ = mw.CreateFormFile("file", "fine.txt")
	fw.Write([]byte("fine"))
	mw.Close()
	req = httptest.NewRequest("POST", "/api/v1/buckets/b/upload", &ok)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+tok)
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("an allowed upload answered %d %s", rr.Code, rr.Body.String())
	}
}

// readZip returns the archive's entries by name.
func readZip(t *testing.T, body []byte) map[string]string {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatalf("not a zip: %v", err)
	}
	out := map[string]string{}
	for _, f := range zr.File {
		rc, _ := f.Open()
		b, _ := io.ReadAll(rc)
		rc.Close()
		out[f.Name] = string(b)
	}
	return out
}

func TestDownloadZipChecksEachKeyAndListsWhatItSkipped(t *testing.T) {
	h, store := newTestAPI(t)
	if err := store.CreateBucket("b"); err != nil {
		t.Fatal(err)
	}
	h.engine.CreateBucketDir("b")
	putPlain(t, h, store, "b", "secret/plan.txt", "classified")
	putPlain(t, h, store, "b", "a,b.txt", "comma")
	putPlain(t, h, store, "b", "public.txt", "hello")
	tok := sessionWithPolicy(t, h, store, "dave", denySecretDoc)

	q := url.Values{}
	q.Add("key", "secret/plan.txt")
	q.Add("key", "a,b.txt")
	q.Add("key", "public.txt")
	q.Add("key", "missing.txt")
	q.Set("token", tok)
	req := httptest.NewRequest("GET", "/api/v1/buckets/b/download-zip?"+q.Encode(), nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("download-zip: %d %s", rr.Code, rr.Body.String())
	}
	files := readZip(t, rr.Body.Bytes())
	if _, ok := files["secret/plan.txt"]; ok {
		t.Fatal("the zip contains an object the caller is denied")
	}
	if files["a,b.txt"] != "comma" || files["public.txt"] != "hello" {
		t.Fatalf("allowed objects missing or wrong: %v", files)
	}
	errs := files[zipErrorsName]
	if !strings.Contains(errs, "secret/plan.txt: access denied") || !strings.Contains(errs, "missing.txt: not found") {
		t.Fatalf("errors.txt does not explain the skipped keys: %q", errs)
	}
}

// On a cluster the zip read only this node's disk and dropped every key held
// elsewhere. A key owned by another node must be fetched through the proxy.
func TestDownloadZipFetchesRemoteKeysThroughTheProxy(t *testing.T) {
	h, store := newTestAPI(t)
	if err := store.CreateBucket("b"); err != nil {
		t.Fatal(err)
	}
	h.engine.CreateBucketDir("b")
	putPlain(t, h, store, "b", "local.txt", "here")
	var proxied []string
	h.SetClusterRouting(func(w http.ResponseWriter, r *http.Request, bucket, key string) bool {
		switch key {
		case "remote.txt":
			proxied = append(proxied, r.Method+" "+r.URL.Path+" "+r.Header.Get("Authorization"))
			w.WriteHeader(http.StatusOK)
			io.WriteString(w, "from the owner")
			return true
		case "down.txt":
			w.WriteHeader(http.StatusServiceUnavailable)
			io.WriteString(w, "owner unreachable")
			return true
		}
		return false
	}, nil)
	tok := getToken(t, h)

	req := httptest.NewRequest("GET", "/api/v1/buckets/b/download-zip?keys=local.txt,remote.txt,down.txt&token="+tok, nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	files := readZip(t, rr.Body.Bytes())
	if files["local.txt"] != "here" || files["remote.txt"] != "from the owner" {
		t.Fatalf("zip entries: %v", files)
	}
	if _, ok := files["down.txt"]; ok {
		t.Fatal("a failed remote read left an entry in the zip")
	}
	if !strings.Contains(files[zipErrorsName], "down.txt: owner node answered 503: owner unreachable") {
		t.Fatalf("errors.txt: %q", files[zipErrorsName])
	}
	want := "GET /api/v1/buckets/b/download/remote.txt Bearer " + tok
	if len(proxied) != 1 || proxied[0] != want {
		t.Fatalf("proxied %v, want [%s]", proxied, want)
	}
}

// The bulk delete hard-deleted the plain file: on a versioned bucket that
// destroyed the object instead of writing a delete marker.
func TestBulkDeleteWritesDeleteMarkersOnAVersionedBucket(t *testing.T) {
	h, store := newTestAPI(t)
	if err := store.CreateBucket("v"); err != nil {
		t.Fatal(err)
	}
	h.engine.CreateBucketDir("v")
	// An object written before versioning was turned on: bytes at the plain
	// path, no version id.
	putPlain(t, h, store, "v", "old.txt", "pre-versioning")
	if err := store.SetBucketVersioning("v", "Enabled"); err != nil {
		t.Fatal(err)
	}

	res := bulkDelete(t, h, "v", getToken(t, h), "old.txt")
	if !res["old.txt"].Deleted {
		t.Fatalf("delete: %+v", res["old.txt"])
	}
	latest, err := store.GetObjectMeta("v", "old.txt")
	if err != nil || latest == nil || !latest.DeleteMarker {
		t.Fatalf("latest after delete is %+v, want a delete marker", latest)
	}
	if !h.engine.ObjectExists("v", "old.txt") {
		t.Fatal("the bytes of the pre-versioning object were destroyed")
	}
	if v, err := store.GetObjectVersion("v", "old.txt", nullVersionID); err != nil || v.DeleteMarker {
		t.Fatalf("the pre-versioning object was not kept as the null version: %+v %v", v, err)
	}
}

func TestBulkDeleteRespectsObjectLock(t *testing.T) {
	h, store := newTestAPI(t)
	if err := store.CreateBucket("b"); err != nil {
		t.Fatal(err)
	}
	h.engine.CreateBucketDir("b")
	putPlain(t, h, store, "b", "held.txt", "evidence")
	putPlain(t, h, store, "b", "retained.txt", "records")
	putPlain(t, h, store, "b", "free.txt", "scratch")
	m, _ := store.GetObjectMeta("b", "held.txt")
	m.LegalHold = true
	store.PutObjectMeta(*m)
	m, _ = store.GetObjectMeta("b", "retained.txt")
	m.RetentionMode, m.RetentionUntil = "COMPLIANCE", 4102444800 // 2100
	store.PutObjectMeta(*m)

	res := bulkDelete(t, h, "b", getToken(t, h), "held.txt", "retained.txt", "free.txt")
	if r := res["held.txt"]; r.Deleted || !strings.Contains(r.Error, "legal hold") {
		t.Errorf("held.txt: %+v, want refused for legal hold", r)
	}
	if r := res["retained.txt"]; r.Deleted || !strings.Contains(r.Error, "COMPLIANCE retention") {
		t.Errorf("retained.txt: %+v, want refused for retention", r)
	}
	if !res["free.txt"].Deleted {
		t.Errorf("free.txt: %+v, want deleted", res["free.txt"])
	}
	for _, k := range []string{"held.txt", "retained.txt"} {
		if !h.engine.ObjectExists("b", k) {
			t.Errorf("%s was destroyed despite object lock", k)
		}
	}
	// The single delete gives the same answer.
	if rr := doRequest(h, "DELETE", "/buckets/b/objects/held.txt", nil, getToken(t, h)); rr.Code != http.StatusForbidden {
		t.Errorf("single delete of a held object: %d", rr.Code)
	}
}

// In a cluster each key of a bulk delete goes to the node that owns it, as the
// single delete always did.
func TestBulkDeleteProxiesRemoteKeys(t *testing.T) {
	h, store := newTestAPI(t)
	if err := store.CreateBucket("b"); err != nil {
		t.Fatal(err)
	}
	h.engine.CreateBucketDir("b")
	putPlain(t, h, store, "b", "local.txt", "x")
	var proxied []string
	h.SetClusterRouting(func(w http.ResponseWriter, r *http.Request, bucket, key string) bool {
		if key != "remote.txt" {
			return false
		}
		proxied = append(proxied, fmt.Sprintf("%s %s", r.Method, r.URL.Path))
		w.WriteHeader(http.StatusNoContent)
		return true
	}, nil)
	res := bulkDelete(t, h, "b", getToken(t, h), "local.txt", "remote.txt")
	if !res["local.txt"].Deleted || !res["remote.txt"].Deleted {
		t.Fatalf("results: %+v", res)
	}
	if len(proxied) != 1 || proxied[0] != "DELETE /api/v1/buckets/b/objects/remote.txt" {
		t.Fatalf("proxied %v", proxied)
	}
}
