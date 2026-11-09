package s3

import (
	"net/http"
	"strings"
	"testing"

	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
)

// Regression (5.0.13): on a cluster each node held a bucket quota to its own
// disk, so a bucket whose objects were spread over three nodes could hold
// about three times its quota. With the stored usage on, the quota counts
// every object the replicated metadata records, wherever its bytes are.
func TestAClusterQuotaCountsObjectsOtherNodesHold(t *testing.T) {
	e := newFixEnv(t)
	e.admin(t, http.MethodPut, "/cqb", "").must(t, "bucket")
	if err := e.store.UpdateBucketQuota("cqb", 1000, 3); err != nil {
		t.Fatal(err)
	}
	// Objects written through other nodes: their records are here, their
	// bytes are on the other nodes' disks.
	for _, k := range []string{"n2/a", "n3/b"} {
		if err := e.store.PutObjectMeta(metadata.ObjectMeta{Bucket: "cqb", Key: k, Size: 400}); err != nil {
			t.Fatal(err)
		}
	}
	// Without the stored usage (one server) this node's empty disk is all
	// that counts, and the write goes through.
	e.admin(t, http.MethodPut, "/cqb/local", strings.Repeat("x", 300)).must(t, "single-node quota counts this disk")
	e.admin(t, http.MethodDelete, "/cqb/local", "").must(t, "delete")

	e.h.SetStoredUsageQuota(true)
	r := e.admin(t, http.MethodPut, "/cqb/local", strings.Repeat("x", 300))
	if r.code != http.StatusForbidden || !strings.Contains(r.body, "QuotaExceeded") {
		t.Fatalf("a write past the cluster-wide quota: %d %s", r.code, r.body)
	}
	e.admin(t, http.MethodPut, "/cqb/local", strings.Repeat("x", 150)).must(t, "a write that fits the cluster-wide quota")

	// An overwrite gives back the recorded size of what it replaces.
	e.admin(t, http.MethodPut, "/cqb/local", strings.Repeat("y", 190)).must(t, "an overwrite within the quota")
	// Also of an object whose bytes are on another node, which this node's
	// disk would count as nothing: 990 stored, quota 1000, n2/a is 400.
	e.admin(t, http.MethodPut, "/cqb/n2/a", strings.Repeat("o", 400)).must(t, "an overwrite of another node's object")

	// The object count is cluster-wide too: three objects stored, quota 3.
	r = e.admin(t, http.MethodPut, "/cqb/fourth", "z")
	if r.code != http.StatusForbidden {
		t.Fatalf("a fourth object past a cluster-wide count of 3: %d %s", r.code, r.body)
	}
	// GET ?quota reports the cluster-wide usage.
	q := e.admin(t, http.MethodGet, "/cqb?quota", "")
	if !strings.Contains(q.body, `"current_object_count":3`) || !strings.Contains(q.body, `"current_size_bytes":990`) {
		t.Fatalf("quota report %s, want 3 objects and 990 bytes", q.body)
	}
}
