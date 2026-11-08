package api

import (
	"bytes"
	"net/http"
	"testing"
	"time"

	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
)

// Regression (5.0.11 round 12, every earlier version): a dashboard download
// read the engine even for a key with no record, and an encoded dot (%2E) is
// not redirected as "./" is, so "%2E/secret/doc" served secret/doc to a user
// denied it.
func TestDownloadOfAKeyWithNoRecordIsNotFound(t *testing.T) {
	h, store := newTestAPI(t)
	admin := getToken(t, h)
	if rr := doRequest(h, "POST", "/buckets", map[string]string{"name": "bkt1"}, admin); rr.Code >= 300 {
		t.Fatal(rr.Code)
	}
	if _, _, err := h.engine.PutObject("bkt1", "secret/doc", bytes.NewReader([]byte("TOPSECRET")), 9); err != nil {
		t.Fatal(err)
	}
	store.PutObjectMeta(metadata.ObjectMeta{Bucket: "bkt1", Key: "secret/doc", Size: 9})
	store.CreateIAMUser(metadata.IAMUser{Name: "eve", PolicyARNs: []string{"p"}})
	store.CreateIAMPolicy(metadata.IAMPolicy{Name: "p", Document: `{"Statement":[{"Effect":"Allow","Action":["s3:*"],"Resource":["arn:aws:s3:::bkt1","arn:aws:s3:::bkt1/*"]},{"Effect":"Deny","Action":["s3:*"],"Resource":["arn:aws:s3:::bkt1/secret/*"]}]}`})
	eve, _ := h.jwt.Generate("eve", time.Hour)
	for _, p := range []string{"secret/doc", "%2E/secret/doc", "secret/%2E/doc", "secret%2F%2Fdoc", "x/%2E%2E/secret/doc"} {
		rr := doRequest(h, "GET", "/buckets/bkt1/download/"+p, nil, eve)
		if p != "secret/doc" && rr.Code == http.StatusOK && rr.Body.String() == "TOPSECRET" {
			t.Errorf("Deny on secret/* bypassed by %s", p)
		}
	}
}
