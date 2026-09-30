package s3

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// x-amz-tagging-directive governs the tag set on a copy, and x-amz-metadata-directive
// governs everything else. They are independent, so all four combinations are legal
// and each means something different. VaultS3 decided tags inside the metadata
// branch, which got both halves wrong at once:
//
//   - "replace the metadata" with no tagging header DISCARDED the source's tags.
//     That is `aws s3 cp --metadata-directive REPLACE`, an ordinary command, and
//     the tags were gone with nothing said.
//   - a tagging directive of REPLACE did nothing unless the metadata directive also
//     said REPLACE, so asking only for new tags copied the old ones instead.
func TestCopyObjectTaggingDirectiveIsIndependent(t *testing.T) {
	ts := newIntegrationServer(t)
	bucket := "copy-tagdir-bucket"

	resp := doSigned(t, http.MethodPut, ts.URL+"/"+bucket, nil)
	resp.Body.Close()

	resp = doSignedWithHeaders(t, http.MethodPut, ts.URL+"/"+bucket+"/src", []byte("payload"),
		map[string]string{
			"X-Amz-Tagging":    "src=original",
			"X-Amz-Meta-Owner": "from-source",
			"Content-Type":     "text/plain",
		})
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("seed PUT: %d", resp.StatusCode)
	}

	const fromSource = "src=original"
	const fromHeader = "new=hdr tag" // sent percent-encoded, so this also covers #61

	cases := []struct {
		name       string
		metaDir    string
		tagDir     string
		sendTags   bool
		wantTag    string
		wantAbsent string
	}{
		{"both absent keeps everything", "", "", false, fromSource, "new"},
		{"tags replaced, metadata copied", "", "REPLACE", true, fromHeader, "src"},
		{"metadata replaced, tags untouched", "REPLACE", "", false, fromSource, "new"},
		{"both replaced", "REPLACE", "REPLACE", true, fromHeader, "src"},
		{"metadata replaced, tags explicitly copied", "REPLACE", "COPY", false, fromSource, "new"},
	}

	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dst := fmt.Sprintf("dst%d", i)
			headers := map[string]string{
				"X-Amz-Copy-Source": "/" + bucket + "/src",
			}
			if tc.metaDir != "" {
				headers["X-Amz-Metadata-Directive"] = tc.metaDir
				headers["Content-Type"] = "text/plain"
			}
			if tc.tagDir != "" {
				headers["X-Amz-Tagging-Directive"] = tc.tagDir
			}
			if tc.sendTags {
				headers["X-Amz-Tagging"] = "new=hdr%20tag"
			}

			resp := doSignedWithHeaders(t, http.MethodPut, ts.URL+"/"+bucket+"/"+dst, nil, headers)
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("copy: %d", resp.StatusCode)
			}

			resp = doSigned(t, http.MethodGet, ts.URL+"/"+bucket+"/"+dst+"?tagging", nil)
			body := readBody(t, resp)

			key, value, _ := strings.Cut(tc.wantTag, "=")
			if !strings.Contains(body, "<Key>"+key+"</Key>") || !strings.Contains(body, "<Value>"+value+"</Value>") {
				t.Errorf("expected tag %q, got: %s", tc.wantTag, body)
			}
			if strings.Contains(body, "<Key>"+tc.wantAbsent+"</Key>") {
				t.Errorf("tag %q should not be present, got: %s", tc.wantAbsent, body)
			}
			if strings.Contains(body, "%20") {
				t.Errorf("tag value still percent-encoded: %s", body)
			}
		})
	}
}
