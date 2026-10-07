package backup

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/Kodiqa-Solutions/VaultS3/internal/config"
)

type Target interface {
	Write(bucket, key string, reader io.Reader, size int64) error
	// WriteMetadata stores a copy of the metadata database, produced by fn.
	WriteMetadata(fn func(io.Writer) error) error
	Close() error
}

func NewTarget(cfg config.BackupTarget) (Target, error) {
	switch cfg.Type {
	case "local", "":
		return NewLocalTarget(cfg.Path)
	default:
		return nil, fmt.Errorf("unsupported backup target type: %s", cfg.Type)
	}
}

// metadataDir holds the metadata copy inside a target. Bucket names cannot
// contain an underscore, so it can never collide with a bucket's directory.
const metadataDir = "_vaults3"

// MetadataFile is where a local target keeps the metadata copy: a bbolt
// database that can replace metadata.db on restore.
const MetadataFile = "metadata.db"

// Backup files hold every object's plaintext and the whole metadata database,
// credentials included, so they are readable by the server's user only.
const (
	backupFileMode = 0o600
	backupDirMode  = 0o700
)

type LocalTarget struct {
	basePath string
}

func NewLocalTarget(basePath string) (*LocalTarget, error) {
	if err := os.MkdirAll(basePath, backupDirMode); err != nil {
		return nil, fmt.Errorf("create backup dir: %w", err)
	}
	return &LocalTarget{basePath: basePath}, nil
}

func (t *LocalTarget) Write(bucket, key string, reader io.Reader, size int64) error {
	p := filepath.Join(t.basePath, bucket, key)
	// Validate the resolved path stays within basePath to prevent traversal
	absBase, err := filepath.Abs(t.basePath)
	if err != nil {
		return fmt.Errorf("resolve base path: %w", err)
	}
	absPath, err := filepath.Abs(p)
	if err != nil {
		return fmt.Errorf("resolve target path: %w", err)
	}
	if !strings.HasPrefix(absPath, absBase+string(filepath.Separator)) {
		return fmt.Errorf("path traversal detected")
	}
	return writeFileAtomic(absPath, func(w io.Writer) error {
		_, err := io.Copy(w, reader)
		return err
	})
}

func (t *LocalTarget) WriteMetadata(fn func(io.Writer) error) error {
	return writeFileAtomic(filepath.Join(t.basePath, metadataDir, MetadataFile), fn)
}

// writeFileAtomic writes through a temp file and renames it into place, so a
// backup that fails part way leaves the previous copy of the file whole rather
// than truncated.
func writeFileAtomic(dst string, fn func(io.Writer) error) error {
	dir := filepath.Dir(dst)
	if err := os.MkdirAll(dir, backupDirMode); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".vaults3-backup-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	if err := f.Chmod(backupFileMode); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := fn(f); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

func (t *LocalTarget) Close() error {
	return nil
}
