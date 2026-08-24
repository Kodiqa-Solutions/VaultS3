package storage

import (
	"crypto/md5"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// FileSystem implements Engine using the local filesystem.
type FileSystem struct {
	dataDir string
}

func NewFileSystem(dataDir string) (*FileSystem, error) {
	if err := os.MkdirAll(dataDir, 0755); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}
	return &FileSystem{dataDir: dataDir}, nil
}

func (fs *FileSystem) DataDir() string {
	return fs.dataDir
}

// IsDirMarker reports whether key denotes an S3 "directory marker": a zero-byte
// object whose key ends in "/" (created by s3fs, MinIO folders, and folder
// uploads). Such an object must map to a real directory so child objects nest
// under it. Storing it as a regular file blocks the child directory and fails
// with ENOTDIR ("not a directory").
func IsDirMarker(key string) bool {
	return strings.HasSuffix(key, "/")
}

// emptyContentETag is the MD5 ETag of empty content, returned for directory
// markers (which hold no bytes).
const emptyContentETag = `"d41d8cd98f00b204e9800998ecf8427e"`

// emptyMarker is the zero-byte content of a directory marker.
type emptyMarker struct{}

func (emptyMarker) Read([]byte) (int, error)       { return 0, io.EOF }
func (emptyMarker) Seek(int64, int) (int64, error) { return 0, nil }
func (emptyMarker) Close() error                   { return nil }

func (fs *FileSystem) ObjectPath(bucket, key string) string {
	return fs.objectPath(bucket, key)
}

// quarantineDir holds paths the engine refused to resolve. It is a real
// directory name rather than an error return so the existing callers, which all
// expect a string, cannot accidentally operate on an escaping path.
const quarantineDir = "invalid-bucket"

// IsSafeBucketName reports whether a bucket name can be used to build a path
// without escaping the data directory.
//
// This is deliberately NOT the full S3 naming rule (3-63 chars, lowercase, and
// so on). Callers that import buckets from somewhere else, such as a migration
// from a remote S3-compatible store, should not fail a whole migration because a
// source bucket is two characters long. They should fail when the name is a path
// traversal, which is the security property. The S3 naming rule stays where it
// belongs, on the CreateBucket API path.
func IsSafeBucketName(name string) bool {
	if name == "" || name == "." || name == ".." {
		return false
	}
	if strings.ContainsRune(name, '/') || strings.ContainsRune(name, filepath.Separator) {
		return false
	}
	// filepath.Clean collapses "a/.." to "."; anything that does not survive
	// cleaning as itself is not a single plain path segment.
	return filepath.Clean(name) == name
}

// containedPath joins segments under the data directory and reports whether the
// result stayed inside it.
//
// The containment check is against the DATA DIRECTORY, not against the bucket
// directory. Checking a path that itself contains the untrusted segment proves
// nothing: the previous version compared the resolved path against
// filepath.Join(dataDir, bucket), so a bucket of "../evil" moved the goalpost
// along with the ball and the prefix test passed while the path sat outside the
// store. Found by FuzzObjectPathStaysInsideDataDir.
//
// Bucket names arriving over the S3 API are validated by CreateBucket, but that
// is not the only door: internal/migrate creates buckets from names listed by a
// REMOTE S3 endpoint the user pointed at, and internal/replication creates them
// from a peer's change feed. Neither validates, so the engine has to.
func (fs *FileSystem) containedPath(segments ...string) (string, bool) {
	root := filepath.Clean(fs.dataDir)
	p := filepath.Join(append([]string{root}, segments...)...)
	if p != root && !strings.HasPrefix(p, root+string(filepath.Separator)) {
		return "", false
	}
	return p, true
}

func (fs *FileSystem) bucketPath(bucket string) string {
	p, ok := fs.containedPath(bucket)
	if !ok {
		return filepath.Join(filepath.Clean(fs.dataDir), quarantineDir)
	}
	return p
}

func (fs *FileSystem) objectPath(bucket, key string) string {
	if p, ok := fs.containedPath(bucket, key); ok {
		return p
	}
	// The key escaped but the bucket is sound: keep the old sentinel so the
	// failure still lands inside that bucket.
	if bp, ok := fs.containedPath(bucket); ok {
		return filepath.Join(bp, "invalid-key")
	}
	// The bucket itself escaped, so there is no bucket directory to fall back to.
	return filepath.Join(filepath.Clean(fs.dataDir), quarantineDir, "invalid-key")
}

func (fs *FileSystem) versionPath(bucket, key, versionID string) string {
	if p, ok := fs.containedPath(bucket, ".vs", key, versionID); ok {
		return p
	}
	if bp, ok := fs.containedPath(bucket, ".vs"); ok {
		return filepath.Join(bp, "invalid-version")
	}
	return filepath.Join(filepath.Clean(fs.dataDir), quarantineDir, "invalid-version")
}

// CreateBucketDir refuses a name that resolves outside the data directory rather
// than quarantining it, so a caller that skipped validation gets told instead of
// silently writing into a directory nobody asked for.
func (fs *FileSystem) CreateBucketDir(bucket string) error {
	p, ok := fs.containedPath(bucket)
	if !ok {
		return fmt.Errorf("storage: refusing bucket %q: it resolves outside the data directory", bucket)
	}
	return os.MkdirAll(p, 0755)
}

// DeleteBucketDir refuses an escaping name for the same reason CreateBucketDir
// does, and more urgently: this one is os.RemoveAll, so resolving outside the
// data directory would recursively delete something that is not ours.
func (fs *FileSystem) DeleteBucketDir(bucket string) error {
	p, ok := fs.containedPath(bucket)
	if !ok {
		return fmt.Errorf("storage: refusing to delete bucket %q: it resolves outside the data directory", bucket)
	}
	return os.RemoveAll(p)
}

func (fs *FileSystem) PutObject(bucket, key string, reader io.Reader, size int64) (int64, string, error) {
	if IsDirMarker(key) {
		// Create the directory so child objects nest under it. The marker holds
		// no bytes and is tracked in the metadata store, so there is no file to
		// collide with the child directory.
		if err := os.MkdirAll(fs.objectPath(bucket, key), 0755); err != nil {
			return 0, "", fmt.Errorf("create dir marker: %w", err)
		}
		io.Copy(io.Discard, reader) // drain any (empty) body
		return 0, emptyContentETag, nil
	}

	objPath := fs.objectPath(bucket, key)

	if err := os.MkdirAll(filepath.Dir(objPath), 0755); err != nil {
		return 0, "", fmt.Errorf("create object dir: %w", err)
	}

	// Write to temp file first, then atomic rename to prevent corruption
	tmpFile, err := os.CreateTemp(filepath.Dir(objPath), ".vaults3-tmp-*")
	if err != nil {
		return 0, "", fmt.Errorf("create temp file: %w", err)
	}
	tmpPath := tmpFile.Name()

	h := md5.New()
	written, err := io.Copy(tmpFile, io.TeeReader(reader, h))
	if err != nil {
		tmpFile.Close()
		os.Remove(tmpPath)
		return 0, "", fmt.Errorf("write object: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		os.Remove(tmpPath)
		return 0, "", fmt.Errorf("close temp file: %w", err)
	}

	// Atomic rename
	if err := os.Rename(tmpPath, objPath); err != nil {
		os.Remove(tmpPath)
		return 0, "", fmt.Errorf("rename object: %w", err)
	}

	etag := fmt.Sprintf("\"%x\"", h.Sum(nil))
	return written, etag, nil
}

func (fs *FileSystem) GetObject(bucket, key string) (ReadSeekCloser, int64, error) {
	if IsDirMarker(key) {
		return emptyMarker{}, 0, nil
	}

	objPath := fs.objectPath(bucket, key)

	info, err := os.Stat(objPath)
	if err != nil {
		return nil, 0, fmt.Errorf("stat object: %w", err)
	}
	if info.IsDir() {
		return nil, 0, fmt.Errorf("object is a directory")
	}

	f, err := os.Open(objPath)
	if err != nil {
		return nil, 0, fmt.Errorf("open object: %w", err)
	}

	return f, info.Size(), nil
}

func (fs *FileSystem) DeleteObject(bucket, key string) error {
	if IsDirMarker(key) {
		// Remove the directory only if empty. If child objects remain, the
		// directory must stay and only the marker's metadata is removed (by the
		// caller); os.Remove refuses a non-empty directory, which we treat as
		// success.
		if err := os.Remove(fs.objectPath(bucket, key)); err != nil && !os.IsNotExist(err) {
			// "directory not empty" (children still present) is expected and fine.
			return nil
		}
		return nil
	}

	objPath := fs.objectPath(bucket, key)
	err := os.Remove(objPath)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("delete object: %w", err)
	}

	// Clean up empty parent directories
	dir := filepath.Dir(objPath)
	bucketDir := fs.bucketPath(bucket)
	for dir != bucketDir {
		entries, _ := os.ReadDir(dir)
		if len(entries) > 0 {
			break
		}
		os.Remove(dir)
		dir = filepath.Dir(dir)
	}

	return nil
}

func (fs *FileSystem) ObjectExists(bucket, key string) bool {
	info, err := os.Stat(fs.objectPath(bucket, key))
	return err == nil && !info.IsDir()
}

func (fs *FileSystem) ObjectSize(bucket, key string) (int64, error) {
	info, err := os.Stat(fs.objectPath(bucket, key))
	if err != nil {
		return 0, err
	}
	return info.Size(), nil
}

func (fs *FileSystem) ListObjects(bucket, prefix, startAfter string, maxKeys int) ([]ObjectInfo, bool, error) {
	bucketDir := fs.bucketPath(bucket)

	// Verify bucket directory is accessible before walking
	if _, err := os.Stat(bucketDir); err != nil {
		return nil, false, fmt.Errorf("bucket directory: %w", err)
	}

	var objects []ObjectInfo
	err := filepath.Walk(bucketDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // skip inaccessible files within the bucket
		}
		// Skip the .vs/ versions directory
		if info.IsDir() && info.Name() == ".vs" {
			return filepath.SkipDir
		}
		if info.IsDir() {
			return nil
		}
		if strings.HasPrefix(info.Name(), ".vaults3-tmp-") {
			return nil // in-flight upload temp file, not a real object yet
		}

		rel, err := filepath.Rel(bucketDir, path)
		if err != nil {
			return nil
		}
		key := strings.ReplaceAll(rel, string(filepath.Separator), "/")

		if prefix != "" && !strings.HasPrefix(key, prefix) {
			return nil
		}
		if startAfter != "" && key <= startAfter {
			return nil
		}

		objects = append(objects, ObjectInfo{
			Key:          key,
			Size:         info.Size(),
			LastModified: info.ModTime().Unix(),
			ETag:         computeETag(path),
		})

		return nil
	})
	if err != nil {
		return nil, false, fmt.Errorf("walk bucket: %w", err)
	}

	sort.Slice(objects, func(i, j int) bool {
		return objects[i].Key < objects[j].Key
	})

	truncated := false
	if maxKeys > 0 && len(objects) > maxKeys {
		objects = objects[:maxKeys]
		truncated = true
	}

	return objects, truncated, nil
}

func (fs *FileSystem) BucketSize(bucket string) (int64, int64, error) {
	var totalSize int64
	var count int64

	bucketDir := fs.bucketPath(bucket)

	// Verify bucket directory is accessible before walking
	if _, err := os.Stat(bucketDir); err != nil {
		return 0, 0, fmt.Errorf("bucket directory: %w", err)
	}

	err := filepath.Walk(bucketDir, func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // skip inaccessible files within the bucket
		}
		if info.IsDir() && info.Name() == ".vs" {
			return filepath.SkipDir
		}
		if !info.IsDir() && !strings.HasPrefix(info.Name(), ".vaults3-tmp-") {
			totalSize += info.Size()
			count++
		}
		return nil
	})

	return totalSize, count, err
}

func (fs *FileSystem) PutObjectVersion(bucket, key, versionID string, reader io.Reader, size int64) (int64, string, error) {
	vPath := fs.versionPath(bucket, key, versionID)

	if err := os.MkdirAll(filepath.Dir(vPath), 0755); err != nil {
		return 0, "", fmt.Errorf("create version dir: %w", err)
	}

	f, err := os.Create(vPath)
	if err != nil {
		return 0, "", fmt.Errorf("create version file: %w", err)
	}
	defer f.Close()

	h := md5.New()
	written, err := io.Copy(f, io.TeeReader(reader, h))
	if err != nil {
		os.Remove(vPath)
		return 0, "", fmt.Errorf("write version: %w", err)
	}

	etag := fmt.Sprintf("\"%x\"", h.Sum(nil))
	return written, etag, nil
}

func (fs *FileSystem) GetObjectVersion(bucket, key, versionID string) (ReadSeekCloser, int64, error) {
	vPath := fs.versionPath(bucket, key, versionID)

	info, err := os.Stat(vPath)
	if err != nil {
		return nil, 0, fmt.Errorf("stat version: %w", err)
	}

	f, err := os.Open(vPath)
	if err != nil {
		return nil, 0, fmt.Errorf("open version: %w", err)
	}

	return f, info.Size(), nil
}

func (fs *FileSystem) DeleteObjectVersion(bucket, key, versionID string) error {
	vPath := fs.versionPath(bucket, key, versionID)
	err := os.Remove(vPath)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("delete version: %w", err)
	}

	// Clean up empty parent directories up to .vs/
	dir := filepath.Dir(vPath)
	vsDir := filepath.Join(fs.bucketPath(bucket), ".vs")
	for dir != vsDir && dir != fs.bucketPath(bucket) {
		entries, _ := os.ReadDir(dir)
		if len(entries) > 0 {
			break
		}
		os.Remove(dir)
		dir = filepath.Dir(dir)
	}

	return nil
}

func computeETag(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()

	h := md5.New()
	io.Copy(h, f)
	return fmt.Sprintf("\"%x\"", h.Sum(nil))
}
