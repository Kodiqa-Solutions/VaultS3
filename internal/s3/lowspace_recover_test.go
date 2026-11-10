package s3

import (
	"net/http"
	"testing"

	"github.com/Kodiqa-Solutions/VaultS3/internal/storage"
)

// Regression (5.0.16 review): below the reserve, releasing a legal hold (needed before the held version
// can be deleted to free space) and a lifecycle rule (which would expire
// objects) are refused with 507.
func TestLowDiskSpaceKeepsWhatFreesSpace(t *testing.T) {
	e := newFixEnv(t)
	e.admin(t, http.MethodPut, "/lkb", "", "X-Amz-Bucket-Object-Lock-Enabled", "true").must(t, "bucket")
	r := e.admin(t, http.MethodPut, "/lkb/held", "data", "X-Amz-Object-Lock-Legal-Hold", "ON")
	if r.code != http.StatusOK {
		t.Fatalf("put held: %d %s", r.code, r.body)
	}
	g := storage.NewSpaceGuard(1<<62, t.TempDir())
	g.Check()
	e.h.SetSpaceGuard(g)

	off := `<LegalHold xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Status>OFF</Status></LegalHold>`
	if r := e.admin(t, http.MethodPut, "/lkb/held?legal-hold", off, "Content-Type", "application/xml"); r.code != http.StatusOK {
		t.Errorf("releasing a legal hold while low: %d (the held object can then never be deleted to free space)", r.code)
	}
	lc := `<LifecycleConfiguration><Rule><ID>x</ID><Status>Enabled</Status><Filter><Prefix></Prefix></Filter><Expiration><Days>1</Days></Expiration></Rule></LifecycleConfiguration>`
	if r := e.admin(t, http.MethodPut, "/lkb?lifecycle", lc, "Content-Type", "application/xml"); r.code != http.StatusOK {
		t.Errorf("lifecycle expiration rule while low: %d", r.code)
	}
	// Uploads, copies and parts are still refused.
	if r := e.admin(t, http.MethodPut, "/lkb/copy", "", "X-Amz-Copy-Source", "/lkb/held"); r.code != http.StatusInsufficientStorage {
		t.Errorf("a copy while low: %d", r.code)
	}
}
