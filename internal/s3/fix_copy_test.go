package s3

import (
	"crypto/md5"
	"encoding/base64"
	"encoding/xml"
	"net/http"
	"strings"
	"testing"
)

// The router authorized the copy source after cutting it at a decoded '?',
// while CopyObject read the decoded key whole. So a grant on "pub" read the
// object literally named "pub?versionId=1".
func TestCopySourceIsAuthorizedAsTheKeyItReads(t *testing.T) {
	e := newFixEnv(t)
	e.admin(t, http.MethodPut, "/cpsrc", "").must(t, "create source bucket")
	e.admin(t, http.MethodPut, "/cpdst", "").must(t, "create destination bucket")
	e.admin(t, http.MethodPut, "/cpsrc/pub", "public").must(t, "put pub")
	e.admin(t, http.MethodPut, "/cpsrc/pub%3FversionId=1", "secret").must(t, "put the bystander")

	ak, sk := e.addUser(t, "copier", allow(`"s3:GetObject","s3:PutObject"`, `"arn:aws:s3:::cpsrc/pub","arn:aws:s3:::cpdst/*"`))
	expect(t, "copy of the bystander through an encoded '?'", e.as(t, ak, sk, http.MethodPut, "/cpdst/out", "",
		"X-Amz-Copy-Source", "/cpsrc/pub%3FversionId%3D1"), http.StatusForbidden, "AccessDenied")
	expect(t, "nothing was copied", e.admin(t, http.MethodGet, "/cpdst/out", ""), http.StatusNotFound, "NoSuchKey")
	expect(t, "the granted copy", e.as(t, ak, sk, http.MethodPut, "/cpdst/out", "", "X-Amz-Copy-Source", "/cpsrc/pub"), http.StatusOK, "")
	if got := e.admin(t, http.MethodGet, "/cpdst/out", ""); got.body != "public" {
		t.Errorf("copied %q", got.body)
	}
}

func putVersions(t *testing.T, e *fixEnv, bucket, key string, bodies ...string) []string {
	t.Helper()
	var ids []string
	for _, b := range bodies {
		req, _ := http.NewRequest(http.MethodPut, e.ts.URL+"/"+bucket+"/"+key, strings.NewReader(b))
		req.ContentLength = int64(len(b))
		signV4Request(req, testAccessKey, testSecretKey, []byte(b))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK || resp.Header.Get("X-Amz-Version-Id") == "" {
			t.Fatalf("put version: %d %q", resp.StatusCode, resp.Header.Get("X-Amz-Version-Id"))
		}
		ids = append(ids, resp.Header.Get("X-Amz-Version-Id"))
	}
	return ids
}

// A copy reads its source through the metadata store, so a versioned source is
// found, a named version is the one copied, and naming one needs
// s3:GetObjectVersion. The plain-path read answered NoSuchKey for every object
// in a versioned bucket and ignored ?versionId.
func TestCopyReadsTheVersionItNames(t *testing.T) {
	e := newFixEnv(t)
	e.admin(t, http.MethodPut, "/cvsrc", "").must(t, "create bucket")
	e.admin(t, http.MethodPut, "/cvsrc?versioning", versioningOn).must(t, "enable versioning")
	ids := putVersions(t, e, "cvsrc", "k", "first", "second")

	expect(t, "copy of the current version", e.admin(t, http.MethodPut, "/cvsrc/latest", "", "X-Amz-Copy-Source", "/cvsrc/k"), http.StatusOK, "")
	if got := e.admin(t, http.MethodGet, "/cvsrc/latest", ""); got.body != "second" {
		t.Errorf("copy of the current version read %q", got.body)
	}
	expect(t, "copy of the first version", e.admin(t, http.MethodPut, "/cvsrc/old", "", "X-Amz-Copy-Source", "/cvsrc/k?versionId="+ids[0]), http.StatusOK, "")
	if got := e.admin(t, http.MethodGet, "/cvsrc/old", ""); got.body != "first" {
		t.Errorf("copy of the first version read %q", got.body)
	}

	ak, sk := e.addUser(t, "nover", allow(`"s3:GetObject","s3:PutObject"`, `"*"`))
	expect(t, "copy of a version without s3:GetObjectVersion", e.as(t, ak, sk, http.MethodPut, "/cvsrc/x", "",
		"X-Amz-Copy-Source", "/cvsrc/k?versionId="+ids[0]), http.StatusForbidden, "AccessDenied")
}

// Tags live in the metadata store, and that is what tagging now asks. It asked
// the local disk for the plain path, which an object in a versioned bucket does
// not have.
func TestTaggingWorksOnVersionedObjects(t *testing.T) {
	e := newFixEnv(t)
	e.admin(t, http.MethodPut, "/tagv", "").must(t, "create bucket")
	e.admin(t, http.MethodPut, "/tagv?versioning", versioningOn).must(t, "enable versioning")
	ids := putVersions(t, e, "tagv", "k", "first", "second")
	tags := func(k, v string) string {
		return `<Tagging><TagSet><Tag><Key>` + k + `</Key><Value>` + v + `</Value></Tag></TagSet></Tagging>`
	}

	expect(t, "tag the current version", e.admin(t, http.MethodPut, "/tagv/k?tagging", tags("which", "current")), http.StatusOK, "")
	expect(t, "tag the first version", e.admin(t, http.MethodPut, "/tagv/k?tagging&versionId="+ids[0], tags("which", "first")), http.StatusOK, "")

	if got := e.admin(t, http.MethodGet, "/tagv/k?tagging", ""); !strings.Contains(got.body, "<Value>current</Value>") {
		t.Errorf("current version's tags: %d %s", got.code, got.body)
	}
	if got := e.admin(t, http.MethodGet, "/tagv/k?tagging&versionId="+ids[0], ""); !strings.Contains(got.body, "<Value>first</Value>") {
		t.Errorf("first version's tags: %d %s", got.code, got.body)
	}
	expect(t, "delete the current version's tags", e.admin(t, http.MethodDelete, "/tagv/k?tagging", ""), http.StatusNoContent, "")
	if got := e.admin(t, http.MethodGet, "/tagv/k?tagging&versionId="+ids[0], ""); !strings.Contains(got.body, "<Value>first</Value>") {
		t.Errorf("deleting the current version's tags touched another version: %s", got.body)
	}
	expect(t, "tags of a missing key", e.admin(t, http.MethodGet, "/tagv/nope?tagging", ""), http.StatusNotFound, "NoSuchKey")
}

// A copy or a completed multipart upload into a versioned bucket adds a
// version. Both wrote the plain path with no version id and replaced the
// current object in place.
func TestCopyAndCompleteIntoAVersionedBucketAddVersions(t *testing.T) {
	e := newFixEnv(t)
	e.admin(t, http.MethodPut, "/vdst", "").must(t, "create bucket")
	e.admin(t, http.MethodPut, "/vdst?versioning", versioningOn).must(t, "enable versioning")
	e.admin(t, http.MethodPut, "/vdst/src", "copied").must(t, "put source")
	putVersions(t, e, "vdst", "k", "original")

	expect(t, "copy onto k", e.admin(t, http.MethodPut, "/vdst/k", "", "X-Amz-Copy-Source", "/vdst/src"), http.StatusOK, "")
	id := e.startUpload(t, "vdst", "k")
	e.admin(t, http.MethodPut, "/vdst/k?partNumber=1&uploadId="+id, "uploaded").must(t, "part")
	expect(t, "complete onto k", e.admin(t, http.MethodPost, "/vdst/k?uploadId="+id, completeXML("1", quotedMD5("uploaded"))), http.StatusOK, "")

	ids := e.versionIDs(t, "vdst", "k")
	if len(ids) != 3 {
		t.Fatalf("k has %d versions, want 3 (original, copy, multipart): %v", len(ids), ids)
	}
	// Versions written in the same second list in no fixed order, so compare
	// the set of contents.
	got := map[string]bool{}
	for _, id := range ids {
		got[e.admin(t, http.MethodGet, "/vdst/k?versionId="+id, "").body] = true
	}
	for _, w := range []string{"uploaded", "copied", "original"} {
		if !got[w] {
			t.Errorf("no version of k reads %q; versions read %v", w, got)
		}
	}
	if cur := e.admin(t, http.MethodGet, "/vdst/k", ""); cur.body != "uploaded" {
		t.Errorf("the current version reads %q", cur.body)
	}
}

// An object written before versioning was turned on is the null version. A PUT
// after versioning was enabled overwrote the only record of it.
func TestPutKeepsAPreVersioningObject(t *testing.T) {
	e := newFixEnv(t)
	e.admin(t, http.MethodPut, "/prev", "").must(t, "create bucket")
	e.admin(t, http.MethodPut, "/prev/k", "before versioning").must(t, "put")
	e.admin(t, http.MethodPut, "/prev?versioning", versioningOn).must(t, "enable versioning")
	putVersions(t, e, "prev", "k", "after versioning")
	if got := e.admin(t, http.MethodGet, "/prev/k?versionId=null", ""); got.body != "before versioning" {
		t.Errorf("the null version reads %d %q", got.code, got.body)
	}
	if got := e.admin(t, http.MethodGet, "/prev/k", ""); got.body != "after versioning" {
		t.Errorf("the current version reads %q", got.body)
	}
}

func ssecHeaderList(prefix string, key []byte) []string {
	sum := md5.Sum(key)
	return []string{
		prefix + "Algorithm", "AES256",
		prefix + "Key", base64.StdEncoding.EncodeToString(key),
		prefix + "Key-Md5", base64.StdEncoding.EncodeToString(sum[:]),
	}
}

// Copying an SSE-C source read its stored ciphertext and stored that at the
// destination as an ordinary object, so the destination served ciphertext.
func TestCopyOfAnSSECSourceNeedsItsKey(t *testing.T) {
	e := newFixEnv(t)
	e.admin(t, http.MethodPut, "/ssecb", "").must(t, "create bucket")
	k1 := []byte("0123456789abcdef0123456789abcdef")
	k2 := []byte("fedcba9876543210fedcba9876543210")
	src := ssecHeaderList("X-Amz-Server-Side-Encryption-Customer-", k1)
	e.admin(t, http.MethodPut, "/ssecb/src", "customer secret", src...).must(t, "put SSE-C source")

	expect(t, "copy without the source key", e.admin(t, http.MethodPut, "/ssecb/plain", "", "X-Amz-Copy-Source", "/ssecb/src"),
		http.StatusBadRequest, "InvalidRequest")
	wrong := ssecHeaderList("X-Amz-Copy-Source-Server-Side-Encryption-Customer-", k2)
	expect(t, "copy with the wrong source key", e.admin(t, http.MethodPut, "/ssecb/plain", "", append([]string{"X-Amz-Copy-Source", "/ssecb/src"}, wrong...)...),
		http.StatusForbidden, "AccessDenied")

	right := ssecHeaderList("X-Amz-Copy-Source-Server-Side-Encryption-Customer-", k1)
	expect(t, "copy to a plain object", e.admin(t, http.MethodPut, "/ssecb/plain", "", append([]string{"X-Amz-Copy-Source", "/ssecb/src"}, right...)...),
		http.StatusOK, "")
	if got := e.admin(t, http.MethodGet, "/ssecb/plain", ""); got.body != "customer secret" {
		t.Errorf("the plain copy reads %q", got.body)
	}

	// Re-sealed under the destination's own key.
	dst := append(append([]string{"X-Amz-Copy-Source", "/ssecb/src"}, right...), ssecHeaderList("X-Amz-Server-Side-Encryption-Customer-", k2)...)
	expect(t, "copy to a re-sealed object", e.admin(t, http.MethodPut, "/ssecb/sealed", "", dst...), http.StatusOK, "")
	expect(t, "read without its key", e.admin(t, http.MethodGet, "/ssecb/sealed", ""), http.StatusBadRequest, "InvalidArgument")
	if got := e.admin(t, http.MethodGet, "/ssecb/sealed", "", ssecHeaderList("X-Amz-Server-Side-Encryption-Customer-", k2)...); got.body != "customer secret" {
		t.Errorf("the re-sealed copy reads %d %q", got.code, got.body)
	}
}

// listVersionPage is one page of ListObjectVersions.
type listVersionPage struct {
	IsTruncated         bool   `xml:"IsTruncated"`
	NextKeyMarker       string `xml:"NextKeyMarker"`
	NextVersionIdMarker string `xml:"NextVersionIdMarker"`
	Versions            []struct {
		Key       string `xml:"Key"`
		VersionId string `xml:"VersionId"`
	} `xml:"Version"`
}

// A truncated listing says where the next page starts. It never did, so SDK
// paginators stopped after the first page.
func TestListObjectVersionsPaginates(t *testing.T) {
	e := newFixEnv(t)
	e.admin(t, http.MethodPut, "/lvpage", "").must(t, "create bucket")
	e.admin(t, http.MethodPut, "/lvpage?versioning", versioningOn).must(t, "enable versioning")
	want := map[string]bool{}
	for _, k := range []string{"a", "b", "c"} {
		for _, id := range putVersions(t, e, "lvpage", k, "1", "2") {
			want[k+"/"+id] = true
		}
	}

	seen := map[string]bool{}
	marker := ""
	for page := 0; page < 10; page++ {
		r := e.admin(t, http.MethodGet, "/lvpage?versions&max-keys=2"+marker, "")
		expect(t, "list page", r, http.StatusOK, "")
		var p listVersionPage
		if err := xml.Unmarshal([]byte(r.body), &p); err != nil {
			t.Fatal(err)
		}
		for _, v := range p.Versions {
			id := v.Key + "/" + v.VersionId
			if seen[id] {
				t.Errorf("version %s listed twice", id)
			}
			seen[id] = true
		}
		if !p.IsTruncated {
			break
		}
		if p.NextKeyMarker == "" || p.NextVersionIdMarker == "" {
			t.Fatalf("a truncated page gave no next markers: %s", r.body)
		}
		marker = "&key-marker=" + p.NextKeyMarker + "&version-id-marker=" + p.NextVersionIdMarker
	}
	if len(seen) != len(want) {
		t.Errorf("paginated through %d versions, want %d", len(seen), len(want))
	}
}
