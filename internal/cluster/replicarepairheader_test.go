package cluster

import "testing"

// Must stay identical to api.BucketEncryptedHeader. They cannot share a constant
// (internal/api imports this package), so both sides pin the literal: a rename on
// one side alone would silently stop the receiver from ever seeing the flag, and
// replicas would quietly go back to being written in the clear.
func TestBucketEncryptedHeaderLiteral(t *testing.T) {
	if bucketEncryptedHeader != "X-Vaults3-Bucket-Encrypted" {
		t.Fatalf("header renamed to %q, update internal/api too", bucketEncryptedHeader)
	}
}
