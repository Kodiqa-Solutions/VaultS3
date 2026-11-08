package api

import (
	"bytes"
	"encoding/json"
	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func uploadOne(t *testing.T, h *APIHandler, bucket, name, content string) (*httptest.ResponseRecorder, []uploadResult) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("file", name)
	fw.Write([]byte(content))
	mw.Close()
	req := httptest.NewRequest("POST", "/api/v1/buckets/"+bucket+"/upload", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+getToken(t, h))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	var res []uploadResult
	json.Unmarshal(rr.Body.Bytes(), &res)
	return rr, res
}

// countFiles counts regular files under the data directory, so a test can see
// bytes left behind with no record.
func countFiles(t *testing.T, h *APIHandler) int {
	t.Helper()
	n := 0
	filepath.Walk(h.engine.DataDir(), func(p string, info os.FileInfo, err error) error {
		if err == nil && info.Mode().IsRegular() && !strings.Contains(p, ".vaults3") {
			n++
		}
		return nil
	})
	return n
}

// An upload whose metadata write failed used to report success and leave the
// bytes on disk with no record, which the reclaim scan later deletes.
func TestUploadReportsAndUndoesAFailedMetadataWrite(t *testing.T) {
	for _, versioned := range []bool{false, true} {
		h, store := newTestAPI(t)
		if err := store.CreateBucket("b"); err != nil {
			t.Fatal(err)
		}
		h.engine.CreateBucketDir("b")
		if versioned {
			store.SetBucketVersioning("b", "Enabled")
		}
		before := countFiles(t, h)
		h.store = &faultyStore{StoreAPI: store, failPutObjectMeta: true}
		rr, res := uploadOne(t, h, "b", "doc.txt", "content")
		if rr.Code != http.StatusInternalServerError || len(res) != 1 || !strings.Contains(res[0].Error, errInjected.Error()) {
			t.Fatalf("versioned=%v: %d %s, want 500 carrying the store error", versioned, rr.Code, rr.Body.String())
		}
		if after := countFiles(t, h); after != before {
			t.Fatalf("versioned=%v: %d files on disk after a failed upload, want %d", versioned, after, before)
		}
		if versioned {
			if v, _, _ := store.ListObjectVersions("b", "doc.txt", "", "", 10); len(v) != 0 {
				t.Fatalf("a version record survived a failed upload: %+v", v)
			}
		}
	}
}

func TestSingleDeleteReportsAFailedDeleteMarker(t *testing.T) {
	h, store := newTestAPI(t)
	store.CreateBucket("v")
	h.engine.CreateBucketDir("v")
	store.SetBucketVersioning("v", "Enabled")
	if rr, _ := uploadOne(t, h, "v", "k.txt", "x"); rr.Code != http.StatusOK {
		t.Fatalf("upload: %d", rr.Code)
	}
	h.store = &faultyStore{StoreAPI: store, failPutObjectVer: true}
	rr := doRequest(h, "DELETE", "/buckets/v/objects/k.txt", nil, getToken(t, h))
	if rr.Code != http.StatusInternalServerError || !strings.Contains(rr.Body.String(), errInjected.Error()) {
		t.Fatalf("got %d %s, want 500 with the store error", rr.Code, rr.Body.String())
	}
	if m, _ := store.GetObjectMeta("v", "k.txt"); m == nil || m.DeleteMarker {
		t.Fatal("the object is gone although the delete reported failure")
	}
}

func readLatest(t *testing.T, h *APIHandler, bucket, key string) string {
	t.Helper()
	r, _, _, err := h.getLatestObject(bucket, key)
	if err != nil {
		t.Fatalf("read %s: %v", key, err)
	}
	defer r.Close()
	b, _ := io.ReadAll(r)
	return string(b)
}

// Rollback wrote the old content to the plain path with no version id and
// left the previous latest marked latest. It is now a new version.
func TestRollbackCreatesANewLatestVersion(t *testing.T) {
	h, store := newTestAPI(t)
	store.CreateBucket("v")
	h.engine.CreateBucketDir("v")
	store.SetBucketVersioning("v", "Enabled")
	uploadOne(t, h, "v", "k.txt", "first")
	first, _ := store.GetObjectMeta("v", "k.txt")
	uploadOne(t, h, "v", "k.txt", "second")
	second, _ := store.GetObjectMeta("v", "k.txt")

	tok := getToken(t, h)
	rr := doRequest(h, "POST", "/versions/rollback", map[string]string{"bucket": "v", "key": "k.txt", "versionId": first.VersionID}, tok)
	if rr.Code != http.StatusOK {
		t.Fatalf("rollback: %d %s", rr.Code, rr.Body.String())
	}
	var resp map[string]interface{}
	json.Unmarshal(rr.Body.Bytes(), &resp)
	newID, _ := resp["versionId"].(string)
	latest, _ := store.GetObjectMeta("v", "k.txt")
	if newID == "" || latest.VersionID != newID || newID == first.VersionID || newID == second.VersionID {
		t.Fatalf("latest is %q, response says %q, want a new version id", latest.VersionID, newID)
	}
	if got := readLatest(t, h, "v", "k.txt"); got != "first" {
		t.Fatalf("content after rollback %q, want first", got)
	}
	versions, _, _ := store.ListObjectVersions("v", "k.txt", "", "", 10)
	latestCount := 0
	for _, v := range versions {
		if v.IsLatest {
			latestCount++
			if v.VersionID != newID {
				t.Errorf("version %s still claims to be latest", v.VersionID)
			}
		}
	}
	if len(versions) != 3 || latestCount != 1 {
		t.Fatalf("%d versions, %d latest, want 3 and 1: %+v", len(versions), latestCount, versions)
	}

	// The next upload keeps the rolled back content as a version.
	uploadOne(t, h, "v", "k.txt", "third")
	r, _, err := h.getVersionData("v", "k.txt", newID)
	if err != nil {
		t.Fatalf("the rolled back content was lost by the next upload: %v", err)
	}
	b, _ := io.ReadAll(r)
	r.Close()
	if string(b) != "first" {
		t.Fatalf("rolled back version holds %q", b)
	}

	// Without versioning a rollback is refused, since it would overwrite.
	store.CreateBucket("plain")
	rr = doRequest(h, "POST", "/versions/rollback", map[string]string{"bucket": "plain", "key": "k.txt", "versionId": "x"}, tok)
	if rr.Code != http.StatusConflict {
		t.Fatalf("rollback on an unversioned bucket: %d", rr.Code)
	}
}

func TestRollbackUndoesAFailedMetadataWrite(t *testing.T) {
	h, store := newTestAPI(t)
	store.CreateBucket("v")
	h.engine.CreateBucketDir("v")
	store.SetBucketVersioning("v", "Enabled")
	uploadOne(t, h, "v", "k.txt", "first")
	first, _ := store.GetObjectMeta("v", "k.txt")
	uploadOne(t, h, "v", "k.txt", "second")
	before := countFiles(t, h)

	h.store = &faultyStore{StoreAPI: store, failPutObjectMeta: true}
	rr := doRequest(h, "POST", "/versions/rollback", map[string]string{"bucket": "v", "key": "k.txt", "versionId": first.VersionID}, getToken(t, h))
	if rr.Code != http.StatusInternalServerError || !strings.Contains(rr.Body.String(), "rollback not recorded") {
		t.Fatalf("got %d %s, want 500 rollback not recorded", rr.Code, rr.Body.String())
	}
	h.store = store
	if got := readLatest(t, h, "v", "k.txt"); got != "second" {
		t.Fatalf("content after a failed rollback %q, want second", got)
	}
	if after := countFiles(t, h); after != before {
		t.Fatalf("%d files after a failed rollback, want %d", after, before)
	}
	versions, _, _ := store.ListObjectVersions("v", "k.txt", "", "", 10)
	for _, v := range versions {
		if v.IsLatest && v.VersionID == first.VersionID {
			t.Fatal("the failed rollback left the old version marked latest")
		}
	}
	m, _ := store.GetObjectMeta("v", "k.txt")
	if !m.IsLatest {
		t.Fatal("the latest pointer lost IsLatest")
	}
}

// Regression (5.0.11): a rollback writes a new version, but was never held to
// the bucket's quota.
func TestRollbackIsHeldToTheQuota(t *testing.T) {
	h, store := newTestAPI(t)
	store.CreateBucket("vq")
	h.engine.CreateBucketDir("vq")
	store.SetBucketVersioning("vq", "Enabled")
	uploadOne(t, h, "vq", "k.txt", "first-version-bytes")
	first, _ := store.GetObjectMeta("vq", "k.txt")
	uploadOne(t, h, "vq", "k.txt", "second")
	if err := store.UpdateBucketQuota("vq", 30, 0); err != nil {
		t.Fatal(err)
	}
	rr := doRequest(h, "POST", "/versions/rollback", map[string]string{"bucket": "vq", "key": "k.txt", "versionId": first.VersionID}, getToken(t, h))
	if rr.Code != http.StatusForbidden || !strings.Contains(rr.Body.String(), "QuotaExceeded") {
		t.Fatalf("a rollback past the quota: %d %s, want 403 QuotaExceeded", rr.Code, rr.Body.String())
	}
	if latest, _ := store.GetObjectMeta("vq", "k.txt"); latest.Size != int64(len("second")) {
		t.Fatalf("the refused rollback changed the latest version: %+v", latest)
	}
}

// Regression (5.0.11 round 10): in a suspended bucket a dashboard upload took
// the plain path, so over a null version it left that version's bytes behind
// and listed a second null version, and the quota gave back bytes that were
// never freed. It writes the null version in place now, as S3 does.
func TestSuspendedUploadReplacesTheNullVersion(t *testing.T) {
	h, store := newTestAPI(t)
	store.CreateBucket("sus")
	h.engine.CreateBucketDir("sus")
	store.SetBucketVersioning("sus", "Suspended")
	for _, content := range []string{"first", "second"} {
		if rr, res := uploadOne(t, h, "sus", "k.txt", content); rr.Code != http.StatusOK {
			t.Fatalf("upload %q: %d %+v", content, rr.Code, res)
		}
	}
	versions, _, _ := store.ListObjectVersions("sus", "k.txt", "", "", 10)
	if len(versions) != 1 || versions[0].VersionID != "null" || !versions[0].IsLatest {
		t.Fatalf("versions after two uploads to a suspended bucket: %+v, want one latest null version", versions)
	}
	if got := readLatest(t, h, "sus", "k.txt"); got != "second" {
		t.Fatalf("content %q, want second", got)
	}
	if n := countFiles(t, h); n != 1 {
		t.Fatalf("%d files on disk, want 1 (the null version)", n)
	}
}

// A dashboard upload never checked object lock, so it could overwrite a
// locked object in a bucket without versioning (5.0.11 round 10).
func TestUploadDoesNotOverwriteALockedObject(t *testing.T) {
	h, store := newTestAPI(t)
	store.CreateBucket("lk")
	h.engine.CreateBucketDir("lk")
	uploadOne(t, h, "lk", "k.txt", "kept")
	meta, _ := store.GetObjectMeta("lk", "k.txt")
	meta.LegalHold = true
	if err := store.PutObjectMeta(*meta); err != nil {
		t.Fatal(err)
	}
	rr, res := uploadOne(t, h, "lk", "k.txt", "replaced")
	if rr.Code != http.StatusForbidden || len(res) != 1 || !strings.Contains(res[0].Error, "legal hold") {
		t.Fatalf("an upload over an object under legal hold: %d %+v, want 403", rr.Code, res)
	}
	if got := readLatest(t, h, "lk", "k.txt"); got != "kept" {
		t.Fatalf("the locked object now holds %q", got)
	}
}

// Regression (5.0.11 round 11): bulk delete took its keys from the body with
// no check, so "./doc" deleted doc's file under another name, past the lock
// and the policy written for doc.
func TestBulkDeleteRefusesAliasingKeys(t *testing.T) {
	h, store := newTestAPI(t)
	store.CreateBucket("bd")
	h.engine.CreateBucketDir("bd")
	uploadOne(t, h, "bd", "doc", "keep")
	rr := doRequest(h, "POST", "/buckets/bd/bulk-delete", map[string][]string{"keys": {"./doc", "a//doc"}}, getToken(t, h))
	if !strings.Contains(rr.Body.String(), "segment") {
		t.Fatalf("bulk delete of aliasing keys: %d %s, want them refused", rr.Code, rr.Body.String())
	}
	if got := readLatest(t, h, "bd", "doc"); got != "keep" {
		t.Fatalf("doc after a bulk delete of ./doc: %q", got)
	}
}

// Regression (5.0.11 round 11): in a suspended bucket whose latest version is
// not the null version, a dashboard upload checked the lock of the latest
// version, not of the null version it replaces, and overwrote a held one.
func TestSuspendedUploadChecksTheNullVersionsLock(t *testing.T) {
	h, store := newTestAPI(t)
	store.CreateBucket("sl")
	h.engine.CreateBucketDir("sl")
	store.SetBucketVersioning("sl", "Suspended")
	uploadOne(t, h, "sl", "k.txt", "held")
	null, _ := store.GetObjectVersion("sl", "k.txt", "null")
	null.LegalHold = true
	null.IsLatest = false
	if err := store.PutObjectVersion(*null); err != nil {
		t.Fatal(err)
	}
	newer := metadata.ObjectMeta{Bucket: "sl", Key: "k.txt", VersionID: "0000000000000009aaaaaaaa", ETag: "e", Size: 1, IsLatest: true}
	if err := store.PutObjectVersion(newer); err != nil {
		t.Fatal(err)
	}
	if err := store.PutObjectMeta(newer); err != nil {
		t.Fatal(err)
	}
	rr, res := uploadOne(t, h, "sl", "k.txt", "replaced")
	if rr.Code != http.StatusForbidden || len(res) != 1 || !strings.Contains(res[0].Error, "legal hold") {
		t.Fatalf("an upload over a held null version below a newer one: %d %+v, want 403", rr.Code, res)
	}
}
