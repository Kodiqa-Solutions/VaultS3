package replication

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Kodiqa-Solutions/VaultS3/internal/config"
	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
	"github.com/Kodiqa-Solutions/VaultS3/internal/storage"
)

// SyncRequest is sent to a remote site to request changes since a given sequence.
type SyncRequest struct {
	SiteID   string `json:"site_id"`
	SinceSeq uint64 `json:"since_seq"`
	Limit    int    `json:"limit"`
}

// SyncResponse is returned by the remote site with its changes and current sequence.
type SyncResponse struct {
	SiteID  string        `json:"site_id"`
	Changes []ChangeEntry `json:"changes"`
	LastSeq uint64        `json:"last_seq"`
}

// BiDirectionalWorker handles active-active replication between VaultS3 sites.
// It periodically pulls changes from each peer, resolves conflicts, and applies them locally.
type BiDirectionalWorker struct {
	store     *metadata.Store
	engine    storage.Engine
	changeLog *ChangeLog
	resolver  ConflictResolver
	siteID    string
	peers     map[string]config.ReplicationPeer
	interval  time.Duration
	batchSize int
	client    *http.Client

	// Track the last-seen sequence per remote peer
	mu          sync.Mutex
	peerCursors map[string]uint64 // peerName → last synced seq from that peer
}

// NewBiDirectionalWorker creates a bidirectional replication worker.
func NewBiDirectionalWorker(
	store *metadata.Store,
	engine storage.Engine,
	cfg config.ReplicationConfig,
) *BiDirectionalWorker {
	siteID := cfg.SiteID
	if siteID == "" {
		siteID = "site-1"
	}

	peers := make(map[string]config.ReplicationPeer)
	for _, p := range cfg.Peers {
		if err := validatePeerURL(p.URL); err != nil {
			slog.Warn("skipping bidirectional peer with invalid URL", "peer", p.Name, "url", p.URL, "error", err)
			continue
		}
		peers[p.Name] = p
	}

	interval := time.Duration(cfg.ScanIntervalSecs) * time.Second
	if interval < 5*time.Second {
		interval = 5 * time.Second
	}

	batchSize := cfg.BatchSize
	if batchSize <= 0 {
		batchSize = 100
	}

	strategy := ConflictStrategy(cfg.ConflictStrategy)
	if strategy == "" {
		strategy = StrategyLastWriterWins
	}
	resolver := NewConflictResolver(strategy, cfg.PreferredSite)

	return &BiDirectionalWorker{
		store:       store,
		engine:      engine,
		changeLog:   NewChangeLog(store, siteID),
		resolver:    resolver,
		siteID:      siteID,
		peers:       peers,
		interval:    interval,
		batchSize:   batchSize,
		client:      &http.Client{Timeout: 60 * time.Second},
		peerCursors: make(map[string]uint64),
	}
}

// ChangeLog returns the underlying change log for recording local mutations.
func (w *BiDirectionalWorker) ChangeLog() *ChangeLog {
	return w.changeLog
}

// SiteID returns this worker's site identifier.
func (w *BiDirectionalWorker) SiteID() string {
	return w.siteID
}

// Run starts the bidirectional sync loop. Blocks until ctx is cancelled.
func (w *BiDirectionalWorker) Run(ctx context.Context) {
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()

	slog.Info("bidirectional replication started",
		"site_id", w.siteID,
		"peers", len(w.peers),
		"interval", w.interval,
	)

	// Initial sync
	w.syncAllPeers(ctx)

	for {
		select {
		case <-ctx.Done():
			slog.Info("bidirectional replication stopped")
			return
		case <-ticker.C:
			w.syncAllPeers(ctx)
		}
	}
}

func (w *BiDirectionalWorker) syncAllPeers(ctx context.Context) {
	for name, peer := range w.peers {
		if ctx.Err() != nil {
			return
		}
		if err := w.syncPeer(ctx, name, peer); err != nil {
			slog.Error("bidirectional sync failed", "peer", name, "error", err)
		}
	}
}

func (w *BiDirectionalWorker) syncPeer(ctx context.Context, name string, peer config.ReplicationPeer) error {
	w.mu.Lock()
	cursor := w.peerCursors[name]
	w.mu.Unlock()

	// Pull changes from remote peer
	syncReq := SyncRequest{
		SiteID:   w.siteID,
		SinceSeq: cursor,
		Limit:    w.batchSize,
	}

	reqBody, err := json.Marshal(syncReq)
	if err != nil {
		return fmt.Errorf("marshal sync request: %w", err)
	}

	syncURL := fmt.Sprintf("%s/_replication/sync", strings.TrimRight(peer.URL, "/"))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, syncURL, bytes.NewReader(reqBody))
	if err != nil {
		return fmt.Errorf("create sync request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-VaultS3-Replication", "active-active")
	req.Header.Set("X-Amz-Content-Sha256", "UNSIGNED-PAYLOAD")
	signV4(req, peer.AccessKey, peer.SecretKey, "us-east-1")

	resp, err := w.client.Do(req)
	if err != nil {
		return fmt.Errorf("sync request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("sync returned HTTP %d: %s", resp.StatusCode, string(body))
	}

	var syncResp SyncResponse
	if err := json.NewDecoder(resp.Body).Decode(&syncResp); err != nil {
		return fmt.Errorf("decode sync response: %w", err)
	}

	// Apply remote changes with conflict resolution
	applied := 0
	for _, change := range syncResp.Changes {
		if ctx.Err() != nil {
			break
		}
		if change.SiteID == w.siteID {
			continue // skip our own echoed changes
		}

		if err := w.applyRemoteChange(ctx, peer, change); err != nil {
			slog.Warn("bidirectional: failed to apply change",
				"peer", name, "bucket", change.Bucket, "key", change.Key, "error", err,
			)
			continue
		}
		applied++
	}

	// Update cursor
	if syncResp.LastSeq > cursor {
		w.mu.Lock()
		w.peerCursors[name] = syncResp.LastSeq
		w.mu.Unlock()
	}

	if applied > 0 {
		slog.Info("bidirectional: sync complete",
			"peer", name, "applied", applied, "new_cursor", syncResp.LastSeq,
		)
	}

	return nil
}

func (w *BiDirectionalWorker) applyRemoteChange(ctx context.Context, peer config.ReplicationPeer, change ChangeEntry) error {
	// Check for conflict with local state
	localMeta, err := w.store.GetObjectMeta(change.Bucket, change.Key)

	if change.EventType == "delete" {
		if err != nil {
			return nil // already gone locally
		}

		// Check if local version is concurrent (conflict)
		if localMeta.VectorClock != nil {
			localVC, _ := ParseVectorClock(localMeta.VectorClock)
			ordering := localVC.Compare(change.VectorClock)
			if ordering == Concurrent || ordering == HappenedAfter {
				// Conflict: local has a concurrent or newer write
				localEntry := ChangeEntry{
					Bucket:      change.Bucket,
					Key:         change.Key,
					EventType:   "put",
					SiteID:      w.siteID,
					VectorClock: localVC,
					ETag:        localMeta.ETag,
					Size:        localMeta.Size,
					Timestamp:   localMeta.LastModified * 1e9, // convert to nanos
				}
				winner := w.resolver.Resolve(localEntry, change)
				if winner.SiteID == w.siteID {
					return nil // keep local version
				}
			}
		}

		// Apply the delete
		w.engine.DeleteObject(change.Bucket, change.Key)
		w.store.DeleteObjectMeta(change.Bucket, change.Key)
		return nil
	}

	// EventType == "put"
	if err == nil && localMeta.VectorClock != nil {
		// Local object exists — check for conflict
		localVC, _ := ParseVectorClock(localMeta.VectorClock)
		ordering := localVC.Compare(change.VectorClock)

		switch ordering {
		case HappenedAfter:
			return nil // local is newer, skip
		case Concurrent:
			// True conflict — use resolver
			localEntry := ChangeEntry{
				Bucket:      change.Bucket,
				Key:         change.Key,
				EventType:   "put",
				SiteID:      w.siteID,
				VectorClock: localVC,
				ETag:        localMeta.ETag,
				Size:        localMeta.Size,
				Timestamp:   localMeta.LastModified * 1e9,
			}
			winner := w.resolver.Resolve(localEntry, change)
			if winner.SiteID == w.siteID {
				return nil // keep local version
			}
			// Remote wins — fall through to apply
		case HappenedBefore, Equal:
			// Remote is newer or same — apply
		}
	}

	// Fetch the actual object data from the remote peer
	objURL := fmt.Sprintf("%s/%s/%s", strings.TrimRight(peer.URL, "/"), change.Bucket, change.Key)
	getReq, err := http.NewRequestWithContext(ctx, http.MethodGet, objURL, nil)
	if err != nil {
		return fmt.Errorf("create GET request: %w", err)
	}
	getReq.Header.Set("X-VaultS3-Replication", "active-active")
	getReq.Header.Set("X-Amz-Content-Sha256", "UNSIGNED-PAYLOAD")
	signV4(getReq, peer.AccessKey, peer.SecretKey, "us-east-1")

	getResp, err := w.client.Do(getReq)
	if err != nil {
		return fmt.Errorf("GET object: %w", err)
	}
	defer getResp.Body.Close()

	if getResp.StatusCode == http.StatusNotFound {
		return nil // object was deleted between sync and fetch
	}
	if getResp.StatusCode >= 400 {
		return fmt.Errorf("GET returned HTTP %d", getResp.StatusCode)
	}

	// Ensure bucket exists locally
	if !w.store.BucketExists(change.Bucket) {
		w.store.CreateBucket(change.Bucket)
	}

	// Write object locally
	_, etag, err := w.engine.PutObject(change.Bucket, change.Key, getResp.Body, getResp.ContentLength)
	if err != nil {
		return fmt.Errorf("put object locally: %w", err)
	}

	// Merge vector clocks and store metadata
	mergedVC := change.VectorClock.Clone()
	if localMeta != nil && localMeta.VectorClock != nil {
		localVC, _ := ParseVectorClock(localMeta.VectorClock)
		mergedVC = mergedVC.Merge(localVC)
	}

	meta := metadata.ObjectMeta{
		Bucket:       change.Bucket,
		Key:          change.Key,
		ContentType:  "", // will be detected by engine
		ETag:         etag,
		Size:         change.Size,
		LastModified: time.Now().Unix(),
		VectorClock:  mergedVC.Bytes(),
	}
	w.store.PutObjectMeta(meta)

	return nil
}

// HandleSyncRequest processes an incoming sync request from a remote site.
// This is the HTTP handler for /_replication/sync.
func (w *BiDirectionalWorker) HandleSyncRequest(rw http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(rw, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req SyncRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(rw, "invalid request body", http.StatusBadRequest)
		return
	}

	if req.Limit <= 0 {
		req.Limit = 100
	}

	changes, lastSeq, err := w.changeLog.ChangesSince(req.SinceSeq, req.Limit)
	if err != nil {
		slog.Error("sync handler: read change log failed", "error", err)
		http.Error(rw, "internal error", http.StatusInternalServerError)
		return
	}

	resp := SyncResponse{
		SiteID:  w.siteID,
		Changes: changes,
		LastSeq: lastSeq,
	}

	rw.Header().Set("Content-Type", "application/json")
	json.NewEncoder(rw).Encode(resp)
}
