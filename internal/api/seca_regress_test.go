package api

import (
	"strings"
	"testing"
	"time"

	"github.com/Kodiqa-Solutions/VaultS3/internal/bucketkeys"
	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
)

// Regression (5.0.14 security review): every bucket setting in the console
// took s3:PutBucketPolicy, and an object grant matched the bucket, so a user
// allowed to upload could crypto-shred the bucket.
func TestAConsoleUserCannotShredOrRotateWithoutTheRightGrant(t *testing.T) {
	h, store := newTestAPI(t)
	km, err := bucketkeys.NewManager(store, make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	h.SetKeyManager(km)
	admin := getToken(t, h)
	if rr := doRequest(h, "POST", "/buckets", map[string]string{"name": "bkt"}, admin); rr.Code >= 300 {
		t.Fatalf("create: %d %s", rr.Code, rr.Body.String())
	}
	if rr := doRequest(h, "POST", "/buckets/bkt/encryption/enable", nil, admin); rr.Code != 200 {
		t.Fatalf("enable: %d %s", rr.Code, rr.Body.String())
	}
	now := time.Now()
	doc := `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":["s3:PutObject","s3:PutObjectTagging","s3:Put*"],"Resource":["arn:aws:s3:::bkt/*"]}]}`
	_ = store.CreateIAMPolicy(metadata.IAMPolicy{Name: "objput", CreatedAt: now, Document: doc})
	_ = store.CreateIAMUser(metadata.IAMUser{Name: "uploader", CreatedAt: now, PolicyARNs: []string{"objput"}})
	tok, _ := h.jwt.Generate("uploader", time.Hour)
	if rr := doRequest(h, "POST", "/buckets/bkt/encryption/shred", nil, tok); rr.Code != 403 {
		t.Errorf("non-admin shred: %d %s, want 403", rr.Code, rr.Body.String())
	}
	if rr := doRequest(h, "GET", "/buckets/bkt/encryption", nil, admin); strings.Contains(rr.Body.String(), `"shredded":true`) {
		t.Fatal("the bucket was shredded")
	}
	// Narrower: user granted only s3:PutBucketPolicy on the bucket.
	doc2 := `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"s3:PutBucketPolicy","Resource":"arn:aws:s3:::bkt"}]}`
	_ = store.CreateIAMPolicy(metadata.IAMPolicy{Name: "polonly", CreatedAt: now, Document: doc2})
	_ = store.CreateIAMUser(metadata.IAMUser{Name: "polman", CreatedAt: now, PolicyARNs: []string{"polonly"}})
	tok2, _ := h.jwt.Generate("polman", time.Hour)
	if rr := doRequest(h, "POST", "/buckets/bkt/encryption/rotate", nil, tok2); rr.Code != 403 {
		t.Errorf("a PutBucketPolicy-only user rotated the key: %d", rr.Code)
	}
	// Not even a user granted every action: shredding is the admin's.
	doc3 := `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"*","Resource":"*"}]}`
	_ = store.CreateIAMPolicy(metadata.IAMPolicy{Name: "everything", CreatedAt: now, Document: doc3})
	_ = store.CreateIAMUser(metadata.IAMUser{Name: "poweruser", CreatedAt: now, PolicyARNs: []string{"everything"}})
	tok3, _ := h.jwt.Generate("poweruser", time.Hour)
	if rr := doRequest(h, "POST", "/buckets/bkt/encryption/shred", nil, tok3); rr.Code != 403 {
		t.Errorf("a non-admin with every action shredded: %d", rr.Code)
	}
	// The admin cannot shred a bucket that holds a locked object.
	_ = store.PutObjectMeta(metadata.ObjectMeta{Bucket: "bkt", Key: "held", Size: 1, LegalHold: true})
	if rr := doRequest(h, "POST", "/buckets/bkt/encryption/shred", nil, admin); rr.Code != 409 {
		t.Errorf("shred with a held object: %d %s, want 409", rr.Code, rr.Body.String())
	}
}
