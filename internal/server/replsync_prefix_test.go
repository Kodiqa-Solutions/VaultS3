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

// Regression (5.0.11 round 5): a key may LIST only its own prefix (the AWS
// home-directory pattern) but read objects by name anywhere in the bucket.
// s3:prefix came from the sync request's own query, so ?prefix=home/alice/x
// made the change log name every key in the bucket. Each change is now judged
// as a listing of its own key, whatever the query says.
func TestSyncChangesAreJudgedByTheirOwnKeyNotTheQuery(t *testing.T) {
	srv := newTestServer(t)
	srv.cfg.Replication.SiteID = "site-a"
	worker := replication.NewBiDirectionalWorker(srv.metaStore, srv.engine, srv.cfg.Replication)
	for _, k := range []string{"home/alice/mine.txt", "home/bob/secret-plan.txt", "payroll/2026.csv"} {
		if err := worker.ChangeLog().Record("shared", k, "put", "etag", 1, replication.NewVectorClock()); err != nil {
			t.Fatal(err)
		}
	}
	ts := httptest.NewServer(srv.requireReplicationPeer(worker.HandleSyncRequest))
	defer ts.Close()
	now := time.Now().UTC()
	doc := `{"Version":"2012-10-17","Statement":[
	 {"Effect":"Allow","Action":["s3:ListBucket"],"Resource":["arn:aws:s3:::shared"],"Condition":{"StringLike":{"s3:prefix":["home/alice/*"]}}},
	 {"Effect":"Allow","Action":["s3:GetObject"],"Resource":["arn:aws:s3:::shared/*"]}]}`
	if err := srv.metaStore.CreateIAMPolicy(metadata.IAMPolicy{Name: "pol-alice", CreatedAt: now, Document: doc}); err != nil {
		t.Fatal(err)
	}
	if err := srv.metaStore.CreateIAMUser(metadata.IAMUser{Name: "alice", CreatedAt: now, PolicyARNs: []string{"pol-alice"}}); err != nil {
		t.Fatal(err)
	}
	if err := srv.metaStore.CreateAccessKey(metadata.AccessKey{AccessKey: "AKalice", SecretKey: "secret-alice", UserID: "alice", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"site_id":"x","since_seq":0,"limit":100}`)
	sum := sha256.Sum256(body)
	for _, q := range []string{"", "?prefix=home/alice/x"} {
		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/_replication/sync"+q, bytes.NewReader(body))
		req.Header.Set("X-Amz-Content-Sha256", hex.EncodeToString(sum[:]))
		replication.SignPeerRequest(req, "AKalice", "secret-alice")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var out replication.SyncResponse
		_ = json.NewDecoder(resp.Body).Decode(&out)
		resp.Body.Close()
		var ks []string
		for _, c := range out.Changes {
			ks = append(ks, c.Key)
		}
		if resp.StatusCode != http.StatusOK || len(ks) != 1 || ks[0] != "home/alice/mine.txt" {
			t.Errorf("query %q: %d %v, want only home/alice/mine.txt", q, resp.StatusCode, ks)
		}
		for _, k := range ks {
			if k != "home/alice/mine.txt" {
				t.Errorf("query %q: change log named %q, a key alice may not list", q, k)
			}
		}
	}
}

// A ListBucket Allow written as AWS documents home folders, s3:prefix equal to
// "home/alice/" exactly, lets the peer receive the changes under that folder
// (a listing with that prefix would show them) and nothing else (5.0.11 r6).
func TestSyncHonoursAFolderPrefixCondition(t *testing.T) {
	srv := newTestServer(t)
	srv.cfg.Replication.SiteID = "site-a"
	worker := replication.NewBiDirectionalWorker(srv.metaStore, srv.engine, srv.cfg.Replication)
	for _, k := range []string{"home/alice/mine.txt", "home/alice/deep/x.txt", "home/bob/secret.txt", "top.txt"} {
		if err := worker.ChangeLog().Record("shared", k, "put", "etag", 1, replication.NewVectorClock()); err != nil {
			t.Fatal(err)
		}
	}
	ts := httptest.NewServer(srv.requireReplicationPeer(worker.HandleSyncRequest))
	defer ts.Close()
	now := time.Now().UTC()
	doc := `{"Version":"2012-10-17","Statement":[
	 {"Effect":"Allow","Action":["s3:ListBucket"],"Resource":["arn:aws:s3:::shared"],"Condition":{"StringEquals":{"s3:prefix":["home/alice/"]}}},
	 {"Effect":"Allow","Action":["s3:GetObject"],"Resource":["arn:aws:s3:::shared/home/alice/*"]}]}`
	if err := srv.metaStore.CreateIAMPolicy(metadata.IAMPolicy{Name: "pol-eq", CreatedAt: now, Document: doc}); err != nil {
		t.Fatal(err)
	}
	if err := srv.metaStore.CreateIAMUser(metadata.IAMUser{Name: "eq", CreatedAt: now, PolicyARNs: []string{"pol-eq"}}); err != nil {
		t.Fatal(err)
	}
	if err := srv.metaStore.CreateAccessKey(metadata.AccessKey{AccessKey: "AKeq", SecretKey: "secret-eq", UserID: "eq", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"site_id":"x","since_seq":0,"limit":100}`)
	sum := sha256.Sum256(body)
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/_replication/sync", bytes.NewReader(body))
	req.Header.Set("X-Amz-Content-Sha256", hex.EncodeToString(sum[:]))
	replication.SignPeerRequest(req, "AKeq", "secret-eq")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var out replication.SyncResponse
	_ = json.NewDecoder(resp.Body).Decode(&out)
	resp.Body.Close()
	var ks []string
	for _, c := range out.Changes {
		ks = append(ks, c.Key)
	}
	if got := strings.Join(ks, ","); got != "home/alice/mine.txt,home/alice/deep/x.txt" {
		t.Errorf("changes for a key allowed to list home/alice/: %q, want only the two under it", got)
	}
}
