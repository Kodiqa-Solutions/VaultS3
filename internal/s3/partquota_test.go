package s3

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

// Regression (5.0.11): parts live outside the bucket until the upload
// completes, so the bucket's size never counted them and a part was not held
// to the quota at all. Parts nobody completed could fill the disk whatever
// the quota. Every uploaded part now counts against it.
func TestMultipartPartsAreHeldToTheBucketQuota(t *testing.T) {
	e := newFixEnv(t)
	e.admin(t, http.MethodPut, "/pquota", "").must(t, "bucket")
	if err := e.store.UpdateBucketQuota("pquota", 1<<20, 0); err != nil {
		t.Fatal(err)
	}
	part := strings.Repeat("p", 600<<10)
	put := func(key, uploadID string, n int) result {
		return e.admin(t, http.MethodPut, fmt.Sprintf("/pquota/%s?partNumber=%d&uploadId=%s", key, n, uploadID), part)
	}
	refused := func(what string, r result) {
		t.Helper()
		if r.code != http.StatusForbidden || !strings.Contains(r.body, "QuotaExceeded") {
			t.Fatalf("%s: %d %s, want 403 QuotaExceeded", what, r.code, r.body)
		}
	}

	up := e.startUpload(t, "pquota", "k")
	put("k", up, 1).must(t, "first part, within the quota")
	refused("second part past the quota", put("k", up, 2))
	put("k", up, 1).must(t, "the first part again: its old copy does not count")

	other := e.startUpload(t, "pquota", "k2")
	refused("a part of another upload, past the quota with the first upload's parts", put("k2", other, 1))

	// A part of unknown length is stopped as it is read.
	req, _ := http.NewRequest(http.MethodPut, e.ts.URL+"/pquota/k?partNumber=2&uploadId="+up, nil)
	signV4Request(req, testAccessKey, testSecretKey, []byte(part))
	req.Body = io.NopCloser(bytes.NewReader([]byte(part)))
	req.ContentLength = -1
	refused("a part of unknown length past the quota", send(t, req))
}

// A copied part is held to the quota too, objects already stored count
// against it, and a part whose declared length is past the quota is refused
// before any of its body is read.
func TestCopiedPartsAndStoredObjectsCountAgainstThePartQuota(t *testing.T) {
	e := newFixEnv(t)
	e.admin(t, http.MethodPut, "/pqcopy", "").must(t, "bucket")
	e.admin(t, http.MethodPut, "/pqcopy/src", strings.Repeat("s", 400<<10)).must(t, "source object")
	if err := e.store.UpdateBucketQuota("pqcopy", 1<<20, 0); err != nil {
		t.Fatal(err)
	}
	refused := func(what string, r result) {
		t.Helper()
		if r.code != http.StatusForbidden || !strings.Contains(r.body, "QuotaExceeded") {
			t.Fatalf("%s: %d %s, want 403 QuotaExceeded", what, r.code, r.body)
		}
	}
	up := e.startUpload(t, "pqcopy", "dst")
	copyPart := func(n int, extra ...string) result {
		return e.admin(t, http.MethodPut, fmt.Sprintf("/pqcopy/dst?partNumber=%d&uploadId=%s", n, up), "",
			append([]string{"X-Amz-Copy-Source", "/pqcopy/src"}, extra...)...)
	}
	copyPart(1).must(t, "a copied part within the quota (400 KiB stored + 400 KiB part)")
	refused("a second copied part past the quota", copyPart(2))
	copyPart(2, "X-Amz-Copy-Source-Range", "bytes=0-1023").must(t, "a small range of the source still fits")

	// 400 KiB stored + 400 KiB part 1 + 1 KiB part 2: a 300 KiB part is past
	// the quota only because the stored object counts.
	e.admin(t, http.MethodPut, fmt.Sprintf("/pqcopy/dst?partNumber=3&uploadId=%s", up), strings.Repeat("p", 100<<10)).must(t, "a part that fits")
	refused("a part past the quota once the stored object counts",
		e.admin(t, http.MethodPut, fmt.Sprintf("/pqcopy/dst?partNumber=4&uploadId=%s", up), strings.Repeat("p", 150<<10)))

	// Declared longer than the quota: refused while the body stalls at 1 MiB,
	// which is within the quota, so only the up-front check can answer.
	e.admin(t, http.MethodPut, "/pqahead", "").must(t, "bucket")
	if err := e.store.UpdateBucketQuota("pqahead", 2<<20, 0); err != nil {
		t.Fatal(err)
	}
	ahead := e.startUpload(t, "pqahead", "k")
	req, _ := http.NewRequest(http.MethodPut, e.ts.URL+"/pqahead/k?partNumber=1&uploadId="+ahead, nil)
	signV4Request(req, testAccessKey, testSecretKey, nil)
	refused("a part declared past the quota", sendStalled(t, req))
}

// Regression (5.0.11 review): a bucket already past its size quota (the quota
// was lowered) had a negative remainder, which every streaming check read as
// "no quota": uploads of unknown length, form uploads and parts of any size
// were stored. The chunked PutObject case was in 5.0.10 too.
func TestBucketPastItsQuotaRefusesStreamingUploads(t *testing.T) {
	e := newFixEnv(t)
	e.admin(t, http.MethodPut, "/pqover", "").must(t, "bucket")
	e.admin(t, http.MethodPut, "/pqover/old", strings.Repeat("o", 2000)).must(t, "object stored before the quota")
	if err := e.store.UpdateBucketQuota("pqover", 1000, 0); err != nil {
		t.Fatal(err)
	}
	refused := func(what string, r result) {
		t.Helper()
		if r.code != http.StatusForbidden || !strings.Contains(r.body, "QuotaExceeded") {
			t.Errorf("%s: %d %s, want 403 QuotaExceeded", what, r.code, r.body)
		}
	}
	body := strings.Repeat("n", 64<<10)

	req, _ := http.NewRequest(http.MethodPut, e.ts.URL+"/pqover/chunked", nil)
	signV4Request(req, testAccessKey, testSecretKey, []byte(body))
	req.Body = io.NopCloser(strings.NewReader(body))
	req.ContentLength = -1
	refused("PutObject of unknown length", send(t, req))

	up := e.startUpload(t, "pqover", "mp")
	refused("UploadPart", e.admin(t, http.MethodPut, "/pqover/mp?partNumber=1&uploadId="+up, body))

	p := policyPost{accessKey: testAccessKey, secretKey: testSecretKey, bucket: "pqover", key: "u/post", prefix: "u/",
		minSize: 0, maxSize: 1 << 20, file: []byte(body)}
	ct, form := p.build(t)
	refused("presigned POST", postForm(t, e.ts.URL, "pqover", ct, form))

	for _, k := range []string{"chunked", "mp", "u/post"} {
		if got := e.admin(t, http.MethodHead, "/pqover/"+k, ""); got.code != http.StatusNotFound {
			t.Errorf("%s was stored in a bucket past its quota", k)
		}
	}
}

// Regression (5.0.11 round 5): CopyObject never checked the destination
// bucket's quota, so copies filled a bucket past it (5.0.10 too).
func TestCopyObjectIsHeldToTheQuota(t *testing.T) {
	e := newFixEnv(t)
	e.admin(t, http.MethodPut, "/pqcp", "").must(t, "bucket")
	e.admin(t, http.MethodPut, "/pqcp/src", strings.Repeat("s", 600<<10)).must(t, "source")
	if err := e.store.UpdateBucketQuota("pqcp", 1<<20, 0); err != nil {
		t.Fatal(err)
	}
	if r := e.admin(t, http.MethodPut, "/pqcp/copy", "", "X-Amz-Copy-Source", "/pqcp/src"); r.code != http.StatusForbidden || !strings.Contains(r.body, "QuotaExceeded") {
		t.Errorf("a copy that takes the bucket past its quota: %d %s, want 403 QuotaExceeded", r.code, r.body)
	}
	if got := e.admin(t, http.MethodHead, "/pqcp/copy", ""); got.code != http.StatusNotFound {
		t.Error("the copy was stored past the quota")
	}
	if err := e.store.UpdateBucketQuota("pqcp", 2<<20, 0); err != nil {
		t.Fatal(err)
	}
	e.admin(t, http.MethodPut, "/pqcp/copy", "", "X-Amz-Copy-Source", "/pqcp/src").must(t, "a copy within the quota")
}

// Regression (5.0.11 round 6): the quota counted an overwrite as one more
// object, so in a bucket at its object count quota a copy of an object onto
// itself to change its metadata (rclone touch, aws s3 cp
// --metadata-directive REPLACE) was refused, and so was a plain overwrite.
// Without versioning a write to an existing key replaces it. Its bytes are
// not given back (round 7): with compression an object's recorded size is not
// what it takes on disk, which let an overwrite store twice the quota.
func TestOverwritesAtTheCountQuotaReplaceRatherThanAdd(t *testing.T) {
	e := newFixEnv(t)
	e.admin(t, http.MethodPut, "/pqow", "").must(t, "bucket")
	obj := strings.Repeat("o", 300<<10)
	e.admin(t, http.MethodPut, "/pqow/f", obj).must(t, "object")
	if err := e.store.UpdateBucketQuota("pqow", 1<<20, 1); err != nil {
		t.Fatal(err)
	}
	e.admin(t, http.MethodPut, "/pqow/f", "", "X-Amz-Copy-Source", "/pqow/f", "X-Amz-Metadata-Directive", "REPLACE", "X-Amz-Meta-Mtime", "2020").must(t, "a copy in place at the count quota")
	e.admin(t, http.MethodPut, "/pqow/f", obj).must(t, "an overwrite at the count quota")
	refused := func(what string, r result) {
		t.Helper()
		if r.code != http.StatusForbidden || !strings.Contains(r.body, "QuotaExceeded") {
			t.Errorf("%s: %d %s, want 403 QuotaExceeded", what, r.code, r.body)
		}
	}
	refused("a copy to a new key at the count quota", e.admin(t, http.MethodPut, "/pqow/g", "", "X-Amz-Copy-Source", "/pqow/f"))
	// The replaced file's bytes on disk are given back: an overwrite that
	// fits once the old file is gone is allowed, one that does not is refused.
	e.admin(t, http.MethodPut, "/pqow/f", strings.Repeat("o", 900<<10)).must(t, "an overwrite that fits once the old bytes are given back")
	e.admin(t, http.MethodPut, "/pqow/f", "", "X-Amz-Copy-Source", "/pqow/f", "X-Amz-Metadata-Directive", "REPLACE", "X-Amz-Meta-Mtime", "2021").must(t, "a copy in place of a 900 KiB object in a 1 MiB bucket")
	refused("an overwrite past the size quota", e.admin(t, http.MethodPut, "/pqow/f", strings.Repeat("o", 1100<<10)))

	// A suspended bucket rewrites the null version in place: not one more.
	e.admin(t, http.MethodPut, "/pqsus", "").must(t, "suspended bucket")
	e.admin(t, http.MethodPut, "/pqsus?versioning", `<VersioningConfiguration><Status>Suspended</Status></VersioningConfiguration>`).must(t, "suspend")
	e.admin(t, http.MethodPut, "/pqsus/k", "one").must(t, "object")
	if err := e.store.UpdateBucketQuota("pqsus", 0, 1); err != nil {
		t.Fatal(err)
	}
	e.admin(t, http.MethodPut, "/pqsus/k", "two").must(t, "an overwrite of the null version at the count quota")
	// Its bytes on disk are given back too (round 9): a 600 KiB null version
	// overwritten with 600 KiB in a 1 MiB bucket fits.
	if err := e.store.UpdateBucketQuota("pqsus", 1<<20, 0); err != nil {
		t.Fatal(err)
	}
	big := strings.Repeat("s", 600<<10)
	e.admin(t, http.MethodPut, "/pqsus/k", big).must(t, "a 600 KiB null version")
	e.admin(t, http.MethodPut, "/pqsus/k", big).must(t, "the 600 KiB null version overwritten with 600 KiB")
	if err := e.store.UpdateBucketQuota("pqsus", 0, 1); err != nil {
		t.Fatal(err)
	}
	refused("a new key in the suspended bucket at the count quota", e.admin(t, http.MethodPut, "/pqsus/other", "x"))
}

// Regression (5.0.11 round 7, present in 5.0.10): with versioning enabled
// every version, the latest included, is kept in the version store, which the
// bucket's size skipped. A versioned bucket counted as empty, so its quota
// limited nothing: five 4 MiB PUTs went into a 5 MiB bucket. Every stored
// version now counts, in bytes and as an object.
func TestVersionedBucketIsHeldToItsQuota(t *testing.T) {
	e := newFixEnv(t)
	e.admin(t, http.MethodPut, "/pqver", "").must(t, "bucket")
	e.admin(t, http.MethodPut, "/pqver?versioning", `<VersioningConfiguration><Status>Enabled</Status></VersioningConfiguration>`).must(t, "versioning")
	if err := e.store.UpdateBucketQuota("pqver", 1<<20, 0); err != nil {
		t.Fatal(err)
	}
	obj := strings.Repeat("v", 300<<10)
	for i := 1; i <= 3; i++ {
		e.admin(t, http.MethodPut, "/pqver/f", obj).must(t, fmt.Sprintf("version %d of f", i))
	}
	if r := e.admin(t, http.MethodPut, "/pqver/f", obj); r.code != http.StatusForbidden || !strings.Contains(r.body, "QuotaExceeded") {
		t.Errorf("a fourth 300 KiB version in a 1 MiB versioned bucket: %d %s, want 403 QuotaExceeded", r.code, r.body)
	}
	if r := e.admin(t, http.MethodPut, "/pqver/g", obj); r.code != http.StatusForbidden {
		t.Errorf("a new key past the quota in a versioned bucket: %d, want 403", r.code)
	}
	if err := e.store.UpdateBucketQuota("pqver", 0, 3); err != nil {
		t.Fatal(err)
	}
	if r := e.admin(t, http.MethodPut, "/pqver/h", "x"); r.code != http.StatusForbidden {
		t.Errorf("a new key with three stored versions at a count quota of 3: %d, want 403", r.code)
	}
}
