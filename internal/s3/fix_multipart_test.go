package s3

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
)

func completeXML(parts ...string) string {
	var b strings.Builder
	b.WriteString("<CompleteMultipartUpload>")
	for i := 0; i+1 < len(parts); i += 2 {
		fmt.Fprintf(&b, "<Part><PartNumber>%s</PartNumber><ETag>%s</ETag></Part>", parts[i], parts[i+1])
	}
	b.WriteString("</CompleteMultipartUpload>")
	return b.String()
}

func quotedMD5(s string) string { return fmt.Sprintf(`"%x"`, md5Bytes(s)) }

// An upload id belongs to the bucket and key it was started for. That was never
// compared, so a caller allowed only on its own key could add parts to, abort,
// or complete someone else's upload by naming it under that key.
func TestUploadIDIsBoundToItsKey(t *testing.T) {
	e := newFixEnv(t)
	e.admin(t, http.MethodPut, "/mpbind", "").must(t, "create bucket")
	victim := e.startUpload(t, "mpbind", "victim")
	e.admin(t, http.MethodPut, "/mpbind/victim?partNumber=1&uploadId="+victim, "victim data").must(t, "victim's part")

	ak, sk := e.addUser(t, "mallory", allow(`"s3:PutObject","s3:AbortMultipartUpload","s3:ListMultipartUploadParts"`, `"arn:aws:s3:::mpbind/mine"`))
	expect(t, "part into another key's upload", e.as(t, ak, sk, http.MethodPut, "/mpbind/mine?partNumber=1&uploadId="+victim, "mallory data"),
		http.StatusNotFound, "NoSuchUpload")
	expect(t, "list another key's parts", e.as(t, ak, sk, http.MethodGet, "/mpbind/mine?uploadId="+victim, ""), http.StatusNotFound, "NoSuchUpload")
	expect(t, "abort another key's upload", e.as(t, ak, sk, http.MethodDelete, "/mpbind/mine?uploadId="+victim, ""), http.StatusNotFound, "NoSuchUpload")
	expect(t, "complete another key's upload", e.as(t, ak, sk, http.MethodPost, "/mpbind/mine?uploadId="+victim, completeXML("1", quotedMD5("victim data"))),
		http.StatusNotFound, "NoSuchUpload")

	// The victim's upload is untouched and still completes with its own data.
	expect(t, "victim completes", e.admin(t, http.MethodPost, "/mpbind/victim?uploadId="+victim, completeXML("1", quotedMD5("victim data"))), http.StatusOK, "")
	if got := e.admin(t, http.MethodGet, "/mpbind/victim", ""); got.body != "victim data" {
		t.Errorf("victim object reads %q", got.body)
	}
}

// The part list is checked against what was uploaded.
func TestCompleteRejectsAPartListItDidNotUpload(t *testing.T) {
	e := newFixEnv(t)
	e.admin(t, http.MethodPut, "/mplist", "").must(t, "create bucket")
	id := e.startUpload(t, "mplist", "k")
	e.admin(t, http.MethodPut, "/mplist/k?partNumber=1&uploadId="+id, "one").must(t, "part 1")
	e.admin(t, http.MethodPut, "/mplist/k?partNumber=2&uploadId="+id, "two").must(t, "part 2")
	p1, p2 := quotedMD5("one"), quotedMD5("two")
	path := "/mplist/k?uploadId=" + id

	expect(t, "a part named twice", e.admin(t, http.MethodPost, path, completeXML("1", p1, "1", p1)), http.StatusBadRequest, "InvalidPartOrder")
	expect(t, "parts out of order", e.admin(t, http.MethodPost, path, completeXML("2", p2, "1", p1)), http.StatusBadRequest, "InvalidPartOrder")
	expect(t, "no parts", e.admin(t, http.MethodPost, path, completeXML()), http.StatusBadRequest, "MalformedXML")
	expect(t, "a wrong ETag", e.admin(t, http.MethodPost, path, completeXML("1", p1, "2", quotedMD5("not two"))), http.StatusBadRequest, "InvalidPart")
	expect(t, "nothing was stored", e.admin(t, http.MethodGet, "/mplist/k", ""), http.StatusNotFound, "NoSuchKey")

	expect(t, "the real list", e.admin(t, http.MethodPost, path, completeXML("1", p1, "2", p2)), http.StatusOK, "")
	if got := e.admin(t, http.MethodGet, "/mplist/k", ""); got.body != "onetwo" {
		t.Errorf("object reads %q", got.body)
	}
}

// A part is checked against its Content-MD5 like a whole object, and a damaged
// retry does not replace the good copy.
func TestUploadPartVerifiesItsDigest(t *testing.T) {
	e := newFixEnv(t)
	e.admin(t, http.MethodPut, "/mpdig", "").must(t, "create bucket")
	id := e.startUpload(t, "mpdig", "k")
	e.admin(t, http.MethodPut, "/mpdig/k?partNumber=1&uploadId="+id, "good part").must(t, "part 1")
	expect(t, "a retry whose Content-MD5 does not match", e.admin(t, http.MethodPut, "/mpdig/k?partNumber=1&uploadId="+id, "damaged part",
		"Content-MD5", contentMD5("good part")), http.StatusBadRequest, "BadDigest")
	expect(t, "complete with the good part", e.admin(t, http.MethodPost, "/mpdig/k?uploadId="+id, completeXML("1", quotedMD5("good part"))), http.StatusOK, "")
	if got := e.admin(t, http.MethodGet, "/mpdig/k", ""); got.body != "good part" {
		t.Errorf("object reads %q", got.body)
	}
}

// putPartFails is a multipart store whose part records cannot be written.
type putPartFails struct {
	metadata.StoreAPI
}

func (putPartFails) PutPart(string, metadata.PartInfo) error {
	return errors.New("raft: leadership lost")
}

// A part the server could not record must not be acknowledged. The error was
// discarded, so the client was told 200 and the completion later failed.
func TestUploadPartReportsAnUnrecordedPart(t *testing.T) {
	e := newFixEnv(t)
	e.admin(t, http.MethodPut, "/mprec", "").must(t, "create bucket")
	id := e.startUpload(t, "mprec", "k")
	e.h.SetLocalMultipartStore(putPartFails{e.store})
	expect(t, "part whose record fails", e.admin(t, http.MethodPut, "/mprec/k?partNumber=1&uploadId="+id, "data"), http.StatusServiceUnavailable, "SlowDown")
}

// UploadPartCopy reads the object the router authorized: the header is URL
// encoded, and was used without decoding, so "a%20b" named the object literally
// called "a%20b" while the router authorized "a b".
func TestUploadPartCopyDecodesItsSource(t *testing.T) {
	e := newFixEnv(t)
	e.admin(t, http.MethodPut, "/mpsrc", "").must(t, "create bucket")
	e.admin(t, http.MethodPut, "/mpsrc/a%20b", "the authorized one").must(t, "put a b")
	e.admin(t, http.MethodPut, "/mpsrc/a%2520b", "a bystander").must(t, "put literal a%20b")
	id := e.startUpload(t, "mpsrc", "dst")
	e.admin(t, http.MethodPut, "/mpsrc/dst?partNumber=1&uploadId="+id, "", "X-Amz-Copy-Source", "/mpsrc/a%20b").must(t, "copy part")
	expect(t, "complete", e.admin(t, http.MethodPost, "/mpsrc/dst?uploadId="+id, completeXML("1", quotedMD5("the authorized one"))), http.StatusOK, "")
	if got := e.admin(t, http.MethodGet, "/mpsrc/dst", ""); got.body != "the authorized one" {
		t.Errorf("the part was copied from %q", got.body)
	}
}

// Concurrent completions of one upload used to assemble into the same fixed
// temp file at once and interleave, and an abort could remove the parts from
// under a completion. A completion now holds the upload's lock throughout.
func TestCompleteHoldsTheUploadLock(t *testing.T) {
	e := newFixEnv(t)
	e.admin(t, http.MethodPut, "/mplock", "").must(t, "create bucket")
	id := e.startUpload(t, "mplock", "k")
	e.admin(t, http.MethodPut, "/mplock/k?partNumber=1&uploadId="+id, "data").must(t, "part 1")

	unlock := lockUpload(id)
	done := make(chan result, 1)
	go func() {
		done <- e.admin(t, http.MethodPost, "/mplock/k?uploadId="+id, completeXML("1", quotedMD5("data")))
	}()
	select {
	case r := <-done:
		unlock()
		t.Fatalf("the completion ran while another request held the upload: %d %s", r.code, r.body)
	case <-time.After(300 * time.Millisecond):
	}
	unlock()
	expect(t, "the completion once the lock is free", <-done, http.StatusOK, "")
}

// Each completion assembles into a temp file of its own. The fixed name meant
// two completions wrote one file, and anything already at that name broke it.
func TestCompleteAssemblesIntoItsOwnTempFile(t *testing.T) {
	e := newFixEnv(t)
	e.admin(t, http.MethodPut, "/mptmp", "").must(t, "create bucket")
	id := e.startUpload(t, "mptmp", "k")
	e.admin(t, http.MethodPut, "/mptmp/k?partNumber=1&uploadId="+id, "data").must(t, "part 1")
	if err := os.Mkdir(filepath.Join(e.h.objects.multipartDir(id), "assembled.tmp"), 0o755); err != nil {
		t.Fatal(err)
	}
	expect(t, "complete", e.admin(t, http.MethodPost, "/mptmp/k?uploadId="+id, completeXML("1", quotedMD5("data"))), http.StatusOK, "")
}
