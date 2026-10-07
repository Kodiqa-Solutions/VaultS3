package s3

import (
	"archive/tar"
	"bytes"
	"fmt"
	"mime/multipart"
	"strings"
	"testing"
)

// Deleting a bucket decided "empty" from a walk of its directory, which skips
// .vs/, where every object of a versioning enabled bucket keeps its bytes. Such
// a bucket was deleted with everything in it, locked objects included.
func TestDeleteBucketRefusesAVersionedBucketThatHoldsData(t *testing.T) {
	ts, _ := fullServer(t)
	for _, b := range []string{"live", "markers"} {
		call(t, ts, "PUT", "/"+b, "").must(t, "create")
		call(t, ts, "PUT", "/"+b+"?versioning", versioningOn).must(t, "versioning")
		call(t, ts, "PUT", "/"+b+"/k", "keep me").must(t, "put")
	}
	// Only a noncurrent version and a delete marker are left here.
	call(t, ts, "DELETE", "/markers/k", "").must(t, "delete object")

	for _, b := range []string{"live", "markers"} {
		r := call(t, ts, "DELETE", "/"+b, "")
		if r.code != 409 || !strings.Contains(r.body, "BucketNotEmpty") {
			t.Errorf("DELETE /%s: %d %s, want 409 BucketNotEmpty", b, r.code, r.body)
		}
	}
	if r := call(t, ts, "GET", "/live/k", ""); r.code != 200 || r.body != "keep me" {
		t.Errorf("the object did not survive the refused delete: %d %q", r.code, r.body)
	}
	// An empty versioned bucket still deletes.
	call(t, ts, "PUT", "/empty", "").must(t, "create")
	call(t, ts, "PUT", "/empty?versioning", versioningOn).must(t, "versioning")
	if r := call(t, ts, "DELETE", "/empty", ""); r.code != 204 {
		t.Errorf("empty versioned bucket: %d %s, want 204", r.code, r.body)
	}
}

// A website bucket waived authentication for every GET and HEAD, while the
// website handler serves only requests with no query string. Every API call
// with a query was answered to anyone: listings, versions, the policy itself.
func TestWebsiteBucketIsPublicOnlyForWebsiteRequests(t *testing.T) {
	ts, _ := fullServer(t)
	call(t, ts, "PUT", "/site", "").must(t, "create")
	call(t, ts, "PUT", "/site/index.html", "<h1>hi</h1>", "Content-Type", "text/html").must(t, "index")
	call(t, ts, "PUT", "/site?policy", `{"Version":"2012-10-17","Statement":[{"Effect":"Deny","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::site/private/*"}]}`).must(t, "policy")
	call(t, ts, "PUT", "/site?website", `<WebsiteConfiguration><IndexDocument><Suffix>index.html</Suffix></IndexDocument></WebsiteConfiguration>`).must(t, "website")

	if r := anonCall(t, ts, "GET", "/site/"); r.code != 200 || !strings.Contains(r.body, "hi") {
		t.Errorf("the website itself stopped working: %d %q", r.code, r.body)
	}
	for _, q := range []string{"?list-type=2", "?versions", "?policy", "?website", "?notification", "?uploads", "?acl"} {
		if r := anonCall(t, ts, "GET", "/site"+q); r.code != 403 {
			t.Errorf("anonymous GET /site%s: %d, want 403", q, r.code)
		}
	}
	for _, q := range []string{"?tagging", "?versionId=x", "?attributes"} {
		if r := anonCall(t, ts, "GET", "/site/index.html"+q); r.code != 403 {
			t.Errorf("anonymous GET /site/index.html%s: %d, want 403", q, r.code)
		}
	}
}

func tarOf(t *testing.T, entries map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for name, data := range entries {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0644, Size: int64(len(data)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		tw.Write([]byte(data))
	}
	tw.Close()
	return buf.Bytes()
}

// A Snowball entry name was turned into a key with path.Clean, which keeps a
// leading "..", and the storage engine checked only that the path stayed in the
// data directory, so "../victim/a.txt" overwrote another bucket's object.
func TestSnowballEntriesStayInsideTheirBucket(t *testing.T) {
	ts, store := fullServer(t)
	call(t, ts, "PUT", "/victim", "").must(t, "victim")
	call(t, ts, "PUT", "/victim/a.txt", "original").must(t, "victim object")
	call(t, ts, "PUT", "/src", "").must(t, "src")

	archive := tarOf(t, map[string]string{
		"ok.txt":               "fine",
		"./dot.txt":            "dot",
		"../victim/a.txt":      "PWNED",
		"x/../../victim/b.txt": "PWNED",
		"/abs.txt":             "abs",
	})
	call(t, ts, "PUT", "/src/batch.tar", string(archive), "X-Amz-Meta-Snowball-Auto-Extract", "true").must(t, "snowball")

	if r := call(t, ts, "GET", "/victim/a.txt", ""); r.body != "original" {
		t.Errorf("victim/a.txt was overwritten: %d %q", r.code, r.body)
	}
	if r := call(t, ts, "GET", "/victim/b.txt", ""); r.code != 404 {
		t.Errorf("victim/b.txt was created: %d", r.code)
	}
	for key, want := range map[string]string{"ok.txt": "fine", "dot.txt": "dot"} {
		if r := call(t, ts, "GET", "/src/"+key, ""); r.body != want {
			t.Errorf("safe entry %s not imported: %d %q", key, r.code, r.body)
		}
	}
	// A refused entry is refused at the door, not written somewhere harmless:
	// the storage engine would keep it inside the bucket, but the import would
	// still record an object under the escaping name.
	for _, key := range []string{"../victim/a.txt", "x/../../victim/b.txt", "/abs.txt", "victim/b.txt"} {
		if _, err := store.GetObjectMeta("src", key); err == nil {
			t.Errorf("a refused entry was recorded as src/%s", key)
		}
	}
	// Imported objects record seconds, as every reader expects.
	meta, err := store.GetObjectMeta("src", "ok.txt")
	if err != nil {
		t.Fatal(err)
	}
	if meta.LastModified > 1e12 {
		t.Errorf("Snowball stored LastModified %d, a nanosecond value", meta.LastModified)
	}
}

func TestObjectKeyProblem(t *testing.T) {
	for key, bad := range map[string]bool{
		"a.txt": false, "dir/a.txt": false, "a..b": false, "..a/b": false, "a//b": false,
		"": true, "../a": true, "a/../../b": true, "a/..": true, "/abs": true, "a\x00b": true,
	} {
		if got := objectKeyProblem(key) != ""; got != bad {
			t.Errorf("objectKeyProblem(%q) refused=%v, want %v", key, got, bad)
		}
	}
}

// Lifecycle parsed only Filter>Prefix and dropped every other filter without a
// word, so a rule meant for one prefix or one tag was stored as "expire every
// object in the bucket".
func TestLifecycleNeverStoresAWiderRuleThanAskedFor(t *testing.T) {
	ts, _ := fullServer(t)
	call(t, ts, "PUT", "/lcbkt", "").must(t, "create")
	rule := func(inner string) string {
		return `<LifecycleConfiguration><Rule><ID>r</ID><Status>Enabled</Status>` + inner +
			`<Expiration><Days>1</Days></Expiration></Rule></LifecycleConfiguration>`
	}
	for name, body := range map[string]string{
		"tag":  rule(`<Filter><Tag><Key>t</Key><Value>v</Value></Tag></Filter>`),
		"and":  rule(`<Filter><And><Prefix>a/</Prefix><Tag><Key>t</Key><Value>v</Value></Tag></And></Filter>`),
		"size": rule(`<Filter><ObjectSizeGreaterThan>100</ObjectSizeGreaterThan></Filter>`),
		"two rules": `<LifecycleConfiguration><Rule><Status>Enabled</Status><Filter><Prefix>a/</Prefix></Filter><Expiration><Days>1</Days></Expiration></Rule>` +
			`<Rule><Status>Enabled</Status><Filter><Prefix>b/</Prefix></Filter><Expiration><Days>1</Days></Expiration></Rule></LifecycleConfiguration>`,
	} {
		if r := call(t, ts, "PUT", "/lcbkt?lifecycle", body); r.code != 501 {
			t.Errorf("%s: %d %s, want 501 NotImplemented", name, r.code, r.body)
		}
		if g := call(t, ts, "GET", "/lcbkt?lifecycle", ""); g.code == 200 {
			t.Errorf("%s: a refused configuration was stored anyway: %s", name, g.body)
		}
	}
	// The legacy rule-level Prefix means the same as Filter>Prefix and is kept.
	call(t, ts, "PUT", "/lcbkt?lifecycle", rule(`<Prefix>logs/</Prefix>`)).must(t, "legacy prefix")
	if g := call(t, ts, "GET", "/lcbkt?lifecycle", ""); !strings.Contains(g.body, "logs/") {
		t.Errorf("the rule-level prefix was dropped: %s", g.body)
	}
}

// A lambda trigger's output is written by the server itself, bypassing IAM,
// object lock and quotas, and any bucket could be named for it.
func TestLambdaOutputMustStayInTheTriggersBucket(t *testing.T) {
	ts, _ := fullServer(t)
	call(t, ts, "PUT", "/own", "").must(t, "own")
	cfg := func(out string) string {
		return fmt.Sprintf(`{"triggers":[{"function_url":"https://fn.example.com/x","events":["s3:ObjectCreated:*"],"output_bucket":%q,"output_key_template":"out/{key}"}]}`, out)
	}
	if r := call(t, ts, "PUT", "/own?lambda", cfg("someone-else")); r.code != 400 {
		t.Errorf("foreign output bucket: %d %s, want 400", r.code, r.body)
	}
	call(t, ts, "PUT", "/own?lambda", cfg("own")).must(t, "own output bucket")
}

const sseOn = `<ServerSideEncryptionConfiguration><Rule><ApplyServerSideEncryptionByDefault><SSEAlgorithm>AES256</SSEAlgorithm></ApplyServerSideEncryptionByDefault></Rule></ServerSideEncryptionConfiguration>`

// The bucket's data keys live in its encryption config record, and both PUT and
// DELETE ?encryption replaced or removed that record. Sending the same config
// again, as Terraform does on every apply, generated a new key and left every
// object encrypted before it unreadable. Removing the config destroyed the keys.
func TestBucketEncryptionConfigChangesKeepTheKeys(t *testing.T) {
	ts, store := fullServer(t)
	call(t, ts, "PUT", "/enc", "").must(t, "create")
	call(t, ts, "PUT", "/enc?encryption", sseOn).must(t, "encrypt")
	call(t, ts, "PUT", "/enc/one", "one").must(t, "put one")

	call(t, ts, "PUT", "/enc?encryption", sseOn).must(t, "same config again")
	if r := call(t, ts, "GET", "/enc/one", ""); r.body != "one" {
		t.Fatalf("object unreadable after the config was sent again: %d %q", r.code, r.body)
	}

	if r := call(t, ts, "DELETE", "/enc?encryption", ""); r.code != 204 {
		t.Fatalf("delete encryption: %d", r.code)
	}
	if r := call(t, ts, "GET", "/enc/one", ""); r.body != "one" {
		t.Fatalf("object unreadable after the default encryption was removed: %d %q", r.code, r.body)
	}
	if r := call(t, ts, "GET", "/enc?encryption", ""); r.code != 404 {
		t.Errorf("GET ?encryption after delete: %d, want 404", r.code)
	}
	call(t, ts, "PUT", "/enc/two", "two").must(t, "put two while off")

	call(t, ts, "PUT", "/enc?encryption", sseOn).must(t, "encrypt again")
	call(t, ts, "PUT", "/enc/three", "three").must(t, "put three")
	for key, want := range map[string]string{"one": "one", "two": "two", "three": "three"} {
		if r := call(t, ts, "GET", "/enc/"+key, ""); r.body != want {
			t.Errorf("%s after re-enabling: %d %q", key, r.code, r.body)
		}
	}
	cfg, err := store.GetEncryptionConfig("enc")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.KeyVersion != 1 || len(cfg.WrappedDEKs) != 1 {
		t.Errorf("re-enabling replaced the key: version %d, %d keys", cfg.KeyVersion, len(cfg.WrappedDEKs))
	}
}

// The POST upload key is a form field, so the router's check on the request
// path never saw it, and "../victim/a.txt" wrote into another bucket.
func TestPostUploadKeyStaysInsideItsBucket(t *testing.T) {
	ts, _ := fullServer(t)
	call(t, ts, "PUT", "/victim", "").must(t, "victim")
	call(t, ts, "PUT", "/victim/a.txt", "original").must(t, "victim object")
	call(t, ts, "PUT", "/src", "").must(t, "src")

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	mw.WriteField("key", "../victim/a.txt")
	fw, _ := mw.CreateFormFile("file", "f.txt")
	fw.Write([]byte("PWNED"))
	mw.Close()

	if r := call(t, ts, "POST", "/src", buf.String(), "Content-Type", mw.FormDataContentType()); r.code != 400 {
		t.Errorf("POST upload with key ../victim/a.txt: %d %s, want 400", r.code, r.body)
	}
	if r := call(t, ts, "GET", "/victim/a.txt", ""); r.body != "original" {
		t.Errorf("victim/a.txt was overwritten: %q", r.body)
	}
}
