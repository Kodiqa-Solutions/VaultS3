package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

// IsNoSpace reports an error from a full disk or an exhausted quota of the
// file system.
func IsNoSpace(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, syscall.ENOSPC) || isQuotaExceeded(err) {
		return true
	}
	// On a cluster the error comes back from another node as text.
	msg := err.Error()
	return strings.Contains(msg, "no space left on device") || strings.Contains(msg, "disk quota exceeded") ||
		strings.Contains(msg, "below the reserve (storage.min_free_mb)")
}

// SpaceGuard watches the free space of the server's own folders. Below the
// reserve it reports the server low on space, and the server refuses new
// writes while deletes go on.
//
// A full disk used to be found by the write that failed on it. On the
// metadata disk that was a trap: the metadata database writes new pages for
// every change, a delete included, so once it filled up nothing could be
// deleted to make room, and the server stayed full until the disk grew.
// Refusing writes before the disk is full keeps room for the deletes that
// free it.
type SpaceGuard struct {
	dirs    []string
	minFree uint64
	low     atomic.Bool
	free    atomic.Uint64 // lowest free bytes of the watched folders at the last check
	checked atomic.Int64  // unix nanoseconds of the last check
}

// recheckAfter is how old a measurement Low trusts. A fast writer fills a
// small reserve within one interval of the background check, so Low measures
// again itself (one statfs per folder, at most once in this period).
const recheckAfter = time.Second

// NewSpaceGuard watches dirs and keeps minFree bytes free in each. A zero
// minFree disables it.
func NewSpaceGuard(minFree uint64, dirs ...string) *SpaceGuard {
	var watched []string
	for _, d := range dirs {
		if d != "" {
			watched = append(watched, d)
		}
	}
	return &SpaceGuard{dirs: watched, minFree: minFree}
}

// Low reports whether a watched folder had less than the reserve free at
// the last check. A nil guard is never low.
func (g *SpaceGuard) Low() bool {
	if g == nil || g.minFree == 0 {
		return false
	}
	now := time.Now().UnixNano()
	last := g.checked.Load()
	if now-last > int64(recheckAfter) && g.checked.CompareAndSwap(last, now) {
		g.Check()
	}
	return g.low.Load()
}

// Check measures now.
func (g *SpaceGuard) Check() {
	if g == nil || g.minFree == 0 {
		return
	}
	g.checked.Store(time.Now().UnixNano())
	lowest, known := uint64(0), false
	for _, d := range g.dirs {
		free, ok := freeBytes(d)
		if !ok {
			continue
		}
		if !known || free < lowest {
			lowest, known = free, true
		}
	}
	if !known {
		return
	}
	g.free.Store(lowest)
	wasLow := g.low.Swap(lowest < g.minFree)
	switch isLow := lowest < g.minFree; {
	case isLow && !wasLow:
		slog.Error("free disk space is below the reserve; new writes are refused until objects are deleted or the disk grows",
			"free_mb", lowest>>20, "reserve_mb", g.minFree>>20)
	case !isLow && wasLow:
		slog.Info("free disk space is above the reserve again; writes are accepted", "free_mb", lowest>>20)
	}
}

// Run checks every interval until ctx ends.
func (g *SpaceGuard) Run(ctx context.Context, interval time.Duration) {
	if g == nil || g.minFree == 0 {
		return
	}
	g.Check()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			g.Check()
		}
	}
}

// errLowSpace is a write refused for the reserve. It wraps ENOSPC, so every
// caller answers it as a full disk (IsNoSpace, 507).
var errLowSpace = fmt.Errorf("free disk space is below the reserve (storage.min_free_mb): %w", syscall.ENOSPC)

// GuardedEngine refuses new object data while the guard is low. The S3 API
// refuses such requests before reading them; this covers every other writer
// (dashboard uploads and version rollbacks, batch jobs, migrations, replica
// copies from other nodes), which went on filling the reserve kept for
// deletes.
type GuardedEngine struct {
	Engine
	guard *SpaceGuard
}

// NewGuardedEngine wraps e.
func NewGuardedEngine(e Engine, g *SpaceGuard) *GuardedEngine {
	return &GuardedEngine{Engine: e, guard: g}
}

// Unwrap returns the wrapped engine (see EngineAs).
func (g *GuardedEngine) Unwrap() Engine { return g.Engine }

func (g *GuardedEngine) PutObject(bucket, key string, r io.Reader, size int64) (int64, string, error) {
	if g.guard.Low() {
		return 0, "", errLowSpace
	}
	return g.Engine.PutObject(bucket, key, r, size)
}

func (g *GuardedEngine) PutObjectVersion(bucket, key, versionID string, r io.Reader, size int64) (int64, string, error) {
	if g.guard.Low() {
		return 0, "", errLowSpace
	}
	return g.Engine.PutObjectVersion(bucket, key, versionID, r, size)
}

// InvalidateBucketSize passes the tiering manager's call through.
func (g *GuardedEngine) InvalidateBucketSize(bucket string) {
	if i, ok := g.Engine.(interface{ InvalidateBucketSize(string) }); ok {
		i.InvalidateBucketSize(bucket)
	}
}
