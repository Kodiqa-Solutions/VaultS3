package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
)

func runCluster(args []string) {
	if len(args) == 0 {
		fmt.Println(`Usage: vaults3-cli cluster <subcommand>

Subcommands:
  status                       Show cluster members, leader, and drain state
  shards                       Show how object metadata is distributed
  join <nodeId> <raftAddr>     Add a member (run against the leader)
  leave <nodeId>               Remove a member (run against the leader)
  drain [nodeId]               Stop a node accepting writes (defaults to the node served)
  undrain [nodeId]             Resume writes on a node
  rebalance                    Move objects to their correct owner after membership changes
  decommission <nodeId>        Drain + rebalance a node so it can be safely replaced`)
		os.Exit(1)
	}

	requireCreds()

	switch args[0] {
	case "status":
		clusterStatus()
	case "shards":
		clusterShards()
	case "join":
		if len(args) < 3 {
			fatal("usage: vaults3-cli cluster join <nodeId> <raftAddr>")
		}
		clusterJoin(args[1], args[2])
	case "leave":
		if len(args) < 2 {
			fatal("usage: vaults3-cli cluster leave <nodeId>")
		}
		clusterLeave(args[1])
	case "drain":
		clusterDrain(argOrEmpty(args, 1), true)
	case "undrain":
		clusterDrain(argOrEmpty(args, 1), false)
	case "rebalance":
		clusterRebalance()
	case "decommission":
		if len(args) < 2 {
			fatal("usage: vaults3-cli cluster decommission <nodeId>")
		}
		clusterDecommission(args[1])
	default:
		fatal("unknown cluster subcommand: " + args[0])
	}
}

func argOrEmpty(args []string, i int) string {
	if len(args) > i {
		return args[i]
	}
	return ""
}

// clusterPost sends an admin POST with an optional JSON body and returns the
// decoded response, exiting on any error or non-2xx status.
func clusterPost(path string, body any) map[string]any {
	var rdr io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	}
	resp, err := apiRequest("POST", path, rdr)
	if err != nil {
		fatal(err.Error())
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		fatal(fmt.Sprintf("HTTP %d: %s", resp.StatusCode, string(raw)))
	}
	var out map[string]any
	json.Unmarshal(raw, &out)
	return out
}

// clusterShards prints how object metadata is distributed: the committed
// assignment, and the shard groups actually running on this node. On a cluster
// that is not sharded it says so plainly, including the consequence, because
// "metadata is replicated to every node" is the thing that decides how many
// objects a cluster can hold (issue #50).
func clusterShards() {
	resp, err := apiRequest("GET", "/cluster/shards", nil)
	if err != nil {
		fatal(err.Error())
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		fatal(fmt.Sprintf("HTTP %d: %s", resp.StatusCode, string(raw)))
	}
	var out struct {
		Clustered bool   `json:"clustered"`
		Sharded   bool   `json:"sharded"`
		SelfID    string `json:"selfId"`
		ShardMap  struct {
			Version  uint64     `json:"version"`
			Epoch    uint64     `json:"epoch"`
			Shards   int        `json:"shards"`
			Replicas int        `json:"replicas"`
			Members  [][]string `json:"members"`
			Founders [][]string `json:"founders"`
		} `json:"shardMap"`
		LocalShards []struct {
			Shard    int      `json:"shard"`
			IsLeader bool     `json:"isLeader"`
			LeaderID string   `json:"leaderId"`
			Members  []string `json:"members"`
		} `json:"localShards"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		fatal("bad response: " + err.Error())
	}
	if !out.Clustered {
		fmt.Println("Not clustered. All metadata is held by this node.")
		return
	}
	if !out.Sharded {
		fmt.Println("Metadata sharding: not enabled")
		fmt.Println("Every node holds a complete copy of the object metadata, so adding")
		fmt.Println("nodes adds capacity for object data but not for metadata. Budget about")
		fmt.Println("600 bytes per object, per node. See docs/SCALING.md.")
		return
	}
	m := out.ShardMap
	fmt.Printf("Metadata sharding: %d shards, %d replicas each (map version %d, epoch %d)\n\n",
		m.Shards, m.Replicas, m.Version, m.Epoch)
	rows := make([][]string, 0, m.Shards)
	for i := 0; i < m.Shards && i < len(m.Members); i++ {
		here := ""
		for _, id := range m.Members[i] {
			if id == out.SelfID {
				here = "yes"
			}
		}
		founders := ""
		if i < len(m.Founders) {
			founders = strings.Join(m.Founders[i], ", ")
		}
		rows = append(rows, []string{
			fmt.Sprintf("%d", i),
			strings.Join(m.Members[i], ", "),
			founders,
			here,
		})
	}
	printTable([]string{"SHARD", "MEMBERS", "FOUNDERS", "LOCAL"}, rows)

	// The groups actually running here. Their membership is what the reconciler
	// drives towards the assignment above, so a difference between the two tables
	// is a membership change still in flight rather than a fault.
	if len(out.LocalShards) == 0 {
		fmt.Printf("\nThis node (%s) is running no metadata shard groups yet.\n", out.SelfID)
		return
	}
	fmt.Printf("\nShard groups running on %s:\n\n", out.SelfID)
	local := make([][]string, 0, len(out.LocalShards))
	for _, g := range out.LocalShards {
		role := "follower"
		if g.IsLeader {
			role = "leader"
		}
		leader := g.LeaderID
		if leader == "" {
			leader = "(none)"
		}
		local = append(local, []string{
			fmt.Sprintf("%d", g.Shard),
			role,
			leader,
			strings.Join(g.Members, ", "),
		})
	}
	printTable([]string{"SHARD", "ROLE", "LEADER", "GROUP MEMBERS"}, local)
}

func clusterStatus() {
	resp, err := apiRequest("GET", "/cluster/status", nil)
	if err != nil {
		fatal(err.Error())
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		fatal(fmt.Sprintf("HTTP %d: %s", resp.StatusCode, string(body)))
	}

	var st struct {
		Clustered bool   `json:"clustered"`
		SelfID    string `json:"selfId"`
		LeaderID  string `json:"leaderId"`
		IsLeader  bool   `json:"isLeader"`
		Writable  bool   `json:"writable"`
		Members   []struct {
			NodeID   string `json:"nodeId"`
			Address  string `json:"address"`
			Suffrage string `json:"suffrage"`
			Leader   bool   `json:"leader"`
		} `json:"members"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		fatal("parse response: " + err.Error())
	}

	if !st.Clustered {
		writable := "writable"
		if !st.Writable {
			writable = "draining (writes rejected)"
		}
		fmt.Printf("This node is running standalone (not clustered). Write state: %s\n", writable)
		return
	}

	fmt.Printf("Cluster: self=%s leader=%s  this node: %s\n", st.SelfID, st.LeaderID, writeState(st.Writable))
	headers := []string{"NODE ID", "RAFT ADDRESS", "SUFFRAGE", "ROLE"}
	var rows [][]string
	for _, m := range st.Members {
		role := "follower"
		if m.Leader {
			role = "leader"
		}
		rows = append(rows, []string{m.NodeID, m.Address, m.Suffrage, role})
	}
	printTable(headers, rows)
}

func writeState(writable bool) string {
	if writable {
		return "writable"
	}
	return "draining (writes rejected)"
}

func clusterJoin(nodeID, addr string) {
	out := clusterPost("/cluster/join", map[string]string{"nodeId": nodeID, "addr": addr})
	fmt.Println(msgOr(out, "node "+nodeID+" joined"))
}

func clusterLeave(nodeID string) {
	out := clusterPost("/cluster/leave", map[string]string{"nodeId": nodeID})
	fmt.Println(msgOr(out, "node "+nodeID+" removed"))
}

func clusterDrain(nodeID string, drain bool) {
	path := "/cluster/undrain"
	if drain {
		path = "/cluster/drain"
	}
	var body any
	if nodeID != "" {
		body = map[string]string{"nodeId": nodeID}
	}
	out := clusterPost(path, body)
	target := fmt.Sprintf("%v", out["nodeId"])
	if target == "" || target == "<nil>" {
		target = "this node"
	}
	if drain {
		fmt.Printf("Draining %s: writes are now rejected, reads continue.\n", target)
	} else {
		fmt.Printf("Resumed writes on %s.\n", target)
	}
}

func clusterRebalance() {
	out := clusterPost("/cluster/rebalance", nil)
	running := out["running"] == true
	fmt.Printf("Rebalance triggered (running=%v). Objects are moving to their correct owner in the background.\n", running)
}

func clusterDecommission(nodeID string) {
	fmt.Printf("Decommissioning %s: this drains the node and triggers a rebalance so its\n", nodeID)
	fmt.Println("data moves to the remaining members. It does NOT remove the node — verify the")
	fmt.Println("data has moved (vaults3-cli info / cluster status), then run:")
	fmt.Printf("  vaults3-cli cluster leave %s\n\n", nodeID)
	fmt.Println("Zero-data-loss requires replica_count >= 2 (replicas already exist elsewhere).")

	clusterPost("/cluster/drain", map[string]string{"nodeId": nodeID})
	fmt.Printf("- %s drained (no new writes)\n", nodeID)
	clusterPost("/cluster/rebalance", nil)
	fmt.Println("- rebalance triggered")
	fmt.Println("\nWatch progress, then leave the node when its data has moved.")
}

// msgOr returns the response "message" field, or a fallback.
func msgOr(out map[string]any, fallback string) string {
	if m, ok := out["message"].(string); ok && m != "" {
		return m
	}
	return fallback
}
