package api

import (
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
)

func shardsResponse(t *testing.T, fc *fakeCluster) map[string]any {
	t.Helper()
	h, _ := newTestAPI(t)
	writable := &atomic.Bool{}
	writable.Store(true)
	h.SetWritable(writable)
	if fc != nil {
		h.SetClusterController(fc, func() {}, func() bool { return false })
	}
	tok := getToken(t, h)
	rr := doRequest(h, "GET", "/cluster/shards", nil, tok)
	if rr.Code != 200 {
		t.Fatalf("GET /cluster/shards: HTTP %d (%s)", rr.Code, rr.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v (%s)", err, rr.Body.String())
	}
	return out
}

// A cluster that replicates all metadata must report that, not an empty
// assignment. "Sharded with nothing in it" is the reading that would let an
// operator believe their metadata is distributed when it is not.
func TestClusterShardsReportsUnshardedHonestly(t *testing.T) {
	out := shardsResponse(t, &fakeCluster{self: "node-0", leader: "node-0", isLeader: true})
	if out["clustered"] != true {
		t.Fatalf("clustered=%v, want true", out["clustered"])
	}
	if out["sharded"] != false {
		t.Fatalf("sharded=%v, want false", out["sharded"])
	}
	if _, ok := out["shardMap"]; ok {
		t.Fatal("an unsharded cluster reported a shard map")
	}
	note, _ := out["note"].(string)
	if !strings.Contains(note, "metadata") {
		t.Fatalf("no explanation of what unsharded means: %q", note)
	}
}

func TestClusterShardsOnASingleNode(t *testing.T) {
	out := shardsResponse(t, nil)
	if out["clustered"] != false || out["sharded"] != false {
		t.Fatalf("standalone node reported %+v", out)
	}
}

func TestClusterShardsReportsACommittedMap(t *testing.T) {
	fc := &fakeCluster{
		self: "node-1", leader: "node-0", isLeader: false,
		shardMap: &ShardAssignment{
			Version: 3, Epoch: 1, Shards: 2, Replicas: 3,
			Members:  [][]string{{"node-0", "node-1", "node-2"}, {"node-1", "node-2", "node-3"}},
			Founders: [][]string{{"node-0", "node-1", "node-2"}, {"node-0", "node-1", "node-2"}},
		},
	}
	out := shardsResponse(t, fc)
	if out["sharded"] != true {
		t.Fatalf("sharded=%v, want true", out["sharded"])
	}
	m, ok := out["shardMap"].(map[string]any)
	if !ok {
		t.Fatalf("no shard map in %+v", out)
	}
	if m["shards"].(float64) != 2 || m["epoch"].(float64) != 1 || m["version"].(float64) != 3 {
		t.Fatalf("shard map fields lost in transit: %+v", m)
	}
	// Founders must survive the hop: they are what an operator checks when a
	// shard will not form.
	founders, ok := m["founders"].([]any)
	if !ok || len(founders) != 2 {
		t.Fatalf("founders not reported: %+v", m["founders"])
	}
}
