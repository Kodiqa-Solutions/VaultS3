package iam

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// External authorization delegates the access decision for a request to an HTTP
// endpoint the operator runs, so entitlements that live in someone else's system
// can gate VaultS3 without being copied into IAM policies.
//
// This was documented as a shipped feature from 2026-02-28 but was never wired
// to anything: the type existed, nothing called it, and no config key could turn
// it on (issue #52). What follows is the wiring, and the three decisions that
// made it safe enough to enable on the request path.
//
// 1. Deny-only by default. IAM must allow AND the webhook must allow. The
//    webhook can narrow access, never widen it, so an endpoint that is spoofed,
//    compromised, or simply wrong costs availability rather than every object in
//    the cluster. Authoritative mode (below) is the opt-in for operators who
//    genuinely want the webhook to grant, and even then an explicit Deny in an
//    IAM policy still wins.
//
// 2. Fail closed. A webhook that errors, times out, or answers with anything but
//    a 200 and a parseable body denies. That means the endpoint going down takes
//    the storage down with it, which is the honest cost of putting a network hop
//    in front of every request. FailOpen inverts it for operators who would
//    rather serve than block.
//
// 3. Cache the decision. Without a cache every GET becomes an HTTP round trip
//    and the TTFB work from issue #38 is undone. Entries are keyed on everything
//    that was sent to the webhook, so a decision is never reused for a request
//    that differed in any input the webhook was given.
//
// Note on SSRF: this URL is NOT run through the endpoint validator that bucket
// notification targets use. That guard exists because a bucket owner sets a
// notification URL over the S3 API, so it is attacker-controlled input. This URL
// comes from the operator's own config file, and an operator can already point
// the process anywhere. Worse, that validator rejects 10/8, 172.16/12 and
// 192.168/16, which is exactly where a private authorization service lives, so
// reusing it would block the normal deployment while stopping nothing.

// ExternalAuthConfig holds configuration for the external authorization webhook.
type ExternalAuthConfig struct {
	Enabled bool          `json:"enabled" yaml:"enabled"`
	URL     string        `json:"url" yaml:"url"`
	Timeout time.Duration `json:"timeout" yaml:"timeout"`
	// CacheTTL is how long one decision is reused. Zero disables caching, which
	// means an HTTP round trip on every authorized request.
	CacheTTL time.Duration `json:"cache_ttl" yaml:"cache_ttl"`
	// FailOpen allows the request when the webhook cannot be reached or does not
	// answer usefully. Off by default: an authorizer that cannot answer has not
	// authorized anything.
	FailOpen bool `json:"fail_open" yaml:"fail_open"`
	// Authoritative lets an allow from the webhook grant access that IAM alone
	// would not. An explicit Deny in an IAM policy still refuses.
	Authoritative bool `json:"authoritative" yaml:"authoritative"`
	// Token, when set, is sent as "Authorization: Bearer <token>" so the endpoint
	// can tell VaultS3 apart from anything else that can reach it.
	Token string `json:"token" yaml:"token"`
}

// maxCacheEntries bounds the decision cache. Reaching it clears the map rather
// than evicting one entry: the cost is an occasional burst of re-asks, and the
// benefit is that the bound holds without a second data structure to get wrong.
// The FUSE caches in this codebase are bounded for the same reason.
const maxCacheEntries = 20000

// ExternalAuth calls an external webhook for access decisions.
type ExternalAuth struct {
	cfg    ExternalAuthConfig
	client *http.Client

	mu    sync.Mutex
	cache map[string]cachedDecision
}

type cachedDecision struct {
	allow   bool
	expires time.Time
}

// AuthRequest is sent to the external auth webhook.
type AuthRequest struct {
	AccessKey string `json:"accessKey"`
	User      string `json:"user,omitempty"`
	Action    string `json:"action"`
	Resource  string `json:"resource"`
	SourceIP  string `json:"sourceIP,omitempty"`
}

// AuthResponse is expected from the external auth webhook.
type AuthResponse struct {
	Allow  bool   `json:"allow"`
	Reason string `json:"reason,omitempty"`
}

// NewExternalAuth creates an external authorizer. It returns an error rather
// than a half-configured client, so a typo in the URL stops the server at
// startup instead of failing every request once traffic arrives.
func NewExternalAuth(cfg ExternalAuthConfig) (*ExternalAuth, error) {
	if cfg.URL == "" {
		return nil, fmt.Errorf("external auth: url is required when enabled")
	}
	u, err := url.Parse(cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("external auth: invalid url %q: %w", cfg.URL, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("external auth: url must be http or https, got %q", u.Scheme)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("external auth: url %q has no host", cfg.URL)
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 2 * time.Second
	}
	return &ExternalAuth{
		cfg:    cfg,
		client: &http.Client{Timeout: cfg.Timeout},
		cache:  make(map[string]cachedDecision),
	}, nil
}

// Authoritative reports whether an allow from this webhook may grant access that
// IAM alone would refuse.
func (e *ExternalAuth) Authoritative() bool { return e != nil && e.cfg.Authoritative }

// Endpoint returns the configured URL, for diagnostics.
func (e *ExternalAuth) Endpoint() string {
	if e == nil {
		return ""
	}
	return e.cfg.URL
}

// Allow asks the webhook whether one request may proceed. A nil receiver allows,
// so a caller that was never configured with an authorizer behaves exactly as it
// did before this feature existed.
func (e *ExternalAuth) Allow(req AuthRequest) (bool, error) {
	if e == nil {
		return true, nil
	}
	key := req.AccessKey + "\x00" + req.User + "\x00" + req.Action + "\x00" + req.Resource + "\x00" + req.SourceIP
	if d, ok := e.lookup(key); ok {
		return d, nil
	}
	allow, err := e.ask(req)
	if err != nil {
		// A failure is never cached. Caching it would turn one slow moment at the
		// endpoint into a guaranteed TTL of refusals after it recovered.
		if e.cfg.FailOpen {
			slog.Warn("external authorization failed open: request ALLOWED without a decision",
				"action", req.Action, "resource", req.Resource, "err", err)
			return true, err
		}
		return false, err
	}
	e.store(key, allow)
	return allow, nil
}

func (e *ExternalAuth) lookup(key string) (bool, bool) {
	if e.cfg.CacheTTL <= 0 {
		return false, false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	d, ok := e.cache[key]
	if !ok || time.Now().After(d.expires) {
		return false, false
	}
	return d.allow, true
}

func (e *ExternalAuth) store(key string, allow bool) {
	if e.cfg.CacheTTL <= 0 {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.cache) >= maxCacheEntries {
		e.cache = make(map[string]cachedDecision, maxCacheEntries/2)
	}
	e.cache[key] = cachedDecision{allow: allow, expires: time.Now().Add(e.cfg.CacheTTL)}
}

// ask performs one webhook call.
func (e *ExternalAuth) ask(req AuthRequest) (bool, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return false, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), e.cfg.Timeout)
	defer cancel()

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, e.cfg.URL, bytes.NewReader(body))
	if err != nil {
		return false, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if e.cfg.Token != "" {
		httpReq.Header.Set("Authorization", "Bearer "+e.cfg.Token)
	}

	resp, err := e.client.Do(httpReq)
	if err != nil {
		return false, fmt.Errorf("external auth webhook error: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("external auth webhook returned %d", resp.StatusCode)
	}

	// Bounded read. The response is two small fields, and an authorizer that
	// streams a gigabyte at the request path would otherwise be able to exhaust
	// memory on every request it answers.
	limited := io.LimitReader(resp.Body, 1<<20)
	var authResp AuthResponse
	if err := json.NewDecoder(limited).Decode(&authResp); err != nil {
		return false, fmt.Errorf("external auth webhook invalid response: %w", err)
	}
	return authResp.Allow, nil
}

// DeniedError describes a refusal that came from the webhook rather than from a
// policy, so the S3 layer can say which authority refused without leaking the
// endpoint's internals to the client.
type DeniedError struct {
	Action   string
	Resource string
	Err      error
}

func (d *DeniedError) Error() string {
	if d.Err != nil {
		return fmt.Sprintf("access denied: %s on %s (external authorizer unavailable)", d.Action, d.Resource)
	}
	return fmt.Sprintf("access denied: %s on %s (refused by the external authorizer)", d.Action, d.Resource)
}

func (d *DeniedError) Unwrap() error { return d.Err }

// SourceIPOf strips the port from a RemoteAddr-style host:port pair, matching
// the value IAM conditions already see as aws:SourceIp.
func SourceIPOf(remoteAddr string) string {
	if i := strings.LastIndex(remoteAddr, ":"); i > 0 && !strings.Contains(remoteAddr[i+1:], "]") {
		return strings.Trim(remoteAddr[:i], "[]")
	}
	return remoteAddr
}
