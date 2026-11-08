package server

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Kodiqa-Solutions/VaultS3/internal/config"
	"github.com/Kodiqa-Solutions/VaultS3/internal/migrate"
)

// Regression (issue found 2026-11-08): the server built the migration manager
// without ever applying an allowance for private sources, so a MinIO on the
// operator's own network could never be migrated from. The setting now
// reaches the manager, and stays off by default.
func TestMigratorHonoursAllowPrivateSources(t *testing.T) {
	src := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `<ListAllMyBucketsResult><Buckets><Bucket><Name>b1</Name></Bucket></Buckets></ListAllMyBucketsResult>`)
	}))
	t.Cleanup(src.Close)
	start := migrate.StartConfig{Endpoint: src.URL, AccessKey: "k", SecretKey: "s"}

	cfg := config.Defaults()
	if _, err := newMigrator(cfg, nil, nil).TestConnection(start); !errors.Is(err, migrate.ErrPrivateSource) {
		t.Fatalf("default: %v, want ErrPrivateSource", err)
	}
	cfg.Migration.AllowPrivateSources = true
	buckets, err := newMigrator(cfg, nil, nil).TestConnection(start)
	if err != nil || len(buckets) != 1 || buckets[0] != "b1" {
		t.Fatalf("allow_private_sources: %v %v, want [b1]", buckets, err)
	}
}
