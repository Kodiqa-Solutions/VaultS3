package replication

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Kodiqa-Solutions/VaultS3/internal/config"
	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
	"github.com/Kodiqa-Solutions/VaultS3/internal/storage"
)

// validatePeerURL checks that a replication peer URL does not point to internal/metadata endpoints.
func validatePeerURL(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("invalid URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("URL scheme must be http or https")
	}
	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("URL must have a host")
	}
	if host == "localhost" || host == "127.0.0.1" || host == "::1" || host == "0.0.0.0" {
		return fmt.Errorf("URL must not point to localhost")
	}
	if strings.HasPrefix(host, "169.254.") || host == "metadata.google.internal" {
		return fmt.Errorf("URL must not point to cloud metadata service")
	}
	if ip := net.ParseIP(host); ip != nil {
		if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsPrivate() {
			return fmt.Errorf("URL must not point to loopback, link-local, or private address")
		}
	}
	return nil
}

// ReplicationEvent is sent via the event channel for real-time replication.
type RealtimeEvent struct {
	Type   string // "put" or "delete"
	Bucket string
	Key    string
	Size   int64
	ETag   string
}

// Worker handles async replication to peer VaultS3 instances.
type Worker struct {
	store      *metadata.Store
	engine     storage.Engine
	peers      map[string]config.ReplicationPeer
	interval   time.Duration
	maxRetries int
	batchSize  int
	client     *http.Client
	eventCh    chan RealtimeEvent
}

func NewWorker(store *metadata.Store, engine storage.Engine, cfg config.ReplicationConfig) *Worker {
	peers := make(map[string]config.ReplicationPeer)
	for _, p := range cfg.Peers {
		// Validate peer URLs against SSRF on startup
		if err := validatePeerURL(p.URL); err != nil {
			slog.Warn("skipping replication peer with invalid URL", "peer", p.Name, "url", p.URL, "error", err)
			continue
		}
		peers[p.Name] = p
	}
	interval := time.Duration(cfg.ScanIntervalSecs) * time.Second
	if interval < 5*time.Second {
		interval = 5 * time.Second
	}
	return &Worker{
		store:      store,
		engine:     engine,
		peers:      peers,
		interval:   interval,
		maxRetries: cfg.MaxRetries,
		batchSize:  cfg.BatchSize,
		client:     &http.Client{Timeout: 60 * time.Second},
		eventCh:    make(chan RealtimeEvent, 1000),
	}
}

// Notify sends a real-time replication event (non-blocking).
func (w *Worker) Notify(event RealtimeEvent) {
	select {
	case w.eventCh <- event:
	default:
		// Channel full, event will be picked up by periodic scan
	}
}

// Run processes the replication queue on a ticker and event channel until ctx is cancelled.
func (w *Worker) Run(ctx context.Context) {
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()

	// Process immediately on start
	w.processQueue()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.processQueue()
		case evt := <-w.eventCh:
			w.handleRealtimeEvent(evt)
		}
	}
}

// handleRealtimeEvent enqueues a realtime event for all peers.
func (w *Worker) handleRealtimeEvent(evt RealtimeEvent) {
	for name := range w.peers {
		event := metadata.ReplicationEvent{
			Peer:   name,
			Type:   evt.Type,
			Bucket: evt.Bucket,
			Key:    evt.Key,
			ETag:   evt.ETag,
		}
		if err := w.store.EnqueueReplication(event); err != nil {
			slog.Error("replication enqueue error", "peer", name, "error", err)
		}
	}
	// Process immediately
	w.processQueue()
}

func (w *Worker) processQueue() {
	now := time.Now().Unix()
	events, err := w.store.DequeueReplication(w.batchSize, now)
	if err != nil {
		slog.Error("replication dequeue error", "error", err)
		return
	}

	for _, event := range events {
		peer, ok := w.peers[event.Peer]
		if !ok {
			slog.Warn("replication unknown peer, dead-lettering", "peer", event.Peer, "event_id", event.ID)
			w.store.DeadLetterReplication(event.ID)
			w.updateStatus(event.Peer, "", true)
			continue
		}

		var replicateErr error
		switch event.Type {
		case "put":
			replicateErr = w.replicatePut(peer, event)
		case "delete":
			replicateErr = w.replicateDelete(peer, event)
		default:
			slog.Warn("replication unknown event type", "type", event.Type)
			w.store.AckReplication(event.ID)
			continue
		}

		if replicateErr != nil {
			slog.Error("replication failed", "peer", peer.Name, "type", event.Type, "bucket", event.Bucket, "key", event.Key, "error", replicateErr)
			event.RetryCount++
			if event.RetryCount >= w.maxRetries {
				slog.Warn("replication max retries exceeded, dead-lettering", "peer", peer.Name, "event_id", event.ID)
				w.store.DeadLetterReplication(event.ID)
				w.updateStatus(peer.Name, replicateErr.Error(), true)
			} else {
				backoff := backoffDelay(event.RetryCount)
				nextRetry := time.Now().Unix() + int64(backoff.Seconds())
				w.store.NackReplication(event.ID, event.RetryCount, nextRetry)
			}
		} else {
			w.store.AckReplication(event.ID)
			w.updateStatus(peer.Name, "", false)
		}
	}
}

func (w *Worker) replicatePut(peer config.ReplicationPeer, event metadata.ReplicationEvent) error {
	// Auto-create bucket on peer (idempotent)
	if err := w.ensureBucket(peer, event.Bucket); err != nil {
		return fmt.Errorf("ensure bucket: %w", err)
	}

	// Read object from local storage
	reader, size, err := w.engine.GetObject(event.Bucket, event.Key)
	if err != nil {
		// Object no longer exists locally — skip
		slog.Debug("replication object not found locally, skipping", "bucket", event.Bucket, "key", event.Key)
		return nil
	}
	defer reader.Close()

	url := fmt.Sprintf("%s/%s/%s", strings.TrimRight(peer.URL, "/"), event.Bucket, event.Key)
	req, err := http.NewRequest("PUT", url, reader)
	if err != nil {
		return err
	}
	req.ContentLength = size
	req.Header.Set("X-VaultS3-Replication", "true")
	req.Header.Set("X-Amz-Content-Sha256", "UNSIGNED-PAYLOAD")
	signV4(req, peer.AccessKey, peer.SecretKey, "us-east-1")

	resp, err := w.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)

	if resp.StatusCode >= 400 {
		return fmt.Errorf("PUT returned %d", resp.StatusCode)
	}
	return nil
}

func (w *Worker) replicateDelete(peer config.ReplicationPeer, event metadata.ReplicationEvent) error {
	url := fmt.Sprintf("%s/%s/%s", strings.TrimRight(peer.URL, "/"), event.Bucket, event.Key)
	req, err := http.NewRequest("DELETE", url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("X-VaultS3-Replication", "true")
	req.Header.Set("X-Amz-Content-Sha256", "UNSIGNED-PAYLOAD")
	signV4(req, peer.AccessKey, peer.SecretKey, "us-east-1")

	resp, err := w.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)

	// 404 is OK for delete (already deleted)
	if resp.StatusCode >= 400 && resp.StatusCode != 404 {
		return fmt.Errorf("DELETE returned %d", resp.StatusCode)
	}
	return nil
}

func (w *Worker) ensureBucket(peer config.ReplicationPeer, bucket string) error {
	url := fmt.Sprintf("%s/%s", strings.TrimRight(peer.URL, "/"), bucket)
	req, err := http.NewRequest("PUT", url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("X-VaultS3-Replication", "true")
	req.Header.Set("X-Amz-Content-Sha256", "UNSIGNED-PAYLOAD")
	signV4(req, peer.AccessKey, peer.SecretKey, "us-east-1")

	resp, err := w.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)

	// 200 = created, 409 = already exists — both OK
	if resp.StatusCode >= 400 && resp.StatusCode != 409 {
		return fmt.Errorf("create bucket returned %d", resp.StatusCode)
	}
	return nil
}

func (w *Worker) updateStatus(peerName, lastErr string, isFail bool) {
	depth, _ := w.store.ReplicationQueueDepth()
	statuses, _ := w.store.GetReplicationStatuses()

	var existing metadata.ReplicationStatus
	for _, s := range statuses {
		if s.Peer == peerName {
			existing = s
			break
		}
	}

	existing.Peer = peerName
	existing.QueueDepth = depth
	existing.LastSyncTime = time.Now().Unix()
	if lastErr != "" {
		existing.LastError = lastErr
	}
	if isFail {
		existing.TotalFailed++
	} else {
		existing.TotalSynced++
		existing.LastError = ""
	}

	w.store.PutReplicationStatus(existing)
}

func backoffDelay(retryCount int) time.Duration {
	delays := []time.Duration{5 * time.Second, 15 * time.Second, 45 * time.Second, 135 * time.Second, 405 * time.Second}
	if retryCount <= 0 {
		return delays[0]
	}
	if retryCount > len(delays) {
		return 10 * time.Minute
	}
	return delays[retryCount-1]
}
