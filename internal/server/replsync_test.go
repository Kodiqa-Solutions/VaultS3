package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
	"github.com/Kodiqa-Solutions/VaultS3/internal/replication"
)

// Regression (5.0.11): /_replication/sync accepted only the cluster secret,
// which no peer sends: a peer signs its pull with SigV4 using the access key
// of its replication.peers entry. Every active-active sync got 403 from
// 4.4.56 on and nothing replicated.
func TestReplicationSyncAcceptsASignedPeer(t *testing.T) {
	srv := newTestServer(t)
	srv.cfg.Cluster.Secret = "the-cluster-secret"
	srv.cfg.Replication.SiteID = "site-a"
	worker := replication.NewBiDirectionalWorker(srv.metaStore, srv.engine, srv.cfg.Replication)
	for _, c := range []struct{ bucket, key string }{{"one", "a.txt"}, {"two", "b.txt"}, {"one", "c.txt"}} {
		if err := worker.ChangeLog().Record(c.bucket, c.key, "put", "etag", 1, replication.NewVectorClock()); err != nil {
			t.Fatal(err)
		}
	}
	ts := httptest.NewServer(srv.requireReplicationPeer(worker.HandleSyncRequest))
	defer ts.Close()

	now := time.Now().UTC()
	addUser := func(name, resources string, actions ...string) {
		t.Helper()
		if err := srv.metaStore.CreateIAMPolicy(metadata.IAMPolicy{Name: "pol-" + name, CreatedAt: now,
			Document: `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":[` + strings.Join(actions, ",") + `],"Resource":[` + resources + `]}]}`}); err != nil {
			t.Fatal(err)
		}
		if err := srv.metaStore.CreateIAMUser(metadata.IAMUser{Name: name, CreatedAt: now, PolicyARNs: []string{"pol-" + name}}); err != nil {
			t.Fatal(err)
		}
		if err := srv.metaStore.CreateAccessKey(metadata.AccessKey{AccessKey: "AK" + name, SecretKey: "secret-" + name, UserID: name, CreatedAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	addUser("narrow", `"arn:aws:s3:::one","arn:aws:s3:::one/*"`, `"s3:GetObject"`, `"s3:ListBucket"`)
	// Allowed everything, but only from another network.
	if err := srv.metaStore.CreateIAMUser(metadata.IAMUser{Name: "faraway", CreatedAt: now, PolicyARNs: []string{"pol-narrow"}, AllowedCIDRs: []string{"10.99.0.0/16"}}); err != nil {
		t.Fatal(err)
	}
	if err := srv.metaStore.CreateAccessKey(metadata.AccessKey{AccessKey: "AKfaraway", SecretKey: "secret-faraway", UserID: "faraway", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	// Allowed only over plain HTTP (aws:SecureTransport false), as this test
	// server is: the condition has to be evaluated, not left undetermined.
	if err := srv.metaStore.CreateIAMPolicy(metadata.IAMPolicy{Name: "pol-cond", CreatedAt: now,
		Document: `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":["s3:GetObject","s3:ListBucket"],"Resource":["*"],"Condition":{"Bool":{"aws:SecureTransport":"false"}}}]}`}); err != nil {
		t.Fatal(err)
	}
	if err := srv.metaStore.CreateIAMUser(metadata.IAMUser{Name: "cond", CreatedAt: now, PolicyARNs: []string{"pol-cond"}}); err != nil {
		t.Fatal(err)
	}
	if err := srv.metaStore.CreateAccessKey(metadata.AccessKey{AccessKey: "AKcond", SecretKey: "secret-cond", UserID: "cond", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	// May read every object by name but list no bucket: the change log would
	// enumerate the keys it cannot list.
	addUser("getonly", `"arn:aws:s3:::*"`, `"s3:GetObject"`)

	body := []byte(`{"site_id":"site-b","since_seq":0,"limit":10}`)
	sum := sha256.Sum256(body)
	send := func(sign func(*http.Request), sentBody []byte) (int, replication.SyncResponse) {
		t.Helper()
		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/_replication/sync", bytes.NewReader(sentBody))
		sign(req)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out replication.SyncResponse
		if resp.StatusCode == http.StatusOK {
			if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
				t.Fatal(err)
			}
		}
		return resp.StatusCode, out
	}
	signed := func(ak, sk string) func(*http.Request) {
		return func(req *http.Request) {
			req.Header.Set("X-Amz-Content-Sha256", hex.EncodeToString(sum[:]))
			replication.SignPeerRequest(req, ak, sk)
		}
	}
	keys := func(r replication.SyncResponse) string {
		var ks []string
		for _, c := range r.Changes {
			ks = append(ks, c.Bucket+"/"+c.Key)
		}
		return strings.Join(ks, ",")
	}

	if code, r := send(signed("vaults3-admin", "vaults3-secret-change-me"), body); code != http.StatusOK || keys(r) != "one/a.txt,two/b.txt,one/c.txt" {
		t.Fatalf("a peer signing with the admin key: %d %q, want 200 and every change", code, keys(r))
	}
	code, r := send(signed("AKnarrow", "secret-narrow"), body)
	if code != http.StatusOK || keys(r) != "one/a.txt,one/c.txt" {
		t.Errorf("a key that may read only bucket one: %d %q, want only bucket one's changes", code, keys(r))
	}
	if code, r := send(signed("AKcond", "secret-cond"), body); code != http.StatusOK || len(r.Changes) != 3 {
		t.Errorf("a key allowed on a condition the request meets: %d %q, want every change", code, keys(r))
	}
	if code, r := send(signed("AKgetonly", "secret-getonly"), body); code != http.StatusOK || len(r.Changes) != 0 {
		t.Errorf("a key that may not list any bucket: %d %q, want no changes", code, keys(r))
	}
	if r.LastSeq != 3 {
		t.Errorf("a key that may read only bucket one: LastSeq %d, want 3 so the cursor moves past the rest", r.LastSeq)
	}
	if code, r := send(func(r *http.Request) { r.Header.Set("X-Cluster-Secret", "the-cluster-secret") }, body); code != http.StatusOK || len(r.Changes) != 3 {
		t.Errorf("the cluster secret: %d with %d changes, want 200 and every change", code, len(r.Changes))
	}
	for name, c := range map[string]struct {
		sign func(*http.Request)
		body []byte
	}{
		"no credentials":                          {func(*http.Request) {}, body},
		"a wrong cluster secret":                  {func(r *http.Request) { r.Header.Set("X-Cluster-Secret", "guess") }, body},
		"a wrong secret key":                      {signed("vaults3-admin", "wrong"), body},
		"a key allowed only from another network": {signed("AKfaraway", "secret-faraway"), body},
		"a body that was changed":                 {signed("vaults3-admin", "vaults3-secret-change-me"), []byte(`{"site_id":"site-b","since_seq":0,"limit":99}`)},
		"an unsigned body": {func(r *http.Request) {
			replication.SignPeerRequest(r, "vaults3-admin", "vaults3-secret-change-me")
		}, body},
	} {
		if code, _ := send(c.sign, c.body); code == http.StatusOK {
			t.Errorf("%s: 200, want it refused", name)
		}
	}
}
