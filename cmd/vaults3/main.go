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

// defaultConfigPath is where the server looks when nobody says otherwise. It is
// allowed not to exist; see the note in main.
const defaultConfigPath = "configs/vaults3.yaml"

func usage() {
	fmt.Fprintf(os.Stderr, `VaultS3 %s

Usage:
  vaults3 [flags]          start the server
  vaults3 setup [flags]    write a config file and create its directories
  vaults3 help             show this message

Flags:
`, version)
	flag.PrintDefaults()
	fmt.Fprintf(os.Stderr, "\nWith no config file present, the server starts on its built-in defaults\nand generates an admin secret on first run.\n")
}

func main() {
	// Subcommands come before flags so `vaults3 setup -data-dir x` parses its own.
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "setup":
			os.Exit(runSetup(os.Args[2:]))
		case "healthcheck":
			os.Exit(runHealthcheck(os.Args[2:]))
		case "help", "-h", "--help":
			usage()
			os.Exit(0)
		}
	}

	showVersion := flag.Bool("version", false, "print version and exit")

	configPath := flag.String("config", defaultConfigPath, "path to config file")
	flag.Usage = usage
	flag.Parse()

	if *showVersion {
		fmt.Printf("vaults3 %s\n", version)
		os.Exit(0)
	}

	// A config path the operator named must exist: a typo has to fail, not start
	// a server with settings nobody chose. The DEFAULT path not existing is an
	// ordinary first run, and the built-in defaults are a working single node,
	// so the binary starts instead of refusing (issue #51).
	explicit := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == "config" {
			explicit = true
		}
	})
	var cfg *config.Config
	var err error
	usedDefaults := false
	if explicit {
		cfg, err = config.Load(*configPath)
	} else {
		cfg, usedDefaults, err = config.LoadOrDefaults(*configPath)
	}
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

	if usedDefaults {
		slog.Info("no config file found, running on built-in defaults",
			"looked_for", *configPath,
			"hint", "run `vaults3 setup` to write one")
	}

	// Make the build version available to the server (update checker, /version API).
	server.Version = version

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
