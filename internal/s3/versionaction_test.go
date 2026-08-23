package s3

import (
	"net/http"
	"net/url"
	"testing"

	"github.com/Kodiqa-Solutions/VaultS3/internal/iam"
)

// S3 treats an operation on one named version as a different action from the
// same operation on the current version. Collapsing them means a policy that
// allows s3:DeleteObject but denies s3:DeleteObjectVersion, which is the standard
// way to let people delete recoverably while never permitting permanent
// destruction, protects nothing.
func TestVersionScopedOperationsMapToVersionActions(t *testing.T) {
	cases := []struct {
		name, method, query string
		want                string
	}{
		{"delete current version", http.MethodDelete, "", "s3:DeleteObject"},
		{"delete a named version", http.MethodDelete, "versionId=abc123", "s3:DeleteObjectVersion"},
		{"get current version", http.MethodGet, "", "s3:GetObject"},
		{"get a named version", http.MethodGet, "versionId=abc123", "s3:GetObjectVersion"},
		{"head a named version", http.MethodHead, "versionId=abc123", "s3:GetObjectVersion"},
		// A version is server-assigned on write, so a PUT is never version-scoped.
		{"put is never version scoped", http.MethodPut, "versionId=abc123", "s3:PutObject"},
	}
	for _, c := range cases {
		q, err := url.ParseQuery(c.query)
		if err != nil {
			t.Fatalf("%s: bad query: %v", c.name, err)
		}
		got := mapMethodToAction(c.method, "b", "k.txt", q)
		if got != c.want {
			t.Errorf("%s: %s /b/k.txt?%s -> %q, want %q", c.name, c.method, c.query, got, c.want)
		}
	}
}

// The decisive property: the non-version action must never be what a version
// operation is checked against, or granting it silently grants the destructive
// one too.
func TestDeleteObjectDoesNotCoverVersionDeletes(t *testing.T) {
	q, _ := url.ParseQuery("versionId=v1")
	if act := mapMethodToAction(http.MethodDelete, "b", "k.txt", q); act == "s3:DeleteObject" {
		t.Error("deleting a named version is authorized as s3:DeleteObject, so a policy" +
			" denying s3:DeleteObjectVersion cannot prevent permanent destruction")
	}
}

// An unreadable policy is not an absent one. Skipping it drops whatever it said,
// and a dropped Deny silently widens access when another attached policy allows.
// The console path already refused outright; the S3 path now agrees.
func TestUnparseablePolicyDeniesInsteadOfBeingSkipped(t *testing.T) {
	a := &Authenticator{}
	id := &iam.Identity{
		UserID: "bob",
		// What a broken policy leaves behind: a blanket allow that parsed fine,
		// while the Deny beside it did not.
		Policies: []iam.Policy{{Statement: []iam.Statement{
			{Effect: "Allow", Action: []string{"s3:*"}, Resource: []string{"*"}},
		}}},
		PolicyLoadFailed: true,
	}
	if err := a.Authorize(id, "s3:DeleteObjectVersion", "arn:aws:s3:::b/k.txt"); err == nil {
		t.Error("an identity with an unparseable policy was authorized from the policies that did parse," +
			" so a Deny lost to a parse error grants access")
	}

	// Without the failure it evaluates normally, so this fails closed only when
	// something is actually broken.
	id.PolicyLoadFailed = false
	if err := a.Authorize(id, "s3:DeleteObjectVersion", "arn:aws:s3:::b/k.txt"); err != nil {
		t.Errorf("a healthy policy set was refused: %v", err)
	}

	// An admin must not be locked out by a broken policy, or a typo bricks the deployment.
	admin := &iam.Identity{IsAdmin: true, PolicyLoadFailed: true}
	if err := a.Authorize(admin, "s3:DeleteObjectVersion", "arn:aws:s3:::b/k.txt"); err != nil {
		t.Errorf("admin locked out by an unparseable policy: %v", err)
	}
}
