package main

import (
	"crypto/tls"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/Kodiqa-Solutions/VaultS3/internal/config"
)

// runHealthcheck probes this server's own /health endpoint and exits 0 when it
// answers.
//
// It exists so a container image does not need a shell or an HTTP client just to
// report liveness. The image used `wget --spider` from busybox, which pulled a
// CVE into every deployment for a check the server can perform on itself, and
// which no upstream fix was coming for. It also makes a distroless image
// possible, where there is no shell to run a probe from at all.
//
// It reads the same config the server does, so it follows a changed port, a
// reverse-proxy base path and TLS rather than assuming the defaults.
func runHealthcheck(args []string) int {
	configPath := defaultConfigPath
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "-config" || args[i] == "--config":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "healthcheck: -config needs a path")
				return 2
			}
			configPath = args[i+1]
			i++
		case strings.HasPrefix(args[i], "-config="), strings.HasPrefix(args[i], "--config="):
			configPath = args[i][strings.Index(args[i], "=")+1:]
		case args[i] == "-h" || args[i] == "--help":
			fmt.Println("Usage: vaults3 healthcheck [-config <path>]\n\n" +
				"Probes this server's own /health endpoint. Exits 0 when healthy,\n" +
				"1 when it is not, and 2 on a usage error. Intended for container\n" +
				"health checks, so no shell or HTTP client is needed in the image.")
			return 0
		default:
			fmt.Fprintf(os.Stderr, "healthcheck: unknown argument %q\n", args[i])
			return 2
		}
	}

	// Defaults are fine here: a server started without a config file is still
	// serving, and the check should follow it rather than refuse.
	cfg, _, err := config.LoadOrDefaults(configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "healthcheck: read config: %v\n", err)
		return 2
	}

	scheme := "http"
	client := &http.Client{Timeout: 4 * time.Second}
	if cfg.Server.TLS.Enabled {
		scheme = "https"
		// The probe connects to the loopback address, so the certificate will not
		// match and is not the thing being verified. Liveness is.
		client.Transport = &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}
	}

	base := strings.TrimSuffix(cfg.Server.BasePath, "/")
	url := fmt.Sprintf("%s://127.0.0.1:%d%s/health", scheme, cfg.Server.Port, base)

	resp, err := client.Get(url)
	if err != nil {
		fmt.Fprintf(os.Stderr, "healthcheck: %v\n", err)
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "healthcheck: %s returned %d\n", url, resp.StatusCode)
		return 1
	}
	return 0
}
