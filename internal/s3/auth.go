package s3

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Kodiqa-Solutions/VaultS3/internal/iam"
	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
)

// Authenticator validates S3 Signature V4 requests.
type Authenticator struct {
	// adminMu guards the admin pair, which the dashboard can change while
	// requests are being authenticated.
	adminMu         sync.RWMutex
	adminAccessKey  string
	adminSecretKey  string
	store           metadata.StoreAPI
	globalAllowCIDR []string
	globalBlockCIDR []string
	basePath        string // reverse-proxy subpath the client signed but a path-stripping proxy removed (issue #36)
	trustForwarded  bool   // honor the client-supplied X-Forwarded-Prefix when basePath is unset
	external        *iam.ExternalAuth
}

// SetExternalAuth attaches an external authorizer consulted on every non-admin
// authorization decision. Nil leaves authorization exactly as it was (issue #52).
func (a *Authenticator) SetExternalAuth(e *iam.ExternalAuth) { a.external = e }

// ExternalAuth returns the configured external authorizer, or nil. The console
// path consults the same instance so its cache and its mode are shared with the
// S3 path rather than duplicated.
func (a *Authenticator) ExternalAuth() *iam.ExternalAuth {
	if a == nil {
		return nil
	}
	return a.external
}

// SetBasePath configures the reverse-proxy subpath (e.g. "/vaults3") under which
// clients reach the S3 API. Behind such a proxy the client signs the URI with the
// prefix but the proxy strips it, so SigV4 verification must add it back to match
// (issue #36). trustForwarded additionally allows the subpath to come from the
// (client-supplied) X-Forwarded-Prefix header when base is empty — off by default.
// Empty base + untrusted header = signature verification unchanged.
func (a *Authenticator) SetBasePath(p string, trustForwarded bool) {
	a.basePath = normalizeBasePrefix(p)
	a.trustForwarded = trustForwarded
}

// canonicalBasePrefix returns the subpath to prepend to r.URL.Path when rebuilding
// the canonical URI the client signed. Configured base_path wins; otherwise the
// proxy's X-Forwarded-Prefix header, but ONLY when trust_forwarded_prefix is set
// (the header is client-supplied). "" when not behind a subpath, so the canonical
// request is then byte-for-byte identical to before.
func (a *Authenticator) canonicalBasePrefix(r *http.Request) string {
	if a.basePath != "" {
		return a.basePath
	}
	if a.trustForwarded {
		return normalizeBasePrefix(r.Header.Get("X-Forwarded-Prefix"))
	}
	return ""
}

// normalizeBasePrefix trims a subpath to a canonical "/prefix" (leading slash, no
// trailing slash); "" for empty or "/".
func normalizeBasePrefix(p string) string {
	p = strings.TrimSpace(p)
	if p == "" || p == "/" {
		return ""
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return strings.TrimRight(p, "/")
}

func NewAuthenticator(accessKey, secretKey string, store metadata.StoreAPI, allowCIDR, blockCIDR []string) *Authenticator {
	return &Authenticator{
		adminAccessKey:  accessKey,
		adminSecretKey:  secretKey,
		store:           store,
		globalAllowCIDR: allowCIDR,
		globalBlockCIDR: blockCIDR,
	}
}

// UpdateAdminCredentials updates the admin access key and secret key at runtime.
func (a *Authenticator) UpdateAdminCredentials(accessKey, secretKey string) {
	a.adminMu.Lock()
	a.adminAccessKey = accessKey
	a.adminSecretKey = secretKey
	a.adminMu.Unlock()
}

func (a *Authenticator) adminPair() (string, string) {
	a.adminMu.RLock()
	defer a.adminMu.RUnlock()
	return a.adminAccessKey, a.adminSecretKey
}

// authError is a refusal to authenticate. code and msg are what the client is
// shown, and they are fixed per code: the reason in detail goes to the server
// log only. The text of the underlying error used to be sent back as the
// message, so a caller could tell an unknown key from an expired one from a bad
// session token before proving it held the secret, and the IP check echoed the
// address ranges it compared against.
type authError struct {
	code   string
	msg    string
	status int
	detail string
	// afterSignature marks a refusal that may be revealed only once the
	// request's signature has verified. Until then the caller has not shown it
	// holds the secret, and the specific reason would tell a stranger which
	// state a credential it guessed is in.
	afterSignature bool
}

func (e *authError) Error() string {
	if e.detail != "" {
		return e.code + ": " + e.detail
	}
	return e.code
}

func errAuthAccessDenied(detail string) *authError {
	return &authError{code: "AccessDenied", msg: "Access Denied", status: http.StatusForbidden, detail: detail}
}

func errAuthUnknownKey() *authError {
	return &authError{code: "InvalidAccessKeyId", msg: "The AWS Access Key Id you provided does not exist in our records.",
		status: http.StatusForbidden, detail: "unknown access key"}
}

func errAuthSignature() *authError {
	return &authError{code: "SignatureDoesNotMatch",
		msg:    "The request signature we calculated does not match the signature you provided. Check your key and signing method.",
		status: http.StatusForbidden, detail: "signature mismatch"}
}

func errAuthHeaderMalformed(msg string) *authError {
	return &authError{code: "AuthorizationHeaderMalformed", msg: msg, status: http.StatusBadRequest, detail: msg}
}

func errAuthQueryParams(msg string) *authError {
	return &authError{code: "AuthorizationQueryParametersError", msg: msg, status: http.StatusBadRequest, detail: msg}
}

// writeAuthError answers a refused authentication. Anything that is not an
// authError is answered as a plain AccessDenied, never with its own text.
func writeAuthError(w http.ResponseWriter, err error) {
	var ae *authError
	if errors.As(err, &ae) {
		writeS3Error(w, ae.code, ae.msg, ae.status)
		return
	}
	writeS3Error(w, "AccessDenied", "Access Denied", http.StatusForbidden)
}

// resolveIdentity looks up the identity for a given access key.
// Returns the identity with user info and policies.
//
// An error with afterSignature set still returns the secret, so the caller can
// verify the signature first and reveal the reason only to the key's holder.
func (a *Authenticator) resolveIdentity(accessKey string, r *http.Request) (*iam.Identity, string, error) {
	if adminAK, adminSK := a.adminPair(); accessKey == adminAK {
		return &iam.Identity{
			AccessKey: accessKey,
			IsAdmin:   true,
		}, adminSK, nil
	}
	if a.store == nil {
		return nil, "", errAuthUnknownKey()
	}
	key, err := a.store.GetAccessKey(accessKey)
	if err != nil {
		return nil, "", errAuthUnknownKey()
	}

	// Check STS expiration
	if key.ExpiresAt > 0 && time.Now().Unix() > key.ExpiresAt {
		return nil, key.SecretKey, &authError{code: "ExpiredToken", msg: "The provided token has expired.",
			status: http.StatusBadRequest, detail: "credentials have expired", afterSignature: true}
	}

	// A session credential must present the session token it was issued
	// with. It was never read anywhere, so the access key and secret alone
	// were sufficient and any token value, or none, was accepted
	// (security assessment finding 11).
	if key.SessionToken != "" {
		presented := r.Header.Get("X-Amz-Security-Token")
		if presented == "" {
			presented = r.URL.Query().Get("X-Amz-Security-Token")
		}
		if !hmac.Equal([]byte(presented), []byte(key.SessionToken)) {
			return nil, key.SecretKey, &authError{code: "InvalidToken", msg: "The provided token is malformed or otherwise invalid.",
				status: http.StatusBadRequest, detail: "invalid session token", afterSignature: true}
		}
	}

	identity := &iam.Identity{AccessKey: accessKey, UserID: key.UserID}

	if key.SourceUserID != "" {
		// A session is only ever as good as the user it was derived from. It used
		// to be resolved without looking at that user at all once it carried a
		// session policy, so deleting the user did not end the session, and the
		// user's IP allowlist did not apply to it.
		source, err := a.store.GetIAMUser(key.SourceUserID)
		if err != nil {
			return nil, key.SecretKey, &authError{code: "AccessDenied", msg: "Access Denied", status: http.StatusForbidden,
				detail: "the user this session was issued for no longer exists", afterSignature: true}
		}
		identity.UserID = key.SourceUserID
		identity.AllowedCIDRs = source.AllowedCIDRs
		identity.Policies, identity.PolicyLoadFailed = a.loadUserPolicies(key.SourceUserID)

		// A session minted with a policy of its own carries it on a synthetic
		// user. That policy narrows the source user and never widens them: AWS
		// grants a session only what BOTH allow. It used to be evaluated on its
		// own, so a session could do things its user could not, and before that
		// (security assessment finding 11) it was not evaluated at all.
		if key.UserID != "" && key.UserID != key.SourceUserID {
			sessionPolicies, failed := a.loadUserPolicies(key.UserID)
			identity.SessionScoped = true
			identity.SessionPolicies = sessionPolicies
			identity.PolicyLoadFailed = identity.PolicyLoadFailed || failed
		}
		return identity, key.SecretKey, nil
	}

	// Load policies if linked to a user
	if key.UserID != "" {
		identity.Policies, identity.PolicyLoadFailed = a.loadUserPolicies(key.UserID)

		// Load user's IP restrictions
		if user, err := a.store.GetIAMUser(key.UserID); err == nil {
			identity.AllowedCIDRs = user.AllowedCIDRs

			// The grant the key was issued with is the key's own, so a
			// second key for the same user cannot change it. It applies
			// only while the user exists: a key left behind by a deleted
			// user must not keep its access.
			if key.PolicyName != "" {
				if p, err := a.store.GetIAMPolicy(key.PolicyName); err == nil {
					var pol iam.Policy
					if err := json.Unmarshal([]byte(p.Document), &pol); err != nil {
						slog.Error("iam: an access key's policy failed to parse, denying this key until it is fixed",
							"policy", p.Name, "user", key.UserID, "error", err)
						identity.PolicyLoadFailed = true
					} else {
						identity.Policies = append(identity.Policies, pol)
					}
				}
			}
		}
	}

	// Keys without policies are denied by default (least privilege).
	// Key creation auto-generates IAM user + policy, so this
	// only affects manually created keys with no IAM setup.
	return identity, key.SecretKey, nil
}

// loadUserPolicies reads and parses every policy attached to a user. failed is
// set when any of them could not be read or parsed. An unreadable policy set is
// not an empty one: the read error used to be ignored, so the user's own
// policies, Denies included, were silently dropped while the key's own grant
// still loaded, and a user-attached Deny protected nothing.
func (a *Authenticator) loadUserPolicies(userID string) ([]iam.Policy, bool) {
	iamPolicies, err := a.store.GetUserPolicies(userID)
	if err != nil {
		slog.Error("iam: could not read a user's policies, denying this identity until it can",
			"user", userID, "error", err)
		return nil, true
	}
	var out []iam.Policy
	failed := false
	for _, p := range iamPolicies {
		var pol iam.Policy
		if err := json.Unmarshal([]byte(p.Document), &pol); err != nil {
			// A policy that cannot be parsed used to be dropped in
			// silence, so an operator saw it created, listed and
			// attached while it took no part in any decision, and a
			// Deny written that way protected nothing. Refuse the
			// whole identity instead of deciding on a partial set.
			slog.Error("iam: a policy failed to parse, denying this identity until it is fixed",
				"policy", p.Name, "user", userID, "error", err)
			failed = true
			continue
		}
		out = append(out, pol)
	}
	return out, failed
}

// CheckIPAccess validates client IP against global and per-user restrictions.
func (a *Authenticator) CheckIPAccess(identity *iam.Identity, clientIP string) error {
	// Admin is still subject to global blocklist
	if identity.IsAdmin {
		if len(a.globalBlockCIDR) > 0 {
			if err := iam.CheckIP(clientIP, nil, a.globalBlockCIDR); err != nil {
				return err
			}
		}
		return nil
	}

	// Combine global and per-user CIDR lists
	blockList := a.globalBlockCIDR
	allowList := a.globalAllowCIDR

	// Per-user restrictions are additive to global
	if len(identity.AllowedCIDRs) > 0 {
		if len(allowList) == 0 {
			allowList = identity.AllowedCIDRs
		} else {
			// Both global and user allowlists — must match at least one from either
			allowList = append(append([]string{}, allowList...), identity.AllowedCIDRs...)
		}
	}

	return iam.CheckIP(clientIP, allowList, blockList)
}

// maxClockSkew is how far a request's own timestamp may be from the server's.
const maxClockSkew = 15 * time.Minute

// Authenticate validates the Authorization header using AWS Signature V4.
// Returns the identity of the caller. A refusal is an *authError.
func (a *Authenticator) Authenticate(r *http.Request) (*iam.Identity, error) {
	authHeader := r.Header.Get("Authorization")
	if authHeader == "" {
		if r.URL.Query().Get("X-Amz-Signature") != "" {
			return a.authenticatePresigned(r)
		}
		return nil, errAuthAccessDenied("missing Authorization header")
	}

	if !strings.HasPrefix(authHeader, "AWS4-HMAC-SHA256") {
		return nil, errAuthAccessDenied("unsupported auth scheme")
	}

	parts := strings.SplitN(authHeader, " ", 2)
	if len(parts) != 2 {
		return nil, errAuthHeaderMalformed("The authorization header is malformed.")
	}

	params := parseAuthParams(parts[1])
	credential := params["Credential"]
	signedHeaders := params["SignedHeaders"]
	signature := params["Signature"]

	if credential == "" || signedHeaders == "" || signature == "" {
		return nil, errAuthHeaderMalformed("The authorization header is malformed; it must carry Credential, SignedHeaders and Signature.")
	}

	credParts := strings.Split(credential, "/")
	if len(credParts) != 5 {
		return nil, errAuthHeaderMalformed("The authorization header is malformed; the Credential is mal-formed.")
	}

	reqAccessKey := credParts[0]
	dateStr := credParts[1]
	region := credParts[2]
	service := credParts[3]

	// The timestamp is part of what is signed, so it has to be one this server
	// can read. An unparseable X-Amz-Date used to skip the skew check entirely,
	// which made a captured request replayable forever.
	amzDate, reqTime, ok := requestTimestamp(r)
	if !ok {
		return nil, errAuthAccessDenied("AWS authentication requires a valid Date or x-amz-date header")
	}
	if skew := time.Since(reqTime); skew > maxClockSkew || skew < -maxClockSkew {
		return nil, &authError{code: "RequestTimeTooSkewed",
			msg:    "The difference between the request time and the server's time is too large.",
			status: http.StatusForbidden, detail: "request time too skewed"}
	}
	// The signing key is derived from the credential scope date, which was never
	// compared with the request time, so a key derived for one day signed
	// requests stamped with any other.
	if dateStr != amzDate[:8] {
		return nil, errAuthHeaderMalformed("Invalid credential date. Date is not the same as X-Amz-Date.")
	}
	// Without host among the signed headers a signature does not bind the
	// request to this endpoint.
	if !signsHost(signedHeaders) {
		return nil, errAuthAccessDenied("host must be a signed header")
	}

	identity, secretKey, err := a.resolveIdentity(reqAccessKey, r)
	if err != nil && !isAfterSignature(err) {
		return nil, err
	}

	canonicalRequest := buildCanonicalRequest(r, signedHeaders, a.canonicalBasePrefix(r))
	stringToSign := buildStringToSignAt(amzDate, dateStr, region, service, canonicalRequest)
	signingKey := deriveSigningKey(secretKey, dateStr, region, service)
	expectedSig := hex.EncodeToString(hmacSHA256(signingKey, []byte(stringToSign)))

	if !hmac.Equal([]byte(signature), []byte(expectedSig)) {
		return nil, errAuthSignature()
	}
	if err != nil {
		// The caller holds the secret, so it may now learn why it was refused.
		return nil, err
	}

	return identity, nil
}

// isAfterSignature reports a refusal that must wait for the signature check.
func isAfterSignature(err error) bool {
	var ae *authError
	return errors.As(err, &ae) && ae.afterSignature
}

// signsHost reports whether a SignedHeaders list includes host.
func signsHost(signedHeaders string) bool {
	for _, h := range strings.Split(signedHeaders, ";") {
		if strings.EqualFold(strings.TrimSpace(h), "host") {
			return true
		}
	}
	return false
}

// requestTimestamp returns the request's signing time, in the ISO 8601 basic
// form the string to sign carries, from X-Amz-Date or failing that the Date
// header, as SigV4 allows.
func requestTimestamp(r *http.Request) (string, time.Time, bool) {
	if v := r.Header.Get("X-Amz-Date"); v != "" {
		t, err := time.Parse("20060102T150405Z", v)
		if err != nil {
			return "", time.Time{}, false
		}
		return v, t, true
	}
	if v := r.Header.Get("Date"); v != "" {
		t, err := http.ParseTime(v)
		if err != nil {
			return "", time.Time{}, false
		}
		return t.UTC().Format("20060102T150405Z"), t, true
	}
	return "", time.Time{}, false
}

func (a *Authenticator) authenticatePresigned(r *http.Request) (*iam.Identity, error) {
	q := r.URL.Query()
	credential := q.Get("X-Amz-Credential")
	signature := q.Get("X-Amz-Signature")
	signedHeaders := q.Get("X-Amz-SignedHeaders")
	dateStr := q.Get("X-Amz-Date")
	expiresStr := q.Get("X-Amz-Expires")

	if credential == "" || signature == "" || dateStr == "" {
		return nil, errAuthQueryParams("Query-string authentication version 4 requires the X-Amz-Algorithm, X-Amz-Credential, X-Amz-Signature, X-Amz-Date, X-Amz-SignedHeaders, and X-Amz-Expires parameters.")
	}

	credParts := strings.Split(credential, "/")
	if len(credParts) != 5 {
		return nil, errAuthQueryParams("Error parsing the X-Amz-Credential parameter; the Credential is mal-formed.")
	}

	// Validate expiry
	t, err := time.Parse("20060102T150405Z", dateStr)
	if err != nil {
		return nil, errAuthQueryParams("X-Amz-Date must be in the ISO8601 Long Format \"yyyyMMdd'T'HHmmss'Z'\"")
	}
	// A presigned URL has to say how long it lives. A missing X-Amz-Expires used
	// to default to the seven day maximum, so a URL meant to last minutes lived
	// for a week.
	expiresSecs, perr := strconv.Atoi(expiresStr)
	if expiresStr == "" || perr != nil || expiresSecs <= 0 {
		return nil, errAuthQueryParams("X-Amz-Expires must be a positive number of seconds.")
	}
	// AWS caps presigned URL expiry at 7 days (604800 seconds)
	if expiresSecs > 604800 {
		return nil, errAuthQueryParams("X-Amz-Expires must be less than a week (in seconds) that is 604800")
	}
	// A URL dated in the future was accepted, which let one be minted to start
	// working later and so outlive its stated lifetime.
	if time.Until(t) > maxClockSkew {
		return nil, errAuthAccessDenied("Request is not valid yet")
	}
	if time.Since(t) > time.Duration(expiresSecs)*time.Second {
		return nil, &authError{code: "AccessDenied", msg: "Request has expired", status: http.StatusForbidden, detail: "presigned URL expired"}
	}
	credDate := credParts[1]
	if credDate != dateStr[:8] {
		return nil, errAuthQueryParams("Invalid credential date. Date is not the same as X-Amz-Date.")
	}

	// Validate signature, rebuilding the canonical request from the query
	if signedHeaders == "" {
		signedHeaders = "host"
	}
	if !signsHost(signedHeaders) {
		return nil, errAuthQueryParams("X-Amz-SignedHeaders must include host.")
	}
	region := credParts[2]
	service := credParts[3]

	identity, secretKey, err := a.resolveIdentity(credParts[0], r)
	if err != nil && !isAfterSignature(err) {
		return nil, err
	}

	// Build canonical query string (all params except X-Amz-Signature)
	canonicalParams := url.Values{}
	for k, vs := range q {
		if k == "X-Amz-Signature" {
			continue
		}
		for _, v := range vs {
			canonicalParams.Add(k, v)
		}
	}

	canonicalHeaders := ""
	for _, h := range strings.Split(signedHeaders, ";") {
		h = strings.TrimSpace(h)
		if h == "host" {
			canonicalHeaders += fmt.Sprintf("host:%s\n", r.Host)
		} else {
			canonicalHeaders += fmt.Sprintf("%s:%s\n", h, strings.TrimSpace(r.Header.Get(h)))
		}
	}

	// Canonical URI must preserve '/' (per-segment encoding), matching the
	// header-auth path (issue #9). Using uriEncode() here escaped every '/' to
	// %2F, so presigned URLs from boto3/aws-cli/SDKs always failed verification.
	// basePrefix restores a reverse-proxy subpath stripped before we saw it (#36).
	uri := uriEncodePath(a.canonicalBasePrefix(r) + r.URL.Path)
	if uri == "" {
		uri = "/"
	}
	canonicalRequest := fmt.Sprintf("%s\n%s\n%s\n%s\n%s\nUNSIGNED-PAYLOAD",
		r.Method,
		uri,
		canonicalQueryEncode(canonicalParams),
		canonicalHeaders,
		signedHeaders,
	)

	stringToSign := buildStringToSignAt(dateStr, credDate, region, service, canonicalRequest)
	signingKey := deriveSigningKey(secretKey, credDate, region, service)
	expectedSig := hex.EncodeToString(hmacSHA256(signingKey, []byte(stringToSign)))

	if !hmac.Equal([]byte(signature), []byte(expectedSig)) {
		return nil, errAuthSignature()
	}
	if err != nil {
		return nil, err
	}

	return identity, nil
}

// Authorize checks if an identity is allowed to perform an action on a resource.
func (a *Authenticator) Authorize(identity *iam.Identity, action, resource string) error {
	return a.AuthorizeWithContext(identity, action, resource, nil)
}

// AuthorizeWithContext is Authorize with the request context that IAM conditions
// evaluate against, at minimum aws:SourceIp and aws:username. Without it a
// policy scoped to a source IP range was treated as unconditional (security
// assessment finding 12).
func (a *Authenticator) AuthorizeWithContext(identity *iam.Identity, action, resource string, ctx map[string]string) error {
	if identity.IsAdmin {
		// Admin is the break-glass path and is never put to the external
		// authorizer, so a broken endpoint cannot lock an operator out. Say so
		// once, because a webhook that never fires otherwise looks broken.
		a.external.NoteAdminBypass()
		return nil
	}
	if ctx == nil {
		ctx = map[string]string{}
	}
	if _, ok := ctx["aws:username"]; !ok && identity.UserID != "" {
		ctx["aws:username"] = identity.UserID
	}
	if identity.PolicyLoadFailed {
		return fmt.Errorf("access denied: %s on %s (a policy attached to this identity could not be parsed)", action, resource)
	}
	// A scoped session is allowed only what both its own policy and its source
	// user's allow, and a Deny in either is final.
	allowed, explicitDeny := iam.EvaluateIdentity(identity, action, resource, ctx)

	// An explicit Deny written by the operator is final. It is not put to the
	// external authorizer even in authoritative mode, so turning that mode on
	// cannot quietly reopen access a policy closed (issue #41).
	if explicitDeny {
		return fmt.Errorf("access denied: %s on %s", action, resource)
	}
	if a.external == nil {
		if allowed {
			return nil
		}
		return fmt.Errorf("access denied: %s on %s", action, resource)
	}
	// Deny-only mode narrows what IAM already allowed, so a request IAM refused
	// is refused without troubling the endpoint. Authoritative mode asks anyway,
	// because there the webhook is what decides.
	if !allowed && !a.external.Authoritative() {
		return fmt.Errorf("access denied: %s on %s", action, resource)
	}
	ok, err := a.external.Allow(iam.AuthRequest{
		AccessKey: identity.AccessKey,
		User:      identity.UserID,
		Action:    action,
		Resource:  resource,
		SourceIP:  ctx["aws:SourceIp"],
	})
	// Trust the decision, not the error. Allow already folded fail_open into ok,
	// so testing err first would deny a request the operator asked to let
	// through. err is carried only to say why a denial happened.
	if !ok {
		return &iam.DeniedError{Action: action, Resource: resource, Err: err}
	}
	return nil
}

func parseAuthParams(s string) map[string]string {
	params := make(map[string]string)
	// Split on comma only, not ", ": the SigV4 Authorization header separates its
	// components with commas and OPTIONAL whitespace. Standard SDKs (boto3, aws-cli)
	// add a space after the comma, but some clients (WinSCP, S3 Browser, others) do
	// not. None of the three values (Credential, SignedHeaders, Signature) ever
	// contains a comma, so splitting on "," and trimming is safe for both forms.
	for _, part := range strings.Split(s, ",") {
		kv := strings.SplitN(part, "=", 2)
		if len(kv) == 2 {
			params[strings.TrimSpace(kv[0])] = strings.TrimSpace(kv[1])
		}
	}
	return params
}

// emptyPayloadSHA256 is the SHA-256 of an empty byte string, the payload hash a
// conformant SigV4 client sends for a request with no body.
const emptyPayloadSHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

func buildCanonicalRequest(r *http.Request, signedHeaders, basePrefix string) string {
	method := r.Method

	// Canonical URI must be the strict per-segment URI-encoding of the path
	// (AWS SigV4 rules), so signatures from standard S3 clients (boto3, aws-cli,
	// the SDKs) match for keys containing '&', '$', spaces, etc. Using the raw
	// r.URL.Path here rejected every such key with "signature mismatch" (issue #9).
	// For keys without special characters this is identical to the raw path.
	// basePrefix restores a reverse-proxy subpath the client signed but a
	// path-stripping proxy removed, so verification matches (issue #36); "" when
	// not proxied, leaving the path unchanged.
	uri := uriEncodePath(basePrefix + r.URL.Path)
	if uri == "" {
		uri = "/"
	}

	canonicalQuery := r.URL.RawQuery
	if canonicalQuery != "" {
		// Parse and re-encode to get canonical sorted form
		queryString := r.URL.Query()
		keys := make([]string, 0, len(queryString))
		for k := range queryString {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var queryParts []string
		for _, k := range keys {
			for _, v := range queryString[k] {
				queryParts = append(queryParts, uriEncode(k)+"="+uriEncode(v))
			}
		}
		canonicalQuery = strings.Join(queryParts, "&")
	}

	headerNames := strings.Split(signedHeaders, ";")
	var canonicalHeaders strings.Builder
	for _, name := range headerNames {
		value := strings.TrimSpace(r.Header.Get(name))
		if name == "host" && value == "" {
			value = r.Host
		}
		canonicalHeaders.WriteString(name)
		canonicalHeaders.WriteString(":")
		canonicalHeaders.WriteString(value)
		canonicalHeaders.WriteString("\n")
	}

	// The client's X-Amz-Content-Sha256 header is the payload hash they signed
	// with, so we use it verbatim in the canonical request and let the signature
	// comparison authenticate it. We deliberately do NOT read the body here:
	// buffering the entire (up to multi-GB) upload in memory would defeat streaming
	// and let any caller with a valid access key exhaust server memory. For
	// UNSIGNED-PAYLOAD / STREAMING-* the sentinel is likewise used as-is.
	payloadHash := r.Header.Get("X-Amz-Content-Sha256")
	if payloadHash == "" {
		// No content-hash header: a conformant SigV4 client always signs one, so
		// this is a bodyless request (GET/HEAD/DELETE). Use the empty-body hash. A
		// request that carries a body but omits the header is non-conformant and
		// will simply fail the signature comparison below.
		payloadHash = emptyPayloadSHA256
	}

	return fmt.Sprintf("%s\n%s\n%s\n%s\n%s\n%s",
		method, uri, canonicalQuery,
		canonicalHeaders.String(), signedHeaders, payloadHash)
}

func buildStringToSign(dateStr, region, service, canonicalRequest string, r *http.Request) string {
	amzDate := r.Header.Get("X-Amz-Date")
	if amzDate == "" {
		amzDate = time.Now().UTC().Format("20060102T150405Z")
	}
	return buildStringToSignAt(amzDate, dateStr, region, service, canonicalRequest)
}

// buildStringToSignAt is the SigV4 string to sign for a request stamped amzDate.
func buildStringToSignAt(amzDate, dateStr, region, service, canonicalRequest string) string {
	scope := fmt.Sprintf("%s/%s/%s/aws4_request", dateStr, region, service)
	hash := sha256.Sum256([]byte(canonicalRequest))

	return fmt.Sprintf("AWS4-HMAC-SHA256\n%s\n%s\n%s",
		amzDate, scope, hex.EncodeToString(hash[:]))
}

func deriveSigningKey(secretKey, dateStr, region, service string) []byte {
	kDate := hmacSHA256([]byte("AWS4"+secretKey), []byte(dateStr))
	kRegion := hmacSHA256(kDate, []byte(region))
	kService := hmacSHA256(kRegion, []byte(service))
	kSigning := hmacSHA256(kService, []byte("aws4_request"))
	return kSigning
}

// canonicalQueryEncode builds an AWS SigV4 canonical query string: every key and
// value strictly URI-encoded (space -> %20, RFC 3986) and the pairs sorted. Go's
// url.Values.Encode() uses '+' for spaces and differs on sub-delimiters, so a
// presigned URL signed by boto3/aws-cli whose query carries a space (e.g. a
// response-content-disposition filename) failed verification here.
func canonicalQueryEncode(v url.Values) string {
	parts := make([]string, 0, len(v))
	for k, vals := range v {
		ek := uriEncode(k)
		for _, val := range vals {
			parts = append(parts, ek+"="+uriEncode(val))
		}
	}
	sort.Strings(parts)
	return strings.Join(parts, "&")
}

func uriEncode(s string) string {
	var buf strings.Builder
	for _, b := range []byte(s) {
		if (b >= 'A' && b <= 'Z') || (b >= 'a' && b <= 'z') || (b >= '0' && b <= '9') || b == '-' || b == '_' || b == '.' || b == '~' {
			buf.WriteByte(b)
		} else {
			fmt.Fprintf(&buf, "%%%02X", b)
		}
	}
	return buf.String()
}

// uriEncodePath strictly URI-encodes each path segment while preserving the '/'
// separators — the AWS SigV4 canonical-URI rule. (uriEncode alone would also
// encode '/', which is wrong for the path.)
func uriEncodePath(p string) string {
	segs := strings.Split(p, "/")
	for i, seg := range segs {
		segs[i] = uriEncode(seg)
	}
	return strings.Join(segs, "/")
}

func hmacSHA256(key, data []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write(data)
	return h.Sum(nil)
}

func (a *Authenticator) GetAccessKey() string {
	ak, _ := a.adminPair()
	return ak
}
