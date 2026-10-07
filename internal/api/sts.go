package api

import (
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
)

type stsResponse struct {
	AccessKey    string `json:"accessKey"`
	SecretKey    string `json:"secretKey"`
	SessionToken string `json:"sessionToken"`
	Expiration   string `json:"expiration"`
}

func (h *APIHandler) handleCreateSessionToken(w http.ResponseWriter, r *http.Request) {
	var req struct {
		DurationSecs int    `json:"durationSecs"`
		Policy       string `json:"policy,omitempty"` // optional inline policy to scope down
		UserID       string `json:"userId,omitempty"` // which IAM user to create STS for (admin only)
	}
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if req.DurationSecs <= 0 {
		req.DurationSecs = 3600 // default 1 hour
	}

	maxDuration := h.cfg.Security.STSMaxDurationSecs
	if maxDuration <= 0 {
		maxDuration = 43200
	}
	if req.DurationSecs > maxDuration {
		writeError(w, http.StatusBadRequest, "duration exceeds maximum allowed")
		return
	}

	// Validate optional inline policy. A policy only scopes a session derived
	// from a user, so one sent without userId used to be dropped in silence and
	// the caller got a 201 for a credential that could reach nothing. Refused
	// instead, and a document the authorizer could not use is refused too.
	if req.Policy != "" {
		if req.UserID == "" {
			writeError(w, http.StatusBadRequest, "policy needs userId: a session policy narrows the access of the user the session is issued for")
			return
		}
		if err := validatePolicyDocument(req.Policy); err != nil {
			writeError(w, http.StatusBadRequest, "policy: "+err.Error())
			return
		}
	}

	// Determine the source user for the STS key
	sourceUserID := req.UserID
	if sourceUserID != "" {
		// Verify user exists
		if _, err := h.store.GetIAMUser(sourceUserID); err != nil {
			writeError(w, http.StatusNotFound, "user not found")
			return
		}
	}

	accessKey, err := randomHex(10)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to generate access key")
		return
	}

	secretKey, err := randomHex(20)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to generate secret key")
		return
	}

	sessionToken, err := randomHex(16)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to generate session token")
		return
	}

	now := time.Now().UTC()
	expiration := now.Add(time.Duration(req.DurationSecs) * time.Second)

	key := metadata.AccessKey{
		AccessKey:    accessKey,
		SecretKey:    secretKey,
		CreatedAt:    now,
		UserID:       sourceUserID,
		ExpiresAt:    expiration.Unix(),
		SessionToken: sessionToken,
		SourceUserID: sourceUserID,
	}

	// If an inline policy was provided, create and attach it to a synthetic STS user
	if req.Policy != "" && sourceUserID != "" {
		policyName := stsSyntheticName(accessKey)
		stsPolicy := metadata.IAMPolicy{
			Name:      policyName,
			CreatedAt: now,
			Document:  req.Policy,
		}
		if err := h.store.CreateIAMPolicy(stsPolicy); err != nil {
			writeError(w, http.StatusInternalServerError, "failed to create STS policy")
			return
		}
		// Create a synthetic IAM user for this STS token with only the inline policy
		stsUser := metadata.IAMUser{
			Name:       stsSyntheticName(accessKey),
			CreatedAt:  now,
			PolicyARNs: []string{policyName},
		}
		if err := h.store.CreateIAMUser(stsUser); err != nil {
			_ = h.store.DeleteIAMPolicy(policyName)
			writeError(w, http.StatusInternalServerError, "failed to create STS user")
			return
		}
		key.UserID = stsUser.Name
		key.SourceUserID = sourceUserID
	}

	if err := h.store.CreateAccessKey(key); err != nil {
		h.deleteSTSArtifacts(key)
		writeError(w, http.StatusInternalServerError, "failed to create STS key")
		return
	}

	writeJSON(w, http.StatusCreated, stsResponse{
		AccessKey:    accessKey,
		SecretKey:    secretKey,
		SessionToken: sessionToken,
		Expiration:   expiration.Format(time.RFC3339),
	})
}

// stsSyntheticName names both the synthetic user a scoped session resolves to
// and the policy that holds its session policy.
func stsSyntheticName(accessKey string) string {
	return "sts-" + accessKey
}

// isSTSSyntheticUser recognises the user a scoped session was given: named
// after its key and holding exactly its own session policy. It is plumbing,
// not a person, so the IAM user list does not show it.
func isSTSSyntheticUser(u metadata.IAMUser) bool {
	return strings.HasPrefix(u.Name, "sts-") && len(u.PolicyARNs) == 1 &&
		u.PolicyARNs[0] == u.Name && len(u.Groups) == 0
}

// deleteSTSArtifacts removes the synthetic user and session policy a scoped
// session key was issued with. They used to outlive the key: deleting the key,
// or the user it was derived from, left both behind, and they showed up in the
// IAM user list as if someone had created them.
func (h *APIHandler) deleteSTSArtifacts(k metadata.AccessKey) {
	name := stsSyntheticName(k.AccessKey)
	if k.SessionToken == "" || k.UserID != name {
		return
	}
	if err := h.store.DeleteIAMUser(name); err != nil {
		slog.Warn("could not remove the synthetic user of a session key", "user", name, "error", err)
	}
	if err := h.store.DeleteIAMPolicy(name); err != nil {
		slog.Warn("could not remove the session policy of a session key", "policy", name, "error", err)
	}
}
