package cluster

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Kodiqa-Solutions/VaultS3/internal/config"
	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
	"github.com/Kodiqa-Solutions/VaultS3/internal/storage"
)

// bucketEncryptedHeader mirrors api.BucketEncryptedHeader. It is repeated rather
// than imported because internal/api already depends on this package.
const bucketEncryptedHeader = "X-Vaults3-Bucket-Encrypted"

// RepairConfig is an alias for config.RepairConfig.
type RepairConfig = config.RepairConfig

// RepairStats reports the outcome of the last completed repair scan.
type RepairStats struct {
	Scanned       int   `json:"scanned"`
	Repaired      int   `json:"repaired"`
	Undecidable   int   `json:"undecidable"`
	Unrecoverable int   `json:"unrecoverable"`
	BytesCopied   int64 `json:"bytesCopied"`
	DurationMs    int64 `json:"durationMs"`
	// LastRun is absent until the first scan finishes, so a freshly started node
	// reports no scan rather than a zero timestamp in the year 1.
	LastRun *time.Time `json:"lastRun,omitempty"`
}

// ReplicaRepairer restores a bucket's replica count after a node is lost.
//
// Copies are placed when an object is written, and nothing re-made them
// afterwards: a node that died for good took its copies with it and every
// object that had one there stayed a copy short, quietly, forever. Rebalance
// does not close that gap because it asks a different question. It moves an
// object when the ring says another node now owns it, and an object whose
// primary never changed is one it skips no matter how few copies survive.
//
// The scan reads metadata rather than the local disk. Metadata is the
// authoritative record (issue #34), so it still lists an object whose bytes
// this node has lost, which is exactly the case worth repairing and the one a
// disk walk cannot see.
//
// Erasure-coded buckets are left alone. The erasure healer rebuilds those from
// parity, and two repairers on one object would duplicate work and fight.
type ReplicaRepairer struct {
	store  metadata.StoreAPI
	engine storage.Engine
	ring   *HashRing
	proxy  *Proxy
	selfID string
	secret string
	scheme string

	replicasFor      func(bucket string) int
	bucketEncrypted  func(bucket string) bool
	erasureFor       func(bucket string) bool
	rebalanceRunning func() bool

	// coordinateAll is set when metadata is sharded across Raft groups. Each
	// node then iterates only the shards it leads, so an object's metadata is
	// visible to exactly one node and that node must repair it whether or not
	// it holds the data. Without sharding every node sees every object, so work
	// is claimed by the ring primary to keep N nodes from repairing the same
	// object N times.
	coordinateAll bool

	interval     time.Duration
	maxBandwidth int64
	batchSize    int
	client       *http.Client
	// probeClient is deliberately not the client that moves data. A probe is a
	// question, and the answer is either quick or not coming: a node that is
	// merely powered off drops the packets rather than refusing them, so a probe
	// on the transfer client's timeout hangs for minutes. One dead node then
	// stalls the whole scan, which is precisely when repair is needed most.
	probeClient *http.Client

	mu         sync.Mutex
	running    atomic.Bool
	cancelFunc context.CancelFunc
	stats      atomic.Value
}

func applyRepairDefaults(c *RepairConfig) {
	if c.IntervalSecs == 0 {
		c.IntervalSecs = 600
	}
	if c.MaxBandwidthMBps <= 0 {
		c.MaxBandwidthMBps = 50
	}
	if c.BatchSize <= 0 {
		c.BatchSize = 100
	}
}

// NewReplicaRepairer builds a repairer. A negative IntervalSecs disables the
// background scan, matching erasure's heal_interval_secs; Trigger still works
// so an operator can run a pass by hand.
func NewReplicaRepairer(
	store metadata.StoreAPI,
	engine storage.Engine,
	ring *HashRing,
	proxy *Proxy,
	selfID, secret, scheme string,
	cfg RepairConfig,
) *ReplicaRepairer {
	applyRepairDefaults(&cfg)
	r := &ReplicaRepairer{
		store:        store,
		engine:       engine,
		ring:         ring,
		proxy:        proxy,
		selfID:       selfID,
		secret:       secret,
		scheme:       scheme,
		interval:     time.Duration(cfg.IntervalSecs) * time.Second,
		maxBandwidth: int64(cfg.MaxBandwidthMBps) * 1024 * 1024,
		batchSize:    cfg.BatchSize,
		client:       &http.Client{Timeout: 5 * time.Minute},
		probeClient: &http.Client{
			Timeout: 10 * time.Second,
			Transport: &http.Transport{
				DialContext:         (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
				TLSHandshakeTimeout: 5 * time.Second,
			},
		},
	}
	r.stats.Store(RepairStats{})
	return r
}

// SetReplicaPolicy wires the per-bucket replica count (issue #39).
func (r *ReplicaRepairer) SetReplicaPolicy(fn func(bucket string) int) { r.replicasFor = fn }

// SetErasurePolicy wires the per-bucket erasure flag so erasure-coded buckets
// are left to the erasure healer.
func (r *ReplicaRepairer) SetErasurePolicy(fn func(bucket string) bool) { r.erasureFor = fn }

// SetBucketEncrypted wires the bucket encryption lookup, so a repaired copy is
// stored the way its source stores it rather than in the clear on a node whose
// view of the config has not caught up.
func (r *ReplicaRepairer) SetBucketEncrypted(fn func(bucket string) bool) { r.bucketEncrypted = fn }

// SetRebalanceGuard wires a check that reports whether a rebalance is running.
// The two must not overlap: rebalance deletes the local copy after handing an
// object to its new owner, and a repairer watching that would read the gap as a
// lost replica and copy it straight back.
func (r *ReplicaRepairer) SetRebalanceGuard(fn func() bool) { r.rebalanceRunning = fn }

// SetShardedMetadata tells the repairer that metadata is split across Raft
// groups, so it should repair every object it can see rather than only those it
// is ring primary for.
func (r *ReplicaRepairer) SetShardedMetadata(sharded bool) { r.coordinateAll = sharded }

// Run drives the periodic scan until ctx is cancelled.
func (r *ReplicaRepairer) Run(ctx context.Context) {
	if r.interval <= 0 {
		slog.Info("replica repair: background scan disabled", "interval_secs", int(r.interval.Seconds()))
		return
	}
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.Trigger()
		}
	}
}

// Trigger starts a scan in the background. Only one runs at a time.
func (r *ReplicaRepairer) Trigger() {
	if r.running.Load() {
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
		r.scan(ctx)
	}()
}

// Stop cancels an in-progress scan.
func (r *ReplicaRepairer) Stop() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cancelFunc != nil {
		r.cancelFunc()
		r.cancelFunc = nil
	}
}

// IsRunning reports whether a scan is in progress.
func (r *ReplicaRepairer) IsRunning() bool { return r.running.Load() }

// Status returns the outcome of the last completed scan.
func (r *ReplicaRepairer) Status() RepairStats {
	s, _ := r.stats.Load().(RepairStats)
	return s
}

// scan walks every object this node is responsible for and tops up any that
// hold fewer copies than their bucket asks for.
func (r *ReplicaRepairer) scan(ctx context.Context) {
	if r.rebalanceRunning != nil && r.rebalanceRunning() {
		slog.Info("replica repair: rebalance in progress, skipping scan")
		return
	}
	start := time.Now()
	var st RepairStats

	// Resolve every per-bucket setting BEFORE walking the objects. Both lookups
	// read the metadata store, and the walk below holds that store's lock for its
	// whole duration: calling back into it from inside the walk re-enters a lock
	// this goroutine already holds, which deadlocks the store the moment a writer
	// queues behind it. Nothing that reads metadata can proceed after that, so a
	// node wedges completely while still answering its health check.
	policy := r.bucketPolicies()

	// Collect first, act second, for the same reason. The callback below must not
	// touch the store, the network or the disk: it only decides whether an object
	// is this node's to repair and remembers it.
	type candidate struct{ bucket, key string }
	var todo []candidate
	truncated := false
	err := r.store.IterateAllObjects(func(bucket, key string, _ metadata.ObjectMeta) bool {
		if ctx.Err() != nil {
			return false
		}
		p, known := policy[bucket]
		if !known || p.replicas <= 1 || p.erasure {
			return true
		}
		if !r.owns(bucket, key) {
			return true
		}
		if len(todo) >= maxRepairCandidates {
			truncated = true
			return false
		}
		todo = append(todo, candidate{bucket, key})
		return true
	})
	if err != nil {
		slog.Warn("replica repair: metadata scan failed", "error", err)
	}
	if truncated {
		slog.Warn("replica repair: too many objects for one pass, the rest follow on the next scan",
			"limit", maxRepairCandidates)
	}

	for _, c := range todo {
		if ctx.Err() != nil {
			break
		}
		st.Scanned++
		copied, outcome := r.repairObject(ctx, c.bucket, c.key, policy[c.bucket].replicas)
		switch outcome {
		case repairDone:
			st.Repaired++
			st.BytesCopied += copied
			r.throttle(ctx, copied, start, &st)
		case repairUndecidable:
			st.Undecidable++
		case repairLost:
			st.Unrecoverable++
		}
	}

	st.DurationMs = time.Since(start).Milliseconds()
	finished := time.Now()
	st.LastRun = &finished
	r.stats.Store(st)

	if st.Repaired > 0 || st.Unrecoverable > 0 {
		slog.Info("replica repair: scan complete",
			"scanned", st.Scanned,
			"repaired", st.Repaired,
			"undecidable", st.Undecidable,
			"unrecoverable", st.Unrecoverable,
			"bytes", st.BytesCopied,
			"duration", time.Since(start).Round(time.Millisecond),
		)
	}
}

// maxRepairCandidates bounds how many objects one pass holds in memory. A pass
// that hits the limit repairs what it took and leaves the rest to the next one.
const maxRepairCandidates = 200000

type bucketPolicy struct {
	replicas int
	erasure  bool
}

// bucketPolicies reads each bucket's durability once, before the object walk, so
// the walk itself never calls back into the metadata store.
func (r *ReplicaRepairer) bucketPolicies() map[string]bucketPolicy {
	out := map[string]bucketPolicy{}
	buckets, err := r.store.ListBuckets()
	if err != nil {
		slog.Warn("replica repair: cannot list buckets", "error", err)
		return out
	}
	for _, b := range buckets {
		p := bucketPolicy{replicas: 1}
		if r.replicasFor != nil {
			if n := r.replicasFor(b.Name); n > 0 {
				p.replicas = n
			}
		}
		if r.erasureFor != nil {
			p.erasure = r.erasureFor(b.Name)
		}
		out[b.Name] = p
	}
	return out
}

// owns reports whether this node repairs the given object. See coordinateAll.
func (r *ReplicaRepairer) owns(bucket, key string) bool {
	if r.coordinateAll {
		return true
	}
	primary := r.ring.GetNode(bucket, key)
	return primary == r.selfID || primary == ""
}

type repairOutcome int

const (
	repairNoop repairOutcome = iota
	repairDone
	repairUndecidable // a holder could not be reached, so nothing can be concluded
	repairLost        // no node has the bytes any more
)

// repairObject tops one object back up to its replica count.
//
// The probe distinguishes three answers, and the difference is the whole safety
// argument. A clean 404 means the node genuinely lacks the object and a copy is
// owed. A 200 means it has one. Anything else, a timeout, a refused connection,
// a 500, means this node cannot say, and an unreachable peer is never counted
// as a lost copy: a brief network partition would otherwise read as every
// object on the far side being under-replicated and start a copy storm at the
// worst possible moment.
func (r *ReplicaRepairer) repairObject(ctx context.Context, bucket, key string, want int) (int64, repairOutcome) {
	addrs := r.proxy.NodeAddrs()
	holders := r.ring.GetNodes(bucket, key, want)

	var missing []string
	var present []string
	unknown := false

	for _, id := range holders {
		if id == r.selfID {
			if r.engine.ObjectExists(bucket, key) {
				present = append(present, id)
			} else {
				missing = append(missing, id)
			}
			continue
		}
		if addrs[id] == "" {
			unknown = true // not a member we can reach, so we cannot judge it
			continue
		}
		switch r.probe(ctx, addrs[id], bucket, key) {
		case probeHas:
			present = append(present, id)
		case probeMissing:
			missing = append(missing, id)
		default:
			unknown = true
		}
	}

	if len(missing) == 0 {
		if unknown {
			return 0, repairUndecidable
		}
		return 0, repairNoop
	}
	if len(present) == 0 {
		// Every node that should hold this object says it does not, so there is
		// nothing left to copy from. Say so loudly and touch nothing: metadata
		// still describes the object and deleting it here would turn a
		// recoverable operator problem into real data loss.
		slog.Error("replica repair: no surviving copy",
			"bucket", bucket, "key", key, "wanted", want,
		)
		return 0, repairLost
	}

	source := present[0]
	var copied int64
	repaired := false
	for _, target := range missing {
		var err error
		var n int64
		switch {
		case target == r.selfID:
			n, err = r.pullLocal(ctx, addrs[source], bucket, key)
		case source == r.selfID:
			n, err = r.push(ctx, addrs[target], bucket, key)
		default:
			// This node coordinates but holds no copy, so it asks the node that
			// is short one to fetch from a node that has it. The bytes go
			// straight between them instead of through here.
			n, err = r.askPull(ctx, addrs[target], source, bucket, key)
		}
		if err != nil {
			slog.Warn("replica repair: copy failed",
				"bucket", bucket, "key", key, "target", target, "error", err,
			)
			continue
		}
		copied += n
		repaired = true
	}
	if !repaired {
		return 0, repairUndecidable
	}
	return copied, repairDone
}

type probeResult int

const (
	probeUnknown probeResult = iota
	probeHas
	probeMissing
)

func (r *ReplicaRepairer) probe(ctx context.Context, addr, bucket, key string) probeResult {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, r.replicaURL(addr, "replica-get", bucket, key), nil)
	if err != nil {
		return probeUnknown
	}
	r.sign(req)
	resp, err := r.probeClient.Do(req)
	if err != nil {
		return probeUnknown
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		return probeHas
	case http.StatusNotFound:
		return probeMissing
	default:
		return probeUnknown
	}
}

// push streams a local copy to a node that is missing one.
func (r *ReplicaRepairer) push(ctx context.Context, addr, bucket, key string) (int64, error) {
	reader, size, err := r.engine.GetObject(bucket, key)
	if err != nil {
		return 0, err
	}
	defer reader.Close()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.replicaURL(addr, "replica-put", bucket, key), reader)
	if err != nil {
		return 0, err
	}
	req.ContentLength = size
	r.sign(req)
	if r.bucketEncrypted != nil && r.bucketEncrypted(bucket) {
		req.Header.Set(bucketEncryptedHeader, "1")
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("replica put returned %d", resp.StatusCode)
	}
	return size, nil
}

// pullLocal fetches a copy from a peer into this node's own engine.
func (r *ReplicaRepairer) pullLocal(ctx context.Context, addr, bucket, key string) (int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.replicaURL(addr, "replica-get", bucket, key), nil)
	if err != nil {
		return 0, err
	}
	r.sign(req)
	resp, err := r.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("replica get returned %d", resp.StatusCode)
	}
	// The source says this bucket is encrypted and this node does not know that
	// yet, so writing here would store the copy in the clear. Leave it for the
	// next pass, by which time the config has arrived.
	if resp.Header.Get(bucketEncryptedHeader) == "1" &&
		r.bucketEncrypted != nil && !r.bucketEncrypted(bucket) {
		return 0, fmt.Errorf("bucket encryption config has not reached this node yet")
	}
	n, _, err := r.engine.PutObject(bucket, key, resp.Body, resp.ContentLength)
	return n, err
}

// askPull tells the node that is short a copy to fetch one from a node that has
// it. The source travels as a node ID, never an address: the receiver resolves
// it against its own membership, so a caller cannot aim it at an arbitrary host.
func (r *ReplicaRepairer) askPull(ctx context.Context, addr, sourceID, bucket, key string) (int64, error) {
	u := r.replicaURL(addr, "replica-pull", bucket, key) + "&from=" + url.QueryEscape(sourceID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, nil)
	if err != nil {
		return 0, err
	}
	r.sign(req)
	resp, err := r.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("replica pull returned %d", resp.StatusCode)
	}
	return 0, nil
}

func (r *ReplicaRepairer) replicaURL(addr, op, bucket, key string) string {
	return r.scheme + "://" + addr + "/cluster/" + op +
		"?bucket=" + url.QueryEscape(bucket) + "&key=" + url.QueryEscape(key)
}

func (r *ReplicaRepairer) sign(req *http.Request) {
	if r.secret != "" {
		req.Header.Set("X-Cluster-Secret", r.secret)
	}
}

// throttle keeps the scan inside its bandwidth budget. Repair runs against a
// cluster that has just lost a node, which is the moment it can least afford a
// background job saturating the network.
func (r *ReplicaRepairer) throttle(ctx context.Context, justCopied int64, start time.Time, st *RepairStats) {
	if r.maxBandwidth <= 0 || justCopied <= 0 {
		return
	}
	elapsed := time.Since(start).Seconds()
	if elapsed <= 0 {
		return
	}
	if float64(st.BytesCopied)/elapsed <= float64(r.maxBandwidth) {
		return
	}
	sleep := time.Duration(float64(justCopied) / float64(r.maxBandwidth) * float64(time.Second))
	select {
	case <-ctx.Done():
	case <-time.After(sleep):
	}
}
