package s3

import (
	"net/http"
	"testing"

	"github.com/Kodiqa-Solutions/VaultS3/internal/storage"
)

// Below the free space reserve, new data is refused with 507 before its body
// is read, and deletes go on: a full metadata disk could not even delete
// (its database writes new pages for a delete), so the server stayed full.
func TestLowDiskSpaceRefusesWritesAndKeepsDeletes(t *testing.T) {
	e := newFixEnv(t)
	e.admin(t, http.MethodPut, "/lowb", "").must(t, "bucket")
	e.admin(t, http.MethodPut, "/lowb/keep", "data").must(t, "put")
	g := storage.NewSpaceGuard(1<<62, t.TempDir())
	g.Check()
	e.h.SetSpaceGuard(g)

	if r := e.admin(t, http.MethodPut, "/lowb/new", "data"); r.code != http.StatusInsufficientStorage {
		t.Errorf("PUT while low: %d", r.code)
	}
	if r := e.admin(t, http.MethodPost, "/lowb/mp?uploads", ""); r.code != http.StatusInsufficientStorage {
		t.Errorf("CreateMultipartUpload while low: %d", r.code)
	}
	e.admin(t, http.MethodGet, "/lowb/keep", "").must(t, "GET while low")
	del := `<Delete><Object><Key>nothing</Key></Object></Delete>`
	if r := e.admin(t, http.MethodPost, "/lowb?delete", del, "Content-Type", "application/xml"); r.code != http.StatusOK {
		t.Errorf("multi-object delete while low: %d %s", r.code, r.body)
	}
	if r := e.admin(t, http.MethodDelete, "/lowb/keep", ""); r.code != http.StatusNoContent {
		t.Errorf("DELETE while low: %d", r.code)
	}
}
