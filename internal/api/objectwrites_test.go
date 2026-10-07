package api

import (
	"bytes"
	"encoding/json"
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
