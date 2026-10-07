package metadata

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"log/slog"
	"sync"

	bolt "go.etcd.io/bbolt"
)

// snapshotMagic opens every snapshot this version writes. The first format had
// no header and began with the length of the first bucket name, a small number,
// so a stream that starts with these bytes cannot be one of those: the reader
// tells the two apart by peeking, and a node can still install a snapshot an
// older version wrote.
//
// The last byte is the format version.
var snapshotMagic = []byte("VS3RAFT\x02")

// Format 2 is:
//
//	magic
//	for each bucket: name length (uint32, never 0), name, bolt sequence (uint64),
//	    then for each pair: key length (uint32, never 0), key, value length, value,
//	    then a zero key length closing the bucket
//	a zero name length closing the snapshot
//
// The first format wrote a key count ahead of each bucket. That needed a full
// extra pass over the bucket before the first byte of it could be written, which
// doubled the time a snapshot holds its read transaction open (see
// RaftSnapshot). The end markers make the count unnecessary, and the closing
// marker means a truncated stream is reported instead of being taken for a
// complete one. bbolt rejects empty keys and empty bucket names, so a zero
// length can never be real data.
//
// The sequence is carried so a restored bucket keeps handing out IDs above the
// ones it already holds.

// RaftSnapshot is a point-in-time view of the Raft-owned part of the store.
//
// The read transaction is opened when the snapshot is taken, not when it is
// written out. Raft records the snapshot's index at the moment it asks for the
// snapshot and keeps applying entries while the bytes are written, so a dump
// read later contained entries after that index, which were then applied a
// second time on top of it after a restore. Most commands survive that, but not
// all of them (an enqueue, for one, used to create a duplicate event).
//
// Holding the transaction has a cost: bbolt cannot grow its memory map while a
// read transaction is open, so a write that needs the file to grow waits for
// the snapshot to finish. Writing the snapshot in one pass keeps that window as
// short as it can be.
type RaftSnapshot struct {
	tx   *bolt.Tx
	once sync.Once
}

// BeginRaftSnapshot opens the read transaction a snapshot will be written from.
// The caller must call Release.
func (s *Store) BeginRaftSnapshot() (*RaftSnapshot, error) {
	tx, err := s.db.Begin(false)
	if err != nil {
		return nil, fmt.Errorf("begin snapshot transaction: %w", err)
	}
	return &RaftSnapshot{tx: tx}, nil
}

// Release ends the snapshot's read transaction. Safe to call more than once.
func (sn *RaftSnapshot) Release() {
	sn.once.Do(func() { sn.tx.Rollback() })
}

// Encode writes the snapshot in the current format.
func (sn *RaftSnapshot) Encode(w io.Writer) error {
	bw := bufio.NewWriterSize(w, 256<<10)
	if _, err := bw.Write(snapshotMagic); err != nil {
		return fmt.Errorf("write snapshot header: %w", err)
	}
	write := func(name []byte, keep func(key []byte) bool) error {
		b := sn.tx.Bucket(name)
		if b == nil {
			return nil
		}
		if err := writeBytes(bw, name); err != nil {
			return fmt.Errorf("write bucket name %s: %w", name, err)
		}
		if err := binary.Write(bw, binary.BigEndian, b.Sequence()); err != nil {
			return fmt.Errorf("write bucket sequence %s: %w", name, err)
		}
		c := b.Cursor()
		for k, v := c.First(); k != nil; k, v = c.Next() {
			if v == nil {
				continue // a nested bucket, which this store never creates
			}
			if keep != nil && !keep(k) {
				continue
			}
			if err := writeBytes(bw, k); err != nil {
				return err
			}
			if err := writeBytes(bw, v); err != nil {
				return err
			}
		}
		return binary.Write(bw, binary.BigEndian, uint32(0))
	}
	for _, name := range raftOwnedBuckets {
		if err := write(name, nil); err != nil {
			return err
		}
	}
	for name := range mixedBuckets {
		bucket := []byte(name)
		if err := write(bucket, func(k []byte) bool { return raftOwnsKey(bucket, k) }); err != nil {
			return err
		}
	}
	if err := binary.Write(bw, binary.BigEndian, uint32(0)); err != nil {
		return err
	}
	return bw.Flush()
}

// WriteSnapshot writes the Raft-owned part of the store to w, as of the moment
// it is called.
func (s *Store) WriteSnapshot(w io.Writer) error {
	sn, err := s.BeginRaftSnapshot()
	if err != nil {
		return err
	}
	defer sn.Release()
	return sn.Encode(w)
}

// restoreStateBucket records that a snapshot restore is under way. It is written
// before the old state is dropped and removed only once the restore finishes, so
// a DB left behind by an interrupted restore is recognisable on the next open
// instead of quietly serving half a cluster's metadata.
var restoreStateBucket = []byte("_restore_state")

var restoreInProgressKey = []byte("in_progress")

// Restoring commits in bounded batches rather than one transaction. BoltDB holds
// every dirty page of a write transaction in memory until it commits, so a
// single-transaction restore cost roughly 1.4 KB of heap per object and scaled
// with the whole cluster's metadata: a node rejoining with a few hundred thousand
// objects allocated more than a gigabyte before committing anything, and was
// OOM-killed during startup before it ever served a request (issue #46 follow-up).
// Batching makes that cost flat.
// Vars rather than consts so tests can shrink them and exercise the batching
// without seeding a realistic amount of data.
var (
	restoreBatchBytes = 32 << 20
	restoreBatchKeys  = 20000
)

// RestoreSnapshot replaces the Raft-owned part of the store with a snapshot.
// Node-local buckets are left exactly as they are, including when the snapshot
// came from an older version that wrote them into it: those are read past and
// dropped.
//
// The restore is NOT atomic: it commits as it goes, so an interrupted restore
// leaves a partial DB. That is deliberate, and safe, because the Raft-owned state
// is entirely derived from Raft: the sentinel below marks the DB as incomplete
// and the next open clears it, after which Raft installs its snapshot again. The
// alternative (one transaction) is atomic but allocates without bound, which is
// the failure this replaced.
func (s *Store) RestoreSnapshot(r io.Reader) error {
	br := bufio.NewReaderSize(r, 256<<10)
	current := false
	if head, err := br.Peek(len(snapshotMagic)); err == nil && bytes.Equal(head, snapshotMagic) {
		current = true
		if _, err := br.Discard(len(snapshotMagic)); err != nil {
			return fmt.Errorf("read snapshot header: %w", err)
		}
	}

	if err := s.beginRestore(); err != nil {
		return err
	}

	for {
		name, err := readBytes(br)
		if err == io.EOF && !current {
			// The first format simply ended after its last bucket.
			return s.finishRestore()
		}
		if err != nil {
			if err == io.EOF {
				err = io.ErrUnexpectedEOF
			}
			return fmt.Errorf("read bucket name: %w", err)
		}
		if current && len(name) == 0 {
			return s.finishRestore()
		}

		var next func() (key, val []byte, ok bool, err error)
		var sequence uint64
		if current {
			if err := binary.Read(br, binary.BigEndian, &sequence); err != nil {
				return fmt.Errorf("read bucket sequence: %w", err)
			}
			next = func() ([]byte, []byte, bool, error) {
				key, err := readBytes(br)
				if err != nil {
					return nil, nil, false, fmt.Errorf("read key: %w", err)
				}
				if len(key) == 0 {
					return nil, nil, false, nil
				}
				val, err := readBytes(br)
				if err != nil {
					return nil, nil, false, fmt.Errorf("read value: %w", err)
				}
				return key, val, true, nil
			}
		} else {
			var count uint64
			if err := binary.Read(br, binary.BigEndian, &count); err != nil {
				return fmt.Errorf("read key count: %w", err)
			}
			next = countedPairs(br, count)
		}

		if err := s.restoreScopedBucket(name, sequence, next); err != nil {
			return err
		}
	}
}

// countedPairs reads the key/value pairs of one bucket in the first snapshot
// format, which gave their number up front.
func countedPairs(r io.Reader, count uint64) func() ([]byte, []byte, bool, error) {
	var read uint64
	return func() ([]byte, []byte, bool, error) {
		if read >= count {
			return nil, nil, false, nil
		}
		read++
		key, err := readBytes(r)
		if err != nil {
			return nil, nil, false, fmt.Errorf("read key: %w", err)
		}
		val, err := readBytes(r)
		if err != nil {
			return nil, nil, false, fmt.Errorf("read value: %w", err)
		}
		return key, val, true, nil
	}
}

// restoreScopedBucket restores one bucket of a snapshot according to who owns
// it. A Raft bucket is restored whole, a mixed one only for its Raft keys, and
// anything else is read past without writing, so a node keeps its own copy.
func (s *Store) restoreScopedBucket(name []byte, sequence uint64, next func() ([]byte, []byte, bool, error)) error {
	switch bucketScopeOf(name) {
	case scopeRaft:
		return s.restorePairs(name, sequence, next, nil)
	case scopeMixed:
		return s.restorePairs(name, 0, next, func(k []byte) bool { return raftOwnsKey(name, k) })
	default:
		if bucketScopeOf(name) == scopeUnknown {
			slog.Warn("metadata: snapshot holds a bucket this version does not know, skipping it", "bucket", string(name))
		}
		for {
			_, _, ok, err := next()
			if err != nil || !ok {
				return err
			}
		}
	}
}

// beginRestore drops the Raft-owned state and marks the DB as mid-restore.
func (s *Store) beginRestore() error {
	return s.db.Update(func(tx *bolt.Tx) error {
		if err := dropRaftState(tx); err != nil {
			return err
		}
		b, err := tx.CreateBucketIfNotExists(restoreStateBucket)
		if err != nil {
			return fmt.Errorf("create restore state: %w", err)
		}
		return b.Put(restoreInProgressKey, []byte("1"))
	})
}

// dropRaftState deletes every Raft-owned bucket and the Raft keys of every mixed
// one, and nothing else.
func dropRaftState(tx *bolt.Tx) error {
	for _, name := range raftOwnedBuckets {
		if tx.Bucket(name) == nil {
			continue
		}
		if err := tx.DeleteBucket(name); err != nil {
			return fmt.Errorf("delete bucket %s: %w", name, err)
		}
	}
	for name, keys := range mixedBuckets {
		b := tx.Bucket([]byte(name))
		if b == nil {
			continue
		}
		for key := range keys {
			if err := b.Delete([]byte(key)); err != nil {
				return fmt.Errorf("delete %s/%s: %w", name, key, err)
			}
		}
	}
	return nil
}

// finishRestore recreates any Raft bucket the snapshot did not carry, so code
// that expects it never meets a missing bucket, and then clears the mid-restore
// marker, making the DB usable.
func (s *Store) finishRestore() error {
	return s.db.Update(func(tx *bolt.Tx) error {
		for _, name := range raftOwnedBuckets {
			if _, err := tx.CreateBucketIfNotExists(name); err != nil {
				return fmt.Errorf("create bucket %s: %w", name, err)
			}
		}
		for name := range mixedBuckets {
			if _, err := tx.CreateBucketIfNotExists([]byte(name)); err != nil {
				return fmt.Errorf("create bucket %s: %w", name, err)
			}
		}
		if b := tx.Bucket(restoreStateBucket); b != nil {
			return b.Delete(restoreInProgressKey)
		}
		return nil
	})
}

// restoreBucket reads count key/value pairs in the first snapshot format into
// one bucket.
func (s *Store) restoreBucket(r io.Reader, name []byte, count uint64) error {
	return s.restorePairs(name, 0, countedPairs(r, count), nil)
}

// restorePairs writes the pairs next yields into one bucket, committing every
// restoreBatchKeys keys or restoreBatchBytes of data so peak memory stays bounded
// by the batch rather than by the size of the snapshot. keep, when set, picks
// which keys are written. A non-zero sequence becomes the bucket's sequence.
func (s *Store) restorePairs(name []byte, sequence uint64, next func() ([]byte, []byte, bool, error), keep func(key []byte) bool) error {
	if err := s.db.Update(func(tx *bolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists(name)
		if err != nil {
			return err
		}
		if sequence > b.Sequence() {
			return b.SetSequence(sequence)
		}
		return nil
	}); err != nil {
		return fmt.Errorf("create bucket %s: %w", name, err)
	}

	type pair struct{ key, val []byte }
	batch := make([]pair, 0, 1024)
	batchBytes := 0

	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		err := s.db.Update(func(tx *bolt.Tx) error {
			b := tx.Bucket(name)
			if b == nil {
				return fmt.Errorf("bucket %s vanished mid-restore", name)
			}
			for _, p := range batch {
				if err := b.Put(p.key, p.val); err != nil {
					return fmt.Errorf("put key: %w", err)
				}
			}
			return nil
		})
		batch = batch[:0]
		batchBytes = 0
		return err
	}

	for {
		key, val, ok, err := next()
		if err != nil {
			return err
		}
		if !ok {
			break
		}
		if keep != nil && !keep(key) {
			continue
		}
		batch = append(batch, pair{key, val})
		batchBytes += len(key) + len(val)

		if len(batch) >= restoreBatchKeys || batchBytes >= restoreBatchBytes {
			if err := flush(); err != nil {
				return err
			}
		}
	}
	return flush()
}

// restoreWasInterrupted reports whether a previous restore did not finish, which
// means the metadata in this DB is a partial copy of the cluster's and must not
// be served.
func (s *Store) restoreWasInterrupted() bool {
	interrupted := false
	s.db.View(func(tx *bolt.Tx) error {
		if b := tx.Bucket(restoreStateBucket); b != nil {
			interrupted = b.Get(restoreInProgressKey) != nil
		}
		return nil
	})
	return interrupted
}

// clearInterruptedRestore empties the Raft-owned state left by an interrupted
// restore. Raft installs its snapshot again on the next restore, so starting
// from empty is the correct recovery. Keeping the partial data would mean serving
// a subset of the cluster's objects as if it were the whole set. Node-local
// buckets were never touched by the restore, so they are kept.
func (s *Store) clearInterruptedRestore() error {
	return s.db.Update(func(tx *bolt.Tx) error {
		if err := dropRaftState(tx); err != nil {
			return err
		}
		if tx.Bucket(restoreStateBucket) != nil {
			return tx.DeleteBucket(restoreStateBucket)
		}
		return nil
	})
}

func writeBytes(w io.Writer, data []byte) error {
	if err := binary.Write(w, binary.BigEndian, uint32(len(data))); err != nil {
		return err
	}
	_, err := w.Write(data)
	return err
}

func readBytes(r io.Reader) ([]byte, error) {
	var length uint32
	if err := binary.Read(r, binary.BigEndian, &length); err != nil {
		return nil, err
	}
	data := make([]byte, length)
	if _, err := io.ReadFull(r, data); err != nil {
		return nil, err
	}
	return data, nil
}
