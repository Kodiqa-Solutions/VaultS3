package lambda

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
)

// Regression (5.0.11 round 12, every earlier version): the output key was
// checked for ".." only, so a template "./doc" overwrote a doc under legal hold
// and ".vs/X/null" X's null version.
func TestLambdaOutputKeyIsChecked(t *testing.T) {
	m, store, fs := newManager(t)
	fn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("PWNED")) }))
	defer fn.Close()
	// a held object and a null version in the version store
	fs.PutObject("own", "doc", bytes.NewReader([]byte("HELD")), 4)
	store.PutObjectMeta(metadata.ObjectMeta{Bucket: "own", Key: "doc", Size: 4, LegalHold: true})
	fs.PutObjectVersion("own", "X", "null", bytes.NewReader([]byte("ORIG")), 4)
	for _, tmpl := range []string{"./doc", ".vs/X/null", "doc"} { // "doc" itself is held: the lock check
		m.executeTrigger(triggerJob{
			trigger: metadata.LambdaTrigger{ID: "t", FunctionURL: fn.URL, OutputBucket: "own", OutputKeyTemplate: tmpl},
			bucket:  "own", key: "in.txt", eventType: "s3:ObjectCreated:Put",
		})
	}
	r, _, _ := fs.GetObject("own", "doc")
	b, _ := io.ReadAll(r)
	r.Close()
	r2, _, _ := fs.GetObjectVersion("own", "X", "null")
	b2, _ := io.ReadAll(r2)
	r2.Close()
	if string(b) == "PWNED" || string(b2) == "PWNED" {
		t.Error("lambda output key reached a held object / the version store")
	}
}
