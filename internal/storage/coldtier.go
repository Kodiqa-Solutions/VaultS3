package storage

import (
	"errors"
	"io"
	iofs "io/fs"
	"os"
	"syscall"
)

// ColdTier puts a cold directory under the hot one. It sits at the bottom of
// the engine stack, directly on the hot FileSystem, so the encryption,
// compression and packing layers above it see the same stored bytes whichever
// tier they come from: the tiering manager moves an object's file as it is on
// disk, below those layers.
//
// A read that finds no hot file reads the cold one. Before 5.0.12 nothing read
// the cold tier at all: every object moved there answered NoSuchKey until it
// was moved back, which is why 5.0.11 paused moving objects there.
//
// Writes go to the hot tier, and a write or delete removes any cold copy of
// the key, so a stale cold file can never be read in place of newer data.
// Versions are not tiered (the manager refuses them), so version calls go to
// the hot tier only.
type ColdTier struct {
	Engine        // the hot tier
	Cold   Engine // the cold tier, a FileSystem on the cold directory
}

// NewColdTier stacks cold under hot.
func NewColdTier(hot, cold Engine) *ColdTier {
	return &ColdTier{Engine: hot, Cold: cold}
}

func notExist(err error) bool { return errors.Is(err, iofs.ErrNotExist) }

// hotAbsent reports a hot read that found no file of the key: nothing there,
// or a path that another key's file or folder occupies. A cold "a" with a hot
// "a/b" leaves a folder at the hot path "a", and a cold "a/b" with a hot file
// "a" makes the hot path "a/b" ENOTDIR. Both tiers can hold such a pair, as a
// single one cannot, so these are not errors of the key.
func hotAbsent(err error) bool {
	return notExist(err) || errors.Is(err, syscall.ENOTDIR) || errors.Is(err, errIsDirectory)
}

// hotFile reports whether the hot tier holds a regular file for key.
func (t *ColdTier) hotFile(bucket, key string) bool {
	fi, err := os.Lstat(t.ObjectPath(bucket, key))
	return err == nil && fi.Mode().IsRegular()
}

// GetObject reads the hot file, or the cold one when there is no hot file.
func (t *ColdTier) GetObject(bucket, key string) (ReadSeekCloser, int64, error) {
	r, n, err := t.Engine.GetObject(bucket, key)
	if err == nil || IsDirMarker(key) || !hotAbsent(err) {
		return r, n, err
	}
	if cr, cn, cerr := t.Cold.GetObject(bucket, key); cerr == nil {
		return cr, cn, nil
	}
	return nil, 0, err
}

// ObjectExists reports a file in either tier.
func (t *ColdTier) ObjectExists(bucket, key string) bool {
	return t.Engine.ObjectExists(bucket, key) || t.Cold.ObjectExists(bucket, key)
}

// ObjectSize is the size of the hot file, or of the cold one.
func (t *ColdTier) ObjectSize(bucket, key string) (int64, error) {
	n, err := t.Engine.ObjectSize(bucket, key)
	if IsDirMarker(key) || (err == nil && t.hotFile(bucket, key)) || (err != nil && !hotAbsent(err)) {
		return n, err
	}
	if err == nil {
		err = errIsDirectory // a folder of other keys, not this object
	}
	if cn, cerr := t.Cold.ObjectSize(bucket, key); cerr == nil {
		return cn, nil
	}
	return 0, err
}

// PutObject writes to the hot tier and then drops any cold copy, which the
// new data replaces. Dropping it first would leave the key unreadable if the
// write then failed.
func (t *ColdTier) PutObject(bucket, key string, r io.Reader, size int64) (int64, string, error) {
	n, etag, err := t.Engine.PutObject(bucket, key, r, size)
	if err == nil {
		t.dropCold(bucket, key)
	}
	return n, etag, err
}

// DeleteObject removes the object from both tiers. Either one missing is
// fine; an object in neither is reported as the hot tier reports it.
func (t *ColdTier) DeleteObject(bucket, key string) error {
	// The hot path may be a folder of other keys (a cold "a" and a hot
	// "a/b"); removing it fails, after the cold copy went, and the delete
	// failed with the record left and its data gone. Only the key's own file
	// is the hot copy.
	err := iofs.ErrNotExist
	if IsDirMarker(key) || t.hotFile(bucket, key) {
		err = t.Engine.DeleteObject(bucket, key)
	}
	if err != nil && hotAbsent(err) {
		err = iofs.ErrNotExist
	}
	cerr := t.Cold.DeleteObject(bucket, key)
	if cerr != nil && hotAbsent(cerr) {
		cerr = iofs.ErrNotExist
	}
	if err == nil || notExist(err) {
		if cerr == nil || notExist(cerr) {
			if err != nil && cerr != nil {
				return err
			}
			return nil
		}
		return cerr
	}
	return err
}

// DeleteBucketDir removes the bucket's directory in both tiers.
func (t *ColdTier) DeleteBucketDir(bucket string) error {
	if err := t.Engine.DeleteBucketDir(bucket); err != nil {
		return err
	}
	if err := t.Cold.DeleteBucketDir(bucket); err != nil && !notExist(err) {
		return err
	}
	return nil
}

// BucketSize counts both tiers: a cold object still takes disk, and a quota
// that stopped counting it once it moved would let the bucket grow past it.
func (t *ColdTier) BucketSize(bucket string) (int64, int64, error) {
	size, count, err := t.Engine.BucketSize(bucket)
	if err != nil {
		return size, count, err
	}
	if cs, cc, cerr := t.Cold.BucketSize(bucket); cerr == nil {
		size += cs
		count += cc
	}
	return size, count, nil
}

// ListObjects lists both tiers, merged in key order. A key in both (which a
// write leaves only until it drops the cold copy) is listed once, as the hot
// file. Listing the hot tier alone let a bucket holding only cold objects pass
// DeleteBucket's emptiness check, and left cold objects out of inventory
// reports and batch jobs.
func (t *ColdTier) ListObjects(bucket, prefix, startAfter string, maxKeys int) ([]ObjectInfo, bool, error) {
	hot, hotMore, err := t.Engine.ListObjects(bucket, prefix, startAfter, maxKeys)
	if err != nil {
		return nil, false, err
	}
	cold, coldMore, cerr := t.Cold.ListObjects(bucket, prefix, startAfter, maxKeys)
	if cerr != nil {
		if !notExist(cerr) {
			return nil, false, cerr
		}
		cold, coldMore = nil, false
	}
	if len(cold) == 0 {
		return hot, hotMore, nil
	}
	// Each list holds its first maxKeys keys after startAfter, so the first
	// maxKeys of the merge are all in them.
	merged := make([]ObjectInfo, 0, len(hot)+len(cold))
	i, j := 0, 0
	for i < len(hot) || j < len(cold) {
		switch {
		case j == len(cold) || (i < len(hot) && hot[i].Key < cold[j].Key):
			merged = append(merged, hot[i])
			i++
		case i == len(hot) || cold[j].Key < hot[i].Key:
			merged = append(merged, cold[j])
			j++
		default: // the same key in both
			merged = append(merged, hot[i])
			i++
			j++
		}
	}
	more := hotMore || coldMore
	if maxKeys > 0 && len(merged) > maxKeys {
		merged = merged[:maxKeys]
		more = true
	}
	return merged, more, nil
}

func (t *ColdTier) dropCold(bucket, key string) {
	if !t.Cold.ObjectExists(bucket, key) {
		return
	}
	_ = t.Cold.DeleteObject(bucket, key)
}
