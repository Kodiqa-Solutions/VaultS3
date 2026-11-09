package s3

import (
	"net/http"
	"testing"

	"github.com/Kodiqa-Solutions/VaultS3/internal/middleware"
)

// Regression (5.0.14 review): server.go wraps the whole S3 mux in middleware.SecurityHeaders, which sets
// the dashboard CSP (default-src 'self') first. sandboxWriter only adds the
// sandbox when no CSP is present, so in the real stack it never does.
func TestObjectsAreSandboxedUnderTheServersMiddleware(t *testing.T) {
	e := newFixEnv(t)
	e.ts.Config.Handler = middleware.SecurityHeaders(e.h)
	e.admin(t, http.MethodPut, "/sbx", "").must(t, "bucket")
	e.admin(t, http.MethodPut, "/sbx/evil.html", "<script src=/sbx/evil.js></script>", "Content-Type", "text/html").must(t, "put")
	r := e.admin(t, http.MethodGet, "/sbx/evil.html", "")
	if got := r.header.Values("Content-Security-Policy"); len(got) != 1 || got[0] != middleware.ObjectSandboxCSP {
		t.Errorf("HTML object served through the production middleware: CSP %q, want %q", got, middleware.ObjectSandboxCSP)
	}
}

// A website page through the same middleware carries only its own policy.
func TestWebsitePagesKeepOnlyTheirPolicyUnderTheServersMiddleware(t *testing.T) {
	e := newFixEnv(t)
	e.ts.Config.Handler = middleware.SecurityHeaders(e.h)
	e.admin(t, http.MethodPut, "/site", "").must(t, "bucket")
	e.admin(t, http.MethodPut, "/site?website", `<WebsiteConfiguration><IndexDocument><Suffix>index.html</Suffix></IndexDocument></WebsiteConfiguration>`).must(t, "website")
	e.admin(t, http.MethodPut, "/site/index.html", "<script>1</script>", "Content-Type", "text/html").must(t, "page")
	r := e.admin(t, http.MethodGet, "/site/", "")
	if got := r.header.Values("Content-Security-Policy"); r.code != http.StatusOK || len(got) != 1 || got[0] != middleware.WebsiteSandboxCSP {
		t.Errorf("website page: %d, CSP %q", r.code, got)
	}
	// A PDF is shown: no policy at all.
	e.admin(t, http.MethodPut, "/site/doc.pdf", "%PDF-1.4", "Content-Type", "application/pdf").must(t, "pdf")
	if got := e.admin(t, http.MethodGet, "/site/doc.pdf?x-id=GetObject", "").header.Values("Content-Security-Policy"); len(got) != 0 {
		t.Errorf("PDF: CSP %q", got)
	}
}
