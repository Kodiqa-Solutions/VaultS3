package s3

import (
	"net/http"
	"testing"
)

// Regression (5.0.11 round 13): RestoreObject only marked the record hot and
// left the bytes in the cold directory, so a restored object still could not
// be read. It moves the data back through the tiering manager now, and is
// refused when tiering is not configured.
func TestRestoreObjectMovesTheDataBack(t *testing.T) {
	e := newFixEnv(t)
	e.admin(t, http.MethodPut, "/cold", "").must(t, "bucket")
	e.admin(t, http.MethodPut, "/cold/k", "data").must(t, "object")
	m, _ := e.store.GetObjectMeta("cold", "k")
	m.Tier = "cold"
	if err := e.store.PutObjectMeta(*m); err != nil {
		t.Fatal(err)
	}
	if r := e.admin(t, http.MethodPost, "/cold/k?restore", ""); r.code != http.StatusConflict {
		t.Errorf("restore with tiering not configured: %d %s, want 409", r.code, r.body)
	}
	var restored []string
	e.h.SetColdRestorer(func(bucket, key string) error {
		restored = append(restored, bucket+"/"+key)
		return nil
	})
	if r := e.admin(t, http.MethodPost, "/cold/k?restore", ""); r.code != http.StatusAccepted {
		t.Fatalf("restore: %d %s, want 202", r.code, r.body)
	}
	if len(restored) != 1 || restored[0] != "cold/k" {
		t.Fatalf("the restorer was called for %v, want cold/k once", restored)
	}
}
