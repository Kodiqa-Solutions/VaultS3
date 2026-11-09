package s3

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// Regression (5.0.14 review): the throttle counted the FIRST Credential= in the header, the verifier uses
// the LAST one (parseAuthParams map overwrite). An attacker rotates the first.
func TestTheThrottleCountsTheVerifiedKey(t *testing.T) {
	r := httptest.NewRequest("GET", "/b", nil)
	r.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=junk123/20261109/us-east-1/s3/aws4_request, Credential=ADMINKEY/20261109/us-east-1/s3/aws4_request, SignedHeaders=host, Signature=00")
	counted := requestAccessKey(r)
	params := parseAuthParams(strings.SplitN(r.Header.Get("Authorization"), " ", 2)[1])
	verified, _, _ := strings.Cut(params["Credential"], "/")
	if counted != verified {
		t.Fatalf("throttle counts key %q but the signature is checked for %q", counted, verified)
	}
}
