package main

import (
	"bufio"
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
  leave <nodeId> [--yes]       Remove a member (run against the leader), asks first
  drain [nodeId]               Stop a node accepting writes (defaults to the node served)
  undrain [nodeId]             Resume writes on a node
  rebalance                    Retired: replica repair restores placement (use cluster repair)
  repair [--status]            Restore replica counts after a node was lost for good
  decommission <nodeId> [--yes]
                               Drain + rebalance a node so it can be safely replaced, asks first

--yes (or -y) skips the confirmation. Without a terminal to ask on, it is required.`)
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
		nodeID, yes := nodeAndYes(args[1:], "usage: vaults3-cli cluster leave <nodeId> [--yes]")
		confirmClusterChange(fmt.Sprintf("This removes node %s from the cluster membership.", nodeID), yes)
		clusterLeave(nodeID)
	case "drain":
		clusterDrain(argOrEmpty(args, 1), true)
	case "undrain":
		clusterDrain(argOrEmpty(args, 1), false)
	case "rebalance":
		clusterRebalance()
	case "repair":
		if argOrEmpty(args, 1) == "--status" {
			clusterRepairStatus()
			return
		}
		clusterRepair()
	case "decommission":
		nodeID, yes := nodeAndYes(args[1:], "usage: vaults3-cli cluster decommission <nodeId> [--yes]")
		confirmClusterChange(fmt.Sprintf("This drains node %s (it stops accepting writes).", nodeID), yes)
		clusterDecommission(nodeID)
	default:
		fatal("unknown cluster subcommand: " + args[0])
	}
}

// nodeAndYes reads "<nodeId> [--yes]" in either order and refuses anything else.
func nodeAndYes(args []string, usage string) (string, bool) {
	nodeID, yes := "", false
	for _, a := range args {
		switch {
		case a == "--yes" || a == "-y":
			yes = true
		case strings.HasPrefix(a, "-"):
			fatal("unknown flag: " + a + "\n" + usage)
		case nodeID == "":
			nodeID = a
		default:
			fatal("unexpected argument: " + a + "\n" + usage)
		}
	}
	if nodeID == "" {
		fatal(usage)
	}
	return nodeID, yes
}

// confirmInput and stdinIsTerminal are variables so a test can answer the
// prompt and choose whether there is a terminal to ask on.
var (
	confirmInput    io.Reader = os.Stdin
	stdinIsTerminal           = func() bool {
		fi, err := os.Stdin.Stat()
		return err == nil && fi.Mode()&os.ModeCharDevice != 0
	}
)

// confirmClusterChange asks before a membership change, the way storage
// reclaim --apply does. Removing or draining the wrong node of a cluster is
// hard to undo, and these ran on the first keystroke. With no terminal to ask
// on, such as in a script, --yes is required rather than taken for granted.
func confirmClusterChange(what string, assumeYes bool) {
	if assumeYes {
		return
	}
	if !stdinIsTerminal() {
		fatal(what + " Pass --yes to confirm, there is no terminal to ask on")
	}
	fmt.Print(what + " Continue? [y/N]: ")
	answer, _ := bufio.NewReader(confirmInput).ReadString('\n')
	if !strings.EqualFold(strings.TrimSpace(answer), "y") {
		fatal("aborted, nothing was changed")
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

// clusterGet reads a cluster admin endpoint that answers with a JSON object.
func clusterGet(path string) map[string]any {
	resp, err := apiRequest("GET", path, nil)
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

// clusterRebalance reports that rebalance is retired. Its data movement never
// worked (every transfer was refused) and, had it worked, it would have deleted
// valid replicas. Replica repair restores placement and copy counts instead.
func clusterRebalance() {
	out := clusterPost("/cluster/rebalance", nil)
	fmt.Println(msgOr(out, "Rebalance no longer moves data. Replica repair restores every object's placement and copy count: run `vaults3-cli cluster repair`."))
}

func clusterRepair() {
	clusterPost("/cluster/repair", nil)
	fmt.Println("Replica repair triggered. Objects holding fewer copies than their bucket asks")
	fmt.Println("for are being topped up in the background, on the node that owns each one.")
	fmt.Println("Run `vaults3-cli cluster repair --status` to see what the last scan found.")
}

func clusterRepairStatus() {
	out := clusterGet("/cluster/repair")
	num := func(k string) int64 {
		if f, ok := out[k].(float64); ok {
			return int64(f)
		}
		return 0
	}
	lastRun, ok := out["lastRun"].(string)
	if !ok || lastRun == "" {
		lastRun = "never (no scan has finished on this node yet)"
	}
	fmt.Printf("Last scan:     %s\n", lastRun)
	fmt.Printf("Scanned:       %d objects owned by this node\n", num("scanned"))
	fmt.Printf("Repaired:      %d (%d bytes copied)\n", num("repaired"), num("bytesCopied"))
	fmt.Printf("Undecidable:   %d (a holder could not be reached, so nothing was concluded)\n", num("undecidable"))
	fmt.Printf("Unrecoverable: %d (no node still has the data)\n", num("unrecoverable"))
	if num("unrecoverable") > 0 {
		fmt.Println("\nUnrecoverable objects are still listed in metadata and nothing was deleted.")
		fmt.Println("Check the server log for the affected keys.")
	}
}

func clusterDecommission(nodeID string) {
	fmt.Printf("Decommissioning %s: this drains the node so it takes no new writes. It does NOT\n", nodeID)
	fmt.Println("remove the node. Next, remove it, then let replica repair re-create on the")
	fmt.Println("remaining members every copy it held:")
	fmt.Printf("  vaults3-cli cluster leave %s\n", nodeID)
	fmt.Println("  vaults3-cli cluster repair")
	fmt.Println("  vaults3-cli cluster repair --status   # until repaired settles at 0")
	fmt.Println("\nZero data loss requires replica_count >= 2: the copies repair re-creates come")
	fmt.Println("from the ones the other members already hold.")

	clusterPost("/cluster/drain", map[string]string{"nodeId": nodeID})
	fmt.Printf("- %s drained (no new writes)\n", nodeID)
}

// msgOr returns the response "message" field, or a fallback.
func msgOr(out map[string]any, fallback string) string {
	if m, ok := out["message"].(string); ok && m != "" {
		return m
	}
	return fallback
}
