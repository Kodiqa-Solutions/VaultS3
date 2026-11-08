package api

import (
	"bytes"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
)

// Regression (5.0.11 round 13): earlier versions accepted keys with "." and
// reserved segments, and their records survive an upgrade. A record
// "./secret/doc" still named secret/doc's file, so a user denied secret/*
// downloaded it, and deleting the record destroyed secret/doc although it was
// under legal hold. Such a record is not served, and deleting it removes the
// record only.
func TestLegacyAliasRecordsReachNoOtherObject(t *testing.T) {
	h, store := newTestAPI(t)
	admin := getToken(t, h)
	if rr := doRequest(h, "POST", "/buckets", map[string]string{"name": "bkt1"}, admin); rr.Code >= 300 {
		t.Fatal(rr.Code)
	}
	h.engine.PutObject("bkt1", "secret/doc", bytes.NewReader([]byte("TOPSECRET")), 9)
	store.PutObjectMeta(metadata.ObjectMeta{Bucket: "bkt1", Key: "secret/doc", Size: 9, LegalHold: true})
	store.PutObjectMeta(metadata.ObjectMeta{Bucket: "bkt1", Key: "./secret/doc", Size: 5})
	store.CreateIAMUser(metadata.IAMUser{Name: "eve", PolicyARNs: []string{"p"}})
	store.CreateIAMPolicy(metadata.IAMPolicy{Name: "p", Document: `{"Statement":[{"Effect":"Allow","Action":["s3:*"],"Resource":["arn:aws:s3:::bkt1","arn:aws:s3:::bkt1/*"]},{"Effect":"Deny","Action":["s3:*"],"Resource":["arn:aws:s3:::bkt1/secret/*"]}]}`})
	eve, _ := h.jwt.Generate("eve", time.Hour)
	if rr := doRequest(h, "GET", "/buckets/bkt1/download/%2E/secret/doc", nil, eve); rr.Code == http.StatusOK {
		t.Errorf("download of the legacy record ./secret/doc: 200 %q, want it refused", rr.Body.String())
	}
	doRequest(h, "DELETE", "/buckets/bkt1/objects/%2E/secret/doc", nil, eve)
	r, _, err := h.engine.GetObject("bkt1", "secret/doc")
	if err != nil {
		t.Fatalf("deleting the legacy record destroyed secret/doc's data: %v", err)
	}
	got, _ := io.ReadAll(r)
	r.Close()
	if string(got) != "TOPSECRET" {
		t.Fatalf("secret/doc after deleting ./secret/doc: %q", got)
	}

	// A legacy record under the version store names a version's file.
	doRequest(h, "POST", "/buckets", map[string]string{"name": "bkt2"}, admin)
	store.SetBucketVersioning("bkt2", "Suspended")
	h.engine.PutObjectVersion("bkt2", "X", "null", bytes.NewReader([]byte("XDATA")), 5)
	store.PutObjectMeta(metadata.ObjectMeta{Bucket: "bkt2", Key: "X", Size: 5, VersionID: "null"})
	store.PutObjectMeta(metadata.ObjectMeta{Bucket: "bkt2", Key: ".vs/X/null", Size: 5})
	if rr := doRequest(h, "DELETE", "/buckets/bkt2/objects/.vs/X/null", nil, admin); rr.Code >= 300 {
		t.Fatalf("deleting the legacy record .vs/X/null: %d %s", rr.Code, rr.Body.String())
	}
	if _, err := store.GetObjectMeta("bkt2", ".vs/X/null"); err == nil {
		t.Error("the legacy record .vs/X/null is still there")
	}
	if _, _, err := h.engine.GetObjectVersion("bkt2", "X", "null"); err != nil {
		t.Fatalf("deleting the legacy record destroyed X's null version: %v", err)
	}
}
