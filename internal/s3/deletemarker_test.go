package s3

import (
	"encoding/xml"
	"net/http"
	"testing"
)

// Deleting a delete marker must make the newest surviving version current again,
// which is what S3 does. Two bugs sat here: the "latest" pointer in the objects
// bucket was never repointed, so the object stayed invisible even though live
// versions remained; and the promotion took the FIRST entry of a version listing,
// which is the OLDEST version, so a fix to the first bug alone resurrected a
// stale copy. Reproduced against the released build before fixing.
func TestDeletingADeleteMarkerRestoresTheNewestVersion(t *testing.T) {
	_, store, _, ts := newObjTestServer(t)
	bucket, key := "vers", "doc.parquet"
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

	// Deleting without a version id creates a delete marker.
	resp := doSigned(t, http.MethodDelete, ts.URL+"/"+bucket+"/"+key, nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE: %d", resp.StatusCode)
	}
	marker := resp.Header.Get("X-Amz-Version-Id")
	if marker == "" {
		t.Fatal("no delete marker version id returned")
	}
	if resp := doSigned(t, http.MethodGet, ts.URL+"/"+bucket+"/"+key, nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET behind a delete marker = %d, want 404", resp.StatusCode)
	}

	// Removing the marker must bring the object back, at its NEWEST version.
	if resp := doSigned(t, http.MethodDelete, ts.URL+"/"+bucket+"/"+key+"?versionId="+marker, nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE marker: %d", resp.StatusCode)
	}
	got := doSigned(t, http.MethodGet, ts.URL+"/"+bucket+"/"+key, nil)
	if got.StatusCode != http.StatusOK {
		t.Fatalf("GET after removing the delete marker = %d, want 200: the object is still invisible", got.StatusCode)
	}
	body := readBody(t, got)
	if body != "two" {
		t.Fatalf("GET returned %q, want \"two\": a stale version was promoted", body)
	}

	// Exactly one version may claim to be latest, and it must be the newest.
	var listed struct {
		Versions []struct {
			VersionId string
			IsLatest  bool
		} `xml:"Version"`
	}
	vr := doSigned(t, http.MethodGet, ts.URL+"/"+bucket+"?versions", nil)
	if err := xml.NewDecoder(vr.Body).Decode(&listed); err != nil {
		t.Fatalf("decode versions: %v", err)
	}
	vr.Body.Close()
	latestCount := 0
	for _, v := range listed.Versions {
		if v.IsLatest {
			latestCount++
		}
	}
	if latestCount != 1 {
		t.Fatalf("%d versions claim to be latest, want exactly 1", latestCount)
	}
}
