package storage

import (
	"bytes"
	"crypto/md5"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/klauspost/compress/zstd"
	bolt "go.etcd.io/bbolt"
)

// zstdDecoder is reused across reads — DecodeAll is safe for concurrent use.
var zstdDecoder, _ = zstd.NewReader(nil)

// locBucket holds blob locations keyed by "bucket\x00key".
var locBucket = []byte("loc")

// PackedEngine packs small objects into a few large append-only "volume" files
// (each object stored as an independent zstd frame) and records byte-offset
// locations in a BoltDB index — avoiding the per-file overhead of millions of
// tiny objects. Objects larger than MaxObjectSize are delegated to the inner
// engine as individual files. Phase 1: no compaction yet, and versioned objects
// are delegated to the inner engine.
type PackedEngine struct {
	inner      Engine
	dir        string // <dataDir>/_volumes
	index      *bolt.DB
	maxObjSize int64
	volMaxSize int64

	mu         sync.Mutex // guards the active volume state below
	activeID   uint32
	activeFile *os.File
	activeSize int64

	compactMu   sync.RWMutex // read-held during a packed read; write-held while deleting a compacted volume
	compactLock sync.Mutex   // ensures only one compaction runs at a time
}

// blobLoc is where a packed object lives within a volume.
type blobLoc struct {
	Vol   uint32 `json:"v"`
	Off   int64  `json:"o"`
	CLen  int64  `json:"c"` // compressed frame length
	Size  int64  `json:"s"` // original size
	ETag  string `json:"e"`
	MTime int64  `json:"t"`
}

// NewPackedEngine wraps inner, packing objects up to maxObjSize and rolling to a
// new volume once one reaches volMaxSize.
func NewPackedEngine(inner Engine, maxObjSize, volMaxSize int64) (*PackedEngine, error) {
	if maxObjSize <= 0 {
		maxObjSize = 1 << 20 // 1 MiB
	}
	if volMaxSize <= 0 {
		volMaxSize = 1 << 30 // 1 GiB
	}
	dir := filepath.Join(inner.DataDir(), "_volumes")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create volumes dir: %w", err)
	}
	index, err := bolt.Open(filepath.Join(dir, "index.db"), 0o644, &bolt.Options{Timeout: 5 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("open volume index: %w", err)
	}
	if err := index.Update(func(tx *bolt.Tx) error {
		_, e := tx.CreateBucketIfNotExists(locBucket)
		return e
	}); err != nil {
		index.Close()
		return nil, err
	}

	p := &PackedEngine{inner: inner, dir: dir, index: index, maxObjSize: maxObjSize, volMaxSize: volMaxSize}
	if err := p.openActiveVolume(); err != nil {
		index.Close()
		return nil, err
	}
	return p, nil
}

// openActiveVolume finds the highest-numbered existing volume (or starts at 1)
// and opens it for append.
func (p *PackedEngine) openActiveVolume() error {
	entries, _ := filepath.Glob(filepath.Join(p.dir, "vol-*.dat"))
	var maxID uint32 = 0
	for _, e := range entries {
		var id uint32
		if _, err := fmt.Sscanf(filepath.Base(e), "vol-%d.dat", &id); err == nil && id > maxID {
			maxID = id
		}
	}
	if maxID == 0 {
		maxID = 1
	}
	return p.openVolume(maxID)
}

func (p *PackedEngine) volPath(id uint32) string {
	return filepath.Join(p.dir, fmt.Sprintf("vol-%010d.dat", id))
}

func (p *PackedEngine) openVolume(id uint32) error {
	f, err := os.OpenFile(p.volPath(id), os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return fmt.Errorf("open volume %d: %w", id, err)
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	p.activeID = id
	p.activeFile = f
	p.activeSize = info.Size()
	return nil
}

// Close releases the index and active volume (for shutdown/tests).
func (p *PackedEngine) Close() error {
	p.mu.Lock()
	if p.activeFile != nil {
		p.activeFile.Sync()
		p.activeFile.Close()
	}
	p.mu.Unlock()
	return p.index.Close()
}

// --- packing helpers ---

func (p *PackedEngine) locKey(bucket, key string) []byte {
	return []byte(bucket + "\x00" + key)
}

func (p *PackedEngine) getLoc(bucket, key string) (blobLoc, bool) {
	var loc blobLoc
	found := false
	p.index.View(func(tx *bolt.Tx) error {
		v := tx.Bucket(locBucket).Get(p.locKey(bucket, key))
		if v != nil {
			found = json.Unmarshal(v, &loc) == nil
		}
		return nil
	})
	return loc, found
}

func (p *PackedEngine) putLoc(bucket, key string, loc blobLoc) error {
	data, _ := json.Marshal(loc)
	return p.index.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(locBucket).Put(p.locKey(bucket, key), data)
	})
}

func (p *PackedEngine) delLoc(bucket, key string) error {
	return p.index.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(locBucket).Delete(p.locKey(bucket, key))
	})
}

// appendFrame writes a zstd frame to the active volume (fsync'd) and returns its
// location. Crash-safety: data is durable before the caller commits the index.
func (p *PackedEngine) appendFrame(frame []byte) (uint32, int64, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	volID := p.activeID
	off := p.activeSize
	if _, err := p.activeFile.WriteAt(frame, off); err != nil {
		return 0, 0, fmt.Errorf("write frame: %w", err)
	}
	if err := p.activeFile.Sync(); err != nil {
		return 0, 0, fmt.Errorf("sync volume: %w", err)
	}
	p.activeSize += int64(len(frame))
	if p.activeSize >= p.volMaxSize {
		p.activeFile.Close()
		if err := p.openVolume(p.activeID + 1); err != nil {
			return 0, 0, err
		}
	}
	return volID, off, nil
}

// readFrame reads clen bytes at off from volume volID.
func (p *PackedEngine) readFrame(volID uint32, off, clen int64) ([]byte, error) {
	buf := make([]byte, clen)
	// Hold the lock across the active-check and the read, so a concurrent volume
	// roll (which closes activeFile under the same lock) can't close the handle
	// between checking and reading it. If the volume is not (or no longer) the
	// active one it has been sealed and is readable by path below.
	p.mu.Lock()
	if volID == p.activeID {
		_, err := p.activeFile.ReadAt(buf, off)
		p.mu.Unlock()
		return buf, err
	}
	p.mu.Unlock()
	f, err := os.Open(p.volPath(volID))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if _, err := f.ReadAt(buf, off); err != nil {
		return nil, err
	}
	return buf, nil
}

// Compact reclaims dead space left by deleted/overwritten objects. For each
// sealed (non-active) volume whose dead-space fraction is at least minDeadRatio,
// it rewrites the still-live frames to the active volume, repoints the index, and
// removes the old volume file. Returns the net bytes reclaimed. Only one
// compaction runs at a time; concurrent reads and writes stay safe.
func (p *PackedEngine) Compact(minDeadRatio float64) (int64, error) {
	if !p.compactLock.TryLock() {
		return 0, nil // a compaction is already running
	}
	defer p.compactLock.Unlock()

	p.mu.Lock()
	activeID := p.activeID
	p.mu.Unlock()

	// Live frames + live bytes per sealed volume, from the index.
	type ref struct {
		ikey []byte
		loc  blobLoc
	}
	live := map[uint32][]ref{}
	liveBytes := map[uint32]int64{}
	p.index.View(func(tx *bolt.Tx) error {
		c := tx.Bucket(locBucket).Cursor()
		for k, v := c.First(); k != nil; k, v = c.Next() {
			var loc blobLoc
			if json.Unmarshal(v, &loc) != nil || loc.Vol >= activeID {
				continue
			}
			live[loc.Vol] = append(live[loc.Vol], ref{ikey: append([]byte(nil), k...), loc: loc})
			liveBytes[loc.Vol] += loc.CLen
		}
		return nil
	})

	var reclaimed int64
	vols, _ := filepath.Glob(filepath.Join(p.dir, "vol-*.dat"))
	for _, vp := range vols {
		var id uint32
		if _, err := fmt.Sscanf(filepath.Base(vp), "vol-%d.dat", &id); err != nil || id >= activeID {
			continue
		}
		info, err := os.Stat(vp)
		if err != nil || info.Size() == 0 {
			continue
		}
		total := info.Size()
		if float64(total-liveBytes[id])/float64(total) < minDeadRatio {
			continue
		}

		// Move each live frame to the active volume, repointing the index with a
		// compare-and-swap so a concurrent overwrite/delete always wins.
		for _, r := range live[id] {
			frame, err := p.readFrame(id, r.loc.Off, r.loc.CLen)
			if err != nil {
				return reclaimed, fmt.Errorf("compact read: %w", err)
			}
			newVol, newOff, err := p.appendFrame(frame)
			if err != nil {
				return reclaimed, fmt.Errorf("compact append: %w", err)
			}
			newLoc := r.loc
			newLoc.Vol, newLoc.Off = newVol, newOff
			if err := p.index.Update(func(tx *bolt.Tx) error {
				b := tx.Bucket(locBucket)
				cur := b.Get(r.ikey)
				var curLoc blobLoc
				if cur == nil || json.Unmarshal(cur, &curLoc) != nil || curLoc.Vol != id || curLoc.Off != r.loc.Off {
					return nil // key was overwritten/deleted concurrently — leave it
				}
				data, _ := json.Marshal(newLoc)
				return b.Put(r.ikey, data)
			}); err != nil {
				return reclaimed, err
			}
		}

		// No index entry references this volume anymore; delete it under the write
		// lock so no read is mid-flight with a location pointing into it.
		p.compactMu.Lock()
		err = os.Remove(vp)
		p.compactMu.Unlock()
		if err != nil && !os.IsNotExist(err) {
			// The volume is still on disk, so nothing was reclaimed. It holds no
			// live frames any more, so the next compaction retries it.
			return reclaimed, fmt.Errorf("compact remove volume %d: %w", id, err)
		}
		reclaimed += total - liveBytes[id]
	}
	return reclaimed, nil
}

// --- Engine interface ---

func (p *PackedEngine) CreateBucketDir(bucket string) error { return p.inner.CreateBucketDir(bucket) }

func (p *PackedEngine) DeleteBucketDir(bucket string) error {
	// Drop packed index entries for the bucket (frames become dead space).
	// The keys are collected first and deleted after the walk, because a delete
	// under a bbolt cursor can make the following Next() skip a key, and every
	// skipped entry would reappear as an object in a recreated bucket.
	prefix := []byte(bucket + "\x00")
	if err := p.index.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(locBucket)
		var keys [][]byte
		c := b.Cursor()
		for k, _ := c.Seek(prefix); k != nil && bytes.HasPrefix(k, prefix); k, _ = c.Next() {
			keys = append(keys, append([]byte(nil), k...))
		}
		for _, k := range keys {
			if err := b.Delete(k); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		// Removing the directory anyway would leave index entries that a
		// recreated bucket of the same name would serve as its own objects.
		return fmt.Errorf("drop packed index for bucket %s: %w", bucket, err)
	}
	return p.inner.DeleteBucketDir(bucket)
}

func (p *PackedEngine) PutObject(bucket, key string, reader io.Reader, size int64) (int64, string, error) {
	// Large or unknown-size objects go to the inner engine as individual files.
	if size < 0 || size > p.maxObjSize {
		return p.putLarge(bucket, key, reader, size)
	}

	data, err := io.ReadAll(io.LimitReader(reader, p.maxObjSize+1))
	if err != nil {
		return 0, "", fmt.Errorf("read object: %w", err)
	}
	if int64(len(data)) > p.maxObjSize {
		// Size hint lied — actually larger than the pack threshold; delegate.
		return p.putLarge(bucket, key, io.MultiReader(bytes.NewReader(data), reader), -1)
	}

	h := md5.Sum(data)
	etag := fmt.Sprintf("\"%x\"", h)
	frame := zstdEncoder.EncodeAll(data, nil)

	volID, off, err := p.appendFrame(frame)
	if err != nil {
		return 0, "", err
	}
	loc := blobLoc{Vol: volID, Off: off, CLen: int64(len(frame)), Size: int64(len(data)), ETag: etag, MTime: time.Now().Unix()}
	if err := p.putLoc(bucket, key, loc); err != nil {
		return 0, "", err
	}
	// Remove any stale individual file from a previous (large) write of this key.
	// Reads already resolve to the packed copy, so a failure here only leaves a
	// stray file behind. It is logged rather than failing a write that landed.
	if err := p.inner.DeleteObject(bucket, key); err != nil && !os.IsNotExist(err) {
		slog.Warn("packed: could not remove superseded large object file", "bucket", bucket, "key", key, "error", err)
	}
	return int64(len(data)), etag, nil
}

// putLarge stores an object too big to pack as an individual file. The packed
// index entry of a previous small version is dropped only after that write
// succeeds: dropping it first meant a failed upload destroyed the old object
// while its metadata still described it.
func (p *PackedEngine) putLarge(bucket, key string, reader io.Reader, size int64) (int64, string, error) {
	n, etag, err := p.inner.PutObject(bucket, key, reader, size)
	if err != nil {
		return n, etag, err
	}
	if _, ok := p.getLoc(bucket, key); ok {
		// A packed entry shadows the individual file on every read, so leaving
		// it in place would keep serving the old content. Report the write as
		// failed so the caller does not record metadata for bytes nobody sees.
		if err := p.delLoc(bucket, key); err != nil {
			return 0, "", fmt.Errorf("drop superseded packed entry: %w", err)
		}
	}
	return n, etag, nil
}

func (p *PackedEngine) GetObject(bucket, key string) (ReadSeekCloser, int64, error) {
	// Hold the read lock from index lookup through the frame read so a compaction
	// can't delete the volume out from under us between the two.
	p.compactMu.RLock()
	loc, ok := p.getLoc(bucket, key)
	if !ok {
		p.compactMu.RUnlock()
		return p.inner.GetObject(bucket, key)
	}
	frame, err := p.readFrame(loc.Vol, loc.Off, loc.CLen)
	p.compactMu.RUnlock()
	if err != nil {
		return nil, 0, fmt.Errorf("read frame: %w", err)
	}
	plain, err := zstdDecoder.DecodeAll(frame, nil)
	if err != nil {
		return nil, 0, fmt.Errorf("decode frame: %w", err)
	}
	if int64(len(plain)) != loc.Size {
		return nil, 0, fmt.Errorf("packed object size mismatch: got %d want %d", len(plain), loc.Size)
	}
	return &bytesReadSeekCloser{Reader: bytes.NewReader(plain)}, loc.Size, nil
}

func (p *PackedEngine) DeleteObject(bucket, key string) error {
	if _, ok := p.getLoc(bucket, key); ok {
		return p.delLoc(bucket, key) // frame becomes dead space (reclaimed by compaction later)
	}
	return p.inner.DeleteObject(bucket, key)
}

func (p *PackedEngine) ObjectExists(bucket, key string) bool {
	if _, ok := p.getLoc(bucket, key); ok {
		return true
	}
	return p.inner.ObjectExists(bucket, key)
}

func (p *PackedEngine) ObjectSize(bucket, key string) (int64, error) {
	if loc, ok := p.getLoc(bucket, key); ok {
		return loc.Size, nil
	}
	return p.inner.ObjectSize(bucket, key)
}

func (p *PackedEngine) ListObjects(bucket, prefix, startAfter string, maxKeys int) ([]ObjectInfo, bool, error) {
	// Packed objects from the index.
	var objs []ObjectInfo
	pfx := []byte(bucket + "\x00" + prefix)
	strip := len(bucket) + 1
	p.index.View(func(tx *bolt.Tx) error {
		c := tx.Bucket(locBucket).Cursor()
		for k, v := c.Seek(pfx); k != nil && bytes.HasPrefix(k, pfx); k, v = c.Next() {
			key := string(k[strip:])
			if startAfter != "" && key <= startAfter {
				continue
			}
			var loc blobLoc
			if json.Unmarshal(v, &loc) == nil {
				objs = append(objs, ObjectInfo{Key: key, Size: loc.Size, LastModified: loc.MTime, ETag: loc.ETag})
			}
		}
		return nil
	})
	// Large objects from the inner engine.
	innerObjs, _, err := p.inner.ListObjects(bucket, prefix, startAfter, 0)
	if err != nil {
		return nil, false, err
	}
	objs = append(objs, innerObjs...)

	sort.Slice(objs, func(i, j int) bool { return objs[i].Key < objs[j].Key })
	truncated := false
	if maxKeys > 0 && len(objs) > maxKeys {
		objs = objs[:maxKeys]
		truncated = true
	}
	return objs, truncated, nil
}

func (p *PackedEngine) BucketSize(bucket string) (int64, int64, error) {
	var total, count int64
	prefix := []byte(bucket + "\x00")
	p.index.View(func(tx *bolt.Tx) error {
		c := tx.Bucket(locBucket).Cursor()
		for k, v := c.Seek(prefix); k != nil && bytes.HasPrefix(k, prefix); k, v = c.Next() {
			var loc blobLoc
			if json.Unmarshal(v, &loc) == nil {
				total += loc.Size
				count++
			}
		}
		return nil
	})
	innerTotal, innerCount, err := p.inner.BucketSize(bucket)
	if err != nil {
		return 0, 0, err
	}
	return total + innerTotal, count + innerCount, nil
}

// Versions are delegated to the inner engine in Phase 1 (not packed).
func (p *PackedEngine) PutObjectVersion(bucket, key, versionID string, reader io.Reader, size int64) (int64, string, error) {
	return p.inner.PutObjectVersion(bucket, key, versionID, reader, size)
}

func (p *PackedEngine) GetObjectVersion(bucket, key, versionID string) (ReadSeekCloser, int64, error) {
	return p.inner.GetObjectVersion(bucket, key, versionID)
}

func (p *PackedEngine) DeleteObjectVersion(bucket, key, versionID string) error {
	return p.inner.DeleteObjectVersion(bucket, key, versionID)
}

func (p *PackedEngine) DataDir() string                      { return p.inner.DataDir() }
func (p *PackedEngine) ObjectPath(bucket, key string) string { return p.inner.ObjectPath(bucket, key) }
