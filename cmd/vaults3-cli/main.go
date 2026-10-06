package main

import (
	"fmt"
	"os"
)

var version = "dev"

var (
	endpoint  string
	accessKey string
	secretKey string
	region    string
)

func init() {
	endpoint = envOrDefault("VAULTS3_ENDPOINT", "http://localhost:9000")
	accessKey = envOrDefault("VAULTS3_ACCESS_KEY", "")
	secretKey = envOrDefault("VAULTS3_SECRET_KEY", "")
	region = envOrDefault("VAULTS3_REGION", "us-east-1")
}

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	// Parse global flags before subcommand
	args := os.Args[1:]
	for len(args) > 0 && len(args[0]) > 0 && args[0][0] == '-' {
		switch args[0] {
		case "--endpoint":
			if len(args) < 2 {
				fatal("--endpoint requires a value")
			}
			endpoint = args[1]
			args = args[2:]
		case "--access-key":
			if len(args) < 2 {
				fatal("--access-key requires a value")
			}
			accessKey = args[1]
			args = args[2:]
		case "--secret-key":
			if len(args) < 2 {
				fatal("--secret-key requires a value")
			}
			secretKey = args[1]
			args = args[2:]
		case "--region":
			if len(args) < 2 {
				fatal("--region requires a value")
			}
			region = args[1]
			args = args[2:]
		case "--version", "-v":
			fmt.Printf("vaults3-cli %s\n", version)
			os.Exit(0)
		case "--help", "-h":
			printUsage()
			os.Exit(0)
		default:
			fatal("unknown flag: " + args[0])
		}
	}

	if len(args) == 0 {
		printUsage()
		os.Exit(1)
	}

	cmd := args[0]
	cmdArgs := args[1:]

	switch cmd {
	case "info":
		runInfo(cmdArgs)
	case "bucket":
		runBucket(cmdArgs)
	case "object":
		runObject(cmdArgs)
	case "user":
		runUser(cmdArgs)
	case "replication":
		runReplication(cmdArgs)
	case "cluster":
		runCluster(cmdArgs)
	case "storage":
		runStorage(cmdArgs)
	case "mount":
		runMount(cmdArgs)
	case "umount":
		runUmount(cmdArgs)
	case "version":
		fmt.Printf("vaults3-cli %s\n", version)
	case "help":
		printUsage()
	default:
		fmt.Fprintf(os.Stderr, "Unknown command: %s\n", cmd)
		printUsage()
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Println(`Usage: vaults3-cli [flags] <command> <subcommand> [args]

Global Flags:
  --endpoint <url>     VaultS3 endpoint (default: $VAULTS3_ENDPOINT or http://localhost:9000)
  --access-key <key>   Access key (default: $VAULTS3_ACCESS_KEY)
  --secret-key <key>   Secret key (default: $VAULTS3_SECRET_KEY)
  --region <region>    Region (default: $VAULTS3_REGION or us-east-1)
  --version, -v        Show version

Commands:
  info                 Server version and storage capacity (used/free/total)
  bucket               Bucket operations (list, create, delete, info)
  object               Object operations (ls, put, get, rm, cp, presign)
  user                 IAM user operations (list, create, delete, attach-policy)
  replication          Replication operations (status, queue)
  cluster              Cluster ops (status, join, leave, drain, undrain, rebalance, decommission)
  storage              Storage maintenance (reclaim orphaned data files)
  mount                Mount a bucket as a local filesystem (FUSE)
  umount               Unmount a FUSE mountpoint
  version              Show version
  help                 Show this help`)
}

// exit is os.Exit, held in a variable so a test can stop a failing command
// without ending the test binary.
var exit = os.Exit

func fatal(msg string) {
	fmt.Fprintf(os.Stderr, "Error: %s\n", msg)
	exit(1)
}

func requireCreds() {
	if accessKey == "" || secretKey == "" {
		fatal("access key and secret key are required. Set VAULTS3_ACCESS_KEY/VAULTS3_SECRET_KEY or use --access-key/--secret-key")
	}
}
