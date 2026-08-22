package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Kodiqa-Solutions/VaultS3/internal/cluster"
	"github.com/Kodiqa-Solutions/VaultS3/internal/config"
	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
)

func shardTestConfig(t *testing.T, shards int) *config.Config {
	t.Helper()
	dir := t.TempDir()
	cfg := &config.Config{}
	cfg.Server.Address = "127.0.0.1"
	cfg.Server.Port = 0
	cfg.Storage.DataDir = filepath.Join(dir, "data")
	cfg.Storage.MetadataDir = filepath.Join(dir, "metadata")
	cfg.Auth.AdminAccessKey = "vaults3-admin"
	cfg.Auth.AdminSecretKey = "vaults3-secret-change-me"
	cfg.Memory.MaxSearchEntries = 100
	cfg.Cluster.Enabled = true
	cfg.Cluster.NodeID = "n1"
	cfg.Cluster.BindAddr = "127.0.0.1"
	cfg.Cluster.RaftPort = 0
	cfg.Cluster.DataDir = filepath.Join(dir, "raft")
	cfg.Cluster.MetadataShards = shards
	return cfg
}

// A shard count no node could run must fail loudly rather than be clamped. An
// operator who asked for 4096 shards and silently got 256 would believe their
// metadata is spread eight times wider than it is (issue #50).
func TestServerRefusesAnImpossibleShardCount(t *testing.T) {
	srv, err := New(shardTestConfig(t, cluster.MaxMetadataShards+1))
	if err == nil {
		srv.Close()
		t.Fatal("server started with a shard count above the supported maximum")
	}
	if !strings.Contains(err.Error(), "metadata_shards") {
		t.Fatalf("error does not name the setting at fault: %v", err)
	}
}

// Sharding must not be switchable on for a store that already holds object
// metadata: the existing records stay in the control group, which nothing reads
// once objects route to shards, so every object would report as missing while
// its metadata and its bytes both still exist. There is no in-place migration,
// so the only safe answer is to refuse to start.
func TestServerRefusesShardingOverExistingObjectMetadata(t *testing.T) {
	cfg := shardTestConfig(t, 4)

	// Write one object record the way an unsharded cluster would have.
	seed := func() {
		store, err := metadata.NewStore(filepath.Join(cfg.Storage.MetadataDir, "vaults3.db"))
		if err != nil {
			t.Fatalf("open store: %v", err)
		}
		defer store.Close()
		if err := store.PutObjectMeta(metadata.ObjectMeta{Bucket: "already-here", Key: "obj"}); err != nil {
			t.Fatalf("seed object metadata: %v", err)
		}
	}
	if err := os.MkdirAll(cfg.Storage.MetadataDir, 0755); err != nil {
		t.Fatal(err)
	}
	seed()

	srv, err := New(cfg)
	if err == nil {
		srv.Close()
		t.Fatal("server enabled sharding over metadata written unsharded")
	}
	if !strings.Contains(err.Error(), "no in-place migration") {
		t.Fatalf("error does not explain that there is no migration path: %v", err)
	}
}

// The unsharded values must still start, or the guard would break clustering for
// every deployment that does not use sharding.
func TestServerStartsWithSingleShard(t *testing.T) {
	for _, shards := range []int{0, 1} {
		srv, err := New(shardTestConfig(t, shards))
		if err != nil {
			t.Fatalf("metadata_shards=%d refused: %v", shards, err)
		}
		srv.Close()
	}
}

// A supported shard count on a store with no objects starts: sharding is created
// on a fresh cluster, not migrated onto an existing one.
func TestServerStartsShardedOnAnEmptyStore(t *testing.T) {
	srv, err := New(shardTestConfig(t, 4))
	if err != nil {
		t.Fatalf("sharded server refused to start on an empty store: %v", err)
	}
	srv.Close()
}
