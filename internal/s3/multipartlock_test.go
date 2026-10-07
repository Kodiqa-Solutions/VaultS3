package s3

import (
	"encoding/xml"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// An upload asks for its object lock when it starts, and completing it used to
// take the lock from the completing request, which carries none. An object
// uploaded in parts with a retention or a legal hold ended up with neither, so
// it could be deleted at once. aws-cli switches to multipart above 8 MB.
func TestMultipartUploadKeepsItsObjectLock(t *testing.T) {
	e := newFixEnv(t)
	e.admin(t, http.MethodPut, "/mpl", "", "X-Amz-Bucket-Object-Lock-Enabled", "true").must(t, "create lock bucket")
	until := time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339)

	upload := func(key string, headers ...string) string {
		r := e.admin(t, http.MethodPost, "/mpl/"+key+"?uploads", "", headers...)
		expect(t, "create upload", r, http.StatusOK, "")
		var res struct {
			UploadID string `xml:"UploadId"`
		}
		if err := xml.Unmarshal([]byte(r.body), &res); err != nil || res.UploadID == "" {
			t.Fatalf("no upload id in %s", r.body)
		}
		etag := fmt.Sprintf(`"%x"`, md5Bytes("abc"))
		e.admin(t, http.MethodPut, fmt.Sprintf("/mpl/%s?partNumber=1&uploadId=%s", key, res.UploadID), "abc").must(t, "upload part")
		body := fmt.Sprintf("<CompleteMultipartUpload><Part><PartNumber>1</PartNumber><ETag>%s</ETag></Part></CompleteMultipartUpload>", etag)
		e.admin(t, http.MethodPost, fmt.Sprintf("/mpl/%s?uploadId=%s", key, res.UploadID), body).must(t, "complete")
		ids := e.versionIDs(t, "mpl", key)
		if len(ids) != 1 {
			t.Fatalf("%s: %d versions, want 1", key, len(ids))
		}
		return ids[0]
	}

	v := upload("gov", "X-Amz-Object-Lock-Mode", "GOVERNANCE", "X-Amz-Object-Lock-Retain-Until-Date", until)
	if got := e.admin(t, http.MethodGet, "/mpl/gov?retention&versionId="+v, "").body; !strings.Contains(got, "GOVERNANCE") {
		t.Errorf("retention of an object uploaded in parts: %s", got)
	}
	expect(t, "delete under GOVERNANCE without bypass", e.admin(t, http.MethodDelete, "/mpl/gov?versionId="+v, ""), http.StatusForbidden, "AccessDenied")

	// HEAD reports the lock, as AWS does.
	req, _ := http.NewRequest(http.MethodHead, e.ts.URL+"/mpl/gov?versionId="+v, nil)
	signV4Request(req, testAccessKey, testSecretKey, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.Header.Get("X-Amz-Object-Lock-Mode") != "GOVERNANCE" || resp.Header.Get("X-Amz-Object-Lock-Retain-Until-Date") != until {
		t.Errorf("HEAD lock headers: mode %q until %q, want GOVERNANCE %s", resp.Header.Get("X-Amz-Object-Lock-Mode"), resp.Header.Get("X-Amz-Object-Lock-Retain-Until-Date"), until)
	}

	h := upload("held", "X-Amz-Object-Lock-Legal-Hold", "ON")
	expect(t, "delete under legal hold", e.admin(t, http.MethodDelete, "/mpl/held?versionId="+h, ""), http.StatusForbidden, "AccessDenied")

	free := upload("free")
	expect(t, "delete of an unlocked object", e.admin(t, http.MethodDelete, "/mpl/free?versionId="+free, ""), http.StatusNoContent, "")
}
