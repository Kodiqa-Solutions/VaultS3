//go:build !linux

package main

import (
	"log/slog"
	"os"

	"github.com/Kodiqa-Solutions/VaultS3/internal/config"
)

// dropPrivileges is a Linux container feature (PUID/PGID). Elsewhere it
// only says the variables are ignored.
func dropPrivileges(cfg *config.Config) error {
	if _, _, ok, err := privDropTarget(os.Getenv); err != nil || ok {
		if err == nil {
			slog.Warn("PUID/PGID are only used on Linux; ignored")
		}
		return err
	}
	return nil
}

func takeFile(string, int, int) error { return nil }

func linkCount(os.FileInfo) uint64 { return 1 }
