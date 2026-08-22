package s3

import (
	"encoding/xml"
	"fmt"
	"net/http"
	"testing"
)

// multiDelete posts a multi-object delete and returns the decoded result.
func multiDelete(t *testing.T, ts, bucket string, keys ...string) deleteResult {
	t.Helper()
	body := "<Delete>"
	for _, k := range keys {
		body += fmt.Sprintf("<Object><Key>%s</Key></Object>", k)
	}
	body += "</Delete>"
	resp := doSigned(t, http.MethodPost, ts+"/"+bucket+"?delete", []byte(body))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("multi-object delete: %d", resp.StatusCode)
	}
	var out deleteResult
	if err := xml.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode delete result: %v", err)
	}
	resp.Body.Close()
	return out
}

func listVersions(t *testing.T, ts, bucket string) (versions []string, markers int) {
	t.Helper()
	var listed struct {
		Versions []struct {
			Key       string `xml:"Key"`
			VersionId string `xml:"VersionId"`
		} `xml:"Version"`
		Markers []struct {
			Key       string `xml:"Key"`
			VersionId string `xml:"VersionId"`
		} `xml:"DeleteMarker"`
	}
	resp := doSigned(t, http.MethodGet, ts+"/"+bucket+"?versions", nil)
	if err := xml.NewDecoder(resp.Body).Decode(&listed); err != nil {
		t.Fatalf("decode versions: %v", err)
	}
	resp.Body.Close()
	for _, v := range listed.Versions {
		versions = append(versions, v.VersionId)
	}
	return versions, len(listed.Markers)
}

// A multi-object delete on a versioning-enabled bucket must hide the object
// behind a delete marker and keep every version, exactly as a single DELETE
// does. It used to remove the data and the metadata outright, so the versions a
// bucket was enabled to protect were destroyed, and the response still said
// "Deleted". Multi-object delete is what Spark and Hadoop S3A clean up with, so
// this was reachable by an ordinary workload.
func TestBatchDeleteWritesADeleteMarkerOnAVersionedBucket(t *testing.T) {
	_, store, _, ts := newObjTestServer(t)
	bucket, key := "vers", "part-0000.parquet"
	if err := store.CreateBucket(bucket); err != nil {
		t.Fatal(err)
	}
	if err := store.SetBucketVersioning(bucket, "Enabled"); err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{"one", "two"} {
		if resp := doSigned(t, http.MethodPut, ts.URL+"/"+bucket+"/"+key, []byte(body)); resp.StatusCode != http.StatusOK {
			t.Fatalf("PUT %q: %d", body, resp.StatusCode)
		}
	}
	before, _ := listVersions(t, ts.URL, bucket)
	if len(before) != 2 {
		t.Fatalf("setup: %d versions, want 2", len(before))
	}

	result := multiDelete(t, ts.URL, bucket, key)
	if len(result.Errors) != 0 {
		t.Fatalf("multi-object delete reported errors: %+v", result.Errors)
	}
	if len(result.Deleted) != 1 {
		t.Fatalf("%d deleted entries, want 1", len(result.Deleted))
	}

	// The point of the fix, asserted first so a regression says so plainly: the
	// versions survive with their DATA intact, behind exactly one delete marker.
	// Checking the version LIST alone is not enough, because the old behaviour
	// left the version records in place while deleting the bytes they name.
	for _, v := range before {
		got := doSigned(t, http.MethodGet, ts.URL+"/"+bucket+"/"+key+"?versionId="+v, nil)
		if got.StatusCode != http.StatusOK {
			t.Fatalf("version %s reads %d after a multi-object delete, want 200: its data was destroyed",
				v, got.StatusCode)
		}
		got.Body.Close()
	}
	after, markers := listVersions(t, ts.URL, bucket)
	if len(after) != 2 {
		t.Fatalf("%d versions survive the multi-object delete, want 2: the records were destroyed", len(after))
	}
	if markers != 1 {
		t.Fatalf("%d delete markers, want 1", markers)
	}
	// The object is hidden, and the result says a marker was written.
	if resp := doSigned(t, http.MethodGet, ts.URL+"/"+bucket+"/"+key, nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET after multi-object delete = %d, want 404", resp.StatusCode)
	}
	if !result.Deleted[0].DeleteMarker || result.Deleted[0].DeleteMarkerVersionID == "" {
		t.Fatalf("result does not report a delete marker: %+v", result.Deleted[0])
	}

	// The decisive check: removing the marker brings the newest version back.
	marker := result.Deleted[0].DeleteMarkerVersionID
	if resp := doSigned(t, http.MethodDelete, ts.URL+"/"+bucket+"/"+key+"?versionId="+marker, nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE marker: %d", resp.StatusCode)
	}
	got := doSigned(t, http.MethodGet, ts.URL+"/"+bucket+"/"+key, nil)
	if got.StatusCode != http.StatusOK {
		t.Fatalf("GET after removing the marker = %d, want 200: the object did not survive", got.StatusCode)
	}
	if body := readBody(t, got); body != "two" {
		t.Fatalf("restored body %q, want \"two\"", body)
	}
}

// A multi-object delete may also name a version, which is a permanent removal of
// that version and nothing else.
func TestBatchDeleteRemovesOnlyTheNamedVersion(t *testing.T) {
	_, store, _, ts := newObjTestServer(t)
	bucket, key := "vers", "doc.txt"
	if err := store.CreateBucket(bucket); err != nil {
		t.Fatal(err)
	}
	if err := store.SetBucketVersioning(bucket, "Enabled"); err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{"one", "two"} {
		if resp := doSigned(t, http.MethodPut, ts.URL+"/"+bucket+"/"+key, []byte(body)); resp.StatusCode != http.StatusOK {
			t.Fatalf("PUT %q: %d", body, resp.StatusCode)
		}
	}
	versions, _ := listVersions(t, ts.URL, bucket)
	if len(versions) != 2 {
		t.Fatalf("setup: %d versions, want 2", len(versions))
	}
	target := versions[0]

	body := fmt.Sprintf("<Delete><Object><Key>%s</Key><VersionId>%s</VersionId></Object></Delete>", key, target)
	resp := doSigned(t, http.MethodPost, ts.URL+"/"+bucket+"?delete", []byte(body))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("multi-object delete: %d", resp.StatusCode)
	}
	var out deleteResult
	if err := xml.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if len(out.Errors) != 0 {
		t.Fatalf("errors: %+v", out.Errors)
	}
	if len(out.Deleted) != 1 || out.Deleted[0].VersionID != target {
		t.Fatalf("result does not echo the version deleted: %+v", out.Deleted)
	}
	if out.Deleted[0].DeleteMarker {
		t.Fatal("a named-version delete reported a delete marker")
	}

	left, markers := listVersions(t, ts.URL, bucket)
	if len(left) != 1 || left[0] == target {
		t.Fatalf("versions left %v, want exactly the one that was not named", left)
	}
	if markers != 0 {
		t.Fatalf("%d delete markers written by a named-version delete, want 0", markers)
	}
	// The surviving version is still readable, so the object was not orphaned.
	if resp := doSigned(t, http.MethodGet, ts.URL+"/"+bucket+"/"+key, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("GET after removing one version = %d, want 200", resp.StatusCode)
	}
}

// On a bucket with no versioning, a multi-object delete still removes the object
// outright. The fix must not turn every delete into a marker.
func TestBatchDeleteStillRemovesUnversionedObjects(t *testing.T) {
	_, store, engine, ts := newObjTestServer(t)
	bucket := "plain"
	if err := store.CreateBucket(bucket); err != nil {
		t.Fatal(err)
	}
	keys := []string{"a.txt", "b.txt"}
	for _, k := range keys {
		if resp := doSigned(t, http.MethodPut, ts.URL+"/"+bucket+"/"+k, []byte("data")); resp.StatusCode != http.StatusOK {
			t.Fatalf("PUT %s: %d", k, resp.StatusCode)
		}
	}

	result := multiDelete(t, ts.URL, bucket, keys...)
	if len(result.Errors) != 0 {
		t.Fatalf("errors: %+v", result.Errors)
	}
	if len(result.Deleted) != 2 {
		t.Fatalf("%d deleted entries, want 2", len(result.Deleted))
	}
	for _, k := range keys {
		if result.Deleted[0].DeleteMarker {
			t.Fatal("an unversioned delete reported a delete marker")
		}
		if meta, _ := store.GetObjectMeta(bucket, k); meta != nil {
			t.Fatalf("%s still has metadata after an unversioned multi-object delete", k)
		}
		if engine.ObjectExists(bucket, k) {
			t.Fatalf("%s still has data after an unversioned multi-object delete", k)
		}
	}
}

// On a bucket with versioning SUSPENDED, a delete replaces the null version with
// a null delete marker. The old multi-object path deleted the null version's
// bytes and its latest pointer and wrote no marker at all, so the object went
// silently missing while the versions taken while versioning was enabled stayed
// on disk, reachable only by naming a version id.
func TestBatchDeleteWritesANullMarkerWhenVersioningIsSuspended(t *testing.T) {
	_, store, _, ts := newObjTestServer(t)
	bucket, key := "susp", "doc.txt"
	if err := store.CreateBucket(bucket); err != nil {
		t.Fatal(err)
	}
	if err := store.SetBucketVersioning(bucket, "Enabled"); err != nil {
		t.Fatal(err)
	}
	if resp := doSigned(t, http.MethodPut, ts.URL+"/"+bucket+"/"+key, []byte("kept")); resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT: %d", resp.StatusCode)
	}
	kept, _ := listVersions(t, ts.URL, bucket)
	if len(kept) != 1 {
		t.Fatalf("setup: %d versions, want 1", len(kept))
	}
	if err := store.SetBucketVersioning(bucket, "Suspended"); err != nil {
		t.Fatal(err)
	}
	if resp := doSigned(t, http.MethodPut, ts.URL+"/"+bucket+"/"+key, []byte("null-version")); resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT null: %d", resp.StatusCode)
	}

	result := multiDelete(t, ts.URL, bucket, key)
	if len(result.Errors) != 0 {
		t.Fatalf("errors: %+v", result.Errors)
	}
	if !result.Deleted[0].DeleteMarker || result.Deleted[0].DeleteMarkerVersionID != "null" {
		t.Fatalf("suspended delete did not write a null delete marker: %+v", result.Deleted[0])
	}
	if resp := doSigned(t, http.MethodGet, ts.URL+"/"+bucket+"/"+key, nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET behind the null marker = %d, want 404", resp.StatusCode)
	}
	// The version taken while versioning was enabled is untouched and readable.
	got := doSigned(t, http.MethodGet, ts.URL+"/"+bucket+"/"+key+"?versionId="+kept[0], nil)
	if got.StatusCode != http.StatusOK {
		t.Fatalf("the pre-suspension version reads %d, want 200", got.StatusCode)
	}
	if body := readBody(t, got); body != "kept" {
		t.Fatalf("pre-suspension version body %q, want \"kept\"", body)
	}
	// And the marker can be removed, which is the whole point of writing one.
	if resp := doSigned(t, http.MethodDelete, ts.URL+"/"+bucket+"/"+key+"?versionId=null", nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE null marker: %d", resp.StatusCode)
	}
	if resp := doSigned(t, http.MethodGet, ts.URL+"/"+bucket+"/"+key, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("GET after removing the null marker = %d, want 200", resp.StatusCode)
	}
}

// The sharpest case, and an ordinary sequence: a bucket holds objects, the owner
// later enables versioning to protect them, and a bulk delete then destroys
// exactly the objects that predate the protection. Those have no version record
// to fall back on, so the old multi-object path removed their bytes outright
// with no delete marker and nothing to restore from. A delete marker keeps the
// data and makes the delete reversible, which is what enabling versioning was
// supposed to buy.
func TestBatchDeleteKeepsObjectsWrittenBeforeVersioningWasEnabled(t *testing.T) {
	_, store, engine, ts := newObjTestServer(t)
	bucket, key := "legacy", "written-first.txt"
	if err := store.CreateBucket(bucket); err != nil {
		t.Fatal(err)
	}
	if resp := doSigned(t, http.MethodPut, ts.URL+"/"+bucket+"/"+key, []byte("legacy bytes")); resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT: %d", resp.StatusCode)
	}
	if err := store.SetBucketVersioning(bucket, "Enabled"); err != nil {
		t.Fatal(err)
	}

	result := multiDelete(t, ts.URL, bucket, key)
	if len(result.Errors) != 0 {
		t.Fatalf("errors: %+v", result.Errors)
	}
	if !engine.ObjectExists(bucket, key) {
		t.Fatal("the object's data was destroyed: it predates versioning, so there is no version to restore from")
	}
	if !result.Deleted[0].DeleteMarker {
		t.Fatalf("no delete marker written: %+v", result.Deleted[0])
	}
	if resp := doSigned(t, http.MethodGet, ts.URL+"/"+bucket+"/"+key, nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET behind the marker = %d, want 404", resp.StatusCode)
	}

	// NOTE: removing the marker does NOT bring this object back yet, because an
	// object written before versioning was enabled has no version record and S3's
	// "it becomes the null version" rule is not implemented. That gap is shared
	// with the single-object DELETE, which behaves identically, and is tracked
	// separately. What this test pins is the part that was destructive: the bytes
	// survive a bulk delete instead of being removed with nothing to restore from.
}
