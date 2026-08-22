package s3

import (
	"errors"
	"net/http"

	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
)

// metadataUnavailable turns a metadata lookup that could NOT be served into a
// 503, and reports that it did so.
//
// The distinction it protects is the one the whole sharded design rests on: a
// shard that cannot be asked is not an empty shard (issue #50). Answering 404
// would tell a client its object is gone when the object is intact and only the
// group holding its record is momentarily unreachable, and a 404 is what makes a
// client stop retrying, a sync tool re-upload, and a reconciler delete.
//
// A 503 is retryable and carries no such claim, which is the honest answer.
func metadataUnavailable(w http.ResponseWriter, err error) bool {
	if err == nil || !errors.Is(err, metadata.ErrShardUnavailable) {
		return false
	}
	w.Header().Set("Retry-After", "1")
	writeS3Error(w, "ServiceUnavailable",
		"The metadata shard holding this bucket is temporarily unavailable, retry the request",
		http.StatusServiceUnavailable)
	return true
}
