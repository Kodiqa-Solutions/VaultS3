package main

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// `vaults3 setup` (issue #51).
//
// A downloaded binary had no way to produce a config, and the server refused to
// start without one, so the documented install did not work. The server now
// starts on defaults alone, and this writes a real config for anyone who wants
// one on disk: it asks, it creates the directories, it mints a secret, and it
// prints the two commands needed next.
//
// It writes only what the answers cover. Clustering, encryption, replication and
// the rest stay out of the file entirely, so an operator who later reads it sees
// the handful of settings they actually chose rather than a wall of disabled
// subsystems.

const defaultSetupConfigPath = "vaults3.yaml"

type setupAnswers struct {
	configPath  string
	dataDir     string
	metadataDir string
	accessLog   string
	accessKey   string
	secretKey   string
	buckets     []string
	address     string
	port        int
	force       bool
}

func runSetup(args []string) int {
	fs := flag.NewFlagSet("setup", flag.ContinueOnError)
	a := setupAnswers{}
	var bucketList string
	var nonInteractive bool
	fs.StringVar(&a.configPath, "config", defaultSetupConfigPath, "path of the config file to write")
	fs.StringVar(&a.dataDir, "data-dir", "./data", "directory for object data")
	fs.StringVar(&a.metadataDir, "metadata-dir", "./metadata", "directory for the metadata database")
	fs.StringVar(&a.accessLog, "access-log", "./logs/access.log", "path of the access log file")
	fs.StringVar(&a.accessKey, "admin-access-key", "vaults3-admin", "admin access key")
	fs.StringVar(&a.secretKey, "admin-secret-key", "", "admin secret key (generated when empty)")
	fs.StringVar(&bucketList, "default-bucket", "", "comma-separated buckets to create at startup")
	fs.StringVar(&a.address, "address", "127.0.0.1", "address to listen on")
	fs.IntVar(&a.port, "port", 9000, "port to listen on")
	fs.BoolVar(&a.force, "force", false, "overwrite an existing config file")
	fs.BoolVar(&nonInteractive, "non-interactive", false, "take every answer from flags and defaults, ask nothing")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: vaults3 setup [flags]\n\nWrites a VaultS3 config file and creates the directories it names.\n\nFlags:\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	a.buckets = splitCommaList(bucketList)

	interactive := !nonInteractive && stdinIsTerminal()
	if interactive {
		if err := promptAnswers(&a); err != nil {
			fmt.Fprintf(os.Stderr, "setup: %v\n", err)
			return 1
		}
	}

	if err := applySetup(&a, interactive); err != nil {
		fmt.Fprintf(os.Stderr, "setup: %v\n", err)
		return 1
	}
	return 0
}

// applySetup performs the whole side-effecting half, so a test can drive it
// without a terminal.
func applySetup(a *setupAnswers, interactive bool) error {
	if a.port < 1 || a.port > 65535 {
		return fmt.Errorf("port %d is not a usable port", a.port)
	}
	// Refuse to clobber a config someone is running. --force is the deliberate
	// way past it, and an interactive run asks instead of refusing.
	if _, err := os.Stat(a.configPath); err == nil && !a.force {
		if !interactive {
			return fmt.Errorf("%s already exists; pass --force to overwrite it", a.configPath)
		}
		if !askYesNo(fmt.Sprintf("%s already exists. Overwrite it?", a.configPath), false) {
			return fmt.Errorf("leaving %s as it is", a.configPath)
		}
	}

	generated := false
	if a.secretKey == "" {
		secret, err := randomSecret(24)
		if err != nil {
			return fmt.Errorf("generate admin secret: %w", err)
		}
		a.secretKey = secret
		generated = true
	}

	dirs := []string{a.dataDir, a.metadataDir}
	if logDir := filepath.Dir(a.accessLog); logDir != "" && logDir != "." {
		dirs = append(dirs, logDir)
	}
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return fmt.Errorf("create %s: %w", d, err)
		}
	}

	if parent := filepath.Dir(a.configPath); parent != "" && parent != "." {
		if err := os.MkdirAll(parent, 0o755); err != nil {
			return fmt.Errorf("create %s: %w", parent, err)
		}
	}
	// 0600: the file holds the admin secret, so it is readable by its owner only.
	if err := os.WriteFile(a.configPath, []byte(renderConfig(a)), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", a.configPath, err)
	}

	printNextSteps(a, generated)
	return nil
}

// renderConfig writes the smallest config that expresses the answers. Anything
// not chosen is left out rather than written as a disabled block.
func renderConfig(a *setupAnswers) string {
	var b strings.Builder
	b.WriteString("# VaultS3 configuration, written by `vaults3 setup`.\n")
	b.WriteString("# Every setting not listed here keeps its built-in default; see\n")
	b.WriteString("# configs/vaults3.yaml in the repository for the full annotated set.\n\n")
	fmt.Fprintf(&b, "server:\n  address: %q\n  port: %d\n\n", a.address, a.port)
	fmt.Fprintf(&b, "storage:\n  data_dir: %q\n  metadata_dir: %q\n", a.dataDir, a.metadataDir)
	if len(a.buckets) > 0 {
		b.WriteString("  # Created at startup if they do not exist.\n  default_buckets:\n")
		for _, bucket := range a.buckets {
			fmt.Fprintf(&b, "    - %q\n", bucket)
		}
	}
	b.WriteString("\n")
	b.WriteString("# These are the dashboard and S3 credentials. Keep this file readable\n")
	b.WriteString("# by its owner only, or set VAULTS3_ACCESS_KEY / VAULTS3_SECRET_KEY\n")
	b.WriteString("# in the environment instead and delete these two lines.\n")
	fmt.Fprintf(&b, "auth:\n  admin_access_key: %q\n  admin_secret_key: %q\n\n", a.accessKey, a.secretKey)
	fmt.Fprintf(&b, "logging:\n  level: \"info\"\n  file_path: %q\n", a.accessLog)
	return b.String()
}

func printNextSteps(a *setupAnswers, generated bool) {
	fmt.Printf("\nSetup complete. Wrote %s\n\n", a.configPath)
	fmt.Printf("Start VaultS3:\n      ./vaults3 -config %s\n\n", a.configPath)
	fmt.Printf("Dashboard:\n      http://%s:%d/dashboard/\n\n", a.address, a.port)
	fmt.Printf("Access key:\n      %s\n\n", a.accessKey)
	fmt.Printf("Secret key:\n      %s\n\n", a.secretKey)
	if generated {
		fmt.Printf("The secret was generated for this installation and is stored in\n%s. It is not shown again.\n\n", a.configPath)
	}
}

func promptAnswers(a *setupAnswers) error {
	fmt.Println("VaultS3 setup. Press enter to accept the value in brackets.")
	fmt.Println()
	a.configPath = ask("Config file to write", a.configPath)
	a.dataDir = ask("Data directory", a.dataDir)
	a.metadataDir = ask("Metadata directory", a.metadataDir)
	a.accessLog = ask("Access log file", a.accessLog)
	a.address = ask("Listen address", a.address)
	for {
		portStr := ask("Port", strconv.Itoa(a.port))
		p, err := strconv.Atoi(portStr)
		if err == nil && p >= 1 && p <= 65535 {
			a.port = p
			break
		}
		fmt.Println("  That is not a port number between 1 and 65535.")
	}
	a.accessKey = ask("Admin access key", a.accessKey)
	fmt.Println("Admin secret key: leave empty to generate a strong one.")
	a.secretKey = ask("Admin secret key", a.secretKey)
	a.buckets = splitCommaList(ask("Buckets to create at startup (comma separated, optional)", strings.Join(a.buckets, ",")))
	fmt.Println()
	return nil
}

var setupIn = bufio.NewReader(os.Stdin)

func ask(question, def string) string {
	if def != "" {
		fmt.Printf("%s [%s]: ", question, def)
	} else {
		fmt.Printf("%s: ", question)
	}
	line, err := setupIn.ReadString('\n')
	if err != nil && line == "" {
		return def
	}
	answer := strings.TrimSpace(line)
	if answer == "" {
		return def
	}
	return answer
}

func askYesNo(question string, def bool) bool {
	hint := "y/N"
	if def {
		hint = "Y/n"
	}
	fmt.Printf("%s [%s]: ", question, hint)
	line, err := setupIn.ReadString('\n')
	if err != nil && line == "" {
		return def
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "":
		return def
	case "y", "yes":
		return true
	default:
		return false
	}
}

func stdinIsTerminal() bool {
	info, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

func splitCommaList(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func randomSecret(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
