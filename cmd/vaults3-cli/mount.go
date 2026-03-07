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
	cacheSizeMB := 64
	metadataTTLSecs := 5

	// Parse optional flags
	for len(args) > 0 && strings.HasPrefix(args[0], "--") {
		switch {
		case args[0] == "--cache-size" && len(args) >= 2:
			n, err := strconv.Atoi(args[1])
			if err != nil || n < 0 {
				fatal("--cache-size must be a non-negative integer (MB)")
			}
			cacheSizeMB = n
			args = args[2:]
		case args[0] == "--metadata-ttl" && len(args) >= 2:
			n, err := strconv.Atoi(args[1])
			if err != nil || n < 0 {
				fatal("--metadata-ttl must be a non-negative integer (seconds)")
			}
			metadataTTLSecs = n
			args = args[2:]
		default:
			break
		}
	}

	if len(args) < 2 {
		fmt.Println(`Usage: vaults3-cli mount [options] <bucket> <mountpoint>

Mount a VaultS3 bucket as a local filesystem directory.

Options:
  --cache-size <MB>     Block cache size in MB (default: 64, 0=disabled)
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

func runUmount(args []string) {
	if len(args) < 1 {
		fmt.Println("Usage: vaults3-cli umount <mountpoint>")
		os.Exit(1)
	}
	// On macOS/Linux, use fusermount or umount
	fmt.Printf("To unmount, run: fusermount -u %s\n", args[0])
}
