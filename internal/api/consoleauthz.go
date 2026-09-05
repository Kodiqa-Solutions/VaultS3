package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/Kodiqa-Solutions/VaultS3/internal/iam"
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
		return nil
	}
	act, ok := consoleActionFor(r.Method, rest)
	if !ok {
		return nil
	}
	allowed := h.allowsConsole(user, act)
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

// allowsConsole reports whether a subject's IAM policies permit an action. A
// subject with no policies is denied, which is the same default the S3 path
// applies, and a policy lookup that fails is denied too: a store error must not
// widen access.
func (h *APIHandler) allowsConsole(user string, act consoleAction) bool {
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
	return iam.Evaluate(converted, act.action, act.resource())
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
	out := make([]string, 0, len(names))
	for _, name := range names {
		if h.allowsConsole(user, consoleAction{action: "s3:ListBucket", bucket: name}) {
			out = append(out, name)
		}
	}
	return out
}
