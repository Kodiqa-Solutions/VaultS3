package cluster

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
	"github.com/hashicorp/raft"
)

func newShardFSM(t *testing.T) (*FSM, *metadata.Store) {
	t.Helper()
	store, err := metadata.NewStore(filepath.Join(t.TempDir(), "meta.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return NewFSM(store), store
}

// applyMap pushes a map through the state machine exactly as Raft would.
func applyMap(t *testing.T, f *FSM, m *ShardMap) error {
	t.Helper()
	data, err := marshalCommand(CmdPutShardMap, m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	resp := f.Apply(&raft.Log{Type: raft.LogCommand, Index: 1, Data: data})
	if resp == nil {
		return nil
	}
	err, _ = resp.(error)
	return err
}

func TestShardMapCommitRoundTrips(t *testing.T) {
	f, store := newShardFSM(t)
	m, err := BuildShardMap(8, 3, ringOf("n1", "n2", "n3"), 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := applyMap(t, f, m); err != nil {
		t.Fatalf("apply: %v", err)
	}
	raw, err := store.GetShardMap()
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	var got ShardMap
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.Equal(m) || got.Epoch != m.Epoch || got.Version != m.Version {
		t.Fatalf("stored map differs from the committed one: %+v vs %+v", got, m)
	}
	for i := range got.Founders {
		if strings.Join(got.Founders[i], ",") != strings.Join(m.Members[i], ",") {
			t.Fatalf("shard %d founders %v, want the creating members %v", i, got.Founders[i], m.Members[i])
		}
	}
}

func TestShardMapIsAbsentBeforeAnyCommit(t *testing.T) {
	_, store := newShardFSM(t)
	raw, err := store.GetShardMap()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(raw) != 0 {
		t.Fatalf("uncommitted cluster reports a shard map: %q", raw)
	}
}

// The immutable parts are immutable because breaking them is unrecoverable: two
// Raft groups would serve one shard, each authoritative for metadata the other
// cannot see. The state machine, not the proposer, must be what refuses.
func TestShardMapRejectsChangesToItsIdentity(t *testing.T) {
	base, err := BuildShardMap(8, 3, ringOf("n1", "n2", "n3"), 1)
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name    string
		mutate  func(*ShardMap)
		wantErr string
	}{
		// The realistic way this happens is a node configured with a different
		// metadata_shards proposing a perfectly well-formed map of its own size.
		// It must be refused for being a different assignment, not for being
		// malformed, because it is not malformed.
		{"shard count", func(m *ShardMap) {
			other, err := BuildShardMap(16, 3, ringOf("n1", "n2", "n3"), 2)
			if err != nil {
				t.Fatal(err)
			}
			other.Epoch = m.Epoch
			*m = *other
		}, "shard count cannot change"},
		{"epoch", func(m *ShardMap) { m.Epoch = 2 }, "epoch cannot change"},
		{"founding set", func(m *ShardMap) { m.Founders[0] = []string{"intruder", "n2", "n3"} }, "founding set of shard 0 cannot change"},
		{"stale version", func(m *ShardMap) { m.Version = 1 }, "does not advance"},
		{"lower version", func(m *ShardMap) { m.Version = 0 }, "does not advance"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, store := newShardFSM(t)
			if err := applyMap(t, f, base); err != nil {
				t.Fatalf("commit the first map: %v", err)
			}
			next := base.WithMembers(base.Members, 2)
			tc.mutate(next)

			err := applyMap(t, f, next)
			if err == nil {
				t.Fatal("the state machine accepted a change it must refuse")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q does not mention %q", err, tc.wantErr)
			}
			// The committed map must be untouched, not partially updated.
			raw, _ := store.GetShardMap()
			var stored ShardMap
			json.Unmarshal(raw, &stored)
			if stored.Version != 1 || stored.Epoch != 1 || stored.Shards != 8 {
				t.Fatalf("a rejected proposal changed the committed map: %+v", stored)
			}
		})
	}
}

// Membership is the one part a later version may change, since nodes come and go.
func TestShardMapAcceptsAMembershipChange(t *testing.T) {
	f, store := newShardFSM(t)
	base, err := BuildShardMap(4, 3, ringOf("n1", "n2", "n3"), 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := applyMap(t, f, base); err != nil {
		t.Fatal(err)
	}

	moved := make([][]string, len(base.Members))
	for i := range base.Members {
		moved[i] = append([]string(nil), base.Members[i]...)
		moved[i][0] = "n4"
	}
	next := base.WithMembers(moved, 2)
	if err := applyMap(t, f, next); err != nil {
		t.Fatalf("a membership change must be allowed: %v", err)
	}

	raw, _ := store.GetShardMap()
	var stored ShardMap
	json.Unmarshal(raw, &stored)
	if stored.Version != 2 || stored.Members[0][0] != "n4" {
		t.Fatalf("membership change not applied: %+v", stored)
	}
	// Founders stay frozen at creation, which is what stops n4 from bootstrapping
	// a rival group for a shard it just joined.
	if stored.Founders[0][0] == "n4" {
		t.Fatal("a node that joined later was recorded as a founder")
	}
	if stored.IsFounder(0, "n4") {
		t.Fatal("a node that joined later may not bootstrap the shard's group")
	}
	if !stored.IsFounder(0, base.Members[0][0]) {
		t.Fatalf("the creating member %q is no longer a founder", base.Members[0][0])
	}
}

func TestShardMapRejectsMalformedProposals(t *testing.T) {
	f, _ := newShardFSM(t)
	noEpoch, _ := BuildShardMap(4, 3, ringOf("n1", "n2", "n3"), 1)
	noEpoch.Epoch = 0
	if err := applyMap(t, f, noEpoch); err == nil {
		t.Fatal("accepted a map with no epoch")
	}

	noFounders, _ := BuildShardMap(4, 3, ringOf("n1", "n2", "n3"), 1)
	noFounders.Founders = nil
	if err := applyMap(t, f, noFounders); err == nil {
		t.Fatal("accepted a map with no founding sets")
	}
}
