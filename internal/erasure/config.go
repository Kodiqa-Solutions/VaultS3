package erasure

import "github.com/Kodiqa-Solutions/VaultS3/internal/config"

// Config is an alias for config.ErasureConfig.
type Config = config.ErasureConfig

func applyDefaults(c *Config) {
	if c.DataShards <= 0 {
		c.DataShards = 4
	}
	if c.ParityShards <= 0 {
		c.ParityShards = 2
	}
	if c.BlockSize <= 0 {
		c.BlockSize = 4 * 1024 * 1024 // 4MB
	}
}

func totalShards(c *Config) int {
	return c.DataShards + c.ParityShards
}
