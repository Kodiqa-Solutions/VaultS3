package s3

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func tagXML(n int, key, value func(i int) string) string {
	var b strings.Builder
	b.WriteString("<Tagging><TagSet>")
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, "<Tag><Key>%s</Key><Value>%s</Value></Tag>", key(i), value(i))
	}
	b.WriteString("</TagSet></Tagging>")
	return b.String()
}

// Object tagging as the s3-tests conformance suite checks it. Tags came back in
// a different order on every request, a tag set over the limits was stored or
// refused with the wrong code, GET and HEAD did not report the tag count, and a
// bucket without tags answered an empty set instead of NoSuchTagSet.
func TestObjectTaggingMatchesS3(t *testing.T) {
	e := newFixEnv(t)
	e.admin(t, http.MethodPut, "/tagc", "").must(t, "create bucket")
	e.admin(t, http.MethodPut, "/tagc/k", "data").must(t, "put")
	digit := func(i int) string { return fmt.Sprint(i) }

	// Ten tags come back in key order, on every read.
	e.admin(t, http.MethodPut, "/tagc/k?tagging", tagXML(10, digit, digit)).must(t, "put 10 tags")
	want := tagXML(10, digit, digit)
	want = want[len("<Tagging>") : len(want)-len("</Tagging>")]
	for i := 0; i < 20; i++ {
		got := e.admin(t, http.MethodGet, "/tagc/k?tagging", "").must(t, "get tags")
		if !strings.Contains(got.body, want) {
			t.Fatalf("tags not returned in key order on read %d: %s", i, got.body)
		}
	}

	// GET and HEAD report how many tags the object has.
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		req, _ := http.NewRequest(method, e.ts.URL+"/tagc/k", nil)
		signV4Request(req, testAccessKey, testSecretKey, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if got := resp.Header.Get("X-Amz-Tagging-Count"); got != "10" {
			t.Errorf("%s x-amz-tagging-count = %q, want 10", method, got)
		}
	}

	// Over the limits is InvalidTag, and stores nothing.
	long := func(n int) func(int) string { return func(i int) string { return strings.Repeat("a", n-1) + digit(i) } }
	expect(t, "11 tags", e.admin(t, http.MethodPut, "/tagc/k?tagging", tagXML(11, digit, digit)), http.StatusBadRequest, "InvalidTag")
	expect(t, "129 character key", e.admin(t, http.MethodPut, "/tagc/k?tagging", tagXML(1, long(129), digit)), http.StatusBadRequest, "InvalidTag")
	expect(t, "257 character value", e.admin(t, http.MethodPut, "/tagc/k?tagging", tagXML(1, digit, long(257))), http.StatusBadRequest, "InvalidTag")
	expect(t, "at the limits", e.admin(t, http.MethodPut, "/tagc/k2", "x"), http.StatusOK, "")
	expect(t, "128 and 256 characters", e.admin(t, http.MethodPut, "/tagc/k2?tagging", tagXML(10, long(128), long(256))), http.StatusOK, "")
	expect(t, "header with a 129 character key", e.admin(t, http.MethodPut, "/tagc/k3", "x", "X-Amz-Tagging", strings.Repeat("a", 129)+"=v"), http.StatusBadRequest, "InvalidTag")
	if got := e.admin(t, http.MethodGet, "/tagc/k?tagging", "").body; !strings.Contains(got, "<Key>9</Key>") || strings.Contains(got, "aaaa") {
		t.Errorf("a refused tag set changed the stored tags: %s", got)
	}

	// A bucket without tags answers NoSuchTagSet.
	expect(t, "untagged bucket", e.admin(t, http.MethodGet, "/tagc?tagging", ""), http.StatusNotFound, "NoSuchTagSet")
	bucketTags := `<Tagging><TagSet><Tag><Key>z</Key><Value>1</Value></Tag><Tag><Key>a</Key><Value>2</Value></Tag></TagSet></Tagging>`
	e.admin(t, http.MethodPut, "/tagc?tagging", bucketTags).must(t, "put bucket tags")
	if got := e.admin(t, http.MethodGet, "/tagc?tagging", "").must(t, "get bucket tags").body; !strings.Contains(got, "<Key>a</Key><Value>2</Value></Tag><Tag><Key>z</Key>") {
		t.Errorf("bucket tags not in key order: %s", got)
	}
	e.admin(t, http.MethodDelete, "/tagc?tagging", "")
	expect(t, "bucket tags deleted", e.admin(t, http.MethodGet, "/tagc?tagging", ""), http.StatusNotFound, "NoSuchTagSet")
}
