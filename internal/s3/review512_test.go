package s3

import (
	"io"
	"strings"
	"testing"
	"time"

	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
)

// Regression (5.0.12 review): Active-active treats the highest version ID as current on every site. A
// Regression (5.0.12 review): local PUT takes its ID before its body arrives and becomes current when it
// Regression (5.0.12 review): finishes, so a slow upload that started first ends current here with an ID
// Regression (5.0.12 review): below a faster one: the other site makes the faster one current.
func TestASlowVersionedPutDoesNotTakeTheKeyFromANewerOne(t *testing.T) {
	metadata.SetVersionOrdering(true) // active-active
	t.Cleanup(func() { metadata.SetVersionOrdering(false) })
	h, store, _, _ := newObjTestServer(t)
	store.CreateBucket("v")
	store.SetBucketVersioning("v", "Enabled")
	h.objects.engine.CreateBucketDir("v")
	pr, pw := io.Pipe()
	done := make(chan string)
	go func() {
		m, err := h.objects.writeObject(newObject{bucket: "v", key: "k", body: pr, size: -1})
		if err != nil {
			t.Error(err)
		}
		done <- m.VersionID
	}()
	time.Sleep(50 * time.Millisecond) // slow upload has taken its ID
	fast, err := h.objects.writeObject(newObject{bucket: "v", key: "k", body: strings.NewReader("fast"), size: 4})
	if err != nil {
		t.Fatal(err)
	}
	pw.Write([]byte("slow"))
	pw.Close()
	slow := <-done
	cur, _ := store.GetObjectMeta("v", "k")
	if slow < fast.VersionID && cur.VersionID == slow {
		t.Fatalf("current here is %s (slow) but the highest ID is %s (fast): a peer applying both makes the fast one current", slow, fast.VersionID)
	}
}
