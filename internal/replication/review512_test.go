package replication

import (
	"context"
	"testing"
	"time"

	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
)

// Regression (5.0.12 review): An ID whose time field has the top bit set is outside VersionIDTime's
// Regression (5.0.12 review): range, so the "more than a day ahead" guard reads it as 0 and lets it in.
func TestAVersionIDBeyondTheTimeRangeIsRefused(t *testing.T) {
	local, remote := newSite(t), newSite(t)
	srv, _ := peerServer(t, remote)
	w := pullWorker(local, srv.URL)
	id := "ffffffffffffffff00000001"
	c := remote.deleteMarker(t, "ver", "k", id, time.Now())
	local.versionedBucket(t, "ver")
	if err := w.applyRemoteChange(context.Background(), w.peers["p"], c); err != nil {
		t.Fatal(err)
	}
	cur, _ := local.store.GetObjectMeta("ver", "k")
	if cur != nil && cur.VersionID == id {
		next := metadata.NewVersionID(cur.VersionID)
		t.Fatalf("version %s dated beyond int64 was applied and is current; the next local ID %s sorts below it (%v)", id, next, next < id)
	}
}
