package api

import (
	"crypto/hmac"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
)

type loginRequest struct {
	AccessKey string `json:"accessKey"`
	SecretKey string `json:"secretKey"`
}

type loginResponse struct {
	Token string `json:"token"`
}

type meResponse struct {
	User      string `json:"user"`
	AccessKey string `json:"accessKey"`
}

func (h *APIHandler) handleLogin(w http.ResponseWriter, r *http.Request) {
	// Lock out an address that has spent its allowance of wrong passwords. This
	// endpoint guards the admin credential pair, so an unlimited guess rate is
	// the whole attack (security assessment finding 4).
	ip := clientIPOf(r)
	if blocked, retryIn := h.logins.blocked(ip); blocked {
		w.Header().Set("Retry-After", strconv.Itoa(int(retryIn.Seconds())+1))
		writeError(w, http.StatusTooManyRequests, "too many failed login attempts, try again later")
		return
	}

	var req loginRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	// Use constant-time comparison to prevent timing attacks
	akMatch := hmac.Equal([]byte(req.AccessKey), []byte(h.cfg.Auth.AdminAccessKey))
	skMatch := hmac.Equal([]byte(req.SecretKey), []byte(h.cfg.Auth.AdminSecretKey))
	if !akMatch || !skMatch {
		h.logins.fail(ip)
		writeError(w, http.StatusUnauthorized, "invalid credentials")
		return
	}
	h.logins.succeed(ip)

	token, err := h.jwt.Generate("admin", 24*time.Hour)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to generate token")
		return
	}

	writeJSON(w, http.StatusOK, loginResponse{Token: token})
}

func (h *APIHandler) handleMe(w http.ResponseWriter, r *http.Request) {
	// Report whoever this session actually belongs to. It used to answer "admin"
	// for everyone, so a user signed in through SSO saw themselves as the admin
	// account and was shown its (masked) access key. Authorization was never
	// affected — that is keyed on the JWT subject — but the answer was wrong.
	user, err := h.authenticateUser(r)
	if err != nil || user == "" {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	resp := meResponse{User: user}
	if user == "admin" {
		// Mask access key — only show first 4 and last 4 chars
		ak := h.cfg.Auth.AdminAccessKey
		masked := ak
		if len(ak) > 8 {
			masked = ak[:4] + strings.Repeat("*", len(ak)-8) + ak[len(ak)-4:]
		}
		resp.AccessKey = masked
	}
	writeJSON(w, http.StatusOK, resp)
}

type oidcLoginRequest struct {
	IDToken string `json:"idToken"`
}

type oidcLoginResponse struct {
	Token string `json:"token"`
	User  string `json:"user"`
	Email string `json:"email"`
}

// handleOIDCLogin is the legacy implicit flow: it turns an ID token the client
// presents into a dashboard session.
//
// It is disabled unless an operator opts in, because the token it accepts is
// bound to nothing this server issued. The nonce in the implicit flow is minted
// by the browser and never registered here, so any valid, unexpired token for
// the configured client works: one captured from a URL fragment, or one the
// attacker obtained by logging in as themselves and then pushing through a
// victim's browser. The authorization-code flow binds the token to a login this
// server started, with PKCE and a server-sealed nonce, and is unaffected
// (security assessment finding 3).
func (h *APIHandler) handleOIDCLogin(w http.ResponseWriter, r *http.Request) {
	if h.oidc == nil {
		writeError(w, http.StatusNotFound, "OIDC not configured")
		return
	}
	if !h.cfg.OIDC.AllowImplicitFlow {
		writeError(w, http.StatusForbidden,
			"the OIDC implicit flow is disabled; use the authorization-code flow, "+
				"or set oidc.allow_implicit_flow if your provider supports nothing newer")
		return
	}

	// The implicit flow hands out sessions, so it is throttled like the password
	// endpoint.
	ip := clientIPOf(r)
	if blocked, retryIn := h.logins.blocked(ip); blocked {
		w.Header().Set("Retry-After", strconv.Itoa(int(retryIn.Seconds())+1))
		writeError(w, http.StatusTooManyRequests, "too many failed login attempts, try again later")
		return
	}

	var req oidcLoginRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	claims, err := h.oidc.ValidateToken(req.IDToken)
	if err != nil {
		h.logins.fail(ip)
		writeError(w, http.StatusUnauthorized, "invalid token")
		return
	}
	h.logins.succeed(ip)

	h.issueOIDCSession(w, claims)
}

// issueOIDCSession turns validated ID-token claims into a dashboard session,
// provisioning the IAM user when auto-create is on. Shared by both OAuth flows.
func (h *APIHandler) issueOIDCSession(w http.ResponseWriter, claims *OIDCClaims) {
	userName := claims.Email
	if userName == "" {
		userName = claims.Sub
	}

	// Prevent OIDC users from claiming the reserved "admin" username
	if userName == "admin" {
		writeError(w, http.StatusForbidden, "username 'admin' is reserved")
		return
	}

	// Look up or auto-create IAM user
	if _, err := h.store.GetIAMUser(userName); err != nil {
		if !h.cfg.OIDC.AutoCreateUsers {
			writeError(w, http.StatusForbidden, "user not found and auto-create disabled")
			return
		}

		// Auto-create user with role mapping
		newUser := metadata.IAMUser{
			Name:      userName,
			CreatedAt: time.Now().UTC(),
		}
		if len(h.cfg.OIDC.RoleMapping) > 0 && len(claims.Groups) > 0 {
			for _, group := range claims.Groups {
				if policyName, ok := h.cfg.OIDC.RoleMapping[group]; ok {
					newUser.PolicyARNs = append(newUser.PolicyARNs, policyName)
				}
			}
		}
		if createErr := h.store.CreateIAMUser(newUser); createErr != nil {
			writeError(w, http.StatusInternalServerError, "failed to create user")
			return
		}
	}

	token, err := h.jwt.Generate(userName, 24*time.Hour)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to generate token")
		return
	}

	writeJSON(w, http.StatusOK, oidcLoginResponse{
		Token: token,
		User:  userName,
		Email: claims.Email,
	})
}

// OIDCFlow reports the OAuth flow in use, for startup logging.
func (h *APIHandler) OIDCFlow() string { return h.oidcFlow() }

// oidcFlow decides which OAuth flow the dashboard drives: the operator's choice
// when they pinned one, otherwise the authorization-code flow whenever the
// provider advertises it, falling back to implicit only when it does not.
func (h *APIHandler) oidcFlow() string {
	switch strings.ToLower(h.cfg.OIDC.Flow) {
	case "code":
		return "code"
	case "implicit":
		return "implicit"
	}
	if h.oidc != nil && h.oidc.SupportsCodeFlow() {
		return "code"
	}
	return "implicit"
}

type oidcStartRequest struct {
	RedirectURI string `json:"redirectUri"`
}

type oidcCallbackRequest struct {
	Code  string `json:"code"`
	State string `json:"state"`
}

// handleOIDCStart begins an authorization-code login and returns the URL the
// browser should open. The PKCE verifier and nonce are generated here and sealed
// into the state, so neither is ever exposed to the page (issue #44).
func (h *APIHandler) handleOIDCStart(w http.ResponseWriter, r *http.Request) {
	if h.oidc == nil {
		writeError(w, http.StatusNotFound, "OIDC not configured")
		return
	}
	var req oidcStartRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if err := validateOIDCRedirectURI(req.RedirectURI, r); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	authURL, err := h.oidc.StartLogin(req.RedirectURI)
	if err != nil {
		slog.Warn("oidc: cannot start code-flow login", "error", err)
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"authorizeUrl": authURL})
}

// handleOIDCCallback redeems the authorization code the provider handed back.
func (h *APIHandler) handleOIDCCallback(w http.ResponseWriter, r *http.Request) {
	if h.oidc == nil {
		writeError(w, http.StatusNotFound, "OIDC not configured")
		return
	}
	var req oidcCallbackRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Code == "" || req.State == "" {
		writeError(w, http.StatusBadRequest, "code and state are required")
		return
	}

	claims, err := h.oidc.CompleteLogin(req.Code, req.State)
	if err != nil {
		// Log the provider's reason (wrong secret, redirect URI mismatch, expired
		// code); tell the browser only that the login failed.
		slog.Warn("oidc: code exchange failed", "error", err)
		writeError(w, http.StatusUnauthorized, "sign-in could not be completed")
		return
	}
	h.issueOIDCSession(w, claims)
}

// validateOIDCRedirectURI keeps the callback pointed at this dashboard. The
// provider checks it against its own registered list too, but this stops us
// building a login URL that sends users somewhere else entirely.
func validateOIDCRedirectURI(raw string, r *http.Request) error {
	if raw == "" {
		return fmt.Errorf("redirectUri is required")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("redirectUri is not a valid URL")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("redirectUri must be http or https")
	}
	if !strings.EqualFold(u.Host, r.Host) {
		return fmt.Errorf("redirectUri must point at this server")
	}
	return nil
}

func (h *APIHandler) handleOIDCConfig(w http.ResponseWriter, _ *http.Request) {
	enabled := h.oidc != nil
	resp := map[string]interface{}{
		"enabled": enabled,
	}
	if enabled {
		resp["issuerUrl"] = h.cfg.OIDC.IssuerURL
		resp["clientId"] = h.cfg.OIDC.ClientID
		// Where the browser must send the user to log in. Discovered from the
		// provider rather than built from the issuer, because the two are
		// unrelated paths on Authentik, Keycloak and Auth0 (issue #44).
		resp["authorizeUrl"] = h.oidc.AuthorizeURL()
		// Which OAuth flow the dashboard should drive. "code" is the modern one and
		// the only one Authentik and Keycloak enable by default; "implicit" remains
		// for providers that offer nothing else.
		resp["flow"] = h.oidcFlow()
		// Only the scopes this provider actually accepts. The implicit path builds
		// its own URL in the browser and must not guess (issue #44).
		resp["scope"] = h.oidc.Scopes()
	}
	writeJSON(w, http.StatusOK, resp)
}

func (h *APIHandler) authenticate(r *http.Request) error {
	_, err := h.authenticateUser(r)
	return err
}

// authenticateUser validates JWT and returns the subject (username).
func (h *APIHandler) authenticateUser(r *http.Request) (string, error) {
	// Check Authorization header first
	auth := r.Header.Get("Authorization")
	if auth != "" {
		token := strings.TrimPrefix(auth, "Bearer ")
		if token == auth {
			return "", fmt.Errorf("invalid authorization format")
		}
		claims, err := h.jwt.Validate(token)
		if err != nil {
			return "", err
		}
		return claims.Sub, nil
	}

	// Fall back to the query parameter, but only for the browser download links
	// that need it. A token in a URL leaks into proxy logs, browser history and
	// Referer headers, so it is accepted on the handful of routes a browser
	// navigates to directly and nowhere else.
	if token := r.URL.Query().Get("token"); token != "" && allowsTokenInURL(r.URL.Path) {
		claims, err := h.jwt.Validate(token)
		if err != nil {
			return "", err
		}
		return claims.Sub, nil
	}

	return "", fmt.Errorf("missing authorization")
}

// allowsTokenInURL reports whether a path may authenticate with ?token=.
//
// Only routes a browser navigates to directly, where no Authorization header can
// be set, qualify. Everything else must use the header, so a leaked URL cannot
// be replayed against the admin API.
func allowsTokenInURL(path string) bool {
	return strings.Contains(path, "/download") || strings.HasSuffix(path, "/export")
}

// isAdminUser returns true if the user is the admin user.
func (h *APIHandler) isAdminUser(r *http.Request) bool {
	user, err := h.authenticateUser(r)
	if err != nil {
		return false
	}
	return user == "admin"
}
