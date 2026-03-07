package api

import (
	"crypto/hmac"
	"fmt"
	"net/http"
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
	var req loginRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	// Use constant-time comparison to prevent timing attacks
	akMatch := hmac.Equal([]byte(req.AccessKey), []byte(h.cfg.Auth.AdminAccessKey))
	skMatch := hmac.Equal([]byte(req.SecretKey), []byte(h.cfg.Auth.AdminSecretKey))
	if !akMatch || !skMatch {
		writeError(w, http.StatusUnauthorized, "invalid credentials")
		return
	}

	token, err := h.jwt.Generate("admin", 24*time.Hour)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to generate token")
		return
	}

	writeJSON(w, http.StatusOK, loginResponse{Token: token})
}

func (h *APIHandler) handleMe(w http.ResponseWriter, _ *http.Request) {
	// Mask access key — only show first 4 and last 4 chars
	ak := h.cfg.Auth.AdminAccessKey
	masked := ak
	if len(ak) > 8 {
		masked = ak[:4] + strings.Repeat("*", len(ak)-8) + ak[len(ak)-4:]
	}
	writeJSON(w, http.StatusOK, meResponse{
		User:      "admin",
		AccessKey: masked,
	})
}

type oidcLoginRequest struct {
	IDToken string `json:"idToken"`
}

type oidcLoginResponse struct {
	Token string `json:"token"`
	User  string `json:"user"`
	Email string `json:"email"`
}

func (h *APIHandler) handleOIDCLogin(w http.ResponseWriter, r *http.Request) {
	if h.oidc == nil {
		writeError(w, http.StatusNotFound, "OIDC not configured")
		return
	}

	var req oidcLoginRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	claims, err := h.oidc.ValidateToken(req.IDToken)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "invalid token")
		return
	}

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
	_, err = h.store.GetIAMUser(userName)
	if err != nil {
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

func (h *APIHandler) handleOIDCConfig(w http.ResponseWriter, _ *http.Request) {
	enabled := h.oidc != nil
	resp := map[string]interface{}{
		"enabled": enabled,
	}
	if enabled {
		resp["issuerUrl"] = h.cfg.OIDC.IssuerURL
		resp["clientId"] = h.cfg.OIDC.ClientID
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

	// Fall back to query param (for browser download links)
	if token := r.URL.Query().Get("token"); token != "" {
		claims, err := h.jwt.Validate(token)
		if err != nil {
			return "", err
		}
		return claims.Sub, nil
	}

	return "", fmt.Errorf("missing authorization")
}

// isAdminUser returns true if the user is the admin user.
func (h *APIHandler) isAdminUser(r *http.Request) bool {
	user, err := h.authenticateUser(r)
	if err != nil {
		return false
	}
	return user == "admin"
}
