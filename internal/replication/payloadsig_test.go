package replication

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Kodiqa-Solutions/VaultS3/internal/config"
	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
	"github.com/Kodiqa-Solutions/VaultS3/internal/s3"
)

// Regression (5.0.11): replication sent every object as UNSIGNED-PAYLOAD, so a
// body changed between the two sites was stored by the peer as the object.
// The body's SHA-256 is signed now, and the peer refuses a changed one.
func TestReplicatedBodyIsSignedSoAChangedOneIsRefused(t *testing.T) {
	local, remote := newSite(t), newSite(t)
	auth := s3.NewAuthenticator("peer-ak", "peer-sk", remote.store, nil, nil)
	h := s3.NewHandler(remote.store, remote.engine, auth, false, "", nil)
	var tamper bool
	var sentHash string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut && r.ContentLength > 0 {
			sentHash = r.Header.Get("X-Amz-Content-Sha256")
			if tamper {
				body, _ := io.ReadAll(r.Body)
				body[0] ^= 1 // one bit, on the way
				r.Body = io.NopCloser(bytes.NewReader(body))
			}
		}
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	remote.put(t, "bkt", "seed", "x") // the peer has the bucket

	local.put(t, "bkt", "honest", "replicated as sent")
	local.put(t, "bkt", "changed", "replicated but changed on the way")
	w := pushWorker(local, srv.URL, nil)

	if err := local.store.EnqueueReplication(metadata.ReplicationEvent{Peer: "p", Type: "put", Bucket: "bkt", Key: "honest"}); err != nil {
		t.Fatal(err)
	}
	w.processQueue()
	if got, ok := remote.read(t, "bkt", "honest"); !ok || got != "replicated as sent" {
		t.Fatalf("honest replication: peer has %q (present %v)", got, ok)
	}
	if len(sentHash) != 64 || strings.Contains(sentHash, "UNSIGNED") {
		t.Fatalf("replication sent X-Amz-Content-Sha256 %q, want the body's SHA-256", sentHash)
	}

	tamper = true
	if err := local.store.EnqueueReplication(metadata.ReplicationEvent{Peer: "p", Type: "put", Bucket: "bkt", Key: "changed"}); err != nil {
		t.Fatal(err)
	}
	w.processQueue()
	if got, ok := remote.read(t, "bkt", "changed"); ok {
		t.Fatalf("the peer stored a body changed on the way: %q", got)
	}
	if ev := queued(t, local); len(ev) != 1 {
		t.Fatalf("the refused replication is not kept for retry: %+v", ev)
	}
}

// The sync request signs its body's SHA-256: the peer's sync endpoint refuses
// a body that does not match it, and an unsigned one (5.0.11).
func TestSyncRequestSignsItsBody(t *testing.T) {
	local := newSite(t)
	var gotHash, wantHash string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		sum := sha256.Sum256(body)
		gotHash, wantHash = r.Header.Get("X-Amz-Content-Sha256"), hex.EncodeToString(sum[:])
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"site_id":"remote","changes":[],"last_seq":0}`))
	}))
	t.Cleanup(srv.Close)
	w := NewBiDirectionalWorker(local.store, local.engine, config.ReplicationConfig{
		SiteID: "local",
		Peers:  []config.ReplicationPeer{{Name: "p", URL: srv.URL, AccessKey: "ak", SecretKey: "sk"}},
	})
	if err := w.syncPeer(context.Background(), "p", w.peers["p"]); err != nil {
		t.Fatal(err)
	}
	if gotHash != wantHash {
		t.Fatalf("sync request X-Amz-Content-Sha256 %q, want the body's SHA-256 %q", gotHash, wantHash)
	}
}
