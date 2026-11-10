package metadata

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	bolt "go.etcd.io/bbolt"
	bolterrors "go.etcd.io/bbolt/errors"
)

// CopyIndex records, on one cluster node, which object each local data file
// holds: the ETag it was written as. It is this node's own knowledge and is
// never replicated (a Raft snapshot restore must not replace it), so it lives
// in a file of its own.
//
// Without it a node judged its copy by size alone (and, in the first minute
// after a write, by hashing it), so a node that was down while an object was
// overwritten with one of the same size served the previous bytes for good,
// and replica repair, which only looked for missing copies, never replaced
// them. A file with no entry (written before this existed) is judged as
// before.
type CopyIndex struct {
	db *bolt.DB
	// off is set when the database fails in use (it is written without
	// fsync, and a crash can leave it damaged in a way that opens but panics
	// later). The index then does nothing and every file is judged as
	// before, which is the safe direction.
	off atomic.Bool
}

// guard turns a panic in the database into switching the index off.
func (c *CopyIndex) guard() {
	if r := recover(); r != nil {
		c.switchOff(fmt.Errorf("panic: %v", r))
	}
}

// switchOff stops using the index and deletes its file, so the next start
// begins empty instead of trusting records that stopped being kept.
func (c *CopyIndex) switchOff(err error) {
	if !c.off.Swap(true) {
		_ = os.Remove(c.db.Path())
		slog.Error("the local copy index failed and is switched off until the next start, which begins a new one", "error", err)
	}
}

var copiesBucket = []byte("copies")

// OpenCopyIndex opens or creates the index at path. The index only ever
// narrows what a node trusts, so one that cannot be opened is recreated
// empty.
func OpenCopyIndex(path string) (*CopyIndex, error) {
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: 5 * time.Second})
	if err != nil {
		if errors.Is(err, bolterrors.ErrTimeout) {
			return nil, err
		}
		slog.Warn("the local copy index could not be opened, starting a new one", "path", path, "error", err)
		_ = os.Remove(path)
		if db, err = bolt.Open(path, 0o600, &bolt.Options{Timeout: 5 * time.Second}); err != nil {
			return nil, err
		}
	}
	// Not synced: see Record. Neither is db.Batch used, which waits up to
	// 10 ms to group writes, twice per client PUT.
	db.NoSync = true
	if err := db.Update(func(tx *bolt.Tx) error {
		_, err := tx.CreateBucketIfNotExists(copiesBucket)
		return err
	}); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &CopyIndex{db: db}, nil
}

func copyIndexKey(bucket, key, version string) []byte {
	return []byte(bucket + "\x00" + key + "\x00" + version)
}

// Record notes that the local file of bucket/key (version "" for the plain
// data) now holds the object with this ETag, and is size bytes as the engine
// reads it back. It is called after the file is written.
//
// The size makes a record that outlived its file harmless. Records are not
// synced (one fsync per client write halved a node's PUT rate), so after an
// OS crash a record can name the object a file held before an overwrite
// whose drop was lost; Lookup trusts a record only while the file still has
// the recorded size. What remains, the same object size and a crash of the
// machine within moments of the overwrite, makes the node distrust a good
// copy (it reads from another holder), never trust a wrong one.
func (c *CopyIndex) Record(bucket, key, version, etag string, size int64) {
	if c == nil || c.off.Load() {
		return
	}
	defer c.guard()
	if etag == "" || size < 0 {
		c.Forget(bucket, key, version)
		return
	}
	val := []byte(strconv.FormatInt(size, 10) + " " + etag)
	if err := c.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(copiesBucket).Put(copyIndexKey(bucket, key, version), val)
	}); err != nil {
		c.switchOff(err)
	}
}

// Forget drops the entry, so the copy is judged as one written before the
// index existed.
func (c *CopyIndex) Forget(bucket, key, version string) {
	if c == nil || c.off.Load() {
		return
	}
	defer c.guard()
	if err := c.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(copiesBucket).Delete(copyIndexKey(bucket, key, version))
	}); err != nil {
		// A record that cannot be dropped would outlive the file it names.
		c.switchOff(err)
	}
}

// Entry returns the record as stored: the ETag and the size the file had.
func (c *CopyIndex) Entry(bucket, key, version string) (etag string, size int64, known bool) {
	if c == nil || c.off.Load() {
		return "", 0, false
	}
	defer func() {
		if r := recover(); r != nil {
			c.switchOff(fmt.Errorf("panic: %v", r))
			etag, size, known = "", 0, false
		}
	}()
	var raw string
	_ = c.db.View(func(tx *bolt.Tx) error {
		if v := tx.Bucket(copiesBucket).Get(copyIndexKey(bucket, key, version)); v != nil {
			raw = string(v)
		}
		return nil
	})
	sizeText, e, ok := strings.Cut(raw, " ")
	if !ok || e == "" {
		return "", 0, false // none, or written by an earlier build
	}
	n, err := strconv.ParseInt(sizeText, 10, 64)
	if err != nil {
		return "", 0, false
	}
	return e, n, true
}

// Lookup returns the ETag the local copy was written as, if known and the
// file, read back by the engine as size bytes, still has the recorded size.
func (c *CopyIndex) Lookup(bucket, key, version string, size int64) (string, bool) {
	etag, recorded, ok := c.Entry(bucket, key, version)
	if !ok || recorded != size {
		return "", false
	}
	return etag, true
}

// Working reports whether the index is in use: not nil and not switched off.
// A caller that requires a record falls back to the behaviour without one
// when it is not.
func (c *CopyIndex) Working() bool { return c != nil && !c.off.Load() }

// Close closes the index.
func (c *CopyIndex) Close() error {
	if c == nil {
		return nil
	}
	return c.db.Close()
}
