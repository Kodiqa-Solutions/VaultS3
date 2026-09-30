package s3

import (
	"encoding/xml"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// CompleteMultipartUpload builds the object's metadata from the upload record,
// not from the request that started the upload, so every header
// CreateMultipartUpload accepts has to be carried on that record. It carried only
// the content type, so an object uploaded in parts silently lost its tags, its
// user metadata and all four content headers. aws-cli and the SDKs switch to
// multipart above a few megabytes on their own, so this hit ordinary uploads of
// large files with no opt-in.
//
// This asserts against a single-shot PUT of the same headers rather than against
// literals: the two paths have to agree, and pinning them to each other is what
// stops one drifting from the other later.
func TestMultipartPreservesObjectHeaders(t *testing.T) {
	ts := newIntegrationServer(t)
	bucket := "mp-hdr-bucket"

	headers := map[string]string{
		"Content-Type":                    "text/csv",
		"X-Amz-Tagging":                   "run=nightly%20batch&owner=analytics",
		"X-Amz-Meta-Owner":                "analytics",
		"X-Amz-Meta-Run":                  "42",
		"Content-Encoding":                "gzip",
		"Content-Disposition":             `attachment; filename="report.csv"`,
		"Cache-Control":                   "max-age=99",
		"Content-Language":                "en-GB",
		"X-Amz-Website-Redirect-Location": "/elsewhere",
	}

	resp := doSigned(t, http.MethodPut, ts.URL+"/"+bucket, nil)
	resp.Body.Close()

	// The reference: one-shot PUT with the same headers.
	resp = doSignedWithHeaders(t, http.MethodPut, ts.URL+"/"+bucket+"/single", []byte("data"), headers)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("single-shot PUT: %d", resp.StatusCode)
	}

	// The same headers through a multipart upload.
	resp = doSignedWithHeaders(t, http.MethodPost, ts.URL+"/"+bucket+"/multi?uploads", nil, headers)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("CreateMultipartUpload: %d", resp.StatusCode)
	}
	var init initiateResult
	if err := xml.NewDecoder(resp.Body).Decode(&init); err != nil {
		t.Fatalf("decode initiate: %v", err)
	}
	resp.Body.Close()

	resp = doSigned(t, http.MethodPut,
		fmt.Sprintf("%s/%s/multi?uploadId=%s&partNumber=1", ts.URL, bucket, init.UploadID),
		[]byte(strings.Repeat("x", 1024)))
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("UploadPart: %d", resp.StatusCode)
	}
	etag := resp.Header.Get("ETag")

	resp = doSigned(t, http.MethodPost,
		fmt.Sprintf("%s/%s/multi?uploadId=%s", ts.URL, bucket, init.UploadID),
		[]byte(fmt.Sprintf(`<CompleteMultipartUpload><Part><PartNumber>1</PartNumber><ETag>%s</ETag></Part></CompleteMultipartUpload>`, etag)))
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("CompleteMultipartUpload: %d", resp.StatusCode)
	}

	// Compare what a client can actually observe: HEAD for the headers, ?tagging for
	// the tag set.
	headSingle := doSigned(t, http.MethodHead, ts.URL+"/"+bucket+"/single", nil)
	headSingle.Body.Close()
	headMulti := doSigned(t, http.MethodHead, ts.URL+"/"+bucket+"/multi", nil)
	headMulti.Body.Close()

	for _, name := range []string{
		"Content-Type",
		"Content-Encoding",
		"Content-Disposition",
		"Cache-Control",
		"Content-Language",
		"X-Amz-Website-Redirect-Location",
		"X-Amz-Meta-Owner",
		"X-Amz-Meta-Run",
	} {
		want := headSingle.Header.Get(name)
		if want == "" {
			t.Errorf("%s: the single-shot PUT returned nothing, so this comparison proves nothing", name)
			continue
		}
		if got := headMulti.Header.Get(name); got != want {
			t.Errorf("%s: multipart %q, single-shot %q", name, got, want)
		}
	}

	tagsOf := func(key string) string {
		resp := doSigned(t, http.MethodGet, ts.URL+"/"+bucket+"/"+key+"?tagging", nil)
		return readBody(t, resp)
	}
	singleTags, multiTags := tagsOf("single"), tagsOf("multi")

	// The tag value was percent-encoded on the wire, so this also covers issue #61
	// on the multipart path.
	for _, want := range []string{"nightly batch", "analytics"} {
		if !strings.Contains(singleTags, want) {
			t.Fatalf("single-shot tags missing %q, so the comparison proves nothing: %s", want, singleTags)
		}
		if !strings.Contains(multiTags, want) {
			t.Errorf("multipart tags missing %q: %s", want, multiTags)
		}
	}
	if strings.Contains(multiTags, "%20") {
		t.Errorf("multipart tags still percent-encoded: %s", multiTags)
	}
}

// A tag set the server will not store should fail the request that declares it,
// not the completion after every part has been uploaded.
func TestCreateMultipartUploadRejectsBadTagSet(t *testing.T) {
	ts := newIntegrationServer(t)
	bucket := "mp-badtag-bucket"

	resp := doSigned(t, http.MethodPut, ts.URL+"/"+bucket, nil)
	resp.Body.Close()

	resp = doSignedWithHeaders(t, http.MethodPost, ts.URL+"/"+bucket+"/obj?uploads", nil,
		map[string]string{"X-Amz-Tagging": "k=%ZZ"})
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400, got %d: %s", resp.StatusCode, body)
	}
	if !strings.Contains(body, "InvalidArgument") {
		t.Errorf("expected InvalidArgument, got: %s", body)
	}
}
