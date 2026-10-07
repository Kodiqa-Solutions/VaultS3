package s3

import (
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
	"github.com/Kodiqa-Solutions/VaultS3/internal/storage"
)

func retention(mode string, until time.Time) string {
	return fmt.Sprintf(`<Retention><Mode>%s</Mode><RetainUntilDate>%s</RetainUntilDate></Retention>`, mode, until.UTC().Format(time.RFC3339))
}

// COMPLIANCE can only be extended. It could be turned into GOVERNANCE with the
// same date, and GOVERNANCE could then be removed.
func TestComplianceRetentionCannotBeDowngraded(t *testing.T) {
	e := newFixEnv(t)
	e.admin(t, http.MethodPut, "/lockb", "").must(t, "create bucket")
	e.admin(t, http.MethodPut, "/lockb/k", "kept").must(t, "put")
	until := time.Now().Add(2 * time.Hour)
	e.admin(t, http.MethodPut, "/lockb/k?retention", retention("COMPLIANCE", until)).must(t, "set COMPLIANCE")

	expect(t, "COMPLIANCE to GOVERNANCE, same date", e.admin(t, http.MethodPut, "/lockb/k?retention", retention("GOVERNANCE", until)),
		http.StatusForbidden, "AccessDenied")
	expect(t, "COMPLIANCE to GOVERNANCE, even with the bypass", e.admin(t, http.MethodPut, "/lockb/k?retention", retention("GOVERNANCE", until.Add(time.Hour)),
		"X-Amz-Bypass-Governance-Retention", "true"), http.StatusForbidden, "AccessDenied")
	expect(t, "COMPLIANCE extended", e.admin(t, http.MethodPut, "/lockb/k?retention", retention("COMPLIANCE", until.Add(time.Hour))),
		http.StatusOK, "")
}

// Shortening or removing GOVERNANCE needs the bypass header AND the
// s3:BypassGovernanceRetention permission. Neither was required.
func TestGovernanceRetentionNeedsAnAuthorizedBypass(t *testing.T) {
	e := newFixEnv(t)
	e.admin(t, http.MethodPut, "/govb", "").must(t, "create bucket")
	e.admin(t, http.MethodPut, "/govb/k", "kept").must(t, "put")
	until := time.Now().Add(2 * time.Hour)
	e.admin(t, http.MethodPut, "/govb/k?retention", retention("GOVERNANCE", until)).must(t, "set GOVERNANCE")

	ak, sk := e.addUser(t, "ret", allow(`"s3:PutObjectRetention","s3:GetObjectRetention"`, `"*"`))
	shorter := retention("GOVERNANCE", until.Add(-time.Hour))
	expect(t, "shorten, no bypass", e.as(t, ak, sk, http.MethodPut, "/govb/k?retention", shorter), http.StatusForbidden, "AccessDenied")
	expect(t, "shorten, bypass header without the permission", e.as(t, ak, sk, http.MethodPut, "/govb/k?retention", shorter,
		"X-Amz-Bypass-Governance-Retention", "true"), http.StatusForbidden, "AccessDenied")
	expect(t, "extend, no bypass needed", e.as(t, ak, sk, http.MethodPut, "/govb/k?retention", retention("GOVERNANCE", until.Add(time.Hour))),
		http.StatusOK, "")

	ak2, sk2 := e.addUser(t, "boss", allow(`"s3:PutObjectRetention","s3:BypassGovernanceRetention"`, `"*"`))
	expect(t, "shorten with an authorized bypass", e.as(t, ak2, sk2, http.MethodPut, "/govb/k?retention", shorter,
		"X-Amz-Bypass-Governance-Retention", "true"), http.StatusOK, "")
}

// The bypass header on a delete was honoured with no permission check, so
// s3:DeleteObject alone deleted through a GOVERNANCE lock.
func TestGovernanceBypassOnDeleteNeedsThePermission(t *testing.T) {
	e := newFixEnv(t)
	e.admin(t, http.MethodPut, "/gdel", "").must(t, "create bucket")
	e.admin(t, http.MethodPut, "/gdel/k", "kept").must(t, "put")
	e.admin(t, http.MethodPut, "/gdel/k?retention", retention("GOVERNANCE", time.Now().Add(time.Hour))).must(t, "lock")

	ak, sk := e.addUser(t, "deleter", allow(`"s3:DeleteObject","s3:GetObject"`, `"*"`))
	expect(t, "delete with the header but not the permission", e.as(t, ak, sk, http.MethodDelete, "/gdel/k", "",
		"X-Amz-Bypass-Governance-Retention", "true"), http.StatusForbidden, "AccessDenied")
	expect(t, "the object", e.admin(t, http.MethodGet, "/gdel/k", ""), http.StatusOK, "")

	ak2, sk2 := e.addUser(t, "bypasser", allow(`"s3:DeleteObject","s3:BypassGovernanceRetention"`, `"*"`))
	expect(t, "delete with an authorized bypass", e.as(t, ak2, sk2, http.MethodDelete, "/gdel/k", "",
		"X-Amz-Bypass-Governance-Retention", "true"), http.StatusNoContent, "")
}

// An overwrite replaces a locked object as surely as a delete removes it. No
// write path checked the lock, so a COMPLIANCE object was replaced by any PUT,
// copy or multipart completion.
func TestOverwritingALockedObjectIsRefused(t *testing.T) {
	e := newFixEnv(t)
	e.admin(t, http.MethodPut, "/owb", "").must(t, "create bucket")
	e.admin(t, http.MethodPut, "/owb/locked", "original", "X-Amz-Object-Lock-Mode", "COMPLIANCE",
		"X-Amz-Object-Lock-Retain-Until-Date", time.Now().Add(time.Hour).UTC().Format(time.RFC3339)).must(t, "put locked")
	e.admin(t, http.MethodPut, "/owb/src", "replacement").must(t, "put source")
	e.admin(t, http.MethodPut, "/owb/free", "free").must(t, "put bystander")

	expect(t, "PUT over it", e.admin(t, http.MethodPut, "/owb/locked", "replacement"), http.StatusForbidden, "AccessDenied")
	expect(t, "copy over it", e.admin(t, http.MethodPut, "/owb/locked", "", "X-Amz-Copy-Source", "/owb/src"), http.StatusForbidden, "AccessDenied")

	id := e.startUpload(t, "owb", "locked")
	e.admin(t, http.MethodPut, "/owb/locked?partNumber=1&uploadId="+id, "replacement").must(t, "upload part")
	etag := fmt.Sprintf(`"%x"`, md5sum("replacement"))
	complete := fmt.Sprintf(`<CompleteMultipartUpload><Part><PartNumber>1</PartNumber><ETag>%s</ETag></Part></CompleteMultipartUpload>`, etag)
	expect(t, "multipart over it", e.admin(t, http.MethodPost, "/owb/locked?uploadId="+id, complete), http.StatusForbidden, "AccessDenied")

	if got := e.admin(t, http.MethodGet, "/owb/locked", ""); got.body != "original" {
		t.Errorf("the locked object now reads %q", got.body)
	}
	// The refusal is about the lock, not about overwriting in general.
	expect(t, "PUT over an unlocked bystander", e.admin(t, http.MethodPut, "/owb/free", "changed"), http.StatusOK, "")
}

// A POST form upload is a write like any other.
func TestFormUploadCannotReplaceALockedObject(t *testing.T) {
	e := newFixEnv(t)
	e.admin(t, http.MethodPut, "/fpost", "").must(t, "create bucket")
	e.admin(t, http.MethodPut, "/fpost/locked", "original", "X-Amz-Object-Lock-Legal-Hold", "ON").must(t, "put locked")
	r := e.formUpload(t, "fpost", "locked", "replacement")
	expect(t, "form upload over a legal hold", r, http.StatusForbidden, "AccessDenied")
	if got := e.admin(t, http.MethodGet, "/fpost/locked", ""); got.body != "original" {
		t.Errorf("the locked object now reads %q", got.body)
	}
}

// formUpload sends a signed multipart/form-data POST upload.
func (e *fixEnv) formUpload(t *testing.T, bucket, key, content string) result {
	t.Helper()
	var buf strings.Builder
	body, ct := formBody(&buf, key, content)
	return e.admin(t, http.MethodPost, "/"+bucket, body, "Content-Type", ct)
}

// formBody builds a form upload of one file under key.
func formBody(buf *strings.Builder, key, content string) (string, string) {
	mw := multipart.NewWriter(buf)
	mw.WriteField("key", key)
	fw, _ := mw.CreateFormFile("file", "f.txt")
	io.WriteString(fw, content)
	mw.Close()
	return buf.String(), mw.FormDataContentType()
}

// On a suspended bucket a delete destroys the null version, so its lock
// applies. It was removed with no lock check at all.
func TestSuspendedDeleteHonoursTheLock(t *testing.T) {
	e := newFixEnv(t)
	e.admin(t, http.MethodPut, "/susb", "").must(t, "create bucket")
	e.admin(t, http.MethodPut, "/susb?versioning", versioningOn).must(t, "enable versioning")
	e.admin(t, http.MethodPut, "/susb?versioning", `<VersioningConfiguration><Status>Suspended</Status></VersioningConfiguration>`).must(t, "suspend")
	e.admin(t, http.MethodPut, "/susb/k", "held", "X-Amz-Object-Lock-Legal-Hold", "ON").must(t, "put held null version")
	expect(t, "delete", e.admin(t, http.MethodDelete, "/susb/k", ""), http.StatusForbidden, "AccessDenied")
	if got := e.admin(t, http.MethodGet, "/susb/k", ""); got.body != "held" {
		t.Errorf("the held object reads %d %q", got.code, got.body)
	}
}

// Deleting the null version deleted its bytes BEFORE checking its lock, so the
// refusal left the record of an object whose data was gone.
func TestRefusedNullVersionDeleteKeepsTheBytes(t *testing.T) {
	e := newFixEnv(t)
	e.admin(t, http.MethodPut, "/nullb", "").must(t, "create bucket")
	e.admin(t, http.MethodPut, "/nullb/k", "pre-versioning", "X-Amz-Object-Lock-Legal-Hold", "ON").must(t, "put before versioning")
	e.admin(t, http.MethodPut, "/nullb?versioning", versioningOn).must(t, "enable versioning")
	del := e.admin(t, http.MethodDelete, "/nullb/k", "")
	expect(t, "delete marker", del, http.StatusNoContent, "")

	expect(t, "delete the null version", e.admin(t, http.MethodDelete, "/nullb/k?versionId=null", ""), http.StatusForbidden, "AccessDenied")
	expect(t, "read the null version", e.admin(t, http.MethodGet, "/nullb/k?versionId=null", ""), http.StatusOK, "")
}

// lockReadFails answers every read of one key's metadata with an error that is
// not "not found", the way a briefly unreachable store does.
type lockReadFails struct {
	metadata.StoreAPI
	key string
}

func (s lockReadFails) GetObjectMeta(bucket, key string) (*metadata.ObjectMeta, error) {
	if key == s.key {
		return nil, errors.New("i/o timeout")
	}
	return s.StoreAPI.GetObjectMeta(bucket, key)
}

// A lock that cannot be read must not count as no lock. Any error used to mean
// "no such object, allow", so a locked object could be deleted while the store
// was having trouble.
func TestUnreadableLockRefusesTheDelete(t *testing.T) {
	e := newFixEnv(t)
	e.admin(t, http.MethodPut, "/lrb", "").must(t, "create bucket")
	e.admin(t, http.MethodPut, "/lrb/k", "held", "X-Amz-Object-Lock-Legal-Hold", "ON").must(t, "put held")

	fs, _ := storage.NewFileSystem(e.dataDir)
	flaky := lockReadFails{StoreAPI: e.store, key: "k"}
	h := NewHandler(flaky, fs, NewAuthenticator(testAccessKey, testSecretKey, e.store, nil, nil), false, "", nil)
	ts := httptest.NewServer(h)
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodDelete, ts.URL+"/lrb/k", nil)
	signV4Request(req, testAccessKey, testSecretKey, nil)
	expect(t, "delete while the lock cannot be read", send(t, req), http.StatusServiceUnavailable, "ServiceUnavailable")
	if got := e.admin(t, http.MethodGet, "/lrb/k", ""); got.body != "held" {
		t.Errorf("the held object reads %d %q", got.code, got.body)
	}
}

// FIFO eviction is a delete, so it skips what object lock protects.
func TestFifoEvictionSkipsLockedObjects(t *testing.T) {
	e := newFixEnv(t)
	e.admin(t, http.MethodPut, "/fifob", "").must(t, "create bucket")
	e.admin(t, http.MethodPut, "/fifob/a", "held", "X-Amz-Object-Lock-Legal-Hold", "ON").must(t, "put held")
	e.admin(t, http.MethodPut, "/fifob/b", "free").must(t, "put free")
	e.h.objects.fifoEvict("fifob", 1, 0)
	if got := e.admin(t, http.MethodGet, "/fifob/a", ""); got.body != "held" {
		t.Errorf("eviction removed an object under legal hold: %d %q", got.code, got.body)
	}
	expect(t, "the unlocked object was the one evicted", e.admin(t, http.MethodGet, "/fifob/b", ""), http.StatusNotFound, "NoSuchKey")
}

func md5sum(s string) []byte { return md5Bytes(s) }

// An overwrite that fails its checks must leave the previous object intact. The
// body streamed into the engine before the checks ran, the engine had already
// renamed the new bytes over the old file, and the rejection then deleted that
// file while the old metadata stayed: the object was gone.
func TestRejectedOverwriteKeepsTheOldObject(t *testing.T) {
	e := newFixEnv(t)
	e.admin(t, http.MethodPut, "/rejb", "").must(t, "create bucket")
	e.admin(t, http.MethodPut, "/rejb/k", "the original").must(t, "put original")

	check := func(what string) {
		t.Helper()
		got := e.admin(t, http.MethodGet, "/rejb/k", "")
		if got.code != http.StatusOK || got.body != "the original" {
			t.Fatalf("after %s the object reads %d %q", what, got.code, got.body)
		}
	}

	expect(t, "overwrite with a wrong Content-MD5", e.admin(t, http.MethodPut, "/rejb/k", "a replacement",
		"Content-MD5", contentMD5("something else")), http.StatusBadRequest, "BadDigest")
	check("a bad Content-MD5")

	wrong := base32Sum(crc32.ChecksumIEEE([]byte("something else")))
	expect(t, "overwrite with a wrong x-amz-checksum-crc32", e.admin(t, http.MethodPut, "/rejb/k", "a replacement",
		"X-Amz-Checksum-Crc32", wrong), http.StatusBadRequest, "BadDigest")
	check("a bad CRC32")

	// The ETag still describes the bytes that are there.
	req, _ := http.NewRequest(http.MethodHead, e.ts.URL+"/rejb/k", nil)
	signV4Request(req, testAccessKey, testSecretKey, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if want := fmt.Sprintf(`"%x"`, md5Bytes("the original")); resp.Header.Get("ETag") != want {
		t.Errorf("ETag = %s, want %s", resp.Header.Get("ETag"), want)
	}
}

// The same for a body that takes the bucket over its quota while streaming.
func TestOverQuotaOverwriteKeepsTheOldObject(t *testing.T) {
	e := newFixEnv(t)
	e.admin(t, http.MethodPut, "/quob", "").must(t, "create bucket")
	e.admin(t, http.MethodPut, "/quob/k", "the original").must(t, "put original")
	if err := e.store.UpdateBucketQuota("quob", 64, 0); err != nil {
		t.Fatal(err)
	}
	big := strings.Repeat("x", 4096)
	req, _ := http.NewRequest(http.MethodPut, e.ts.URL+"/quob/k", nil)
	signV4Request(req, testAccessKey, testSecretKey, []byte(big))
	// Chunked, so no length is declared and the pre-write check cannot see it.
	req.ContentLength = -1
	req.Body = io.NopCloser(strings.NewReader(big))
	expect(t, "chunked overwrite over the quota", send(t, req), http.StatusForbidden, "QuotaExceeded")
	if got := e.admin(t, http.MethodGet, "/quob/k", ""); got.body != "the original" {
		t.Fatalf("the object reads %d %q", got.code, got.body)
	}
}
