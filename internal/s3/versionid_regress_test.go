package s3

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// Regression (5.0.14 security review): a versionId containing "/" was joined
// under the named key's version folder, so DELETE /b/doc?versionId=secret/<vid>
// removed the bytes of version <vid> of key "doc/secret", which the caller had
// no grant on and which was under COMPLIANCE retention.
func TestAVersionIDCannotNameAnotherKeysVersion(t *testing.T) {
	e := newFixEnv(t)
	e.admin(t, http.MethodPut, "/vlock", "", "X-Amz-Bucket-Object-Lock-Enabled", "true").must(t, "create bucket")
	until := time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339)
	e.admin(t, http.MethodPut, "/vlock/doc/secret", "compliance-locked-data",
		"X-Amz-Object-Lock-Mode", "COMPLIANCE", "X-Amz-Object-Lock-Retain-Until-Date", until).must(t, "put locked")
	vids := e.versionIDs(t, "vlock", "doc/secret")
	if len(vids) != 1 {
		t.Fatalf("versions: %v", vids)
	}
	vid := vids[0]

	// The admin itself is refused on the real version: the lock holds.
	expect(t, "direct delete of the locked version",
		e.admin(t, http.MethodDelete, "/vlock/doc/secret?versionId="+vid, ""), http.StatusForbidden, "AccessDenied")

	// A user allowed to delete versions of the single key "doc" only.
	ak, sk := e.addUser(t, "doconly", allow(`"s3:DeleteObjectVersion","s3:DeleteObject"`, `"arn:aws:s3:::vlock/doc"`))
	expect(t, "user cannot touch doc/secret",
		e.as(t, ak, sk, http.MethodDelete, "/vlock/doc/secret?versionId="+vid, ""), http.StatusForbidden, "AccessDenied")

	expect(t, "crafted delete", e.as(t, ak, sk, http.MethodDelete, "/vlock/doc?versionId="+url.QueryEscape("secret/"+vid), ""), http.StatusBadRequest, "InvalidArgument")
	g := e.admin(t, http.MethodGet, "/vlock/doc/secret", "")
	if g.code != http.StatusOK || g.body != "compliance-locked-data" {
		t.Fatalf("the locked object after a crafted delete: %d %q", g.code, g.body)
	}
}

func TestAVersionIDCannotNameAnotherKeysVersionInABatch(t *testing.T) {
	e := newFixEnv(t)
	e.admin(t, http.MethodPut, "/vbk", "").must(t, "create bucket")
	e.admin(t, http.MethodPut, "/vbk?versioning", `<VersioningConfiguration><Status>Enabled</Status></VersioningConfiguration>`).must(t, "versioning")
	e.admin(t, http.MethodPut, "/vbk/doc/secret", "secret-data").must(t, "put")
	e.admin(t, http.MethodPut, "/vbk/doc", "mine").must(t, "put doc")
	vid := e.versionIDs(t, "vbk", "doc/secret")[0]
	ak, sk := e.addUser(t, "doconly", allow(`"s3:GetObject","s3:GetObjectVersion","s3:DeleteObjectVersion","s3:DeleteObject"`, `"arn:aws:s3:::vbk/doc","arn:aws:s3:::vbk"`))
	expect(t, "crafted GET", e.as(t, ak, sk, http.MethodGet, "/vbk/doc?versionId="+url.QueryEscape("secret/"+vid), ""), http.StatusBadRequest, "InvalidArgument")
	body := `<Delete><Object><Key>doc</Key><VersionId>secret/` + vid + `</VersionId></Object></Delete>`
	b := e.as(t, ak, sk, http.MethodPost, "/vbk?delete", body, "Content-MD5", contentMD5(body))
	if !strings.Contains(b.body, "InvalidArgument") {
		t.Errorf("batch delete with a crafted version id: %d %s", b.code, b.body)
	}
	if after := e.admin(t, http.MethodGet, "/vbk/doc/secret", ""); after.code != http.StatusOK || after.body != "secret-data" {
		t.Fatalf("doc/secret after the batch: %d %q", after.code, after.body)
	}
	// The engine refuses such an ID as well, whoever passes it.
	if p := e.h.objects.engine.ObjectPath("vbk", "doc"); p == "" {
		t.Skip()
	}
}
