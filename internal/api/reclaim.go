package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
	"github.com/Kodiqa-Solutions/VaultS3/internal/reclaim"
)

// defaultReclaimMinAge protects data written recently enough that its metadata
// commit may still be in flight. A PUT lands the bytes before the metadata is
// committed, so a brand new object is indistinguishable from an orphan; a day of
// slack makes that race unreachable while still freeing anything genuinely stale.
const defaultReclaimMinAge = 24 * time.Hour

// maxReclaimMinAgeHours is a sanity bound so a typo cannot mean "scan nothing"
// silently for a decade.
const maxReclaimMinAgeHours = 24 * 365

// reclaimClient talks to peers over the cluster channel. A full scan walks every
// data file on a node, so the timeout is generous compared with the other
// inter-node calls.
var reclaimClient = &http.Client{Timeout: 30 * time.Minute}

// reclaimLookup answers reclaim's "does metadata still refer to this?" questions.
// Object metadata comes from the Raft-replicated store, so every node sees the
// whole cluster's objects and a file with no entry anywhere is genuinely
// unreachable. Multipart state is deliberately node-local (issue #32), so uploads
// are answered from the local store.
type reclaimLookup struct {
	store metadata.StoreAPI
	local metadata.StoreAPI
}

// A failed lookup reports Unknown, never Absent. The scanner deletes only on
// Absent, so a metadata store that errors (or, once metadata is sharded, a shard
// this node cannot reach) makes the scan skip the file instead of treating live
// data as junk.
func presence(found bool, err error) reclaim.Presence {
	switch {
	case err != nil:
		return reclaim.Unknown
	case found:
		return reclaim.Present
	default:
		return reclaim.Absent
	}
}

func (l reclaimLookup) HasObject(bucket, key string) reclaim.Presence {
	m, err := l.store.GetObjectMeta(bucket, key)
	return presence(m != nil, err)
}

func (l reclaimLookup) HasVersion(bucket, key, versionID string) reclaim.Presence {
	m, err := l.store.GetObjectVersion(bucket, key, versionID)
	return presence(m != nil, err)
}

func (l reclaimLookup) HasUpload(uploadID string) reclaim.Presence {
	u, err := l.local.GetMultipartUpload(uploadID)
	return presence(u != nil, err)
}

// reclaimOptions parses the shared query parameters. Dry run is the default
// everywhere: reclaiming deletes files, so it must be asked for explicitly.
func (h *APIHandler) reclaimOptions(r *http.Request) (reclaim.Options, error) {
	q := r.URL.Query()

	minAge := defaultReclaimMinAge
	if v := q.Get("min_age_hours"); v != "" {
		hours, err := strconv.ParseFloat(v, 64)
		if err != nil || hours <= 0 || hours > maxReclaimMinAgeHours {
			return reclaim.Options{}, fmt.Errorf("min_age_hours must be a positive number of hours up to %d", maxReclaimMinAgeHours)
		}
		minAge = time.Duration(hours * float64(time.Hour))
	}

	dataDir := ""
	if h.engine != nil {
		dataDir = h.engine.DataDir()
	}
	return reclaim.Options{
		DataDir: dataDir,
		MinAge:  minAge,
		DryRun:  q.Get("apply") != "true",
		Buckets: uniqueNonEmpty(q["bucket"]),
	}, nil
}

// runLocalReclaim scans this node only.
func (h *APIHandler) runLocalReclaim(r *http.Request) (*reclaim.Report, error) {
	opts, err := h.reclaimOptions(r)
	if err != nil {
		return nil, err
	}
	if opts.DataDir == "" {
		return nil, fmt.Errorf("no data directory configured")
	}
	local := h.localStore
	if local == nil {
		local = h.store
	}
	return reclaim.Run(opts, reclaimLookup{store: h.store, local: local})
}

// nodeReclaim is one node's contribution to a cluster-wide reclaim.
type nodeReclaim struct {
	NodeID    string          `json:"nodeId"`
	Address   string          `json:"address,omitempty"`
	Reachable bool            `json:"reachable"`
	Error     string          `json:"error,omitempty"`
	Report    *reclaim.Report `json:"report,omitempty"`
}

// handleReclaim serves POST /api/v1/reclaim — find (and with apply=true, remove)
// object data that no metadata refers to any more.
//
// This exists because several delete paths used to drop the Raft-replicated
// metadata cluster-wide while deleting the data file only on the node serving the
// request, stranding (N-1)/N of every bulk-deleted byte (issue #47). Those paths
// are fixed, but a cluster that ran the older builds still holds the orphans, and
// no S3 call can reach them.
//
// The scan is per node because each node can only see its own disk, so it fans out
// over the cluster channel exactly like the capacity rollup does. Dry run is the
// default; apply=true is what actually deletes.
func (h *APIHandler) handleReclaim(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "use POST")
		return
	}

	self := nodeReclaim{NodeID: h.clusterSelfID, Reachable: true}
	rep, err := h.runLocalReclaim(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	self.Report = rep
	nodes := []nodeReclaim{self}

	if r.URL.Query().Get("scope") != "local" && h.clusterNodeAddrs != nil {
		for id, addr := range h.clusterNodeAddrs() {
			if id == h.clusterSelfID || addr == "" {
				continue
			}
			nodes = append(nodes, h.fetchPeerReclaim(id, addr, r.URL.RawQuery))
		}
	}

	var orphans, orphanBytes, deleted, deletedBytes, scanned, skipped uint64
	reachable := 0
	for _, n := range nodes {
		if !n.Reachable || n.Report == nil {
			continue
		}
		reachable++
		orphans += n.Report.Orphans
		orphanBytes += n.Report.OrphanBytes
		deleted += n.Report.Deleted
		deletedBytes += n.Report.DeletedBytes
		scanned += n.Report.Scanned
		skipped += n.Report.SkippedTooNew
	}

	if deleted > 0 {
		slog.Info("reclaimed orphaned object data",
			"files", deleted, "bytes", deletedBytes, "nodes", reachable)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"dryRun":         rep.DryRun,
		"minAge":         rep.MinAge,
		"nodeCount":      len(nodes),
		"reachableNodes": reachable,
		"nodes":          nodes,
		"totals": map[string]any{
			"scanned":       scanned,
			"orphans":       orphans,
			"orphanBytes":   orphanBytes,
			"deleted":       deleted,
			"deletedBytes":  deletedBytes,
			"skippedTooNew": skipped,
		},
	})
}

// fetchPeerReclaim asks a peer to scan its own disk over the cluster channel. An
// unreachable peer is reported rather than failing the whole run, but it is
// counted separately so a partial result is never read as a complete one.
func (h *APIHandler) fetchPeerReclaim(id, addr, rawQuery string) nodeReclaim {
	nr := nodeReclaim{NodeID: id, Address: addr}
	scheme := "http"
	if h.cfg != nil && h.cfg.Server.TLS.Enabled {
		scheme = "https"
	}
	u := scheme + "://" + addr + "/cluster/reclaim"
	if rawQuery != "" {
		// scope is a coordinator-side concept; a peer always scans only itself, and
		// forwarding it would be harmless but misleading in logs.
		q, err := url.ParseQuery(rawQuery)
		if err == nil {
			q.Del("scope")
			if enc := q.Encode(); enc != "" {
				u += "?" + enc
			}
		}
	}
	req, err := http.NewRequest(http.MethodPost, u, bytes.NewReader(nil))
	if err != nil {
		nr.Error = err.Error()
		return nr
	}
	if h.clusterSecret != "" {
		req.Header.Set(clusterSecretHeader, h.clusterSecret)
	}
	resp, err := reclaimClient.Do(req)
	if err != nil {
		nr.Error = err.Error()
		return nr
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		nr.Error = fmt.Sprintf("cluster/reclaim returned HTTP %d", resp.StatusCode)
		return nr
	}
	var rep reclaim.Report
	if err := json.NewDecoder(resp.Body).Decode(&rep); err != nil {
		nr.Error = err.Error()
		return nr
	}
	nr.Report = &rep
	nr.Reachable = true
	return nr
}

// ClusterReclaimHandler serves POST /cluster/reclaim on the cluster channel
// (cluster-secret authed): scan THIS node's disk and return the report. The
// coordinator fans out to every node because a node can only see its own disk.
func (h *APIHandler) ClusterReclaimHandler(secret string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !clusterAuthOK(r, secret) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		rep, err := h.runLocalReclaim(r)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, rep)
	}
}
