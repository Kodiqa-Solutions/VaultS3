package cluster

import "github.com/Kodiqa-Solutions/VaultS3/internal/config"

// PlacementConfig is an alias for config.PlacementConfig.
type PlacementConfig = config.PlacementConfig

func applyPlacementDefaults(p *PlacementConfig) {
	if p.ReplicaCount <= 0 {
		p.ReplicaCount = 1
	}
	if p.ReadQuorum <= 0 {
		p.ReadQuorum = 1
	}
	if p.WriteQuorum <= 0 {
		p.WriteQuorum = 1
	}
	if p.VirtualNodes <= 0 {
		p.VirtualNodes = defaultVnodes
	}
}
