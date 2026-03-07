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

type LocalTarget struct {
	basePath string
}

func NewLocalTarget(basePath string) (*LocalTarget, error) {
	if err := os.MkdirAll(basePath, 0755); err != nil {
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

	dir := filepath.Dir(absPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}

	f, err := os.Create(absPath)
	if err != nil {
		return err
	}
	defer f.Close()

	_, err = io.Copy(f, reader)
	return err
}

func (t *LocalTarget) Close() error {
	return nil
}
