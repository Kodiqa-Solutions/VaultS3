package replication

import (
	"net/http"
	"strings"
	"testing"
)

// The server refuses a signed request whose x-amz-* headers are not all
// signed, as AWS does, so the replication signer must sign every one it
// sends, not only the payload hash and date.
func TestSignerSignsEveryAmzHeader(t *testing.T) {
	req, _ := http.NewRequest(http.MethodPut, "http://peer:9000/b/k", nil)
	req.Header.Set("X-Amz-Meta-Origin", "dc1")
	req.Header.Set("X-Amz-Tagging", "a=b")
	req.Header.Set("X-VaultS3-Replication", "1")
	signV4(req, "ak", "sk", "us-east-1")

	auth := req.Header.Get("Authorization")
	i := strings.Index(auth, "SignedHeaders=")
	if i < 0 {
		t.Fatalf("no SignedHeaders in %q", auth)
	}
	signed := strings.SplitN(auth[i+len("SignedHeaders="):], ",", 2)[0]
	for _, want := range []string{"host", "x-amz-content-sha256", "x-amz-date", "x-amz-meta-origin", "x-amz-tagging", "x-vaults3-replication"} {
		if !strings.Contains(";"+signed+";", ";"+want+";") {
			t.Errorf("SignedHeaders %q does not include %s", signed, want)
		}
	}
}
