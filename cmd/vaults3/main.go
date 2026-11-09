package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"strings"

	"github.com/Kodiqa-Solutions/VaultS3/internal/api"
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
  vaults3 diagnose         print version and enabled subsystems, for bug reports
  vaults3 healthcheck      probe this server's own /health, for container checks
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
		case "diagnose":
			os.Exit(runDiagnose(os.Args[2:]))
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
	// Every log line also goes to the dashboard's live log view.
	server.LogBroadcaster = api.NewLogBroadcaster()
	handler := slog.NewTextHandler(io.MultiWriter(os.Stderr, server.LogBroadcaster), &slog.HandlerOptions{Level: level})
	slog.SetDefault(slog.New(handler))

	if usedDefaults {
		slog.Info("no config file found, running on built-in defaults",
			"looked_for", *configPath,
			"hint", "run `vaults3 setup` to write one")
	}

	// Started as root with PUID/PGID (a NAS container): own the folders and
	// run as that user, before the server opens any file.
	if err := dropPrivileges(cfg); err != nil {
		slog.Error("could not switch to the PUID/PGID user", "error", err)
		os.Exit(1)
	}

	// Make the build version available to the server (update checker, /version API).
	server.Version = version

	// Create server
	srv, err := server.New(cfg)
	if err != nil {
		slog.Error("failed to create server", "error", err)
		if errors.Is(err, fs.ErrPermission) {
			fix := "give the folders to this user (chown -R), or start the container as root with PUID and PGID set (--user 0:0 -e PUID=99 -e PGID=100) and it takes them over"
			if os.Getenv("PUID") != "" || os.Getenv("PGID") != "" {
				fix = "a file or folder the server uses belongs to another user: give it to this user (chown -R), for example a cold tier, erasure or backup folder whose contents were not given over, or a TLS certificate or key readable only by root"
			}
			slog.Error("the server cannot write a file or folder it needs",
				"running_as", fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()),
				"fix", fix)
		}
		os.Exit(1)
	}
	// Run blocks until a shutdown signal. Close is called on every way out:
	// os.Exit skips deferred calls, so a listener error used to leave Raft,
	// the metadata store and the access log unclosed.
	if err := srv.Run(); err != nil {
		slog.Error("server error", "error", err)
		srv.Close()
		os.Exit(1)
	}
	srv.Close()
}
