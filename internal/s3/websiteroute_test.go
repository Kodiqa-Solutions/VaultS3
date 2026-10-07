package s3

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
)

// On a cluster the site root of a website bucket was routed like a bucket
// listing, to the leader, which answered 404 whenever index.html lived on
// another node. The request is routed by the index document it serves.
func TestWebsiteDirectoryRequestsRouteByTheirIndexDocument(t *testing.T) {
	h := newTestHandler(t)
	h.store.CreateBucket("site")
	h.store.CreateBucket("plain")
	if err := h.store.PutWebsiteConfig("site", metadata.WebsiteConfig{IndexDocument: "index.html"}); err != nil {
		t.Fatal(err)
	}
	var routed []string
	h.SetClusterProxy(func(w http.ResponseWriter, r *http.Request, bucket, key string) bool {
		routed = append(routed, key)
		w.WriteHeader(http.StatusTeapot)
		return true
	})
	for _, tc := range []struct{ method, target, want string }{
		{"GET", "/site/", "index.html"},
		{"HEAD", "/site/docs/", "docs/index.html"},
		{"GET", "/site/page.html", "page.html"},
		{"GET", "/site/?list-type=2", ""},
		{"PUT", "/site/", ""},
		{"GET", "/plain/", ""},
	} {
		routed = nil
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(tc.method, tc.target, nil))
		if len(routed) != 1 || routed[0] != tc.want {
			t.Errorf("%s %s routed by %q, want %q", tc.method, tc.target, routed, tc.want)
		}
	}
}
