//go:build !windows

package main

import (
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"

	vfuse "github.com/Kodiqa-Solutions/VaultS3/internal/fuse"
)

func runMount(args []string) {
	cacheSizeMB, metadataTTLSecs, args, err := parseMountFlags(args)
	if err != nil {
		fatal(err.Error())
	}

	if len(args) < 2 {
		fmt.Println(`Usage: vaults3-cli mount [options] <bucket> <mountpoint>

Mount a VaultS3 bucket as a local filesystem directory.

Options:
  --cache-size <MB>     Block cache size in MB (default: 64)
  --metadata-ttl <s>    Metadata cache TTL in seconds (default: 5)

Examples:
  vaults3-cli mount my-bucket /mnt/vaults3
  vaults3-cli mount --cache-size 128 my-bucket ./mnt`)
		os.Exit(1)
	}

	requireCreds()

	bucket := args[0]
	mountpoint := args[1]

	// Create mountpoint if it doesn't exist
	if err := os.MkdirAll(mountpoint, 0755); err != nil {
		fatal(fmt.Sprintf("create mountpoint: %v", err))
	}

	cfg := vfuse.MountConfig{
		Endpoint:        endpoint,
		AccessKey:       accessKey,
		SecretKey:       secretKey,
		Bucket:          bucket,
		Region:          region,
		CacheSizeMB:     cacheSizeMB,
		MetadataTTLSecs: metadataTTLSecs,
	}

	fmt.Printf("Mounting %s at %s (endpoint: %s)\n", bucket, mountpoint, endpoint)
	fmt.Println("Press Ctrl+C to unmount")

	server, err := vfuse.Mount(mountpoint, cfg)
	if err != nil {
		fatal(fmt.Sprintf("mount failed: %v", err))
	}

	// Handle Ctrl+C for clean unmount
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-sigCh
		fmt.Println("\nUnmounting...")
		server.Unmount()
	}()

	server.Wait()
	fmt.Println("Unmounted")
}

// parseMountFlags reads the options in front of <bucket> <mountpoint>. Every
// flag that starts the argument list is consumed or refused. The loop used to
// end on "break", which only leaves the switch, so an unknown flag, or a flag
// given last with no value, kept the loop spinning at full CPU forever.
func parseMountFlags(args []string) (cacheSizeMB, metadataTTLSecs int, rest []string, err error) {
	cacheSizeMB, metadataTTLSecs = 64, 5
	for len(args) > 0 && strings.HasPrefix(args[0], "--") {
		name, value, inline := strings.Cut(args[0], "=")
		if name != "--cache-size" && name != "--metadata-ttl" {
			return 0, 0, nil, fmt.Errorf("unknown mount option %s (options: --cache-size <MB>, --metadata-ttl <seconds>)", name)
		}
		consumed := 1
		if !inline {
			if len(args) < 2 {
				return 0, 0, nil, fmt.Errorf("%s needs a value", name)
			}
			value, consumed = args[1], 2
		}
		n, convErr := strconv.Atoi(value)
		switch {
		case name == "--cache-size" && (convErr != nil || n < 0):
			return 0, 0, nil, fmt.Errorf("--cache-size must be a non-negative integer (MB)")
		case name == "--metadata-ttl" && (convErr != nil || n < 0):
			return 0, 0, nil, fmt.Errorf("--metadata-ttl must be a non-negative integer (seconds)")
		case name == "--cache-size":
			cacheSizeMB = n
		default:
			metadataTTLSecs = n
		}
		args = args[consumed:]
	}
	return cacheSizeMB, metadataTTLSecs, args, nil
}

func runUmount(args []string) {
	if len(args) < 1 {
		fmt.Println("Usage: vaults3-cli umount <mountpoint>")
		os.Exit(1)
	}
	// On macOS/Linux, use fusermount or umount
	fmt.Printf("To unmount, run: fusermount -u %s\n", args[0])
}
