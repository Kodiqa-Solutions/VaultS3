package server

import (
	"testing"
	"time"

	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
	"github.com/Kodiqa-Solutions/VaultS3/internal/replication"
)

// A change in a bucket with versioning is recorded as the version or delete
// marker it made, with its ID. A version delete, which leaves no version, and
// a null version are not recorded.
func TestActiveActiveRecordsVersionsAndMarkersOnly(t *testing.T) {
	srv := newTestServer(t)
	cl := replication.NewChangeLog(srv.metaStore, "site-a")
	now := time.Now().Unix()
	for _, m := range []metadata.ObjectMeta{
		{Bucket: "b", Key: "k", VersionID: "0000000000000001aaaaaaaa", ETag: `"e1"`, Size: 5, LastModified: now},
		{Bucket: "b", Key: "k", VersionID: "0000000000000002bbbbbbbb", DeleteMarker: true, LastModified: now + 1},
	} {
		if err := srv.metaStore.PutObjectVersion(m); err != nil {
			t.Fatal(err)
		}
	}
	recordVersionChange(srv.metaStore, cl, "b", "k", "0000000000000001aaaaaaaa")
	recordVersionChange(srv.metaStore, cl, "b", "k", "0000000000000002bbbbbbbb")
	recordVersionChange(srv.metaStore, cl, "b", "k", "0000000000000003cccccccc") // deleted: no such version
	recordVersionChange(srv.metaStore, cl, "b", "k", "null")

	got, _, err := cl.ChangesSince(0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("%d changes recorded, want 2: %+v", len(got), got)
	}
	if c := got[0]; c.EventType != "put" || c.VersionID != "0000000000000001aaaaaaaa" || c.ETag != `"e1"` || c.Size != 5 || c.LastModified != now || c.DeleteMarker {
		t.Errorf("version change %+v", c)
	}
	if c := got[1]; c.EventType != "delete-marker" || !c.DeleteMarker || c.VersionID != "0000000000000002bbbbbbbb" {
		t.Errorf("delete marker change %+v", c)
	}
}
