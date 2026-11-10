package cluster

import (
	"context"
	"errors"
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
	copies *metadata.CopyIndex // nil: no records, every copy judged by presence
	// plainCursor and versionCursor are where the next pass's walks go on
	// (entries to skip), so a cluster larger than one pass is covered.
	plainCursor, versionCursor int
	ring                       *HashRing
	proxy                      *Proxy
	selfID                     string
	secret                     string
	scheme                     string

	replicasFor     func(bucket string) int
	bucketEncrypted func(bucket string) bool
	erasureFor      func(bucket string) bool

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

// SetRebalanceGuard is kept for callers and ignored. It used to hold repair off
// while a rebalance ran, because rebalance deleted local copies. Rebalance no
// longer moves data, and a guard that could suppress the one mechanism restoring
// lost replicas is not worth keeping in case it ever reports true again.
func (r *ReplicaRepairer) SetRebalanceGuard(fn func() bool) {}

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
	type candidate struct{ bucket, key, version string }
	var todo []candidate
	// Unversioned objects and versions each take half a pass, and each walk
	// goes on where the last pass stopped: every pass used to start from the
	// first object, so past the limit the same objects were probed again and
	// again, the rest never, and versions not at all.
	half := maxRepairCandidates / 2
	walk := func(cursor *int, add func(func(candidate) bool) error, what string) {
		seen, taken, full := 0, 0, false
		err := add(func(c candidate) bool {
			if ctx.Err() != nil {
				return false
			}
			seen++
			if seen <= *cursor {
				return true
			}
			if taken >= half {
				full = true
				return false
			}
			todo = append(todo, c)
			taken++
			return true
		})
		if err != nil && !errors.Is(err, metadata.ErrStopIteration) {
			slog.Warn("replica repair: "+what+" scan failed", "error", err)
		}
		if full {
			*cursor += taken
			slog.Info("replica repair: more "+what+" than one pass takes, the next pass goes on from here", "limit", half)
		} else if ctx.Err() == nil {
			*cursor = 0
		}
	}
	walk(&r.plainCursor, func(take func(candidate) bool) error {
		return r.store.IterateAllObjects(func(bucket, key string, meta metadata.ObjectMeta) bool {
			// A key with versions is repaired version by version below: its
			// bytes are not at the plain path, so looking there reported every
			// versioned object as lost ("no surviving copy").
			if meta.VersionID != "" || meta.DeleteMarker {
				return true
			}
			if p, known := policy[bucket]; !known || p.replicas <= 1 || p.erasure || !r.owns(bucket, key) {
				return true
			}
			return take(candidate{bucket, key, ""})
		})
	}, "object")
	walk(&r.versionCursor, func(take func(candidate) bool) error {
		return r.store.ScanObjectVersions(func(v metadata.ObjectMeta) bool {
			if v.DeleteMarker || v.VersionID == "" {
				return true
			}
			if p, known := policy[v.Bucket]; !known || p.replicas <= 1 || p.erasure || !r.owns(v.Bucket, v.Key) {
				return true
			}
			return take(candidate{v.Bucket, v.Key, v.VersionID})
		})
	}, "version")

	for _, c := range todo {
		if ctx.Err() != nil {
			break
		}
		st.Scanned++
		copied, outcome := r.repairObject(ctx, c.bucket, c.key, c.version, policy[c.bucket].replicas)
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
func (r *ReplicaRepairer) repairObject(ctx context.Context, bucket, key, version string, want int) (int64, repairOutcome) {
	addrs := r.proxy.NodeAddrs()
	holders := r.ring.GetNodes(bucket, key, want)

	// The object the metadata names. A record that is gone (deleted since
	// the walk) leaves nothing to repair.
	record, err := r.recordETag(bucket, key, version)
	if err != nil {
		return 0, repairNoop
	}

	var missing []string
	var present []string
	var verified []string // holders whose copy is recorded as this object
	unknown, wrong := false, 0

	// A copy recorded as another object (metadata.CopyIndex) is replaced like
	// a missing one: a node that was down while the object was overwritten
	// kept the previous bytes, and repair, which looked for missing copies
	// only, never replaced them. A copy with no record is trusted as before.
	judge := func(id, copyETag string) {
		switch {
		case copyETag != "" && copyETag != record:
			missing = append(missing, id)
			wrong++
		case copyETag != "":
			present = append(present, id)
			verified = append(verified, id)
		default:
			present = append(present, id)
		}
	}
	for _, id := range holders {
		if id == r.selfID {
			if !r.hasLocal(bucket, key, version) {
				missing = append(missing, id)
				continue
			}
			etag, _ := r.copies.Lookup(bucket, key, version, storage.LocalCopySize(r.engine, bucket, key, version))
			judge(id, etag)
			continue
		}
		if addrs[id] == "" {
			unknown = true // not a member we can reach, so we cannot judge it
			continue
		}
		switch res, etag := r.probe(ctx, addrs[id], bucket, key, version); res {
		case probeHas:
			judge(id, etag)
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
			"bucket", bucket, "key", key, "version", version, "wanted", want, "copies_of_another_object", wrong,
		)
		return 0, repairLost
	}

	source := present[0]
	if len(verified) > 0 {
		source = verified[0]
	}
	var copied int64
	repaired := false
	for _, target := range missing {
		var err error
		var n int64
		switch {
		case target == r.selfID:
			n, err = r.pullLocal(ctx, addrs[source], bucket, key, version)
		case source == r.selfID:
			n, err = r.push(ctx, addrs[target], bucket, key, version)
		default:
			// This node coordinates but holds no copy, so it asks the node that
			// is short one to fetch from a node that has it. The bytes go
			// straight between them instead of through here.
			n, err = r.askPull(ctx, addrs[target], source, bucket, key, version)
		}
		if err != nil {
			slog.Warn("replica repair: copy failed",
				"bucket", bucket, "key", key, "version", version, "target", target, "error", err,
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

func (r *ReplicaRepairer) probe(ctx context.Context, addr, bucket, key, version string) (probeResult, string) {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, r.replicaURL(addr, "replica-get", bucket, key, version), nil)
	if err != nil {
		return probeUnknown, ""
	}
	r.sign(req)
	resp, err := r.probeClient.Do(req)
	if err != nil {
		return probeUnknown, ""
	}
	defer func() { _ = resp.Body.Close() }()
	switch resp.StatusCode {
	case http.StatusOK:
		// A node still on 5.0.14 ignores the version parameter and answers
		// about the plain file: that says nothing about the version.
		if version != "" && resp.Header.Get(copyVersionHeader) != version {
			return probeUnknown, ""
		}
		return probeHas, resp.Header.Get(copyETagHeader)
	case http.StatusNotFound:
		return probeMissing, ""
	default:
		return probeUnknown, ""
	}
}

// push streams a local copy to a node that is missing one.
func (r *ReplicaRepairer) push(ctx context.Context, addr, bucket, key, version string) (int64, error) {
	// The copy names the object it is, as a write's copy does: the receiver
	// stores it only while its record names the same ETag. A node still on
	// 5.0.14 ignores the version parameter, and without the ETag stored a
	// version's bytes at the key's plain path; with it, it refuses the copy.
	etag, err := r.recordETag(bucket, key, version)
	if err != nil {
		return 0, err
	}
	reader, size, err := r.openLocal(bucket, key, version)
	if err != nil {
		return 0, err
	}
	defer reader.Close()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.replicaURL(addr, "replica-put", bucket, key, version), reader)
	if err != nil {
		return 0, err
	}
	req.ContentLength = size
	r.sign(req)
	req.Header.Set(replicaETagHeader, etag)
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
func (r *ReplicaRepairer) pullLocal(ctx context.Context, addr, bucket, key, version string) (int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.replicaURL(addr, "replica-get", bucket, key, version), nil)
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
	// A node still on 5.0.14 ignores the version parameter and answers about
	// the key's plain file, which would be stored as the version.
	if version != "" && resp.Header.Get(copyVersionHeader) != version {
		return 0, fmt.Errorf("the source did not answer for version %s", version)
	}
	copyETag := resp.Header.Get(copyETagHeader)
	if record, err := r.recordETag(bucket, key, version); err != nil || (copyETag != "" && copyETag != record) {
		return 0, fmt.Errorf("the source's copy is not the current object")
	}
	// Staged without the key's lock, installed under it: a client overwrite
	// on this node holds the lock (shared) from its write to its metadata
	// commit, and in that window its file is recorded as the new object
	// while the metadata still names the old one, which repair took for a
	// wrong copy and replaced with the old bytes. Held across the transfer,
	// the lock made every client write of the key wait for the network.
	staged, err := storage.StageCopy(r.engine.DataDir(), resp.Body, resp.ContentLength)
	if err != nil {
		return 0, err
	}
	defer staged.Close()
	unlock := storage.LockObjectKey(bucket, storage.VersionLockKey(key, version))
	defer unlock()
	// The record again, caught up with the leader: this node's own metadata
	// can lag a write it forwarded a moment ago.
	record, err := r.recordETagConsistent(bucket, key, version)
	if err != nil {
		return 0, err
	}
	if local, ok := r.copies.Lookup(bucket, key, version, storage.LocalCopySize(r.engine, bucket, key, version)); ok && local == record {
		return 0, nil // this node's copy is the current object after all
	}
	if copyETag != "" && copyETag != record {
		return 0, fmt.Errorf("the source's copy is not the current object")
	}
	body, err := staged.Reader()
	if err != nil {
		return 0, err
	}
	var n int64
	if version != "" {
		n, _, err = r.engine.PutObjectVersion(bucket, key, version, body, staged.Size)
	} else {
		n, _, err = r.engine.PutObject(bucket, key, body, staged.Size)
	}
	if err == nil {
		r.copies.Record(bucket, key, version, copyETag, storage.LocalCopySize(r.engine, bucket, key, version))
	}
	return n, err
}

// askPull tells the node that is short a copy to fetch one from a node that has
// it. The source travels as a node ID, never an address: the receiver resolves
// it against its own membership, so a caller cannot aim it at an arbitrary host.
func (r *ReplicaRepairer) askPull(ctx context.Context, addr, sourceID, bucket, key, version string) (int64, error) {
	u := r.replicaURL(addr, "replica-pull", bucket, key, version) + "&from=" + url.QueryEscape(sourceID)
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

func (r *ReplicaRepairer) replicaURL(addr, op, bucket, key, version string) string {
	u := r.scheme + "://" + addr + "/cluster/" + op +
		"?bucket=" + url.QueryEscape(bucket) + "&key=" + url.QueryEscape(key)
	if version != "" {
		u += "&version=" + url.QueryEscape(version)
	}
	return u
}

// openLocal opens this node's copy of an object's plain data or of one
// version. The null version of an object written before versioning was
// enabled is still at the plain path.
func (r *ReplicaRepairer) openLocal(bucket, key, version string) (storage.ReadSeekCloser, int64, error) {
	if version == "" {
		return r.engine.GetObject(bucket, key)
	}
	rc, size, err := r.engine.GetObjectVersion(bucket, key, version)
	if err != nil && version == "null" {
		return r.engine.GetObject(bucket, key)
	}
	return rc, size, err
}

func (r *ReplicaRepairer) hasLocal(bucket, key, version string) bool {
	if version == "" {
		return r.engine.ObjectExists(bucket, key)
	}
	rc, _, err := r.openLocal(bucket, key, version)
	if err != nil {
		return false
	}
	_ = rc.Close()
	return true
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

// replicaETagHeader and copyETagHeader are api.ReplicaETagHeader and
// api.CopyETagHeader (api imports this package).
const (
	replicaETagHeader = "X-VaultS3-Replica-ETag"
	copyETagHeader    = "X-VaultS3-Copy-ETag"
	copyVersionHeader = "X-VaultS3-Copy-Version"
)

// SetCopyIndex wires the node-local record of which object each local data
// file holds.
func (r *ReplicaRepairer) SetCopyIndex(c *metadata.CopyIndex) { r.copies = c }

// recordETagConsistent is recordETag read after catching up with the leader.
func (r *ReplicaRepairer) recordETagConsistent(bucket, key, version string) (string, error) {
	meta, err := r.store.GetObjectMetaConsistent(bucket, key)
	if err != nil && errors.Is(err, metadata.ErrShardUnavailable) {
		return "", err
	}
	if version == "" {
		if meta == nil || meta.DeleteMarker || meta.ETag == "" || meta.VersionID != "" {
			return "", fmt.Errorf("no record to name the copy")
		}
		return meta.ETag, nil
	}
	return r.recordETag(bucket, key, version)
}

// recordETag is the ETag the metadata names for an object's plain data or
// one of its versions. Read while repairing, after the object walk.
func (r *ReplicaRepairer) recordETag(bucket, key, version string) (string, error) {
	var meta *metadata.ObjectMeta
	var err error
	if version == "" {
		meta, err = r.store.GetObjectMeta(bucket, key)
	} else {
		meta, err = r.store.GetObjectVersion(bucket, key, version)
	}
	if err != nil {
		return "", err
	}
	if meta == nil || meta.DeleteMarker || meta.ETag == "" || (version == "" && meta.VersionID != "") {
		return "", fmt.Errorf("no record to name the copy")
	}
	return meta.ETag, nil
}
