package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Kodiqa-Solutions/VaultS3/internal/iam"
	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
)

// Per-bucket authorization for the console API (security assessment finding 14).
//
// The console used to authorize on one thing only: is this JWT valid, and for a
// short allowlist of routes, is the subject "admin". Everything else, every
// bucket and every object, was open to any authenticated subject. The S3 API
// evaluated IAM policies for exactly the same data, so a user with default-deny
// permissions could be refused a GET over S3 and then read, overwrite or delete
// the same object through the dashboard.
//
// That is a total bypass rather than intended public access, and it is reachable
// by anyone who can obtain any session: with OIDC auto_create_users, that is
// anyone with an account at the configured identity provider.
//
// So every console route that names a bucket now resolves the caller to an IAM
// identity and evaluates the equivalent S3 action against it, using the same
// evaluator the S3 path uses.

// consoleAction is the S3 action a console route corresponds to, and the
// resource it applies to.
type consoleAction struct {
	action string
	bucket string
	key    string
}

// resource renders the ARN the IAM evaluator matches against.
func (c consoleAction) resource() string {
	if c.key == "" {
		return "arn:aws:s3:::" + c.bucket
	}
	return "arn:aws:s3:::" + c.bucket + "/" + c.key
}

// consoleActionFor maps a console request to the S3 action it performs. It
// returns ok=false for routes that name no bucket, which are covered by the
// admin allowlist or by per-handler filtering instead.
//
// Unknown bucket sub-resources deliberately map to a bucket-level WRITE. A route
// nobody mapped is far more likely to be a mutation than a read, and the failure
// mode of guessing "write" is a denied request rather than an unauthorized one.
func consoleActionFor(method, rest string) (consoleAction, bool) {
	parts := strings.SplitN(rest, "/", 3)
	bucket := parts[0]
	if bucket == "" {
		return consoleAction{}, false
	}
	if len(parts) == 1 {
		switch method {
		case http.MethodGet:
			return consoleAction{action: "s3:ListBucket", bucket: bucket}, true
		case http.MethodDelete:
			return consoleAction{action: "s3:DeleteBucket", bucket: bucket}, true
		}
		return consoleAction{action: "s3:PutBucketPolicy", bucket: bucket}, true
	}

	sub := parts[1]
	key := ""
	if len(parts) == 3 {
		key = parts[2]
	}

	switch sub {
	case "objects":
		if key == "" {
			return consoleAction{action: "s3:ListBucket", bucket: bucket}, true
		}
		if method == http.MethodDelete {
			return consoleAction{action: "s3:DeleteObject", bucket: bucket, key: key}, true
		}
		return consoleAction{action: "s3:GetObject", bucket: bucket, key: key}, true
	case "search":
		// Filtering a folder reveals the same key names a listing does.
		return consoleAction{action: "s3:ListBucket", bucket: bucket}, true
	case "download":
		return consoleAction{action: "s3:GetObject", bucket: bucket, key: key}, true
	case "download-zip":
		// The zip streams many objects; the individual keys come from the query
		// string, so gate it at the bucket and let the handler read what it lists.
		return consoleAction{action: "s3:GetObject", bucket: bucket, key: "*"}, true
	case "upload":
		return consoleAction{action: "s3:PutObject", bucket: bucket, key: "*"}, true
	case "bulk-delete":
		return consoleAction{action: "s3:DeleteObject", bucket: bucket, key: "*"}, true
	case "versioning":
		if method == http.MethodGet {
			return consoleAction{action: "s3:GetBucketVersioning", bucket: bucket}, true
		}
		return consoleAction{action: "s3:PutBucketVersioning", bucket: bucket}, true
	case "policy":
		if method == http.MethodGet {
			return consoleAction{action: "s3:GetBucketPolicy", bucket: bucket}, true
		}
		return consoleAction{action: "s3:PutBucketPolicy", bucket: bucket}, true
	case "lifecycle", "cors", "encryption", "quota", "snapshots":
		if method == http.MethodGet {
			return consoleAction{action: "s3:GetBucketPolicy", bucket: bucket}, true
		}
		return consoleAction{action: "s3:PutBucketPolicy", bucket: bucket}, true
	}
	return consoleAction{action: "s3:PutBucketPolicy", bucket: bucket}, true
}

// authorizeConsoleBucket enforces the caller's IAM policies on a console route
// that names a bucket. Admin keeps its existing full access.
func (h *APIHandler) authorizeConsoleBucket(r *http.Request, rest string) error {
	user, err := h.authenticateUser(r)
	if err != nil {
		return err
	}
	if user == "admin" {
		h.s3Auth.ExternalAuth().NoteAdminBypass()
		return nil
	}
	act, ok := consoleActionFor(r.Method, rest)
	if !ok {
		return nil
	}
	return h.authorizeConsoleAction(r, user, act)
}

// authorizeConsoleAction decides one action for a non-admin subject, with the
// same IAM evaluation and external authorizer the route gate uses. The bulk
// routes call it once per key: their gate only proves the caller may act on
// SOME key in the bucket, and a Deny on one prefix is invisible to a check
// against the bucket wildcard.
func (h *APIHandler) authorizeConsoleAction(r *http.Request, user string, act consoleAction) error {
	if user == "admin" {
		return nil
	}
	allowed := h.allowsConsoleCtx(user, act, consoleConditionContext(r, user))
	ext := h.s3Auth.ExternalAuth()
	if ext == nil {
		if allowed {
			return nil
		}
		return fmt.Errorf("access denied: %s on %s", act.action, act.resource())
	}
	// Same shape as the S3 path: deny-only narrows what IAM allowed, so a refusal
	// here needs no webhook call; authoritative mode asks either way (issue #52).
	if !allowed && !ext.Authoritative() {
		return fmt.Errorf("access denied: %s on %s", act.action, act.resource())
	}
	permit, aerr := ext.Allow(iam.AuthRequest{
		User:     user,
		Action:   act.action,
		Resource: act.resource(),
		SourceIP: iam.SourceIPOf(r.RemoteAddr),
	})
	// As on the S3 path: the decision is authoritative, the error only explains a
	// denial. Checking the error first would defeat fail_open.
	if !permit {
		return &iam.DeniedError{Action: act.action, Resource: act.resource(), Err: aerr}
	}
	return nil
}

// consoleConditionContext is what IAM conditions see for a console request.
// The console used to evaluate with no context at all, so every Allow carrying
// a Condition failed closed and a policy that granted a bucket only from the
// office network granted nothing in the dashboard. aws:SourceIp is the TCP
// peer, never X-Forwarded-For, which any client can set to an allowed address.
func consoleConditionContext(r *http.Request, user string) map[string]string {
	ctx := map[string]string{
		"aws:SourceIp":        iam.SourceIPOf(r.RemoteAddr),
		"aws:SecureTransport": strconv.FormatBool(r.TLS != nil),
		"aws:CurrentTime":     time.Now().UTC().Format(time.RFC3339),
	}
	if user != "" {
		ctx["aws:username"] = user
	}
	if ua := r.UserAgent(); ua != "" {
		ctx["aws:UserAgent"] = ua
	}
	return ctx
}

// checkConsoleIP applies a user's AllowedCIDRs, and the server's global allow
// and block lists, to a console request the way the S3 path applies them to a
// signed one. The console never looked at them, so a user restricted to the
// office network over S3 could do the same things from anywhere through the
// dashboard. A user record that cannot be read is refused rather than treated
// as unrestricted.
func (h *APIHandler) checkConsoleIP(r *http.Request, user string) error {
	var cidrs []string
	u, err := h.store.GetIAMUser(user)
	if err == nil && u != nil {
		cidrs = u.AllowedCIDRs
	} else if h.iamUserMissing(user) != nil {
		return fmt.Errorf("could not read the restrictions of user %q", user)
	}
	ip := iam.SourceIPOf(r.RemoteAddr)
	if h.s3Auth != nil {
		return h.s3Auth.CheckIPAccess(&iam.Identity{UserID: user, AllowedCIDRs: cidrs}, ip)
	}
	return iam.CheckIP(ip, cidrs, nil)
}

// iamUserMissing tells a user that does not exist (nil) from a store that
// could not answer (an error). A session for a user with no record has no
// restrictions to apply and no policies to grant anything, so it is not an
// error here.
func (h *APIHandler) iamUserMissing(user string) error {
	users, err := h.store.ListIAMUsers()
	if err != nil {
		return err
	}
	for _, u := range users {
		if u.Name == user {
			return fmt.Errorf("user %q exists but could not be read", user)
		}
	}
	return nil
}

// allowsConsole reports whether a subject's IAM policies permit an action with
// no request context. Kept for callers with no request to hand.
func (h *APIHandler) allowsConsole(user string, act consoleAction) bool {
	return h.allowsConsoleCtx(user, act, nil)
}

// allowsConsoleCtx reports whether a subject's IAM policies permit an action. A
// subject with no policies is denied, which is the same default the S3 path
// applies, and a policy lookup that fails is denied too: a store error must not
// widen access.
func (h *APIHandler) allowsConsoleCtx(user string, act consoleAction, ctx map[string]string) bool {
	if h.store == nil {
		return false
	}
	policies, err := h.store.GetUserPolicies(user)
	if err != nil {
		return false
	}
	converted := make([]iam.Policy, 0, len(policies))
	for _, p := range policies {
		var pol iam.Policy
		if err := json.Unmarshal([]byte(p.Document), &pol); err != nil {
			// An unreadable policy is not an absent one. Refuse rather than
			// evaluate a partial policy set.
			return false
		}
		converted = append(converted, pol)
	}
	return iam.EvaluateWithContext(converted, act.action, act.resource(), ctx)
}

// visibleBucketSet is visibleBuckets over the whole bucket list, as a set, for
// the cross-bucket read routes a non-admin may call. It answers nil for admin,
// meaning no filtering.
func (h *APIHandler) visibleBucketSet(r *http.Request, buckets []metadata.BucketInfo) map[string]bool {
	if h.isAdminUser(r) {
		return nil
	}
	names := make([]string, 0, len(buckets))
	for _, b := range buckets {
		names = append(names, b.Name)
	}
	set := make(map[string]bool, len(names))
	for _, n := range h.visibleBuckets(r, names) {
		set[n] = true
	}
	return set
}

// visibleBuckets filters a bucket list to those the caller may list. Admin sees
// everything; anyone else sees only what their policies allow, so the dashboard
// stops advertising the existence of buckets a user cannot open.
//
// This is deliberately IAM-only. An external authorizer is consulted when the
// user acts on a bucket, not when the list is drawn: asking it once per bucket
// would turn one dashboard load into an unbounded fan-out of webhook calls. The
// cost is that a bucket the webhook would refuse can still appear in the list,
// which leaks its name and nothing else, since opening it is still refused.
func (h *APIHandler) visibleBuckets(r *http.Request, names []string) []string {
	user, err := h.authenticateUser(r)
	if err != nil {
		return nil
	}
	if user == "admin" {
		return names
	}
	ctx := consoleConditionContext(r, user)
	out := make([]string, 0, len(names))
	for _, name := range names {
		if h.allowsConsoleCtx(user, consoleAction{action: "s3:ListBucket", bucket: name}, ctx) {
			out = append(out, name)
		}
	}
	return out
}
