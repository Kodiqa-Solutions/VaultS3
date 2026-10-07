package replication

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
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
	store     metadata.StoreAPI
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
	store metadata.StoreAPI,
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
			slog.Error("bidirectional peer has an unusable URL, it is not synced", "peer", p.Name, "url", p.URL, "error", err)
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

	// Apply remote changes with conflict resolution. The cursor only moves past
	// changes that were applied (or skipped on purpose). It used to jump to the
	// peer's last sequence whatever happened, so a change that failed to apply,
	// or was cut off by shutdown, was never pulled again and the sites stayed
	// different with nothing left to retry. A change that keeps failing now
	// holds the cursor and is retried on every sync, and says so in the log.
	applied := 0
	newCursor := cursor
	complete := true
	for _, change := range syncResp.Changes {
		if ctx.Err() != nil {
			complete = false
			break
		}
		if change.SiteID != w.siteID { // our own echoed changes need nothing
			if err := w.applyRemoteChange(ctx, peer, change); err != nil {
				slog.Error("bidirectional: failed to apply change, holding the sync cursor to retry it",
					"peer", name, "bucket", change.Bucket, "key", change.Key, "seq", change.Seq, "error", err,
				)
				complete = false
				break
			}
			applied++
		}
		if change.Seq > newCursor {
			newCursor = change.Seq
		}
	}
	if complete && syncResp.LastSeq > newCursor {
		// Entries the peer skipped as unreadable still count as seen.
		newCursor = syncResp.LastSeq
	}

	// A peer from before ChangeEntry.Seq sends no positions, so after a failure
	// there is no safe place to resume but the start of this batch. Applying a
	// change twice is harmless, since the vector clocks make it a no-op.
	if newCursor > cursor {
		w.mu.Lock()
		w.peerCursors[name] = newCursor
		w.mu.Unlock()
	}

	if applied > 0 {
		slog.Info("bidirectional: sync complete",
			"peer", name, "applied", applied, "new_cursor", newCursor,
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

		// Apply the delete. Metadata goes first, so a failure leaves the
		// object readable and the change is retried, rather than leaving a
		// listed object whose data is gone.
		if err := w.store.DeleteObjectMeta(change.Bucket, change.Key); err != nil {
			return fmt.Errorf("delete object metadata: %w", err)
		}
		if err := w.engine.DeleteObject(change.Bucket, change.Key); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("delete object data: %w", err)
		}
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
	objURL, err := peerURL(peer.URL, change.Bucket, change.Key)
	if err != nil {
		return err
	}
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
		if err := w.store.CreateBucket(change.Bucket); err != nil && !w.store.BucketExists(change.Bucket) {
			return fmt.Errorf("create bucket locally: %w", err)
		}
	}

	// Write object locally
	written, etag, err := w.engine.PutObject(change.Bucket, change.Key, getResp.Body, getResp.ContentLength)
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
		Size:         written,
		LastModified: time.Now().Unix(),
		VectorClock:  mergedVC.Bytes(),
	}
	// The data is on disk but nothing lists it until this succeeds, so a
	// failure here is a failed apply and is retried, not a success.
	if err := w.store.PutObjectMeta(meta); err != nil {
		return fmt.Errorf("put object metadata: %w", err)
	}

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

	// The limit comes from the remote site. Unbounded, one request could ask
	// for the whole change log in a single answer.
	if req.Limit <= 0 {
		req.Limit = 100
	}
	if req.Limit > maxSyncBatch {
		req.Limit = maxSyncBatch
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
	if err := json.NewEncoder(rw).Encode(resp); err != nil {
		slog.Warn("sync handler: write response failed", "error", err)
	}
}

// maxSyncBatch caps how many change log entries one sync answer carries. It is
// a variable so a test can lower it instead of writing a thousand entries.
var maxSyncBatch = 1000
