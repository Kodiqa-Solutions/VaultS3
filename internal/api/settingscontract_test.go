package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/Kodiqa-Solutions/VaultS3/internal/config"
)

// The dashboard reads these flags by JSON key. A key that does not match renders
// nothing: no error, no failing test, and the compiler cannot see across the
// language boundary. Two such bugs shipped and were found by the user, so the
// contract is pinned here rather than trusted.
//
// This checks the direction that actually breaks: every feature flag the
// dashboard reads must exist in the Go response.
func TestSettingsFeatureKeysMatchDashboard(t *testing.T) {
	src, err := os.ReadFile("../../web/src/pages/SettingsPage.tsx")
	if err != nil {
		t.Skipf("dashboard source not available: %v", err)
	}

	// features.<name> as used in the feature list.
	re := regexp.MustCompile(`features\.([A-Za-z0-9_]+)`)
	used := map[string]bool{}
	for _, m := range re.FindAllStringSubmatch(string(src), -1) {
		used[m[1]] = true
	}
	if len(used) == 0 {
		t.Fatal("found no features.* references, the regex or the page changed shape")
	}

	var resp struct {
		Features map[string]json.RawMessage `json:"features"`
	}
	h := &APIHandler{cfg: config.Defaults()}
	rec := httptest.NewRecorder()
	h.handleSettings(rec, httptest.NewRequest(http.MethodGet, "/api/v1/settings", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("settings returned %d", rec.Code)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("settings response is not JSON: %v", err)
	}

	var missing []string
	for name := range used {
		if _, ok := resp.Features[name]; !ok {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		t.Fatalf("the dashboard reads feature flags the API never sends: %s\n"+
			"a mismatch here renders an empty row rather than failing", strings.Join(missing, ", "))
	}
}
