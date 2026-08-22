package s3

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Kodiqa-Solutions/VaultS3/internal/iam"
	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
)

// Authenticator validates S3 Signature V4 requests.
type Authenticator struct {
	adminAccessKey  string
	adminSecretKey  string
	store           metadata.StoreAPI
	globalAllowCIDR []string
	globalBlockCIDR []string
	basePath        string // reverse-proxy subpath the client signed but a path-stripping proxy removed (issue #36)
	trustForwarded  bool   // honor the client-supplied X-Forwarded-Prefix when basePath is unset
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
	a.adminAccessKey = accessKey
	a.adminSecretKey = secretKey
}

// resolveIdentity looks up the identity for a given access key.
// Returns the identity with user info and policies.
func (a *Authenticator) resolveIdentity(accessKey string, r *http.Request) (*iam.Identity, string, error) {
	if accessKey == a.adminAccessKey {
		return &iam.Identity{
			AccessKey: accessKey,
			IsAdmin:   true,
		}, a.adminSecretKey, nil
	}
	if a.store != nil {
		if key, err := a.store.GetAccessKey(accessKey); err == nil {
			// Check STS expiration
			if key.ExpiresAt > 0 && time.Now().Unix() > key.ExpiresAt {
				return nil, "", fmt.Errorf("credentials have expired")
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
					return nil, "", fmt.Errorf("invalid session token")
				}
			}

			// An STS key resolves against its OWN synthetic user, which carries the
			// session policy supplied when the token was minted. It used to resolve
			// against SourceUserID, the user the session was derived from, so the
			// scoped-down credential inherited that user's full permissions and the
			// session policy was never evaluated at all: a session created to allow
			// only GetObject could PUT, and one that explicitly denied GetObject
			// could read (security assessment finding 11).
			//
			// The fall back to the source user applies only when the session was
			// created with no policy of its own, which is the unscoped case where
			// inheriting is the intent.
			userID := key.UserID
			if key.SourceUserID != "" && !a.hasOwnPolicies(key.UserID) {
				userID = key.SourceUserID
			}

			identity := &iam.Identity{
				AccessKey: accessKey,
				UserID:    userID,
			}

			// Load policies if linked to a user
			if userID != "" {
				iamPolicies, err := a.store.GetUserPolicies(userID)
				if err == nil {
					for _, p := range iamPolicies {
						var pol iam.Policy
						if err := json.Unmarshal([]byte(p.Document), &pol); err == nil {
							identity.Policies = append(identity.Policies, pol)
						}
					}
				}

				// Load user's IP restrictions
				if user, err := a.store.GetIAMUser(userID); err == nil {
					identity.AllowedCIDRs = user.AllowedCIDRs
				}
			}

			// Keys without policies are denied by default (least privilege).
			// Key creation auto-generates IAM user + policy, so this
			// only affects manually created keys with no IAM setup.
			return identity, key.SecretKey, nil
		}
	}
	return nil, "", fmt.Errorf("invalid access key")
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

// Authenticate validates the Authorization header using AWS Signature V4.
// Returns the identity of the caller.
func (a *Authenticator) Authenticate(r *http.Request) (*iam.Identity, error) {
	authHeader := r.Header.Get("Authorization")
	if authHeader == "" {
		if r.URL.Query().Get("X-Amz-Signature") != "" {
			return a.authenticatePresigned(r)
		}
		return nil, fmt.Errorf("missing Authorization header")
	}

	if !strings.HasPrefix(authHeader, "AWS4-HMAC-SHA256") {
		return nil, fmt.Errorf("unsupported auth scheme")
	}

	parts := strings.SplitN(authHeader, " ", 2)
	if len(parts) != 2 {
		return nil, fmt.Errorf("malformed auth header")
	}

	params := parseAuthParams(parts[1])
	credential := params["Credential"]
	signedHeaders := params["SignedHeaders"]
	signature := params["Signature"]

	if credential == "" || signedHeaders == "" || signature == "" {
		return nil, fmt.Errorf("missing auth parameters")
	}

	credParts := strings.Split(credential, "/")
	if len(credParts) != 5 {
		return nil, fmt.Errorf("malformed credential")
	}

	reqAccessKey := credParts[0]
	dateStr := credParts[1]
	region := credParts[2]
	service := credParts[3]

	identity, secretKey, err := a.resolveIdentity(reqAccessKey, r)
	if err != nil {
		return nil, err
	}

	// Validate request timestamp is within 15 minutes of server time
	amzDate := r.Header.Get("X-Amz-Date")
	if amzDate != "" {
		if t, err := time.Parse("20060102T150405Z", amzDate); err == nil {
			skew := time.Since(t)
			if skew < 0 {
				skew = -skew
			}
			if skew > 15*time.Minute {
				return nil, fmt.Errorf("request time too skewed")
			}
		}
	}

	canonicalRequest := buildCanonicalRequest(r, signedHeaders, a.canonicalBasePrefix(r))
	stringToSign := buildStringToSign(dateStr, region, service, canonicalRequest, r)
	signingKey := deriveSigningKey(secretKey, dateStr, region, service)
	expectedSig := hex.EncodeToString(hmacSHA256(signingKey, []byte(stringToSign)))

	if !hmac.Equal([]byte(signature), []byte(expectedSig)) {
		return nil, fmt.Errorf("signature mismatch")
	}

	return identity, nil
}

func (a *Authenticator) authenticatePresigned(r *http.Request) (*iam.Identity, error) {
	q := r.URL.Query()
	credential := q.Get("X-Amz-Credential")
	signature := q.Get("X-Amz-Signature")
	signedHeaders := q.Get("X-Amz-SignedHeaders")
	dateStr := q.Get("X-Amz-Date")
	expiresStr := q.Get("X-Amz-Expires")

	if credential == "" || signature == "" || dateStr == "" {
		return nil, fmt.Errorf("missing presigned parameters")
	}

	credParts := strings.Split(credential, "/")
	if len(credParts) != 5 {
		return nil, fmt.Errorf("invalid credential")
	}

	identity, secretKey, err := a.resolveIdentity(credParts[0], r)
	if err != nil {
		return nil, err
	}

	// Validate expiry
	t, err := time.Parse("20060102T150405Z", dateStr)
	if err != nil {
		return nil, fmt.Errorf("invalid date: %w", err)
	}
	expiresSecs := 604800 // default 7 days max
	if expiresStr != "" {
		if parsed, parseErr := strconv.Atoi(expiresStr); parseErr == nil && parsed > 0 {
			expiresSecs = parsed
		}
	}
	// AWS caps presigned URL expiry at 7 days (604800 seconds)
	if expiresSecs > 604800 {
		return nil, fmt.Errorf("presigned URL expiry exceeds maximum of 604800 seconds")
	}
	if time.Since(t) > time.Duration(expiresSecs)*time.Second {
		return nil, fmt.Errorf("presigned URL expired")
	}

	// Validate signature — rebuild canonical request from query params
	if signedHeaders == "" {
		signedHeaders = "host"
	}
	region := credParts[2]
	service := credParts[3]
	credDate := credParts[1]

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

	scope := fmt.Sprintf("%s/%s/%s/aws4_request", credDate, region, service)
	hash := sha256.Sum256([]byte(canonicalRequest))
	stringToSign := fmt.Sprintf("AWS4-HMAC-SHA256\n%s\n%s\n%s",
		dateStr, scope, hex.EncodeToString(hash[:]))

	signingKey := deriveSigningKey(secretKey, credDate, region, service)
	expectedSig := hex.EncodeToString(hmacSHA256(signingKey, []byte(stringToSign)))

	if !hmac.Equal([]byte(signature), []byte(expectedSig)) {
		return nil, fmt.Errorf("signature mismatch")
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
		return nil
	}
	if ctx == nil {
		ctx = map[string]string{}
	}
	if _, ok := ctx["aws:username"]; !ok && identity.UserID != "" {
		ctx["aws:username"] = identity.UserID
	}
	if iam.EvaluateWithContext(identity.Policies, action, resource, ctx) {
		return nil
	}
	return fmt.Errorf("access denied: %s on %s", action, resource)
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
	return a.adminAccessKey
}

// hasOwnPolicies reports whether an identity carries policies of its own. For an
// STS session this is the inline session policy: when it exists it is the whole
// permission set, and it must not be widened by the source user's policies.
func (a *Authenticator) hasOwnPolicies(userID string) bool {
	if userID == "" || a.store == nil {
		return false
	}
	policies, err := a.store.GetUserPolicies(userID)
	if err != nil {
		// An unreadable policy set is not an empty one. Treat the session as
		// scoped so a store error cannot silently promote it to the source
		// user's full permissions.
		return true
	}
	return len(policies) > 0
}
