package ratelimit

import (
	"sync"
	"sync/atomic"
	"time"
)

type bucket struct {
	tokens   float64
	lastTime time.Time
	rps      float64
	burst    int
}

func (b *bucket) allow(now time.Time) bool {
	elapsed := now.Sub(b.lastTime).Seconds()
	b.tokens += elapsed * b.rps
	if b.tokens > float64(b.burst) {
		b.tokens = float64(b.burst)
	}
	b.lastTime = now

	if b.tokens >= 1 {
		b.tokens--
		return true
	}
	return false
}

type Limiter struct {
	mu sync.Mutex

	ipBuckets  map[string]*bucket
	keyBuckets map[string]*bucket

	ipRPS    float64
	ipBurst  int
	keyRPS   float64
	keyBurst int

	rejected atomic.Int64
	stopCh   chan struct{}
}

func NewLimiter(ipRPS float64, ipBurst int, keyRPS float64, keyBurst int) *Limiter {
	l := &Limiter{
		ipBuckets:  make(map[string]*bucket),
		keyBuckets: make(map[string]*bucket),
		ipRPS:      ipRPS,
		ipBurst:    ipBurst,
		keyRPS:     keyRPS,
		keyBurst:   keyBurst,
		stopCh:     make(chan struct{}),
	}
	go l.cleanup()
	return l
}

func (l *Limiter) Allow(clientIP, accessKey string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := time.Now()

	// Check IP bucket
	ib, ok := l.ipBuckets[clientIP]
	if !ok {
		ib = &bucket{tokens: float64(l.ipBurst), lastTime: now, rps: l.ipRPS, burst: l.ipBurst}
		l.ipBuckets[clientIP] = ib
	}
	if !ib.allow(now) {
		l.rejected.Add(1)
		return false
	}

	// Check per-key bucket if access key is present
	if accessKey != "" {
		kb, ok := l.keyBuckets[accessKey]
		if !ok {
			kb = &bucket{tokens: float64(l.keyBurst), lastTime: now, rps: l.keyRPS, burst: l.keyBurst}
			l.keyBuckets[accessKey] = kb
		}
		if !kb.allow(now) {
			l.rejected.Add(1)
			return false
		}
	}

	return true
}

// AllowKey charges only the per-access-key bucket. The S3 handler calls it
// after authentication: charging a key before its signature was checked let
// anyone who knew an access key ID, which is not a secret, drain that key's
// allowance with unsigned requests, and grow the key map with random IDs.
func (l *Limiter) AllowKey(accessKey string) bool {
	if accessKey == "" {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	kb, ok := l.keyBuckets[accessKey]
	if !ok {
		kb = &bucket{tokens: float64(l.keyBurst), lastTime: now, rps: l.keyRPS, burst: l.keyBurst}
		l.keyBuckets[accessKey] = kb
	}
	if !kb.allow(now) {
		l.rejected.Add(1)
		return false
	}
	return true
}

func (l *Limiter) Status() map[string]interface{} {
	l.mu.Lock()
	ipCount := len(l.ipBuckets)
	keyCount := len(l.keyBuckets)
	l.mu.Unlock()

	return map[string]interface{}{
		"active_ip_limiters":  ipCount,
		"active_key_limiters": keyCount,
		"total_rejected":      l.rejected.Load(),
		"ip_rps":              l.ipRPS,
		"ip_burst":            l.ipBurst,
		"per_key_rps":         l.keyRPS,
		"per_key_burst":       l.keyBurst,
	}
}

func (l *Limiter) Stop() {
	close(l.stopCh)
}

func (l *Limiter) cleanup() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-l.stopCh:
			return
		case <-ticker.C:
			l.mu.Lock()
			now := time.Now()
			for ip, b := range l.ipBuckets {
				if now.Sub(b.lastTime) > 5*time.Minute {
					delete(l.ipBuckets, ip)
				}
			}
			for key, b := range l.keyBuckets {
				if now.Sub(b.lastTime) > 5*time.Minute {
					delete(l.keyBuckets, key)
				}
			}
			l.mu.Unlock()
		}
	}
}
