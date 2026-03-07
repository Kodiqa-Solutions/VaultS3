package cluster

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Kodiqa-Solutions/VaultS3/internal/config"
	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
	"github.com/Kodiqa-Solutions/VaultS3/internal/storage"
)

// Rebalancer handles background migration of objects when cluster membership changes.
// When a node joins or leaves, some objects need to move to their new primary node.
type Rebalancer struct {
	store        *metadata.Store
	engine       storage.Engine
	ring         *HashRing
	proxy        *Proxy
	selfID       string
	maxBandwidth int64 // bytes/sec throttle (0 = unlimited)
	batchSize    int

	mu         sync.Mutex
	running    atomic.Bool
	cancelFunc context.CancelFunc
}

// RebalanceConfig is an alias for config.RebalanceConfig.
type RebalanceConfig = config.RebalanceConfig

func applyRebalanceDefaults(c *RebalanceConfig) {
	if c.MaxBandwidthMBps <= 0 {
		c.MaxBandwidthMBps = 50
	}
	if c.BatchSize <= 0 {
		c.BatchSize = 100
	}
}

// NewRebalancer creates a new rebalancer.
func NewRebalancer(
	store *metadata.Store,
	engine storage.Engine,
	ring *HashRing,
	proxy *Proxy,
	selfID string,
	cfg RebalanceConfig,
) *Rebalancer {
	applyRebalanceDefaults(&cfg)
	return &Rebalancer{
		store:        store,
		engine:       engine,
		ring:         ring,
		proxy:        proxy,
		selfID:       selfID,
		maxBandwidth: int64(cfg.MaxBandwidthMBps) * 1024 * 1024,
		batchSize:    cfg.BatchSize,
	}
}

// Trigger starts a rebalance scan in the background.
// Safe to call multiple times — only one scan runs at a time.
func (r *Rebalancer) Trigger() {
	if r.running.Load() {
		slog.Info("rebalance: already running, skipping trigger")
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if r.running.Load() {
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	r.cancelFunc = cancel
	r.running.Store(true)

	go func() {
		defer r.running.Store(false)
		r.run(ctx)
	}()
}

// Stop cancels any in-progress rebalance.
func (r *Rebalancer) Stop() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cancelFunc != nil {
		r.cancelFunc()
		r.cancelFunc = nil
	}
}

// IsRunning returns true if a rebalance is in progress.
func (r *Rebalancer) IsRunning() bool {
	return r.running.Load()
}

func (r *Rebalancer) run(ctx context.Context) {
	slog.Info("rebalance: starting scan")
	start := time.Now()

	migrated := 0
	scanned := 0
	var bytesTransferred int64

	buckets, _ := r.store.ListBuckets()
	for _, bucket := range buckets {
		if ctx.Err() != nil {
			break
		}

		startAfter := ""
		for {
			if ctx.Err() != nil {
				break
			}

			objects, truncated, err := r.engine.ListObjects(bucket.Name, "", startAfter, r.batchSize)
			if err != nil {
				slog.Warn("rebalance: list objects failed",
					"bucket", bucket.Name, "error", err,
				)
				break
			}

			for _, obj := range objects {
				if ctx.Err() != nil {
					break
				}
				scanned++

				// Check if this object still belongs to us
				primaryNode := r.ring.GetNode(bucket.Name, obj.Key)
				if primaryNode == r.selfID || primaryNode == "" {
					continue // object belongs here
				}

				// Object needs to move — transfer via proxy
				if err := r.transferObject(ctx, bucket.Name, obj.Key, primaryNode); err != nil {
					slog.Warn("rebalance: transfer failed",
						"bucket", bucket.Name, "key", obj.Key,
						"target", primaryNode, "error", err,
					)
					continue
				}

				migrated++
				bytesTransferred += obj.Size

				// Throttle bandwidth
				if r.maxBandwidth > 0 && bytesTransferred > 0 {
					elapsed := time.Since(start).Seconds()
					if elapsed > 0 {
						currentRate := float64(bytesTransferred) / elapsed
						if currentRate > float64(r.maxBandwidth) {
							sleepTime := time.Duration(float64(obj.Size) / float64(r.maxBandwidth) * float64(time.Second))
							select {
							case <-ctx.Done():
								return
							case <-time.After(sleepTime):
							}
						}
					}
				}

				startAfter = obj.Key
			}

			if !truncated || len(objects) == 0 {
				break
			}
			if len(objects) > 0 {
				startAfter = objects[len(objects)-1].Key
			}
		}
	}

	slog.Info("rebalance: scan complete",
		"scanned", scanned,
		"migrated", migrated,
		"bytes", bytesTransferred,
		"duration", time.Since(start).Round(time.Millisecond),
	)
}

// transferObject copies an object to the target node via the S3 API,
// then deletes the local copy.
func (r *Rebalancer) transferObject(ctx context.Context, bucket, key, targetNodeID string) error {
	// Read the object locally
	reader, size, err := r.engine.GetObject(bucket, key)
	if err != nil {
		return fmt.Errorf("read local object: %w", err)
	}
	defer reader.Close()

	data, err := io.ReadAll(reader)
	if err != nil {
		return fmt.Errorf("read object data: %w", err)
	}

	// Put to the target node via the proxy
	r.proxy.mu.RLock()
	addr, ok := r.proxy.nodeAddrs[targetNodeID]
	r.proxy.mu.RUnlock()

	if !ok {
		return fmt.Errorf("unknown target node: %s", targetNodeID)
	}

	// Use direct HTTP PUT to the target node's S3 endpoint
	url := fmt.Sprintf("http://%s/%s/%s", addr, bucket, key)
	req, err := newRequestWithContext(ctx, http.MethodPut, url, bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.ContentLength = size
	req.Header.Set("X-VaultS3-Rebalance", r.selfID) // mark as internal rebalance

	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("PUT to target: %w", err)
	}
	resp.Body.Close()

	if resp.StatusCode >= 400 {
		return fmt.Errorf("target returned HTTP %d", resp.StatusCode)
	}

	// Delete local copy after successful transfer
	if err := r.engine.DeleteObject(bucket, key); err != nil {
		slog.Warn("rebalance: delete local copy failed (object was transferred)",
			"bucket", bucket, "key", key, "error", err,
		)
	}

	// Also delete metadata for the object locally
	r.store.DeleteObjectMeta(bucket, key)

	slog.Debug("rebalance: transferred object",
		"bucket", bucket, "key", key,
		"target", targetNodeID, "size", size,
	)
	return nil
}

// newRequestWithContext creates an HTTP request with context.
func newRequestWithContext(ctx context.Context, method, url string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return nil, err
	}
	return req, nil
}

// RebalanceStatus reports the current state.
type RebalanceStatus struct {
	Running bool   `json:"running"`
	Message string `json:"message,omitempty"`
}

// Status returns the current rebalance status.
func (r *Rebalancer) Status() RebalanceStatus {
	return RebalanceStatus{
		Running: r.running.Load(),
	}
}
