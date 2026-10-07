// Package selfupdate checks GitHub Releases for newer VaultS3 versions and,
// optionally, downloads + verifies + installs them in place. It only ever
// replaces the running binary — object data, metadata, and config are never
// touched. Installation is checksum-verified (it refuses to run an unverified
// download) and opt-in.
package selfupdate

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const defaultRepo = "Kodiqa-Solutions/VaultS3"

// Status is the last known update state, surfaced to the dashboard via the API.
type Status struct {
	Current         string `json:"current"`
	Latest          string `json:"latest,omitempty"`
	UpdateAvailable bool   `json:"updateAvailable"`
	CheckedAt       int64  `json:"checkedAt,omitempty"`
	Error           string `json:"error,omitempty"`
}

// Updater checks for and applies updates.
type Updater struct {
	repo    string
	apiBase string // GitHub API base; overridable in tests
	current string
	client  *http.Client
	mu      sync.RWMutex
	status  Status
}

// New creates an updater for the given running version (e.g. "v4.2.6").
func New(currentVersion string) *Updater {
	return &Updater{
		repo:    defaultRepo,
		apiBase: "https://api.github.com",
		current: currentVersion,
		client:  &http.Client{Timeout: 30 * time.Second},
		status:  Status{Current: currentVersion},
	}
}

// LastStatus returns the most recent check result.
func (u *Updater) LastStatus() Status {
	u.mu.RLock()
	defer u.mu.RUnlock()
	return u.status
}

type ghRelease struct {
	TagName string `json:"tag_name"`
	Assets  []struct {
		Name string `json:"name"`
		URL  string `json:"browser_download_url"`
	} `json:"assets"`
}

func (u *Updater) fetchLatest(ctx context.Context) (*ghRelease, error) {
	url := fmt.Sprintf("%s/repos/%s/releases/latest", u.apiBase, u.repo)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := u.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("github releases API returned %d", resp.StatusCode)
	}
	var rel ghRelease
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&rel); err != nil {
		return nil, err
	}
	return &rel, nil
}

// Check queries the latest release and records the result. It never errors out
// of band — failures are stored in Status.Error so the caller can keep polling.
func (u *Updater) Check(ctx context.Context) Status {
	st := Status{Current: u.current, CheckedAt: time.Now().Unix()}
	rel, err := u.fetchLatest(ctx)
	if err != nil {
		st.Error = err.Error()
	} else {
		st.Latest = rel.TagName
		st.UpdateAvailable = compareVersions(u.current, rel.TagName) < 0
	}
	u.mu.Lock()
	u.status = st
	u.mu.Unlock()
	return st
}

// Apply downloads the latest release for this platform, verifies its checksum,
// installs it over the running binary, and re-execs into the new version. It
// returns an error (and changes nothing) if anything is off; on success it does
// not return (the process is replaced).
func (u *Updater) Apply(ctx context.Context, allowMajor bool) error {
	if runtime.GOOS == "windows" {
		return fmt.Errorf("self-update is not supported on Windows; update manually or via Docker")
	}
	rel, err := u.fetchLatest(ctx)
	if err != nil {
		return err
	}
	if compareVersions(u.current, rel.TagName) >= 0 {
		return fmt.Errorf("already up to date (%s)", u.current)
	}
	if !allowMajor && majorOf(u.current) != majorOf(rel.TagName) {
		return fmt.Errorf("refusing to auto-cross a major version (%s → %s); update manually", u.current, rel.TagName)
	}

	// A checksum file downloaded from the same release proves only that the
	// download is intact, not who published it: anyone able to publish a release
	// could have shipped a binary every node with apply on would run. The
	// checksums must carry a signature from the release key built into this
	// binary, and without a key nothing is installed.
	pub, err := releasePublicKey()
	if err != nil {
		return err
	}

	want := assetBaseName()
	var assetURL, checksumsURL, signatureURL string
	for _, a := range rel.Assets {
		switch a.Name {
		case want:
			assetURL = a.URL
		case "checksums.txt":
			checksumsURL = a.URL
		case "checksums.txt.sig":
			signatureURL = a.URL
		}
	}
	if signatureURL == "" {
		return fmt.Errorf("release has no checksums.txt.sig; refusing to install an unsigned binary")
	}
	if assetURL == "" {
		return fmt.Errorf("no release asset for %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	if checksumsURL == "" {
		return fmt.Errorf("release has no checksums.txt; refusing to install an unverified binary")
	}

	checksums, err := u.download(ctx, checksumsURL)
	if err != nil {
		return fmt.Errorf("download checksums: %w", err)
	}
	sig, err := u.download(ctx, signatureURL)
	if err != nil {
		return fmt.Errorf("download checksum signature: %w", err)
	}
	if err := verifyChecksums(pub, checksums, sig); err != nil {
		return err
	}
	wantSum, err := checksumFor(string(checksums), want)
	if err != nil {
		return err
	}

	archive, err := u.download(ctx, assetURL)
	if err != nil {
		return fmt.Errorf("download release: %w", err)
	}
	gotSum := sha256.Sum256(archive)
	if hex.EncodeToString(gotSum[:]) != wantSum {
		return fmt.Errorf("checksum mismatch — refusing to install (expected %s, got %x)", wantSum, gotSum)
	}

	bin, err := extractBinary(archive, serverBinaryName())
	if err != nil {
		return err
	}

	slog.Info("self-update: verified new binary, installing and restarting", "version", rel.TagName)
	return replaceAndRestart(bin)
}

func (u *Updater) download(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := u.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download %s returned %d", url, resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 500<<20)) // cap 500MB
}

// IsDocker reports whether we're running inside a container (where self-update is
// pointless — the change is lost on restart; use Watchtower instead).
func IsDocker() bool {
	_, err := os.Stat("/.dockerenv")
	return err == nil
}

func assetBaseName() string {
	return fmt.Sprintf("vaults3-%s-%s.tar.gz", runtime.GOOS, runtime.GOARCH)
}

func serverBinaryName() string {
	n := fmt.Sprintf("vaults3-%s-%s", runtime.GOOS, runtime.GOARCH)
	if runtime.GOOS == "windows" {
		n += ".exe"
	}
	return n
}

// checksumFor finds the sha256 hash for a filename in `sha256sum`-format text
// ("<hex>␠␠<name>").
func checksumFor(checksums, name string) (string, error) {
	for _, line := range strings.Split(checksums, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[1] == name {
			return fields[0], nil
		}
	}
	return "", fmt.Errorf("no checksum entry for %s", name)
}

// extractBinary pulls a single file out of a .tar.gz archive.
func extractBinary(targz []byte, name string) ([]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(targz))
	if err != nil {
		return nil, fmt.Errorf("open gzip: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read tar: %w", err)
		}
		if filepath.Base(hdr.Name) == name {
			return io.ReadAll(io.LimitReader(tr, 500<<20))
		}
	}
	return nil, fmt.Errorf("binary %s not found in archive", name)
}

// replaceAndRestart atomically swaps the running executable and re-execs it.
func replaceAndRestart(newBin []byte) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	tmp := exe + ".update.tmp"
	if err := os.WriteFile(tmp, newBin, 0755); err != nil {
		return fmt.Errorf("write new binary: %w", err)
	}
	// Keep the running binary, so a new one that does not start can be put back
	// by hand. It used to be overwritten in place with no copy left.
	if old, err := os.ReadFile(exe); err == nil {
		if err := os.WriteFile(exe+".prev", old, 0755); err != nil {
			os.Remove(tmp)
			return fmt.Errorf("keep the previous binary: %w", err)
		}
	}
	// On Unix, renaming over the running executable is safe — the running
	// process keeps its open inode; new starts use the new file.
	if err := os.Rename(tmp, exe); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("install new binary: %w", err)
	}
	// Replace the process image with the new binary, preserving args and env.
	return syscall.Exec(exe, os.Args, os.Environ())
}

// compareVersions returns -1 if a<b, 0 if equal, 1 if a>b. Non-semver inputs
// (e.g. "dev") sort lowest so a dev build is always "older" than any release.
func compareVersions(a, b string) int {
	pa, oka := parseVersion(a)
	pb, okb := parseVersion(b)
	switch {
	case !oka && !okb:
		return 0
	case !oka:
		return -1
	case !okb:
		return 1
	}
	for i := 0; i < 3; i++ {
		if pa[i] != pb[i] {
			if pa[i] < pb[i] {
				return -1
			}
			return 1
		}
	}
	return 0
}

func parseVersion(v string) ([3]int, bool) {
	var out [3]int
	v = strings.TrimSpace(v)
	v = strings.TrimPrefix(v, "v")
	if i := strings.IndexAny(v, "-+"); i >= 0 { // drop pre-release/build metadata
		v = v[:i]
	}
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return out, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			return out, false
		}
		out[i] = n
	}
	return out, true
}

func majorOf(v string) int {
	if p, ok := parseVersion(v); ok {
		return p[0]
	}
	return -1
}

// releaseSigningKey is the base64 Ed25519 public key that release checksums
// are signed with. It is set at build time by the release workflow
// (-X .../internal/selfupdate.releaseSigningKey=...). A build without it cannot
// verify a release and refuses to apply one.
var releaseSigningKey = ""

func releasePublicKey() (ed25519.PublicKey, error) {
	if releaseSigningKey == "" {
		return nil, fmt.Errorf("this build has no release signing key, so it cannot verify an update; " +
			"update by replacing the binary or the image")
	}
	k, err := base64.StdEncoding.DecodeString(releaseSigningKey)
	if err != nil || len(k) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("the release signing key built into this binary is malformed")
	}
	return ed25519.PublicKey(k), nil
}

// verifyChecksums checks checksums.txt against its detached signature, which is
// the raw or base64 Ed25519 signature over the file's exact bytes.
func verifyChecksums(pub ed25519.PublicKey, checksums, sig []byte) error {
	raw := sig
	if len(raw) != ed25519.SignatureSize {
		d, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(sig)))
		if err != nil {
			return fmt.Errorf("checksum signature is malformed; refusing to install")
		}
		raw = d
	}
	if len(raw) != ed25519.SignatureSize || !ed25519.Verify(pub, checksums, raw) {
		return fmt.Errorf("checksum signature does not verify against the release key; refusing to install")
	}
	return nil
}
