//go:build !linux && !darwin && !freebsd

package storage

func freeBytes(string) (uint64, bool) { return 0, false }

func isQuotaExceeded(error) bool { return false }
