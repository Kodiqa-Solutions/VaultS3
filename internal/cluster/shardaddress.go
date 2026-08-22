package cluster

import (
	"fmt"

	"github.com/hashicorp/raft"
)

// Raft records a member's address when it joins and keeps using it. That is fine
// for a fixed host, and wrong for Kubernetes, where the address is a pod IP that
// changes on every restart. The control group survives this because auto-join
// re-announces the current address there on every boot, which rewrites the
// configuration.
//
// Nothing re-announces into a shard group. Without help, one rolling restart
// would leave every shard configuration pointing at addresses that no longer
// exist, and the groups would never form a quorum again.
//
// So shard transports resolve addresses through the control group, which is the
// membership that stays current, instead of trusting what is written in their
// own configuration. Shard traffic goes to the same port as control traffic (it
// is demultiplexed on arrival), so the control group's address for a node is
// exactly the address its shard groups need.
type controlAddressProvider struct {
	// servers returns the control group's current membership.
	servers func() ([]raft.Server, error)
}

// ShardAddressProvider returns an address provider backed by this node's view of
// the control group.
func (n *Node) ShardAddressProvider() raft.ServerAddressProvider {
	return &controlAddressProvider{servers: n.Servers}
}

// ServerAddr resolves a member id to its current address.
//
// An unknown id is an error, never a guess. Returning a stale or invented
// address would make Raft dial a node that is not there and report the member as
// merely unreachable, which reads as a network problem rather than the
// membership problem it is.
func (p *controlAddressProvider) ServerAddr(id raft.ServerID) (raft.ServerAddress, error) {
	servers, err := p.servers()
	if err != nil {
		return "", fmt.Errorf("cluster: resolve address of %s: %w", id, err)
	}
	for _, s := range servers {
		if s.ID == id {
			return s.Address, nil
		}
	}
	return "", fmt.Errorf("cluster: %w: no current address for member %s", ErrShardUnavailable, id)
}

// staticAddressProvider resolves from a fixed table. Used by tests, and by the
// founder bootstrap path before a group has any membership of its own.
type staticAddressProvider struct {
	addrs map[string]string
}

func (p *staticAddressProvider) ServerAddr(id raft.ServerID) (raft.ServerAddress, error) {
	if addr, ok := p.addrs[string(id)]; ok {
		return raft.ServerAddress(addr), nil
	}
	return "", fmt.Errorf("cluster: %w: no address configured for member %s", ErrShardUnavailable, id)
}
