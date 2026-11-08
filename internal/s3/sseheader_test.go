package s3

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/Kodiqa-Solutions/VaultS3/internal/bucketcrypto"
)

// The SSE response header used to come from the global encryption flag alone.
// In per-bucket mode that is a different question, and the server was telling
// clients that objects in buckets which never opted in were encrypted at rest
// when they were plaintext on disk (issue #53).
func TestSSEHeaderReflectsTheBucketNotTheGlobalFlag(t *testing.T) {
	t.Run("encryption off says nothing", func(t *testing.T) {
		h := &ObjectHandler{encryptionEnabled: false}
		if h.sseHeaderApplies("any") {
			t.Fatal("no encryption configured, so no object is encrypted")
		}
	})

	t.Run("server-wide key covers every bucket", func(t *testing.T) {
		h := &ObjectHandler{encryptionEnabled: true} // not per-bucket mode
		if !h.sseHeaderApplies("any") {
			t.Fatal("one server-wide key encrypts every object, the header is correct")
		}
	})

	// SSE-KMS requires an `encryption.key` it never uses, so a key manager exists
	// even though no bucket has a per-bucket key. Reading the answer off that
	// manager reported every KMS bucket as unencrypted.
	t.Run("SSE-KMS covers every bucket despite a key manager", func(t *testing.T) {
		h := &ObjectHandler{encryptionEnabled: true, keyMgr: newSSEHeaderMgr(t)}
		if !h.sseHeaderApplies("any") {
			t.Fatal("SSE-KMS encrypts every object, the header must say so")
		}
	})
}

// The per-bucket case needs a real manager, since opt-in is its state.
func TestSSEHeaderPerBucket(t *testing.T) {
	mgr := newSSEHeaderMgr(t)
	// perBucketMode is what makes encryption a per-bucket question at all.
	h := &ObjectHandler{encryptionEnabled: true, perBucketMode: true, keyMgr: mgr}

	if h.sseHeaderApplies("optedout") {
		t.Fatal("a bucket that never opted in stores plaintext, claiming AES256 is a false claim")
	}
	if err := mgr.EnableBucket("optedin"); err != nil {
		t.Fatal(err)
	}
	if !h.sseHeaderApplies("optedin") {
		t.Fatal("a bucket that opted in really is encrypted, the header must say so")
	}
}

func newSSEHeaderMgr(t *testing.T) *bucketcrypto.Manager {
	t.Helper()
	mk := make([]byte, 32)
	for i := range mk {
		mk[i] = byte(i)
	}
	kek, err := bucketcrypto.NewKEK(mk)
	if err != nil {
		t.Fatal(err)
	}
	return bucketcrypto.NewManager(kek, bucketcrypto.NewMemKeyStore())
}

// Regression (5.0.11): PutObject, GET and HEAD said x-amz-server-side-encryption
// for an object encrypted at rest, but CopyObject, CreateMultipartUpload and
// CompleteMultipartUpload did not, although they write the same encrypted
// object. They say it now, and still only when the bucket encrypts. A part's
// answer never says it: parts are stored as sent until Complete.
func TestCopyAndMultipartAnswersReportEncryption(t *testing.T) {
	for _, encrypted := range []bool{true, false} {
		t.Run(fmt.Sprintf("encrypted=%v", encrypted), func(t *testing.T) {
			e := newFixEnv(t)
			e.h.objects.encryptionEnabled = encrypted
			e.admin(t, http.MethodPut, "/sseb", "").must(t, "bucket")
			e.admin(t, http.MethodPut, "/sseb/src", strings.Repeat("s", 6<<20)).must(t, "source")
			checkAs := func(what string, r result, claim bool) {
				t.Helper()
				if r.code != http.StatusOK {
					t.Fatalf("%s: %d %s", what, r.code, r.body)
				}
				if got := r.header.Get("X-Amz-Server-Side-Encryption"); (got == "AES256") != claim {
					t.Errorf("%s: x-amz-server-side-encryption %q, want it set: %v", what, got, claim)
				}
			}
			check := func(what string, r result) { t.Helper(); checkAs(what, r, encrypted) }
			// A part is stored as sent until Complete, so its answer never
			// claims encryption.
			part := func(what string, r result) { t.Helper(); checkAs(what, r, false) }
			check("CopyObject", e.admin(t, http.MethodPut, "/sseb/copy", "", "X-Amz-Copy-Source", "/sseb/src"))
			init := e.admin(t, http.MethodPost, "/sseb/mp?uploads", "")
			check("CreateMultipartUpload", init)
			up := init.body[strings.Index(init.body, "<UploadId>")+len("<UploadId>") : strings.Index(init.body, "</UploadId>")]
			p1 := e.admin(t, http.MethodPut, "/sseb/mp?partNumber=1&uploadId="+up, strings.Repeat("p", 5<<20))
			part("UploadPart", p1)
			p2 := e.admin(t, http.MethodPut, "/sseb/mp?partNumber=2&uploadId="+up, "", "X-Amz-Copy-Source", "/sseb/src")
			part("UploadPartCopy", p2)
			etag2 := p2.body[strings.Index(p2.body, "<ETag>")+len("<ETag>") : strings.Index(p2.body, "</ETag>")]
			complete := fmt.Sprintf(`<CompleteMultipartUpload><Part><PartNumber>1</PartNumber><ETag>%s</ETag></Part><Part><PartNumber>2</PartNumber><ETag>%s</ETag></Part></CompleteMultipartUpload>`,
				p1.header.Get("ETag"), etag2)
			check("CompleteMultipartUpload", e.admin(t, http.MethodPost, "/sseb/mp?uploadId="+up, complete))
			form := policyPost{accessKey: testAccessKey, secretKey: testSecretKey, bucket: "sseb", key: "u/post", prefix: "u/",
				minSize: 0, maxSize: 100, file: []byte("x"), covered: map[string]string{"success_action_status": "201"}}
			ct, body := form.build(t)
			pr := postForm(t, e.ts.URL, "sseb", ct, body)
			if pr.code != http.StatusCreated || (pr.header.Get("X-Amz-Server-Side-Encryption") == "AES256") != encrypted {
				t.Errorf("presigned POST: %d, x-amz-server-side-encryption %q, want it only when encrypted (%v)", pr.code, pr.header.Get("X-Amz-Server-Side-Encryption"), encrypted)
			}
		})
	}
}

// Regression (5.0.11): a lifecycle rule's ID was stored but not returned by
// GetBucketLifecycleConfiguration.
func TestLifecycleRuleIDIsReturned(t *testing.T) {
	e := newFixEnv(t)
	e.admin(t, http.MethodPut, "/lcid", "").must(t, "bucket")
	e.admin(t, http.MethodPut, "/lcid?lifecycle", `<LifecycleConfiguration><Rule><ID>expire-logs</ID><Filter><Prefix>logs/</Prefix></Filter><Status>Enabled</Status><Expiration><Days>7</Days></Expiration></Rule></LifecycleConfiguration>`).must(t, "put lifecycle")
	if got := e.admin(t, http.MethodGet, "/lcid?lifecycle", ""); !strings.Contains(got.body, "<ID>expire-logs</ID>") {
		t.Fatalf("lifecycle read back without its rule ID: %s", got.body)
	}
}

// Regression (5.0.11): CompleteMultipartUpload answered with the raw key as
// its Location, so a key with a space or '#' linked elsewhere.
func TestCompleteMultipartLocationIsEncoded(t *testing.T) {
	e := newFixEnv(t)
	e.admin(t, http.MethodPut, "/cloc", "").must(t, "bucket")
	esc := "mp/%C3%A4%20b%23.bin"
	up := e.startUpload(t, "cloc", esc)
	p1 := e.admin(t, http.MethodPut, "/cloc/"+esc+"?partNumber=1&uploadId="+up, "part")
	p1.must(t, "part")
	done := e.admin(t, http.MethodPost, "/cloc/"+esc+"?uploadId="+up,
		fmt.Sprintf(`<CompleteMultipartUpload><Part><PartNumber>1</PartNumber><ETag>%s</ETag></Part></CompleteMultipartUpload>`, p1.header.Get("ETag")))
	done.must(t, "complete")
	if want := "<Location>/cloc/" + esc + "</Location>"; !strings.Contains(done.body, want) {
		t.Errorf("Complete answered %s, want %s", done.body, want)
	}
}
