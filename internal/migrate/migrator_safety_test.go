package migrate

import (
	"strings"
	"testing"

	"github.com/Kodiqa-Solutions/VaultS3/internal/storage"
)

// A migration source is a remote endpoint the user pointed at, so its bucket
// names have had no validation. Before this guard, a source that listed a bucket
// called "../evil" reached the filesystem engine and created a directory outside
// the data directory. migrateJob refuses those names up front.
//
// The predicate is the storage layer's safety check rather than the full S3
// naming rule, so a legitimately short source bucket still migrates.
func TestMigrationRejectsTraversingSourceBucketNames(t *testing.T) {
	for _, name := range []string{"../evil", "..", "a/../..", "a/b", ".", ""} {
		if storage.IsSafeBucketName(name) {
			t.Errorf("source bucket %q would be accepted, it must be refused", name)
		}
	}
	// These are not valid under the S3 naming rule but are safe as paths, and a
	// migration must not fail on them.
	for _, name := range []string{"b", "ab", "My.Bucket"} {
		if !storage.IsSafeBucketName(name) {
			t.Errorf("source bucket %q is path-safe and must still migrate", name)
		}
	}
}

// The error a user sees has to say which bucket and why, or a failed migration
// against a large source is untriageable.
func TestMigrationRefusalMessageNamesTheBucket(t *testing.T) {
	const bucket = "../evil"
	msg := "source bucket \"" + bucket + "\" is not a usable name: it resolves outside the data directory"
	if !strings.Contains(msg, bucket) {
		t.Fatal("the refusal must name the offending bucket")
	}
}
