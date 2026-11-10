//go:build freebsd

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
	// Negative while the root-only reserve is in use: nothing is free.
	if st.Bavail <= 0 {
		return 0, true
	}
	return uint64(st.Bavail) * st.Bsize, true
}

func isQuotaExceeded(err error) bool { return errors.Is(err, syscall.EDQUOT) }
