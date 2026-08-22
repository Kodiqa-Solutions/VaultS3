package cluster

import (
	"errors"
	"testing"

	"github.com/hashicorp/raft"
)

// Shard groups must resolve addresses through the control group, because that
// is the membership something keeps current. Trusting an address recorded at
// join time would strand every shard after a rolling restart, when the pods come
// back on new IPs.
func TestAddressProviderFollowsTheLiveMembership(t *testing.T) {
	current := []raft.Server{
		{ID: "node-0", Address: "10.0.0.1:9001"},
		{ID: "node-1", Address: "10.0.0.2:9001"},
	}
	p := &controlAddressProvider{servers: func() ([]raft.Server, error) { return current, nil }}

	addr, err := p.ServerAddr("node-1")
	if err != nil || addr != "10.0.0.2:9001" {
		t.Fatalf("resolved %q, %v", addr, err)
	}

	// The pod restarts with a new IP and re-announces into the control group.
	current[1].Address = "10.0.0.9:9001"
	addr, err = p.ServerAddr("node-1")
	if err != nil {
		t.Fatalf("resolve after the address changed: %v", err)
	}
	if addr != "10.0.0.9:9001" {
		t.Fatalf("resolved the stale address %q; shard groups would dial a pod that is gone", addr)
	}
}

// An unknown member is an error, never a guess: a fabricated address would make
// Raft report a membership problem as an unreachable peer.
func TestAddressProviderRefusesToInventAnAddress(t *testing.T) {
	p := &controlAddressProvider{
		servers: func() ([]raft.Server, error) { return []raft.Server{{ID: "node-0", Address: "10.0.0.1:9001"}}, nil },
	}
	if _, err := p.ServerAddr("node-7"); err == nil {
		t.Fatal("an unknown member resolved to an address")
	} else if !errors.Is(err, ErrShardUnavailable) {
		t.Fatalf("error %v does not report the shard as unavailable", err)
	}

	failing := &controlAddressProvider{servers: func() ([]raft.Server, error) { return nil, errors.New("no configuration") }}
	if _, err := failing.ServerAddr("node-0"); err == nil {
		t.Fatal("an unreadable membership resolved to an address")
	}
}

// The provider a real node hands to its shard transports must resolve that
// cluster's actual members.
func TestNodeAddressProviderResolvesRealMembers(t *testing.T) {
	nodes := newRaftCluster(t, 3)
	ld := mustLeader(t, nodes)
	p := ld.node.ShardAddressProvider()

	servers, err := ld.node.Servers()
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range servers {
		got, err := p.ServerAddr(s.ID)
		if err != nil {
			t.Fatalf("resolve %s: %v", s.ID, err)
		}
		if got != s.Address {
			t.Fatalf("resolved %s to %q, the control group says %q", s.ID, got, s.Address)
		}
	}
	if _, err := p.ServerAddr("not-a-member"); err == nil {
		t.Fatal("a non-member resolved to an address")
	}
}

// A Raft address carries the raft port and says nothing about the API port, so
// the conversion has to use the API port the node actually serves on. Assuming
// 9000 meant a cluster on any other port could not forward a write to its
// leader at all.
func TestAPIAddrFromRaftUsesTheConfiguredPort(t *testing.T) {
	cases := []struct {
		raftAddr string
		apiPort  int
		want     string
	}{
		{"10.0.0.7:9001", 9000, "10.0.0.7:9000"},
		{"10.0.0.7:9001", 0, "10.0.0.7:9000"}, // unset keeps the historic default
		{"10.0.0.7:7001", 8080, "10.0.0.7:8080"},
		{"127.0.0.1:9202", 9101, "127.0.0.1:9101"},
	}
	for _, c := range cases {
		if got := apiAddrFromRaft(c.raftAddr, c.apiPort); got != c.want {
			t.Errorf("apiAddrFromRaft(%q, %d) = %q, want %q", c.raftAddr, c.apiPort, got, c.want)
		}
	}
}
