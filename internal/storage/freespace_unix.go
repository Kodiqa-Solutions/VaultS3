//go:build linux || darwin

package storage

import (
	"errors"
	"syscall"
)

func freeBytes(dir string) (uint64, bool) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, false
	}
	return uint64(st.Bavail) * uint64(st.Bsize), true //nolint:unconvert // field types differ by platform
}

func isQuotaExceeded(err error) bool { return errors.Is(err, syscall.EDQUOT) }
