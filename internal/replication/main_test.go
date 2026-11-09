package replication

import (
	"os"
	"testing"

	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
)

// Active-active runs with the highest-version-ID-is-current rule on, as the
// server sets it.
func TestMain(m *testing.M) {
	metadata.SetVersionOrdering(true)
	os.Exit(m.Run())
}
