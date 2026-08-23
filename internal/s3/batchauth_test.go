package s3

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"

	"github.com/Kodiqa-Solutions/VaultS3/internal/iam"
)

// DeleteObjects names its objects in the body, so the route can only authorize
// the floor. It used to require s3:* instead, which failed closed but meant a
// user holding s3:DeleteObject could not batch delete at all.
func TestBatchDeleteRouteRequiresDeleteObjectNotWildcard(t *testing.T) {
	q, _ := url.ParseQuery("delete=")
	got := mapMethodToAction(http.MethodPost, "b", "", q)
	if got == "s3:*" {
		t.Error("batch delete still demands s3:*, so a user with s3:DeleteObject cannot use it")
	}
	if got != "s3:DeleteObject" {
		t.Errorf("batch delete maps to %q, want s3:DeleteObject", got)
	}
}

// The floor must not become a licence to destroy versions. Each entry naming a
// versionId has to be checked as s3:DeleteObjectVersion, or the batch route
// becomes a way around the per-object rule.
func TestBatchEntryAuthorizationSeparatesVersionDeletes(t *testing.T) {
	h := &ObjectHandler{auth: &Authenticator{}}

	// Allowed to delete, explicitly denied permanent version destruction: the
	// standard recoverable-delete policy.
	pol := iam.Policy{Statement: []iam.Statement{
		{Effect: "Allow", Action: []string{"s3:DeleteObject"}, Resource: []string{"arn:aws:s3:::b/*"}},
		{Effect: "Deny", Action: []string{"s3:DeleteObjectVersion"}, Resource: []string{"arn:aws:s3:::b/*"}},
	}}
	id := &iam.Identity{UserID: "bob", Policies: []iam.Policy{pol}}
	r := withIdentity(httptestRequest(), id, map[string]string{})

	if err := h.authorizeEntry(r, "s3:DeleteObject", "arn:aws:s3:::b/k.txt"); err != nil {
		t.Errorf("an ordinary delete was refused: %v", err)
	}
	if err := h.authorizeEntry(r, "s3:DeleteObjectVersion", "arn:aws:s3:::b/k.txt"); err == nil {
		t.Error("a version delete was allowed through the batch route, so the batch route" +
			" is a way around the deny that protects versions")
	}
}

// A wiring mistake must deny, never authorize everything.
func TestBatchEntryAuthorizationFailsClosed(t *testing.T) {
	h := &ObjectHandler{auth: &Authenticator{}}
	// No identity on the request at all.
	if err := h.authorizeEntry(httptestRequest(), "s3:DeleteObject", "arn:aws:s3:::b/k.txt"); err == nil {
		t.Error("an entry was authorized with no identity on the request")
	}
	// No authenticator wired.
	bare := &ObjectHandler{}
	r := withIdentity(httptestRequest(), &iam.Identity{UserID: "bob"}, nil)
	if err := bare.authorizeEntry(r, "s3:DeleteObject", "arn:aws:s3:::b/k.txt"); err == nil {
		t.Error("an entry was authorized with no authenticator wired")
	}
}

// An admin bypasses policy evaluation everywhere else and must here too, or a
// batch delete would fail for the one identity that should always work.
func TestBatchEntryAuthorizationAllowsAdmin(t *testing.T) {
	h := &ObjectHandler{auth: &Authenticator{}}
	r := withIdentity(httptestRequest(), &iam.Identity{IsAdmin: true}, nil)
	if err := h.authorizeEntry(r, "s3:DeleteObjectVersion", "arn:aws:s3:::b/k.txt"); err != nil {
		t.Errorf("admin refused on a batch entry: %v", err)
	}
}

func httptestRequest() *http.Request {
	r, _ := http.NewRequest(http.MethodPost, "http://x/b?delete", nil)
	return r
}

// The wiring test, and the one that matters. The checks above exercise
// authorizeEntry directly, so they pass even if BatchDelete never calls it.
// This drives a real request through the handler with a non-admin identity and
// asserts the denial reaches the response, which is the mistake the others miss.
func TestBatchDeleteActuallyAuthorizesEachEntry(t *testing.T) {
	h := newTestHandler(t)
	if err := h.store.CreateBucket("b"); err != nil {
		t.Fatalf("CreateBucket: %v", err)
	}
	if err := h.store.PutObjectMeta(metadata.ObjectMeta{Bucket: "b", Key: "keep.txt", Size: 3}); err != nil {
		t.Fatal(err)
	}

	body := `<Delete><Object><Key>keep.txt</Key></Object></Delete>`
	r := httptest.NewRequest(http.MethodPost, "/b?delete", strings.NewReader(body))

	// A real non-admin identity with no policy at all: nothing grants the delete.
	r = withIdentity(r, &iam.Identity{UserID: "mallory"}, map[string]string{})

	rr := httptest.NewRecorder()
	h.objects.BatchDelete(rr, r, "b")

	if !strings.Contains(rr.Body.String(), "AccessDenied") {
		t.Fatalf("BatchDelete did not authorize the entry, body was: %s", rr.Body.String())
	}
	// And the object must still be there.
	if _, err := h.store.GetObjectMeta("b", "keep.txt"); err != nil {
		t.Error("the object was deleted despite the identity having no permission to delete it")
	}
}
