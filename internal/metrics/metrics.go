package metrics

import (
	"fmt"
	"net/http"
	"runtime"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
	"github.com/Kodiqa-Solutions/VaultS3/internal/storage"
)

// bucketMetrics holds per-bucket request counters.
type bucketMetrics struct {
	requests [methodCount]atomic.Int64
	bytesIn  atomic.Int64
	bytesOut atomic.Int64
	errors   atomic.Int64
}

// Collector tracks request metrics and exposes Prometheus-compatible /metrics.
type Collector struct {
	store  *metadata.Store
	engine storage.Engine

	// Request counters by method
	requestsTotal [methodCount]atomic.Int64
	requestErrors atomic.Int64
	bytesIn       atomic.Int64
	bytesOut      atomic.Int64
	startTime     time.Time

	// Per-bucket metrics
	bucketMu      sync.RWMutex
	bucketMetrics map[string]*bucketMetrics

	// Request latency histogram
	latencyMu      sync.Mutex
	latencyBuckets [latencyBucketCount]atomic.Int64
	latencySum     atomic.Int64 // microseconds
	latencyCount   atomic.Int64
}

// Histogram bucket boundaries in seconds
var latencyBounds = [latencyBucketCount]float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}

const latencyBucketCount = 11

// HTTP method indices for counter array
const (
	mGET = iota
	mPUT
	mDELETE
	mHEAD
	mPOST
	mOTHER
	methodCount
)

func methodIndex(method string) int {
	switch method {
	case http.MethodGet:
		return mGET
	case http.MethodPut:
		return mPUT
	case http.MethodDelete:
		return mDELETE
	case http.MethodHead:
		return mHEAD
	case http.MethodPost:
		return mPOST
	default:
		return mOTHER
	}
}

func methodLabel(idx int) string {
	switch idx {
	case mGET:
		return "GET"
	case mPUT:
		return "PUT"
	case mDELETE:
		return "DELETE"
	case mHEAD:
		return "HEAD"
	case mPOST:
		return "POST"
	default:
		return "OTHER"
	}
}

func NewCollector(store *metadata.Store, engine storage.Engine) *Collector {
	return &Collector{
		store:         store,
		engine:        engine,
		startTime:     time.Now(),
		bucketMetrics: make(map[string]*bucketMetrics),
	}
}

const maxBucketMetrics = 100

// StartTime returns when the collector was created (server start time).
func (c *Collector) StartTime() time.Time {
	return c.startTime
}

// RequestsByMethod returns request counts per HTTP method.
func (c *Collector) RequestsByMethod() map[string]int64 {
	m := make(map[string]int64, methodCount)
	for i := 0; i < methodCount; i++ {
		v := c.requestsTotal[i].Load()
		if v > 0 {
			m[methodLabel(i)] = v
		}
	}
	return m
}

// TotalRequests returns the total number of requests.
func (c *Collector) TotalRequests() int64 {
	var total int64
	for i := 0; i < methodCount; i++ {
		total += c.requestsTotal[i].Load()
	}
	return total
}

// TotalErrors returns the total number of errors.
func (c *Collector) TotalErrors() int64 {
	return c.requestErrors.Load()
}

// TotalBytesIn returns total bytes received.
func (c *Collector) TotalBytesIn() int64 {
	return c.bytesIn.Load()
}

// TotalBytesOut returns total bytes sent.
func (c *Collector) TotalBytesOut() int64 {
	return c.bytesOut.Load()
}

// RecordRequest increments the request counter for the given method.
func (c *Collector) RecordRequest(method string) {
	c.requestsTotal[methodIndex(method)].Add(1)
}

// RecordError increments the error counter.
func (c *Collector) RecordError() {
	c.requestErrors.Add(1)
}

// RecordBytesIn adds to the ingress byte counter.
func (c *Collector) RecordBytesIn(n int64) {
	c.bytesIn.Add(n)
}

// RecordBytesOut adds to the egress byte counter.
func (c *Collector) RecordBytesOut(n int64) {
	c.bytesOut.Add(n)
}

// RecordLatency records a request duration in the histogram.
func (c *Collector) RecordLatency(d time.Duration) {
	secs := d.Seconds()
	for i, bound := range latencyBounds {
		if secs <= bound {
			c.latencyBuckets[i].Add(1)
		}
	}
	c.latencySum.Add(d.Microseconds())
	c.latencyCount.Add(1)
}

// getBucketMetrics returns the per-bucket metrics entry, creating it if needed.
func (c *Collector) getBucketMetrics(bucket string) *bucketMetrics {
	c.bucketMu.RLock()
	bm, ok := c.bucketMetrics[bucket]
	c.bucketMu.RUnlock()
	if ok {
		return bm
	}

	c.bucketMu.Lock()
	defer c.bucketMu.Unlock()
	// Double-check after acquiring write lock
	if bm, ok = c.bucketMetrics[bucket]; ok {
		return bm
	}
	// Limit to prevent label explosion
	if len(c.bucketMetrics) >= maxBucketMetrics {
		return nil
	}
	bm = &bucketMetrics{}
	c.bucketMetrics[bucket] = bm
	return bm
}

// RecordBucketRequest records a request for a specific bucket.
func (c *Collector) RecordBucketRequest(bucket, method string) {
	if bm := c.getBucketMetrics(bucket); bm != nil {
		bm.requests[methodIndex(method)].Add(1)
	}
}

// RecordBucketBytesIn records bytes uploaded to a specific bucket.
func (c *Collector) RecordBucketBytesIn(bucket string, n int64) {
	if bm := c.getBucketMetrics(bucket); bm != nil {
		bm.bytesIn.Add(n)
	}
}

// RecordBucketBytesOut records bytes downloaded from a specific bucket.
func (c *Collector) RecordBucketBytesOut(bucket string, n int64) {
	if bm := c.getBucketMetrics(bucket); bm != nil {
		bm.bytesOut.Add(n)
	}
}

// RecordBucketError records an error for a specific bucket.
func (c *Collector) RecordBucketError(bucket string) {
	if bm := c.getBucketMetrics(bucket); bm != nil {
		bm.errors.Add(1)
	}
}

// ServeHTTP handles GET /metrics in Prometheus exposition format.
func (c *Collector) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")

	// Process metrics
	var totalRequests int64
	for i := 0; i < methodCount; i++ {
		v := c.requestsTotal[i].Load()
		totalRequests += v
		fmt.Fprintf(w, "vaults3_requests_total{method=%q} %d\n", methodLabel(i), v)
	}
	fmt.Fprintf(w, "vaults3_requests_total_sum %d\n", totalRequests)
	fmt.Fprintf(w, "vaults3_request_errors_total %d\n", c.requestErrors.Load())
	fmt.Fprintf(w, "vaults3_bytes_received_total %d\n", c.bytesIn.Load())
	fmt.Fprintf(w, "vaults3_bytes_sent_total %d\n", c.bytesOut.Load())

	// Uptime
	fmt.Fprintf(w, "vaults3_uptime_seconds %.0f\n", time.Since(c.startTime).Seconds())

	// Storage metrics — per bucket
	buckets, err := c.store.ListBuckets()
	if err == nil {
		fmt.Fprintf(w, "vaults3_buckets_total %d\n", len(buckets))

		var totalSize, totalObjects int64
		for _, b := range buckets {
			size, count, err := c.engine.BucketSize(b.Name)
			if err != nil {
				continue
			}
			totalSize += size
			totalObjects += count
			fmt.Fprintf(w, "vaults3_bucket_size_bytes{bucket=%q} %d\n", b.Name, size)
			fmt.Fprintf(w, "vaults3_bucket_objects_total{bucket=%q} %d\n", b.Name, count)
			if b.MaxSizeBytes > 0 {
				fmt.Fprintf(w, "vaults3_bucket_quota_size_bytes{bucket=%q} %d\n", b.Name, b.MaxSizeBytes)
			}
			if b.MaxObjects > 0 {
				fmt.Fprintf(w, "vaults3_bucket_quota_objects{bucket=%q} %d\n", b.Name, b.MaxObjects)
			}
		}
		fmt.Fprintf(w, "vaults3_storage_size_bytes_total %d\n", totalSize)
		fmt.Fprintf(w, "vaults3_objects_total %d\n", totalObjects)
	}

	// Per-bucket request metrics
	c.bucketMu.RLock()
	bucketNames := make([]string, 0, len(c.bucketMetrics))
	for name := range c.bucketMetrics {
		bucketNames = append(bucketNames, name)
	}
	c.bucketMu.RUnlock()
	sort.Strings(bucketNames)
	for _, name := range bucketNames {
		c.bucketMu.RLock()
		bm := c.bucketMetrics[name]
		c.bucketMu.RUnlock()
		if bm == nil {
			continue
		}
		for i := 0; i < methodCount; i++ {
			v := bm.requests[i].Load()
			if v > 0 {
				fmt.Fprintf(w, "vaults3_bucket_requests_total{bucket=%q,method=%q} %d\n", name, methodLabel(i), v)
			}
		}
		if v := bm.bytesIn.Load(); v > 0 {
			fmt.Fprintf(w, "vaults3_bucket_bytes_in_total{bucket=%q} %d\n", name, v)
		}
		if v := bm.bytesOut.Load(); v > 0 {
			fmt.Fprintf(w, "vaults3_bucket_bytes_out_total{bucket=%q} %d\n", name, v)
		}
		if v := bm.errors.Load(); v > 0 {
			fmt.Fprintf(w, "vaults3_bucket_errors_total{bucket=%q} %d\n", name, v)
		}
	}

	// Request latency histogram
	for i, bound := range latencyBounds {
		fmt.Fprintf(w, "vaults3_request_duration_seconds_bucket{le=\"%.3f\"} %d\n", bound, c.latencyBuckets[i].Load())
	}
	fmt.Fprintf(w, "vaults3_request_duration_seconds_bucket{le=\"+Inf\"} %d\n", c.latencyCount.Load())
	fmt.Fprintf(w, "vaults3_request_duration_seconds_sum %.6f\n", float64(c.latencySum.Load())/1e6)
	fmt.Fprintf(w, "vaults3_request_duration_seconds_count %d\n", c.latencyCount.Load())

	// Go runtime metrics
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)
	fmt.Fprintf(w, "vaults3_go_goroutines %d\n", runtime.NumGoroutine())
	fmt.Fprintf(w, "vaults3_go_memory_alloc_bytes %d\n", mem.Alloc)
	fmt.Fprintf(w, "vaults3_go_memory_sys_bytes %d\n", mem.Sys)
	fmt.Fprintf(w, "vaults3_go_gc_total %d\n", mem.NumGC)
}
