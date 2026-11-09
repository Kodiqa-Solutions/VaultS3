package api

import (
	"net/http"
	"testing"
)

// A failed metadata write of a suspended upload over the current null version
// must leave the key readable.
func TestAFailedPointerWriteOverTheNullVersionKeepsTheKey(t *testing.T) {
	for _, withOlder := range []bool{false, true} {
		h, store := newTestAPI(t)
		store.CreateBucket("sus")
		h.engine.CreateBucketDir("sus")
		if withOlder {
			store.SetBucketVersioning("sus", "Enabled")
			if rr, _ := uploadOne(t, h, "sus", "k.txt", "older"); rr.Code != http.StatusOK {
				t.Fatal(rr.Code)
			}
		}
		store.SetBucketVersioning("sus", "Suspended")
		if rr, _ := uploadOne(t, h, "sus", "k.txt", "first"); rr.Code != http.StatusOK {
			t.Fatal(rr.Code)
		}
		h.store = &faultyStore{StoreAPI: store, failPutObjectMeta: true}
		uploadOne(t, h, "sus", "k.txt", "second")
		h.store = store
		m, err := store.GetObjectMeta("sus", "k.txt")
		vs, _, _ := store.ListObjectVersions("sus", "k.txt", "", "", 10)
		t.Logf("withOlder=%v pointer=%+v err=%v", withOlder, m, err)
		for _, v := range vs {
			t.Logf("  version %s latest=%v", v.VersionID, v.IsLatest)
		}
		if err != nil || m == nil || m.VersionID != "null" {
			t.Errorf("withOlder=%v: pointer after the failed upload = %+v (%v), want the null version still current", withOlder, m, err)
		}
	}
}
