package main

import (
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/Kodiqa-Solutions/VaultS3/internal/config"
	"github.com/Kodiqa-Solutions/VaultS3/internal/server"
)

var version = "dev"

func main() {
	showVersion := flag.Bool("version", false, "print version and exit")

	configPath := flag.String("config", "configs/vaults3.yaml", "path to config file")
	flag.Parse()

	if *showVersion {
		fmt.Printf("vaults3 %s\n", version)
		os.Exit(0)
	}

	// Load config
	cfg, err := config.Load(*configPath)
	if err != nil {
		slog.Error("failed to load config", "error", err)
		os.Exit(1)
	}

	// Set up structured logging
	var level slog.Level
	switch strings.ToLower(cfg.Logging.Level) {
	case "debug":
		level = slog.LevelDebug
	case "warn", "warning":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		level = slog.LevelInfo
	}
	handler := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})
	slog.SetDefault(slog.New(handler))

	// Create server
	srv, err := server.New(cfg)
	if err != nil {
		slog.Error("failed to create server", "error", err)
		os.Exit(1)
	}
	defer srv.Close()

	// Run blocks until shutdown signal
	if err := srv.Run(); err != nil {
		slog.Error("server error", "error", err)
		os.Exit(1)
	}
}
