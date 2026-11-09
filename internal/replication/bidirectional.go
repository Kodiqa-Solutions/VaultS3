package replication

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
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
	reqSum := sha256.Sum256(reqBody)
	req.Header.Set("X-Amz-Content-Sha256", hex.EncodeToString(reqSum[:]))
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
	// A change is applied as a plain write or delete. In a bucket with
	// versioning that corrupted its history (two versions both latest, a
	// replicated object outside the version store), so such a bucket is not
	// replicated yet: the change is skipped and said so. Sync never worked
	// before 5.0.11, so this never ran.
	// A key the server cannot store (one with "./" or "//", which would name
	// another key's file) is skipped, so one such change cannot hold the
	// cursor and stop replication for good.
	if msg := storage.KeyProblem(change.Key); msg != "" {
		slog.Warn("bidirectional: skipping a change to a key this server cannot store", "bucket", change.Bucket, "key", change.Key, "reason", msg)
		return nil
	}
	v, _ := w.store.GetBucketVersioning(change.Bucket)
	if change.VersionID != "" {
		return w.applyRemoteVersion(ctx, peer, change, v)
	}
	if v != "" {
		// A plain change comes from a site where the bucket has no
		// versioning. Writing it here would replace a version in place.
		slog.Warn("bidirectional: skipping a change from a site where the bucket has no versioning, while it has versioning here",
			"bucket", change.Bucket, "key", change.Key, "versioning", v)
		return nil
	}
	// Check for conflict with local state
	localMeta, err := w.store.GetObjectMeta(change.Bucket, change.Key)
	// Object lock holds against a replicated write or delete as against any
	// other: a locked object is not replaced or removed.
	if err == nil && localMeta != nil && objectLocked(localMeta) {
		slog.Warn("bidirectional: skipping a change to a locked object", "bucket", change.Bucket, "key", change.Key)
		return nil
	}

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
	getResp, err := w.fetchObject(ctx, peer, change.Bucket, change.Key, "")
	if err != nil {
		return err
	}
	if getResp == nil {
		return nil // object was deleted between sync and fetch
	}
	defer func() { _ = getResp.Body.Close() }()

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
	if getResp.ContentLength >= 0 && written != getResp.ContentLength {
		return fmt.Errorf("fetched %d of %d bytes", written, getResp.ContentLength)
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
		ETag:         etag,
		Size:         written,
		LastModified: time.Now().Unix(),
		VectorClock:  mergedVC.Bytes(),
	}
	headersToMeta(getResp.Header, &meta, false)
	// The data is on disk but nothing lists it until this succeeds, so a
	// failure here is a failed apply and is retried, not a success.
	if err := w.store.PutObjectMeta(meta); err != nil {
		return fmt.Errorf("put object metadata: %w", err)
	}

	return nil
}

// fetchObject GETs an object, or one version of it, from a peer. A nil
// response with no error means the peer no longer has it.
func (w *BiDirectionalWorker) fetchObject(ctx context.Context, peer config.ReplicationPeer, bucket, key, versionID string) (*http.Response, error) {
	objURL, err := peerURL(peer.URL, bucket, key)
	if err != nil {
		return nil, err
	}
	if versionID != "" {
		objURL += "?versionId=" + url.QueryEscape(versionID)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, objURL, nil)
	if err != nil {
		return nil, fmt.Errorf("create GET request: %w", err)
	}
	req.Header.Set("X-VaultS3-Replication", "active-active")
	req.Header.Set("X-Amz-Content-Sha256", emptyPayloadSHA256)
	// The stored bytes, as they are. Without this the client asks for gzip
	// and decodes the answer, which turned an object stored with
	// Content-Encoding: gzip into its decompressed bytes on the other site.
	req.Header.Set("Accept-Encoding", "identity")
	signV4(req, peer.AccessKey, peer.SecretKey, "us-east-1")

	resp, err := w.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET object: %w", err)
	}
	if resp.StatusCode == http.StatusNotFound {
		_ = resp.Body.Close()
		return nil, nil
	}
	if resp.StatusCode >= 400 {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("GET returned HTTP %d", resp.StatusCode)
	}
	return resp, nil
}

// headersToMeta copies an object's metadata from the peer's GET answer, so
// the copy here is served with the same headers. withLock also takes its
// object lock, for a version (a plain copy keeps this site's own lock rules).
func headersToMeta(h http.Header, m *metadata.ObjectMeta, withLock bool) {
	m.ContentType = h.Get("Content-Type")
	if m.ContentType == "" {
		m.ContentType = "application/octet-stream"
	}
	m.ContentEncoding = h.Get("Content-Encoding")
	m.ContentDisposition = h.Get("Content-Disposition")
	m.CacheControl = h.Get("Cache-Control")
	m.ContentLanguage = h.Get("Content-Language")
	for name, vals := range h {
		if len(vals) == 0 {
			continue
		}
		if k, ok := strings.CutPrefix(strings.ToLower(name), "x-amz-meta-"); ok && k != "" {
			if m.UserMetadata == nil {
				m.UserMetadata = map[string]string{}
			}
			m.UserMetadata[k] = vals[0]
		}
	}
	if !withLock {
		return
	}
	if mode := h.Get("X-Amz-Object-Lock-Mode"); mode == "GOVERNANCE" || mode == "COMPLIANCE" {
		if until, err := time.Parse(time.RFC3339, h.Get("X-Amz-Object-Lock-Retain-Until-Date")); err == nil {
			m.RetentionMode = mode
			m.RetentionUntil = until.Unix()
		}
	}
	m.LegalHold = strings.EqualFold(h.Get("X-Amz-Object-Lock-Legal-Hold"), "ON")
}

// applyRemoteVersion stores a version or delete marker another site created,
// under the same version ID, in a bucket with versioning.
//
// Versions are never rewritten, so there is nothing to resolve: a site that
// already has the ID has the change, which makes applying it again harmless.
// Which version is current does not depend on the order changes arrive in:
// the highest ID is current on every site. IDs start with their creation
// time, and a local write takes an ID above the key's current version
// (metadata.NewVersionID), so it is current everywhere, even on a site whose
// clock runs behind.
//
// Versions are added even to a key under object lock, as a local write may:
// a new version replaces nothing.
func (w *BiDirectionalWorker) applyRemoteVersion(ctx context.Context, peer config.ReplicationPeer, change ChangeEntry, versioning string) error {
	if change.VersionID == "null" || !validVersionID(change.VersionID) {
		slog.Warn("bidirectional: skipping a change with a version ID this server does not use",
			"bucket", change.Bucket, "key", change.Key, "version", change.VersionID)
		return nil
	}
	// Every new version takes an ID above the key's current one, so a
	// version dated far ahead would push every later ID there with it. A
	// day covers any clock that is merely wrong.
	if ts, ok := metadata.VersionIDTime(change.VersionID); !ok || ts > time.Now().Add(24*time.Hour).UnixNano() {
		slog.Warn("bidirectional: skipping a version whose ID is not a time this server could have made, or is dated more than a day ahead; check the sending site's clock",
			"bucket", change.Bucket, "key", change.Key, "version", change.VersionID)
		return nil
	}
	if versioning != "Enabled" {
		slog.Warn("bidirectional: skipping a versioned change to a bucket without versioning enabled here; enable versioning on every site",
			"bucket", change.Bucket, "key", change.Key, "versioning", versioning)
		return nil
	}
	if existing, err := w.store.GetObjectVersion(change.Bucket, change.Key, change.VersionID); err == nil && existing != nil {
		return nil
	}

	meta := metadata.ObjectMeta{
		Bucket:       change.Bucket,
		Key:          change.Key,
		VersionID:    change.VersionID,
		DeleteMarker: change.DeleteMarker,
		LastModified: change.LastModified,
	}
	if meta.LastModified == 0 {
		meta.LastModified = time.Now().Unix()
	}
	if !change.DeleteMarker {
		resp, err := w.fetchObject(ctx, peer, change.Bucket, change.Key, change.VersionID)
		if err != nil {
			return err
		}
		if resp == nil {
			return nil // the version was deleted there since
		}
		defer func() { _ = resp.Body.Close() }()
		written, etag, err := w.engine.PutObjectVersion(change.Bucket, change.Key, change.VersionID, resp.Body, resp.ContentLength)
		if err != nil {
			return fmt.Errorf("put version locally: %w", err)
		}
		if (resp.ContentLength >= 0 && written != resp.ContentLength) || (change.Size > 0 && written != change.Size) {
			if derr := w.engine.DeleteObjectVersion(change.Bucket, change.Key, change.VersionID); derr != nil {
				slog.Warn("bidirectional: could not remove a short version's data", "bucket", change.Bucket, "key", change.Key, "version", change.VersionID, "error", derr)
			}
			return fmt.Errorf("fetched %d bytes of a %d byte version", written, change.Size)
		}
		meta.Size = written
		// The ETag the version has on its own site, which for an object
		// uploaded in parts is not the digest of its bytes.
		meta.ETag = change.ETag
		if meta.ETag == "" {
			meta.ETag = etag
		}
		headersToMeta(resp.Header, &meta, true)
		// A version that arrives with no retention gets this bucket's
		// default, as a write here would.
		if meta.RetentionMode == "" {
			if bi, err := w.store.GetBucket(change.Bucket); err == nil && bi.DefaultRetentionMode != "" && bi.DefaultRetentionDays > 0 {
				meta.RetentionMode = bi.DefaultRetentionMode
				meta.RetentionUntil = time.Now().UTC().Unix() + int64(bi.DefaultRetentionDays*86400)
			}
		}
	}

	cur, _ := w.store.GetObjectMeta(change.Bucket, change.Key)
	meta.IsLatest = newerThanCurrent(meta, cur)
	if meta.IsLatest && cur != nil && cur.VersionID == "" && !cur.DeleteMarker {
		// An object written before versioning was enabled is the null
		// version, kept as a local write keeps it.
		cur.VersionID = "null"
		cur.IsLatest = false
		if err := w.store.PutObjectVersion(*cur); err != nil {
			return fmt.Errorf("keep the pre-versioning object as the null version: %w", err)
		}
	}
	if err := w.store.PutObjectVersion(meta); err != nil {
		return fmt.Errorf("put version metadata: %w", err)
	}
	if meta.IsLatest {
		if err := w.store.PutObjectMeta(meta); err != nil {
			return fmt.Errorf("point the key at the new version: %w", err)
		}
	}
	return nil
}

// newerThanCurrent reports whether version m becomes the key's current one.
func newerThanCurrent(m metadata.ObjectMeta, cur *metadata.ObjectMeta) bool {
	if cur == nil {
		return true
	}
	// A pre-versioning object or a null version has no time in its ID.
	if cur.VersionID == "" || cur.VersionID == "null" {
		return m.LastModified >= cur.LastModified
	}
	return m.VersionID > cur.VersionID
}

// validVersionID reports an ID in the form this server generates: the
// creation time in nanoseconds as 16 hex digits, then random hex.
func validVersionID(id string) bool {
	if len(id) < 17 || len(id) > 64 {
		return false
	}
	for _, c := range id {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
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

	// A peer that signed with an access key sees only the changes that key may
	// read. The cursor still moves past the others (LastSeq).
	if may, ok := r.Context().Value(syncReaderKey{}).(SyncReader); ok {
		readable := changes[:0:0]
		for _, c := range changes {
			if may(c.Bucket, c.Key) {
				readable = append(readable, c)
			}
		}
		changes = readable
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

// SyncReader reports whether the peer asking for a sync may read an object.
type SyncReader func(bucket, key string) bool

type syncReaderKey struct{}

// WithSyncReader limits the sync answer for r to the changes may allows.
func WithSyncReader(r *http.Request, may SyncReader) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), syncReaderKey{}, may))
}

// maxSyncBatch caps how many change log entries one sync answer carries. It is
// a variable so a test can lower it instead of writing a thousand entries.
var maxSyncBatch = 1000

// objectLocked reports whether object lock forbids replacing or removing the
// object: a legal hold, or retention that has not expired.
func objectLocked(m *metadata.ObjectMeta) bool {
	if m.LegalHold {
		return true
	}
	return m.RetentionMode != "" && m.RetentionUntil > 0 && time.Now().UTC().Unix() < m.RetentionUntil
}
