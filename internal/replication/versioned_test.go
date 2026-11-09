package replication

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Kodiqa-Solutions/VaultS3/internal/config"
	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
)

// vid makes a version ID the way the server does: creation time, then random.
func vid(t time.Time, n int) string { return fmt.Sprintf("%016x%08x", t.UnixNano(), n) }

// putVersion stores a version the way the S3 handler does in a bucket with
// versioning enabled, and returns its change entry.
func (s site) putVersion(t *testing.T, bucket, key, id, body string, at time.Time, extra func(*metadata.ObjectMeta)) ChangeEntry {
	t.Helper()
	s.versionedBucket(t, bucket)
	n, etag, err := s.engine.PutObjectVersion(bucket, key, id, strings.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatal(err)
	}
	m := metadata.ObjectMeta{Bucket: bucket, Key: key, VersionID: id, Size: n, ETag: etag, LastModified: at.Unix(), ContentType: "text/plain"}
	if extra != nil {
		extra(&m)
	}
	s.latest(t, m)
	return ChangeEntry{Bucket: bucket, Key: key, EventType: "put", SiteID: "remote", VersionID: id, ETag: etag, Size: n, LastModified: at.Unix()}
}

func (s site) deleteMarker(t *testing.T, bucket, key, id string, at time.Time) ChangeEntry {
	t.Helper()
	s.versionedBucket(t, bucket)
	s.latest(t, metadata.ObjectMeta{Bucket: bucket, Key: key, VersionID: id, DeleteMarker: true, LastModified: at.Unix()})
	return ChangeEntry{Bucket: bucket, Key: key, EventType: "delete-marker", SiteID: "remote", VersionID: id, DeleteMarker: true, LastModified: at.Unix()}
}

// latest records m as the key's newest version, as a local write does.
func (s site) latest(t *testing.T, m metadata.ObjectMeta) {
	t.Helper()
	if cur, err := s.store.GetObjectMeta(m.Bucket, m.Key); err == nil && cur != nil && cur.VersionID > m.VersionID {
		if err := s.store.PutObjectVersion(m); err != nil { // an older version, as replication may add
			t.Fatal(err)
		}
		return
	}
	m.IsLatest = true
	if err := s.store.PutObjectVersion(m); err != nil {
		t.Fatal(err)
	}
	if err := s.store.PutObjectMeta(m); err != nil {
		t.Fatal(err)
	}
}

func (s site) versionedBucket(t *testing.T, bucket string) {
	t.Helper()
	if !s.store.BucketExists(bucket) {
		if err := s.store.CreateBucket(bucket); err != nil {
			t.Fatal(err)
		}
		s.engine.CreateBucketDir(bucket)
	}
	if err := s.store.SetBucketVersioning(bucket, "Enabled"); err != nil {
		t.Fatal(err)
	}
}

func (s site) readVersion(t *testing.T, bucket, key, id string) string {
	t.Helper()
	r, _, err := s.engine.GetObjectVersion(bucket, key, id)
	if err != nil {
		return "<missing>"
	}
	defer r.Close()
	b, _ := io.ReadAll(r)
	return string(b)
}

func pullWorker(local site, url string) *BiDirectionalWorker {
	return NewBiDirectionalWorker(local.store, local.engine, config.ReplicationConfig{
		SiteID: "local",
		Peers:  []config.ReplicationPeer{{Name: "p", URL: url, AccessKey: "peer-ak", SecretKey: "peer-sk"}},
	})
}

// 5.0.11 skipped every change to a bucket with versioning. Versions and
// delete markers now replicate under their own IDs, and every site ends with
// the same current version whatever order the changes arrive in.
func TestVersionsReplicateAndTheNewestIsCurrentWhateverTheOrder(t *testing.T) {
	local, remote := newSite(t), newSite(t)
	srv, _ := peerServer(t, remote)
	w := pullWorker(local, srv.URL)
	ctx := context.Background()
	t0 := time.Now().Add(-time.Hour)

	c1 := remote.putVersion(t, "ver", "k", vid(t0, 1), "first", t0, nil)
	c3 := remote.putVersion(t, "ver", "k", vid(t0.Add(2*time.Minute), 3), "third", t0.Add(2*time.Minute), func(m *metadata.ObjectMeta) {
		m.ContentType = "application/x-third"
		m.UserMetadata = map[string]string{"owner": "remote"}
		m.LegalHold = true
	})
	local.versionedBucket(t, "ver")
	mine := local.putVersion(t, "ver", "k", vid(t0.Add(time.Minute), 2), "second, written here", t0.Add(time.Minute), nil)

	// Newest first, then the oldest, then the newest again.
	for _, c := range []ChangeEntry{c3, c1, c3} {
		if err := w.applyRemoteChange(ctx, w.peers["p"], c); err != nil {
			t.Fatalf("apply %s: %v", c.VersionID, err)
		}
	}
	cur, err := local.store.GetObjectMeta("ver", "k")
	if err != nil || cur.VersionID != c3.VersionID {
		t.Fatalf("current version %v (%v), want the newest %s", cur, err, c3.VersionID)
	}
	if cur.ContentType != "application/x-third" || cur.UserMetadata["owner"] != "remote" || !cur.LegalHold || cur.ETag != c3.ETag {
		t.Errorf("metadata not carried: type %q meta %v hold %v etag %q", cur.ContentType, cur.UserMetadata, cur.LegalHold, cur.ETag)
	}
	for id, want := range map[string]string{c1.VersionID: "first", mine.VersionID: "second, written here", c3.VersionID: "third"} {
		if got := local.readVersion(t, "ver", "k", id); got != want {
			t.Errorf("version %s = %q, want %q", id, got, want)
		}
	}
	versions, _, _ := local.store.ListObjectVersions("ver", "k", "", "", 100)
	latest := 0
	for _, v := range versions {
		if v.IsLatest {
			latest++
		}
	}
	if len(versions) != 3 || latest != 1 {
		t.Errorf("%d versions, %d marked latest; want 3 and 1", len(versions), latest)
	}

	// A delete marker hides the key and keeps every version.
	dm := remote.deleteMarker(t, "ver", "k", vid(t0.Add(3*time.Minute), 4), t0.Add(3*time.Minute))
	if err := w.applyRemoteChange(ctx, w.peers["p"], dm); err != nil {
		t.Fatal(err)
	}
	cur, _ = local.store.GetObjectMeta("ver", "k")
	if cur == nil || !cur.DeleteMarker || cur.VersionID != dm.VersionID {
		t.Fatalf("current after a replicated delete: %+v, want the delete marker", cur)
	}
	if got := local.readVersion(t, "ver", "k", c3.VersionID); got != "third" {
		t.Errorf("a delete marker removed a version: %q", got)
	}
	// An older delete marker arriving late does not hide a newer version.
	late := remote.deleteMarker(t, "ver", "late", vid(t0, 9), t0)
	fresh := remote.putVersion(t, "ver", "late", vid(t0.Add(time.Minute), 10), "fresh", t0.Add(time.Minute), nil)
	for _, c := range []ChangeEntry{fresh, late} {
		if err := w.applyRemoteChange(ctx, w.peers["p"], c); err != nil {
			t.Fatal(err)
		}
	}
	if cur, _ := local.store.GetObjectMeta("ver", "late"); cur == nil || cur.DeleteMarker || cur.VersionID != fresh.VersionID {
		t.Errorf("an older delete marker became current: %+v", cur)
	}
}

// An object written before versioning was enabled is the null version. A
// replicated version took the key's record without keeping it.
func TestAReplicatedVersionKeepsThePreVersioningObject(t *testing.T) {
	local, remote := newSite(t), newSite(t)
	srv, _ := peerServer(t, remote)
	w := pullWorker(local, srv.URL)
	local.put(t, "ver", "k", "from before versioning")
	local.versionedBucket(t, "ver")
	c := remote.putVersion(t, "ver", "k", vid(time.Now(), 1), "new", time.Now().Add(time.Second), nil)
	if err := w.applyRemoteChange(context.Background(), w.peers["p"], c); err != nil {
		t.Fatal(err)
	}
	null, err := local.store.GetObjectVersion("ver", "k", "null")
	if err != nil || null.IsLatest {
		t.Fatalf("the pre-versioning object is not kept as the null version: %+v %v", null, err)
	}
	if got, _ := local.read(t, "ver", "k"); got != "from before versioning" {
		t.Errorf("the null version's data at the plain path: %q", got)
	}
	if cur, _ := local.store.GetObjectMeta("ver", "k"); cur.VersionID != c.VersionID {
		t.Errorf("current %s, want %s", cur.VersionID, c.VersionID)
	}
}

// Changes that cannot be applied as a version are skipped, not retried for
// ever: a bucket without versioning enabled here, a null version, an ID this
// server would not generate. A short fetch is an error and leaves nothing.
func TestVersionedChangesThatCannotApply(t *testing.T) {
	local, remote := newSite(t), newSite(t)
	srv, _ := peerServer(t, remote)
	w := pullWorker(local, srv.URL)
	ctx := context.Background()
	now := time.Now()
	c := remote.putVersion(t, "ver", "k", vid(now, 1), "data", now, nil)

	local.put(t, "ver", "seed", "x") // bucket exists here, versioning off
	if err := w.applyRemoteChange(ctx, w.peers["p"], c); err != nil {
		t.Fatal(err)
	}
	if _, err := local.store.GetObjectVersion("ver", "k", c.VersionID); err == nil {
		t.Fatal("a version was stored in a bucket without versioning")
	}
	local.store.SetBucketVersioning("ver", "Suspended")
	if err := w.applyRemoteChange(ctx, w.peers["p"], c); err != nil {
		t.Fatal(err)
	}
	if _, err := local.store.GetObjectVersion("ver", "k", c.VersionID); err == nil {
		t.Fatal("a version was stored in a bucket with versioning suspended")
	}

	local.versionedBucket(t, "ver")
	// A peer that answers any GET, so only the ID check stands between a
	// "../" version ID and a write outside the key's version directory.
	liar := pullWorker(local, syncPeerServer(t, nil, nil).URL)
	for _, bad := range []string{"null", "../../x", "ZZZZZZZZZZZZZZZZZZZZ"} {
		for _, marker := range []bool{false, true} {
			b := ChangeEntry{Bucket: "ver", Key: "k", EventType: "put", SiteID: "remote", VersionID: bad, DeleteMarker: marker}
			if err := liar.applyRemoteChange(ctx, liar.peers["p"], b); err != nil {
				t.Errorf("version %q: %v, want it skipped", bad, err)
			}
			if _, err := local.store.GetObjectVersion("ver", "k", bad); err == nil {
				t.Errorf("version %q (marker %v) was stored", bad, marker)
			}
		}
	}
	if _, err := os.Stat(filepath.Join(local.engine.DataDir(), "ver", "x")); err == nil {
		t.Error("a ../ version ID wrote outside the version store")
	}

	short := c
	short.Size = c.Size + 10
	if err := w.applyRemoteChange(ctx, w.peers["p"], short); err == nil {
		t.Fatal("a version shorter than its change applied")
	}
	if _, err := local.store.GetObjectVersion("ver", "k", c.VersionID); err == nil {
		t.Error("a short version was recorded")
	}
	if got := local.readVersion(t, "ver", "k", c.VersionID); got != "<missing>" {
		t.Errorf("a short version's data was left: %q", got)
	}
	if err := w.applyRemoteChange(ctx, w.peers["p"], c); err != nil {
		t.Fatal(err)
	}
	if got := local.readVersion(t, "ver", "k", c.VersionID); got != "data" {
		t.Errorf("version after retry: %q", got)
	}
}

// An object stored with Content-Encoding: gzip was fetched with the HTTP
// client's own gzip handling, which decoded it, so the other site stored the
// decompressed bytes under the same ETag.
func TestReplicationKeepsTheStoredBytesOfAGzipObject(t *testing.T) {
	local, remote := newSite(t), newSite(t)
	srv, _ := peerServer(t, remote)
	w := pullWorker(local, srv.URL)
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	zw.Write([]byte(strings.Repeat("plain text ", 100)))
	zw.Close()
	now := time.Now()
	c := remote.putVersion(t, "ver", "k.gz", vid(now, 1), gz.String(), now, func(m *metadata.ObjectMeta) { m.ContentEncoding = "gzip" })
	local.versionedBucket(t, "ver")
	if err := w.applyRemoteChange(context.Background(), w.peers["p"], c); err != nil {
		t.Fatal(err)
	}
	if got := local.readVersion(t, "ver", "k.gz", c.VersionID); got != gz.String() {
		t.Fatalf("stored %d bytes, want the %d gzip bytes as they were", len(got), gz.Len())
	}
	if m, _ := local.store.GetObjectMeta("ver", "k.gz"); m == nil || m.ContentEncoding != "gzip" {
		t.Errorf("Content-Encoding not carried: %+v", m)
	}
}

// handlerWrite stores a version as the S3 handler does: it takes the next ID
// above the key's current version and always becomes current.
func (s site) handlerWrite(t *testing.T, bucket, key, body string, marker bool) ChangeEntry {
	t.Helper()
	s.versionedBucket(t, bucket)
	cur := ""
	if m, err := s.store.GetObjectMeta(bucket, key); err == nil && m != nil {
		cur = m.VersionID
	}
	id := metadata.NewVersionID(cur)
	m := metadata.ObjectMeta{Bucket: bucket, Key: key, VersionID: id, IsLatest: true, LastModified: time.Now().Unix(), DeleteMarker: marker}
	if !marker {
		n, etag, err := s.engine.PutObjectVersion(bucket, key, id, strings.NewReader(body), int64(len(body)))
		if err != nil {
			t.Fatal(err)
		}
		m.Size, m.ETag, m.ContentType = n, etag, "text/plain"
	}
	if err := s.store.PutObjectVersion(m); err != nil {
		t.Fatal(err)
	}
	if err := s.store.PutObjectMeta(m); err != nil {
		t.Fatal(err)
	}
	ev := "put"
	if marker {
		ev = "delete-marker"
	}
	return ChangeEntry{Bucket: bucket, Key: key, EventType: ev, SiteID: "x", VersionID: id, DeleteMarker: marker, ETag: m.ETag, Size: m.Size, LastModified: m.LastModified}
}

// Regression (5.0.12 review): site B's clock runs 30 s ahead. A pulled B's
// version, then wrote its own, later in real time but with a lower ID from
// its own clock: current at A, not at B, for good. A local write now takes
// an ID above the current one, so both sites agree. The same for a delete.
func TestSitesAgreeOnTheCurrentVersionDespiteClockSkew(t *testing.T) {
	a, b := newSite(t), newSite(t)
	srvA, _ := peerServer(t, a)
	srvB, _ := peerServer(t, b)
	wA, wB := pullWorker(a, srvB.URL), pullWorker(b, srvA.URL)
	ctx := context.Background()
	future := time.Now().Add(30 * time.Second)
	cB := b.putVersion(t, "ver", "k", vid(future, 1), "from B, clock ahead", future, nil)
	a.versionedBucket(t, "ver")
	if err := wA.applyRemoteChange(ctx, wA.peers["p"], cB); err != nil {
		t.Fatal(err)
	}
	for _, marker := range []bool{false, true} {
		cA := a.handlerWrite(t, "ver", "k", "from A, later", marker)
		if err := wB.applyRemoteChange(ctx, wB.peers["p"], cA); err != nil {
			t.Fatal(err)
		}
		ca, _ := a.store.GetObjectMeta("ver", "k")
		cb, _ := b.store.GetObjectMeta("ver", "k")
		if ca.VersionID != cA.VersionID || cb.VersionID != cA.VersionID {
			t.Fatalf("marker=%v: current at A %s, at B %s, want A's newer write %s", marker, ca.VersionID, cb.VersionID, cA.VersionID)
		}
	}
	// An ID dated far ahead is refused, so it cannot drag every later ID on.
	far := time.Now().Add(48 * time.Hour)
	cF := b.putVersion(t, "ver", "f", vid(far, 9), "far", far, nil)
	if err := wA.applyRemoteChange(ctx, wA.peers["p"], cF); err != nil {
		t.Fatal(err)
	}
	if _, err := a.store.GetObjectVersion("ver", "f", cF.VersionID); err == nil {
		t.Error("a version dated two days ahead was stored")
	}
}

func TestNewVersionIDIsAboveTheCurrentOne(t *testing.T) {
	ahead := vid(time.Now().Add(time.Hour), 7)
	for i := 0; i < 3; i++ {
		next := metadata.NewVersionID(ahead)
		if next <= ahead || len(next) != len(ahead) {
			t.Fatalf("NewVersionID(%s) = %s, want a higher ID of the same length", ahead, next)
		}
		ahead = next
	}
	if id := metadata.NewVersionID(""); len(id) != 24 {
		t.Errorf("fresh ID %q", id)
	}
	if id := metadata.NewVersionID("null"); len(id) != 24 {
		t.Errorf("after null: %q", id)
	}
}

// A replicated version with no retention of its own gets the receiving
// bucket's default retention, as a write there would.
func TestReplicatedVersionGetsTheBucketsDefaultRetention(t *testing.T) {
	local, remote := newSite(t), newSite(t)
	srv, _ := peerServer(t, remote)
	w := pullWorker(local, srv.URL)
	now := time.Now()
	c := remote.putVersion(t, "ver", "k", vid(now, 1), "data", now, nil)
	local.versionedBucket(t, "ver")
	if err := local.store.SetBucketDefaultRetention("ver", "GOVERNANCE", 3); err != nil {
		t.Skipf("no default retention setter: %v", err)
	}
	if err := w.applyRemoteChange(context.Background(), w.peers["p"], c); err != nil {
		t.Fatal(err)
	}
	m, _ := local.store.GetObjectVersion("ver", "k", c.VersionID)
	if m == nil || m.RetentionMode != "GOVERNANCE" || m.RetentionUntil < now.Unix()+2*86400 {
		t.Fatalf("replicated version retention %+v", m)
	}
}
