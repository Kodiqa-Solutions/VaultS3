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

func (fs *FileSystem) ObjectPath(bucket, key string) string {
	return fs.objectPath(bucket, key)
}

func (fs *FileSystem) bucketPath(bucket string) string {
	return filepath.Join(fs.dataDir, bucket)
}

func (fs *FileSystem) objectPath(bucket, key string) string {
	p := filepath.Join(fs.dataDir, bucket, key)
	// Prevent path traversal — resolved path must stay under bucket dir
	bucketDir := filepath.Join(fs.dataDir, bucket) + string(filepath.Separator)
	if !strings.HasPrefix(p+string(filepath.Separator), bucketDir) && p != filepath.Join(fs.dataDir, bucket) {
		return filepath.Join(fs.dataDir, bucket, "invalid-key")
	}
	return p
}

func (fs *FileSystem) versionPath(bucket, key, versionID string) string {
	p := filepath.Join(fs.dataDir, bucket, ".vs", key, versionID)
	// Prevent path traversal — resolved path must stay under bucket's .vs dir
	vsDir := filepath.Join(fs.dataDir, bucket, ".vs") + string(filepath.Separator)
	if !strings.HasPrefix(p+string(filepath.Separator), vsDir) {
		return filepath.Join(fs.dataDir, bucket, ".vs", "invalid-version")
	}
	return p
}

func (fs *FileSystem) CreateBucketDir(bucket string) error {
	return os.MkdirAll(fs.bucketPath(bucket), 0755)
}

func (fs *FileSystem) DeleteBucketDir(bucket string) error {
	return os.RemoveAll(fs.bucketPath(bucket))
}

func (fs *FileSystem) PutObject(bucket, key string, reader io.Reader, size int64) (int64, string, error) {
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
		if !info.IsDir() {
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
