package api

import (
	"encoding/json"
	"net/http"
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

	// Validate optional inline policy
	if req.Policy != "" {
		var js json.RawMessage
		if err := json.Unmarshal([]byte(req.Policy), &js); err != nil {
			writeError(w, http.StatusBadRequest, "policy must be valid JSON")
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
		policyName := "sts-" + accessKey
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
			Name:       "sts-" + accessKey,
			CreatedAt:  now,
			PolicyARNs: []string{policyName},
		}
		if err := h.store.CreateIAMUser(stsUser); err != nil {
			writeError(w, http.StatusInternalServerError, "failed to create STS user")
			return
		}
		key.UserID = stsUser.Name
		key.SourceUserID = sourceUserID
	}

	if err := h.store.CreateAccessKey(key); err != nil {
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
