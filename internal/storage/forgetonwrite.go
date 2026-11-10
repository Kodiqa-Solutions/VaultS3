package storage

import "io"

// CopyForgetter drops what a node recorded about one local data file
// (metadata.CopyIndex).
type CopyForgetter interface {
	Forget(bucket, key, version string)
}

// ForgetOnWrite wraps a cluster node's engine so that every write or delete
// of a data file first drops the node's record of which object the file
// holds. Writers that know the object (an S3 write, a replica copy) record it
// again afterwards; every other writer (a dashboard upload, a batch job, a
// migration) leaves the file with no record, judged by size as before. A
// record could otherwise name an object the file no longer holds: a
// dashboard upload over an S3 object left the old record, and the node
// refused its own current copy with 503 SlowDown.
type ForgetOnWrite struct {
	Engine
	forget CopyForgetter
}

// NewForgetOnWrite wraps e.
func NewForgetOnWrite(e Engine, f CopyForgetter) *ForgetOnWrite {
	return &ForgetOnWrite{Engine: e, forget: f}
}

// Unwrap returns the wrapped engine (see EngineAs).
func (w *ForgetOnWrite) Unwrap() Engine { return w.Engine }

func (w *ForgetOnWrite) PutObject(bucket, key string, r io.Reader, size int64) (int64, string, error) {
	w.forget.Forget(bucket, key, "")
	return w.Engine.PutObject(bucket, key, r, size)
}

func (w *ForgetOnWrite) PutObjectVersion(bucket, key, versionID string, r io.Reader, size int64) (int64, string, error) {
	w.forget.Forget(bucket, key, versionID)
	return w.Engine.PutObjectVersion(bucket, key, versionID, r, size)
}

func (w *ForgetOnWrite) DeleteObject(bucket, key string) error {
	w.forget.Forget(bucket, key, "")
	return w.Engine.DeleteObject(bucket, key)
}

func (w *ForgetOnWrite) DeleteObjectVersion(bucket, key, versionID string) error {
	w.forget.Forget(bucket, key, versionID)
	return w.Engine.DeleteObjectVersion(bucket, key, versionID)
}

// InvalidateBucketSize passes the tiering manager's call through.
func (w *ForgetOnWrite) InvalidateBucketSize(bucket string) {
	if i, ok := w.Engine.(interface{ InvalidateBucketSize(string) }); ok {
		i.InvalidateBucketSize(bucket)
	}
}

// EngineAs finds an engine of type T in e or in the engines it wraps.
func EngineAs[T any](e Engine) (T, bool) {
	for e != nil {
		if t, ok := e.(T); ok {
			return t, true
		}
		u, ok := e.(interface{ Unwrap() Engine })
		if !ok {
			break
		}
		e = u.Unwrap()
	}
	var zero T
	return zero, false
}

// LocalCopySize is the size of this node's copy of an object's plain data
// (version "") or of one version, as the engine reads it back, or -1 when
// there is none. It is what a copy record (metadata.CopyIndex) is kept with.
func LocalCopySize(e Engine, bucket, key, version string) int64 {
	var rc ReadSeekCloser
	var size int64
	var err error
	if version == "" {
		rc, size, err = e.GetObject(bucket, key)
	} else {
		rc, size, err = e.GetObjectVersion(bucket, key, version)
	}
	if err != nil {
		return -1
	}
	_ = rc.Close()
	return size
}
