package s3

import (
	"encoding/xml"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

const websiteOn = `<WebsiteConfiguration><IndexDocument><Suffix>index.html</Suffix></IndexDocument></WebsiteConfiguration>`

// The website read the plain path on the local disk, which a versioned bucket
// never writes, so every page of a versioned website was a 404.
func TestWebsiteServesAVersionedBucket(t *testing.T) {
	e := newFixEnv(t)
	e.admin(t, http.MethodPut, "/vsite", "").must(t, "create bucket")
	e.admin(t, http.MethodPut, "/vsite?versioning", versioningOn).must(t, "enable versioning")
	e.admin(t, http.MethodPut, "/vsite/index.html", "<h1>v2</h1>", "Content-Type", "text/html").must(t, "page")
	e.admin(t, http.MethodPut, "/vsite?website", websiteOn).must(t, "website")
	if r := anonCall(t, e.ts, http.MethodGet, "/vsite/"); r.code != http.StatusOK || r.body != "<h1>v2</h1>" {
		t.Errorf("versioned website page: %d %q", r.code, r.body)
	}
	// A page hidden behind a delete marker is gone from the website too.
	e.admin(t, http.MethodDelete, "/vsite/index.html", "")
	if r := anonCall(t, e.ts, http.MethodGet, "/vsite/"); r.code != http.StatusNotFound {
		t.Errorf("deleted page: %d %q", r.code, r.body)
	}
}

// A website visitor cannot present a customer key, and the website used to hand
// out the stored ciphertext as the page.
func TestWebsiteDoesNotServeSSECObjects(t *testing.T) {
	e := newFixEnv(t)
	e.admin(t, http.MethodPut, "/csite", "").must(t, "create bucket")
	key := []byte("0123456789abcdef0123456789abcdef")
	e.admin(t, http.MethodPut, "/csite/index.html", "<h1>secret</h1>", ssecHeaderList("X-Amz-Server-Side-Encryption-Customer-", key)...).must(t, "page")
	e.admin(t, http.MethodPut, "/csite?website", websiteOn).must(t, "website")
	r := anonCall(t, e.ts, http.MethodGet, "/csite/")
	if r.code != http.StatusForbidden {
		t.Errorf("SSE-C page served to the website: %d (%d bytes)", r.code, len(r.body))
	}
}

// An empty <Key></Key> in a multi-object delete reached the engine as a delete
// of "", and was reported as deleted.
func TestBatchDeleteRefusesAnEmptyKey(t *testing.T) {
	e := newFixEnv(t)
	e.admin(t, http.MethodPut, "/bdempty", "").must(t, "create bucket")
	e.admin(t, http.MethodPut, "/bdempty/gone", "x").must(t, "put gone")
	e.admin(t, http.MethodPut, "/bdempty/keep", "x").must(t, "put bystander")
	body := `<Delete><Object><Key></Key></Object><Object><Key>gone</Key></Object></Delete>`
	r := e.admin(t, http.MethodPost, "/bdempty?delete", body, "Content-MD5", contentMD5(body))
	expect(t, "multi-object delete", r, http.StatusOK, "")
	var res deleteResult
	if err := xml.Unmarshal([]byte(r.body), &res); err != nil {
		t.Fatal(err)
	}
	if len(res.Errors) != 1 || res.Errors[0].Code != "UserKeyMustBeSpecified" {
		t.Errorf("errors = %+v, want one UserKeyMustBeSpecified", res.Errors)
	}
	if len(res.Deleted) != 1 || res.Deleted[0].Key != "gone" {
		t.Errorf("deleted = %+v, want only gone", res.Deleted)
	}
	expect(t, "the bystander", e.admin(t, http.MethodGet, "/bdempty/keep", ""), http.StatusOK, "")
}

// RFC 7232: If-Match decides over If-Unmodified-Since, and If-None-Match over
// If-Modified-Since. The date conditions were applied on top.
func TestConditionalGetPrecedence(t *testing.T) {
	e := newFixEnv(t)
	e.admin(t, http.MethodPut, "/condb", "").must(t, "create bucket")
	e.admin(t, http.MethodPut, "/condb/k", "0123456789").must(t, "put")
	etag := quotedMD5("0123456789")
	past := time.Now().Add(-48 * time.Hour).UTC().Format(http.TimeFormat)
	future := time.Now().Add(48 * time.Hour).UTC().Format(http.TimeFormat)

	expect(t, "If-Match true, If-Unmodified-Since false", e.admin(t, http.MethodGet, "/condb/k", "",
		"If-Match", etag, "If-Unmodified-Since", past), http.StatusOK, "")
	expect(t, "If-None-Match false, If-Modified-Since false", e.admin(t, http.MethodGet, "/condb/k", "",
		"If-None-Match", `"something-else"`, "If-Modified-Since", future), http.StatusOK, "")
	// Each condition alone still applies.
	if r := e.admin(t, http.MethodGet, "/condb/k", "", "If-Unmodified-Since", past); r.code != http.StatusPreconditionFailed {
		t.Errorf("If-Unmodified-Since alone: %d", r.code)
	}
	if r := e.admin(t, http.MethodGet, "/condb/k", "", "If-None-Match", etag); r.code != http.StatusNotModified {
		t.Errorf("If-None-Match alone: %d", r.code)
	}
}

// RFC 7233: an invalid or multi-range Range is ignored and the whole object
// served. Only a valid range that starts past the end is a 416.
func TestRangeThatCannotBeUsedIsIgnored(t *testing.T) {
	e := newFixEnv(t)
	e.admin(t, http.MethodPut, "/rangeb", "").must(t, "create bucket")
	e.admin(t, http.MethodPut, "/rangeb/k", "0123456789").must(t, "put")
	for _, rg := range []string{"bytes=5-2", "bytes=0-1,3-4", "items=0-1"} {
		r := e.admin(t, http.MethodGet, "/rangeb/k", "", "Range", rg)
		if r.code != http.StatusOK || r.body != "0123456789" {
			t.Errorf("Range %q: %d %q, want the whole object", rg, r.code, r.body)
		}
	}
	expect(t, "a range past the end", e.admin(t, http.MethodGet, "/rangeb/k", "", "Range", "bytes=100-"),
		http.StatusRequestedRangeNotSatisfiable, "InvalidRange")
	if r := e.admin(t, http.MethodGet, "/rangeb/k", "", "Range", "bytes=2-4"); r.code != http.StatusPartialContent || r.body != "234" {
		t.Errorf("a valid range: %d %q", r.code, r.body)
	}
}

// The engine's ETag is already quoted, and the form upload quoted it again.
func TestFormUploadETagIsQuotedOnce(t *testing.T) {
	e := newFixEnv(t)
	e.admin(t, http.MethodPut, "/formb", "").must(t, "create bucket")
	var buf strings.Builder
	body, ct := formBody(&buf, "k", "content")
	req, _ := http.NewRequest(http.MethodPost, e.ts.URL+"/formb", strings.NewReader(body))
	req.Header.Set("Content-Type", ct)
	req.ContentLength = int64(len(body))
	signV4Request(req, testAccessKey, testSecretKey, []byte(body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got, want := resp.Header.Get("ETag"), quotedMD5("content"); got != want {
		t.Errorf("ETag = %s, want %s", got, want)
	}
}

// A presigned PUT limited to a size measured only the declared length, so a
// chunked body (no length) or an aws-chunked one declaring a small decoded
// length got past it with any amount of data.
func TestPresignedMaxSizeIsEnforcedOnTheBody(t *testing.T) {
	e := newFixEnv(t)
	e.admin(t, http.MethodPut, "/psize", "").must(t, "create bucket")
	u := GeneratePresignedPutURL(e.ts.URL, "psize", "k", testAccessKey, testSecretKey, testRegion, time.Minute,
		&PresignedUploadRestrictions{MaxSize: 10})
	big := strings.Repeat("x", 1000)

	req, _ := http.NewRequest(http.MethodPut, u, io.NopCloser(strings.NewReader(big)))
	req.ContentLength = -1 // chunked: no declared length
	expect(t, "chunked body on a size-limited URL", send(t, req), http.StatusForbidden, "AccessDenied")

	framed := awsChunkedEncode([]byte(big), false)
	req, _ = http.NewRequest(http.MethodPut, u, strings.NewReader(framed))
	req.ContentLength = int64(len(framed))
	req.Header.Set("Content-Encoding", "aws-chunked")
	req.Header.Set("X-Amz-Decoded-Content-Length", "5")
	expect(t, "aws-chunked body declaring 5 bytes", send(t, req), http.StatusBadRequest, "EntityTooLarge")
	expect(t, "nothing was stored", e.admin(t, http.MethodGet, "/psize/k", ""), http.StatusNotFound, "NoSuchKey")

	req, _ = http.NewRequest(http.MethodPut, u, strings.NewReader("small"))
	expect(t, "a body within the limit", send(t, req), http.StatusOK, "")
}
