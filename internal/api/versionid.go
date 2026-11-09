package api

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
)

// genVersionID produces a sortable, unique version ID (matches the S3 layer's
// format: nanosecond timestamp + random suffix).
// nextVersionID is the ID of a new version of key, above its current one
// (see metadata.NewVersionID).
func (h *APIHandler) nextVersionID(bucket, key string) string {
	cur := ""
	if m, err := h.store.GetObjectMeta(bucket, key); err == nil && m != nil {
		cur = m.VersionID
	}
	return metadata.NewVersionID(cur)
}

func genVersionID() string {
	b := make([]byte, 4)
	rand.Read(b)
	return fmt.Sprintf("%016x%s", time.Now().UnixNano(), hex.EncodeToString(b))
}
