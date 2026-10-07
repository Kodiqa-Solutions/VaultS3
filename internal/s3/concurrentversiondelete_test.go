package s3

import (
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
)

// Deleting every version of one key at once used to leave one behind: the
// handler promoted the "newest survivor" after its delete, and another delete
// could remove that survivor in between, so the promotion wrote it back. The
// bucket then kept a version no client could see removed and could never be
// deleted. This is s3-tests' test_versioned_concurrent_object_create_concurrent_remove.
func TestConcurrentVersionDeletesLeaveNothingBehind(t *testing.T) {
	e := newFixEnv(t)
	e.admin(t, http.MethodPut, "/cvd", "").must(t, "create bucket")
	e.admin(t, http.MethodPut, "/cvd?versioning", `<VersioningConfiguration><Status>Enabled</Status></VersioningConfiguration>`).must(t, "enable versioning")

	for round := 0; round < 20; round++ {
		for i := 0; i < 5; i++ {
			e.admin(t, http.MethodPut, "/cvd/myobj", fmt.Sprint(i)).must(t, "put version")
		}
		ids := e.versionIDs(t, "cvd", "myobj")
		if len(ids) != 5 {
			t.Fatalf("round %d: %d versions before the delete, want 5", round, len(ids))
		}
		var wg sync.WaitGroup
		for _, id := range ids {
			wg.Add(1)
			go func(id string) {
				defer wg.Done()
				e.admin(t, http.MethodDelete, "/cvd/myobj?versionId="+id, "")
			}(id)
		}
		wg.Wait()
		if left := e.versionIDs(t, "cvd", "myobj"); len(left) != 0 {
			t.Fatalf("round %d: %d versions survived deleting all of them: %v", round, len(left), left)
		}
		if got := e.admin(t, http.MethodGet, "/cvd/myobj", ""); got.code != http.StatusNotFound {
			t.Fatalf("round %d: GET after deleting every version = %d, want 404", round, got.code)
		}
	}
	if got := e.admin(t, http.MethodGet, "/cvd?versions", "").body; strings.Contains(got, "<Version>") || strings.Contains(got, "<DeleteMarker>") {
		t.Fatalf("versions left in the bucket: %s", got)
	}
	e.admin(t, http.MethodDelete, "/cvd", "").must(t, "delete the emptied bucket")
}
