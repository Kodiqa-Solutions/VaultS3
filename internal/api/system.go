package api

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net/http"
	"runtime"
	"time"

	"github.com/Kodiqa-Solutions/VaultS3/internal/sysinfo"
)

// clusterSecretHeader authenticates the inter-node /cluster/sysinfo request,
// matching the cluster package's convention.
const clusterSecretHeader = "X-Cluster-Secret"

// NodeSystemInfo is one node's version, capacity, and object usage. The cluster
// fields (NodeID/Address/Reachable) are omitted from the single-node
// /api/v1/system response and populated only in the cluster rollup.
//
// Three different sizes are reported deliberately, because conflating them is
// what made issue #43 look like a bug:
//
//	ObjectBytes — logical: each object's current version counted once, cluster-wide.
//	Usage       — physical: what VaultS3's own directories occupy on this node.
//	Disk        — the whole filesystem, VaultS3's share and everyone else's.
type NodeSystemInfo struct {
	NodeID      string       `json:"nodeId,omitempty"`
	Address     string       `json:"address,omitempty"`
	Reachable   bool         `json:"reachable,omitempty"`
	Error       string       `json:"error,omitempty"` // why a peer is unreachable
	Version     string       `json:"version"`
	OS          string       `json:"os"`
	Arch        string       `json:"arch"`
	DataDirs    []string     `json:"dataDirs"`
	Disk        sysinfo.Disk `json:"disk"`
	ObjectBytes int64        `json:"objectBytes"`
	ObjectCount int64        `json:"objectCount"`
	BucketCount int          `json:"bucketCount"`
	// Usage is this node's measured footprint, nil until the first background
	// walk finishes (or always, if the walk is disabled). UsageScanning says a
	// walk is running now, so a dashboard can show "measuring" rather than "0".
	Usage         *sysinfo.Usage `json:"usage,omitempty"`
	UsageScanning bool           `json:"usageScanning,omitempty"`
}

// clusterInfoClient fetches peers' /api/v1/system for the cluster rollup. Inter
// -node TLS is commonly self-signed, so certificate verification is skipped for
// these internal, admin-authenticated calls.
var clusterInfoClient = &http.Client{
	Timeout:   5 * time.Second,
	Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}},
}

// storageDirs lists every directory VaultS3 writes to on this node. It backs
// both the capacity readout and the footprint walk, so anything missing here is
// storage the operator cannot see: the Raft directory used to be absent, which
// hid the log and snapshot growth on a clustered node (issue #43).
func (h *APIHandler) storageDirs() []string {
	if h.cfg == nil {
		return nil
	}
	dirs := []string{h.cfg.Storage.DataDir, h.cfg.Storage.MetadataDir}
	if h.cfg.Tiering.Enabled && h.cfg.Tiering.ColdDataDir != "" {
		dirs = append(dirs, h.cfg.Tiering.ColdDataDir)
	}
	if h.cfg.Erasure.Enabled {
		dirs = append(dirs, h.cfg.Erasure.DataDirs...)
	}
	if h.cfg.Cluster.Enabled && h.cfg.Cluster.DataDir != "" {
		dirs = append(dirs, h.cfg.Cluster.DataDir)
	}
	return uniqueNonEmpty(dirs)
}

// localSystemInfo gathers this node's version, on-disk capacity, and logical
// object usage.
func (h *APIHandler) localSystemInfo() NodeSystemInfo {
	dirs := h.storageDirs()

	var objectBytes, objectCount int64
	var bucketCount int
	if buckets, err := h.store.ListBuckets(); err == nil {
		bucketCount = len(buckets)
		for _, b := range buckets {
			size, count := h.bucketStatCounter(b.Name)
			objectBytes += size
			objectCount += count
		}
	}

	version := "dev"
	if h.updater != nil {
		if v := h.updater.LastStatus().Current; v != "" {
			version = v
		}
	}

	usage, scanning := h.diskUsageCache().Get()

	return NodeSystemInfo{
		Version:       version,
		OS:            runtime.GOOS,
		Arch:          runtime.GOARCH,
		DataDirs:      dirs,
		Disk:          sysinfo.DiskUsage(dirs),
		ObjectBytes:   objectBytes,
		ObjectCount:   objectCount,
		BucketCount:   bucketCount,
		Usage:         usage,
		UsageScanning: scanning,
	}
}

// DiskUsage returns this node's last completed footprint scan, or nil if none
// has finished yet or measurement is disabled. It never blocks, so it is safe
// on a /metrics scrape.
func (h *APIHandler) DiskUsage() *sysinfo.Usage {
	u, _ := h.diskUsageCache().Get()
	return u
}

// diskUsageCache returns the footprint cache, creating it on first use so the
// walk costs nothing until someone opens the dashboard. Returns nil (a usable
// no-op cache) when storage.usage_scan_interval_secs is 0.
func (h *APIHandler) diskUsageCache() *sysinfo.UsageCache {
	if h.cfg == nil {
		return nil
	}
	h.usageOnce.Do(func() {
		h.usage = sysinfo.NewUsageCache(
			h.storageDirs,
			time.Duration(h.cfg.Storage.UsageScanIntervalSecs)*time.Second,
		)
	})
	return h.usage
}

// handleSystemInfo handles GET /api/v1/system: this node's version, data
// directories, on-disk capacity (total/used/free), and logical object usage.
func (h *APIHandler) handleSystemInfo(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, h.localSystemInfo())
}

// ClusterSysInfoHandler serves this node's system info on the cluster channel
// (registered next to /cluster/status). The coordinator calls it peer-to-peer to
// build the capacity rollup, so it does not depend on the dashboard /api/v1 port.
// It is authenticated by the shared cluster secret when one is configured.
func (h *APIHandler) ClusterSysInfoHandler(secret string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !clusterAuthOK(r, secret) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		writeJSON(w, http.StatusOK, h.localSystemInfo())
	}
}

// handleClusterInfo handles GET /api/v1/cluster/info: the version and capacity of
// every node in the cluster, plus aggregate totals — a cluster-wide equivalent of
// `mc admin info`. On a single node it returns just this node.
func (h *APIHandler) handleClusterInfo(w http.ResponseWriter, _ *http.Request) {
	self := h.localSystemInfo()
	self.NodeID = h.clusterSelfID
	self.Reachable = true
	nodes := []NodeSystemInfo{self}

	if h.clusterNodeAddrs != nil {
		for id, addr := range h.clusterNodeAddrs() {
			if id == h.clusterSelfID || addr == "" {
				continue
			}
			nodes = append(nodes, h.fetchPeerSystemInfo(id, addr))
		}
	}

	// Aggregate physical disk across reachable nodes (replicas legitimately use
	// disk on multiple nodes, so this is the true "how full is the cluster").
	// VaultS3's own footprint sums the same way, but only over the nodes that
	// have finished a walk: measuredNodes says how much of the cluster the
	// number covers, because a partial sum silently understates the total.
	var totalDisk sysinfo.Disk
	var vaultBytes, vaultFiles uint64
	reachable, measured := 0, 0
	for _, n := range nodes {
		if !n.Reachable {
			continue
		}
		reachable++
		totalDisk.TotalBytes += n.Disk.TotalBytes
		totalDisk.UsedBytes += n.Disk.UsedBytes
		totalDisk.FreeBytes += n.Disk.FreeBytes
		if n.Usage != nil {
			measured++
			vaultBytes += n.Usage.Bytes
			vaultFiles += n.Usage.Files
		}
	}

	// Logical usage is NOT summed. Object metadata is replicated by Raft, so every
	// node reports the same cluster-wide totals, and adding them up multiplied the
	// figure by the node count: a 12-node cluster holding 82.2 GB in 396k objects
	// reported 986.4 GB in 4.75M objects, exactly 12x (issue #43). Take it from
	// this node, which already has the whole picture.
	objectBytes, objectCount := self.ObjectBytes, self.ObjectCount

	writeJSON(w, http.StatusOK, map[string]any{
		"clustered":      h.clusterSelfID != "",
		"nodeCount":      len(nodes),
		"reachableNodes": reachable,
		"nodes":          nodes,
		"totals": map[string]any{
			"disk":          totalDisk,
			"objectBytes":   objectBytes,
			"objectCount":   objectCount,
			"vaultBytes":    vaultBytes,
			"vaultFiles":    vaultFiles,
			"measuredNodes": measured,
		},
	})
}

// fetchPeerSystemInfo reads a peer's capacity over the cluster channel
// (/cluster/sysinfo, the same address the object-placement proxy already reaches
// for S3 forwarding). This avoids the dashboard /api/v1 port and admin login,
// which are not reachable peer-to-peer in split-port or proxied deployments. An
// unreachable peer is returned with Reachable=false rather than failing the
// whole rollup.
func (h *APIHandler) fetchPeerSystemInfo(id, addr string) NodeSystemInfo {
	ni := NodeSystemInfo{NodeID: id, Address: addr}
	scheme := "http"
	if h.cfg != nil && h.cfg.Server.TLS.Enabled {
		scheme = "https"
	}

	req, err := http.NewRequest(http.MethodGet, scheme+"://"+addr+"/cluster/sysinfo", nil)
	if err != nil {
		ni.Error = err.Error()
		return ni
	}
	if h.clusterSecret != "" {
		req.Header.Set(clusterSecretHeader, h.clusterSecret)
	}
	resp, err := clusterInfoClient.Do(req)
	if err != nil {
		ni.Error = err.Error()
		return ni
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		ni.Error = fmt.Sprintf("cluster/sysinfo returned HTTP %d", resp.StatusCode)
		return ni
	}
	if err := json.NewDecoder(resp.Body).Decode(&ni); err != nil {
		ni.Error = err.Error()
		return ni
	}
	ni.NodeID, ni.Address, ni.Reachable, ni.Error = id, addr, true, ""
	return ni
}

func uniqueNonEmpty(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}
