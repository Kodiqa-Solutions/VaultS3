package main

import (
	"encoding/json"
	"fmt"
	"os"
	"runtime"
	"strings"

	"github.com/Kodiqa-Solutions/VaultS3/internal/config"
)

// packagedConfigPath is where the deb/rpm/apk packages and the container image
// put the config. The binary's own default is a relative path suited to a source
// checkout, so neither alone covers every install.
const packagedConfigPath = "/etc/vaults3/vaults3.yaml"

// notFoundAfterSearch distinguishes "we looked in the usual places and there is
// genuinely no config" from "you pointed us at a path that does not exist".
const notFoundAfterSearch = "(none found, using built-in defaults)"

// firstExistingConfig returns the first config that exists among the paths a
// VaultS3 install actually uses, or the default path when none do.
func firstExistingConfig() string {
	for _, p := range []string{defaultConfigPath, packagedConfigPath} {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p
		}
	}
	return defaultConfigPath
}

// runDiagnose prints what a bug report needs and a user cannot easily assemble:
// the version, and which optional subsystems are actually switched on.
//
// It exists because every hard issue this year was slowed by not knowing that.
// Issue #49 (concurrent GETs OOM a replica) was only reproduced after enabling
// wrapper engines one at a time, because the reporter never said encryption was
// on and there was no way to ask for that in one step. #50 needed the same
// engine-by-engine walk. A default-config benchmark proves nothing about an
// optional feature, so "it does not reproduce" is worthless without this list.
//
// It reads the config file only. It never contacts the server, so it works on a
// box where the server will not start, which is when it is needed most.
//
// Secrets are never printed. Anything sensitive is reduced to whether it is set,
// so the output can be pasted into a public issue without redaction.
func runDiagnose(args []string) int {
	configPath := ""
	asJSON := false
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "-config" || args[i] == "--config":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "diagnose: -config needs a path")
				return 2
			}
			configPath = args[i+1]
			i++
		case strings.HasPrefix(args[i], "-config="), strings.HasPrefix(args[i], "--config="):
			configPath = args[i][strings.Index(args[i], "=")+1:]
		case args[i] == "-json" || args[i] == "--json":
			asJSON = true
		case args[i] == "-h" || args[i] == "--help":
			fmt.Println("Usage: vaults3 diagnose [-config <path>] [-json]\n\n" +
				"With no -config, looks for " + defaultConfigPath + " then " + packagedConfigPath + ".\n\n" +
				"Prints the version and which optional subsystems are enabled, which is\n" +
				"what a bug report needs. Reads the config only, so it works even when the\n" +
				"server will not start. No secrets are printed, so the output is safe to\n" +
				"paste into a public issue.")
			return 0
		default:
			fmt.Fprintf(os.Stderr, "diagnose: unknown argument %q\n", args[i])
			return 2
		}
	}

	// With no -config, look where the config actually lives for each install
	// method rather than only the source-checkout default. Getting this wrong is
	// not a cosmetic failure: inside a container the bare command found nothing
	// and cheerfully reported "no subsystems enabled" while every one of them was
	// on, which is the exact opposite of what a bug report needs.
	searched := false
	if configPath == "" {
		searched = true
		configPath = firstExistingConfig()
	}

	cfg, usedDefaults, err := config.LoadOrDefaults(configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "diagnose: read config: %v\n", err)
		return 2
	}

	loadedFrom := configPath
	if usedDefaults {
		loadedFrom = ""
		if searched {
			loadedFrom = notFoundAfterSearch
		}
	}
	rep := buildDiagnosis(cfg, version, loadedFrom)

	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(rep); err != nil {
			fmt.Fprintf(os.Stderr, "diagnose: encode: %v\n", err)
			return 2
		}
		return 0
	}
	fmt.Print(rep.text())
	return 0
}

// diagnosis is the whole report. Field names are stable because people paste
// them into issues and search for them.
type diagnosis struct {
	Version    string   `json:"version"`
	Go         string   `json:"go"`
	Platform   string   `json:"platform"`
	NumCPU     int      `json:"numCpu"`
	ConfigFile string   `json:"configFile"`
	Enabled    []string `json:"enabled"`
	Notes      []string `json:"notes"`
}

// buildDiagnosis is separated from printing so it can be tested without running
// a server or capturing stdout.
func buildDiagnosis(cfg *config.Config, ver, loadedFrom string) diagnosis {
	d := diagnosis{
		Version:    ver,
		Go:         runtime.Version(),
		Platform:   runtime.GOOS + "/" + runtime.GOARCH,
		NumCPU:     runtime.NumCPU(),
		ConfigFile: loadedFrom,
		Enabled:    []string{},
		Notes:      []string{},
	}
	if d.ConfigFile == "" {
		d.ConfigFile = "(none, using built-in defaults)"
	}

	on := func(cond bool, format string, a ...any) {
		if cond {
			d.Enabled = append(d.Enabled, fmt.Sprintf(format, a...))
		}
	}

	// The storage wrappers come first: these are the ones that change how bytes
	// are written, and so the ones that decide whether a bug reproduces at all.
	switch {
	case cfg.Encryption.Enabled && cfg.Encryption.PerBucket:
		on(true, "encryption: per-bucket keys (opt-in per bucket)")
	case cfg.Encryption.Enabled && cfg.Encryption.KMS.Enabled:
		on(true, "encryption: SSE-KMS (provider %s)", cfg.Encryption.KMS.Provider)
	case cfg.Encryption.Enabled:
		on(true, "encryption: SSE-S3 (static key)")
	}
	on(cfg.Compression.Enabled, "compression: zstd")
	on(cfg.Packing.Enabled, "small-file packing: objects <= %d bytes", cfg.Packing.MaxObjectSize)
	on(cfg.Erasure.Enabled, "erasure coding: %d data + %d parity", cfg.Erasure.DataShards, cfg.Erasure.ParityShards)

	if cfg.Cluster.Enabled {
		shards := cfg.Cluster.MetadataShards
		if shards > 1 {
			on(true, "clustering: raft, %d peers, metadata sharded into %d groups", len(cfg.Cluster.Peers), shards)
		} else {
			on(true, "clustering: raft, %d peers, single metadata group", len(cfg.Cluster.Peers))
		}
	}

	on(cfg.Replication.Enabled, "async replication")
	on(cfg.Tiering.Enabled, "tiering: hot/cold")
	on(cfg.Backup.Enabled, "backup scheduler")
	on(cfg.Scanner.Enabled, "virus scanning webhook")
	on(cfg.Lambda.Enabled, "lambda triggers")
	on(cfg.Vector.Enabled, "vector/semantic search")
	on(cfg.OIDC.Enabled, "OIDC SSO")
	on(cfg.RateLimit.Enabled, "rate limiting")
	on(cfg.Server.TLS.Enabled, "TLS")
	on(cfg.Server.BasePath != "", "reverse-proxy base path: %s", cfg.Server.BasePath)
	on(cfg.Debug, "debug endpoints (/debug/pprof)")

	// Known interactions worth surfacing without being asked, because each one
	// has already cost a round trip on an issue.
	if cfg.Compression.Enabled && cfg.Encryption.Enabled {
		d.Notes = append(d.Notes, "compression saves nothing here: encryption wraps compression, so the "+
			"compressor only sees ciphertext (measured 1.00x). The CPU cost is still paid.")
	}
	if cfg.Packing.Enabled && (cfg.Encryption.Enabled || cfg.Erasure.Enabled) {
		d.Notes = append(d.Notes, "small-file packing is skipped while encryption or erasure coding is on, "+
			"so objects are stored individually despite packing being enabled.")
	}
	if cfg.Cluster.Enabled && cfg.Cluster.Secret == "" {
		d.Notes = append(d.Notes, "cluster.secret is empty, so inter-node endpoints are unauthenticated. "+
			"Set it on every node.")
	}

	return d
}

// text renders the human form. Kept deliberately paste-friendly: no colour, no
// box drawing, so it survives a GitHub comment intact.
func (d diagnosis) text() string {
	var b strings.Builder
	b.WriteString("VaultS3 diagnosis\n")
	b.WriteString("=================\n\n")
	fmt.Fprintf(&b, "version:     %s\n", d.Version)
	fmt.Fprintf(&b, "go:          %s\n", d.Go)
	fmt.Fprintf(&b, "platform:    %s (%d CPU)\n", d.Platform, d.NumCPU)
	fmt.Fprintf(&b, "config file: %s\n", d.ConfigFile)

	b.WriteString("\nenabled subsystems:\n")
	if len(d.Enabled) == 0 {
		b.WriteString("  (none, this is a plain default deployment)\n")
	}
	for _, e := range d.Enabled {
		fmt.Fprintf(&b, "  - %s\n", e)
	}

	if len(d.Notes) > 0 {
		b.WriteString("\nnotes:\n")
		for _, n := range d.Notes {
			fmt.Fprintf(&b, "  ! %s\n", n)
		}
	}
	b.WriteString("\nNo secrets are included above, so this is safe to paste into an issue.\n")
	return b.String()
}
