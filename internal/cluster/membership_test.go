package cluster

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// TestPostJoinSuccess: a 200 from the join endpoint ends the attempt.
func TestPostJoinSuccess(t *testing.T) {
	var gotBody atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, r.ContentLength)
		r.Body.Read(buf)
		gotBody.Store(string(buf))
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	body := []byte(`{"node_id":"n2","addr":"n2:9001"}`)
	if err := postJoin(context.Background(), srv.Client(), strings.TrimPrefix(srv.URL, "http://"), body, ""); err != nil {
		t.Fatalf("postJoin: %v", err)
	}
	if s, _ := gotBody.Load().(string); !strings.Contains(s, "n2") {
		t.Fatalf("leader did not receive the join body, got %q", s)
	}
}

// TestPostJoinFollowsRedirect: a follower answers 307 → Location(leader); postJoin
// must follow it and succeed at the leader.
func TestPostJoinFollowsRedirect(t *testing.T) {
	var leaderHits int32
	leader := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&leaderHits, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer leader.Close()

	follower := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", leader.URL+"/cluster/join")
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	defer follower.Close()

	body := []byte(`{"node_id":"n3","addr":"n3:9001"}`)
	if err := postJoin(context.Background(), follower.Client(), strings.TrimPrefix(follower.URL, "http://"), body, ""); err != nil {
		t.Fatalf("postJoin via redirect: %v", err)
	}
	if atomic.LoadInt32(&leaderHits) != 1 {
		t.Fatalf("leader hits = %d, want 1 (redirect not followed)", leaderHits)
	}
}

// TestPostJoinErrorOnFailure: a non-2xx/307 is an error so AutoJoin keeps retrying.
func TestPostJoinErrorOnFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no leader available", http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	if err := postJoin(context.Background(), srv.Client(), strings.TrimPrefix(srv.URL, "http://"), []byte(`{}`), ""); err == nil {
		t.Fatal("expected an error for 503 so the caller retries")
	}
}

// TestAuthOK verifies inter-node auth: the configured secret must match, and an
// empty configured secret leaves the endpoints open (backward compatible).
func TestAuthOK(t *testing.T) {
	withHdr := func(v string) *http.Request {
		r := httptest.NewRequest(http.MethodPost, "/cluster/apply", nil)
		if v != "" {
			r.Header.Set(clusterSecretHeader, v)
		}
		return r
	}
	secured := &Node{cfg: ClusterConfig{Secret: "topsecret"}}
	if !secured.authOK(withHdr("topsecret")) {
		t.Fatal("correct secret should be authorized")
	}
	if secured.authOK(withHdr("wrong")) {
		t.Fatal("wrong secret must be rejected")
	}
	if secured.authOK(withHdr("")) {
		t.Fatal("missing secret must be rejected when one is configured")
	}
	// An unset secret must REFUSE, not wave everything through. This test used to
	// assert the opposite, in the name of backward compatibility, which is how a
	// default clustered deployment ended up serving /cluster/* to anonymous
	// callers: topology disclosure, an object-existence oracle, arbitrary object
	// deletion and a rogue node joining the Raft quorum (security assessment
	// finding 7). The server now refuses to start clustered without a secret, so
	// this branch is only reachable by misconfiguration.
	open := &Node{cfg: ClusterConfig{}}
	if open.authOK(withHdr("")) {
		t.Fatal("an unset cluster secret authorized an anonymous inter-node request")
	}
	if open.authOK(withHdr("anything")) {
		t.Fatal("an unset cluster secret authorized a request carrying an arbitrary secret")
	}
}
