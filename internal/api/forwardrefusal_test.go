package api

import (
	"bytes"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Regression (5.0.11 round 11): an upload the owner node refused with 403
// (quota, object lock) was answered 500 by the node that forwarded it.
func TestForwardedUploadRefusalIsAnswered403(t *testing.T) {
	owner := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// what the owner's handleUpload answers for a file over quota
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`[{"key":"X","size":0,"contentType":"","error":"QuotaExceeded: the bucket's quota does not leave room for this file"}]`))
	}))
	defer owner.Close()
	h, store := newTestAPI(t)
	store.CreateBucket("b")
	h.SetClusterRouting(nil, func(bucket, key string) (string, bool) { return strings.TrimPrefix(owner.URL, "http://"), true })
	tok := sessionWithPolicy(t, h, store, "dave", `{"Statement":[{"Effect":"Allow","Action":"s3:*","Resource":["arn:aws:s3:::b","arn:aws:s3:::b/*"]}]}`)
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("file", "X")
	fw.Write([]byte("data"))
	mw.Close()
	req := httptest.NewRequest("POST", "/api/v1/buckets/b/upload", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+tok)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Errorf("a forwarded upload the owner refused: %d %s, want 403 as on a single node", rr.Code, rr.Body.String())
	}
}
