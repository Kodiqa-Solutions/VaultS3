package api

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
)

type keyListItem struct {
	AccessKey    string `json:"accessKey"`
	MaskedSecret string `json:"maskedSecret"`
	CreatedAt    string `json:"createdAt"`
	IsAdmin      bool   `json:"isAdmin"`
	UserID       string `json:"userId,omitempty"`
}

type keyCreateResponse struct {
	AccessKey string `json:"accessKey"`
	SecretKey string `json:"secretKey"`
	CreatedAt string `json:"createdAt"`
}

func (h *APIHandler) handleListKeys(w http.ResponseWriter, _ *http.Request) {
	keys, err := h.store.ListAccessKeys()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list keys")
		return
	}

	items := make([]keyListItem, 0, len(keys)+1)

	// Include admin key
	items = append(items, keyListItem{
		AccessKey:    h.cfg.Auth.AdminAccessKey,
		MaskedSecret: maskSecret(h.cfg.Auth.AdminSecretKey),
		CreatedAt:    "",
		IsAdmin:      true,
	})

	for _, k := range keys {
		items = append(items, keyListItem{
			AccessKey:    k.AccessKey,
			MaskedSecret: maskSecret(k.SecretKey),
			CreatedAt:    k.CreatedAt.Format(time.RFC3339),
			UserID:       k.UserID,
		})
	}

	writeJSON(w, http.StatusOK, items)
}

func (h *APIHandler) handleCreateKey(w http.ResponseWriter, r *http.Request) {
	var reqBody struct {
		UserID  string   `json:"userId"`
		Buckets []string `json:"buckets"`
	}
	if err := readJSON(r, &reqBody); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if reqBody.UserID == "" {
		writeError(w, http.StatusBadRequest, "userId is required")
		return
	}

	// Validate bucket names if provided
	for _, bucket := range reqBody.Buckets {
		if !h.store.BucketExists(bucket) {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("bucket %q does not exist", bucket))
			return
		}
	}

	accessKey, err := randomHex(10) // 20 hex chars
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to generate access key")
		return
	}

	secretKey, err := randomHex(20) // 40 hex chars
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to generate secret key")
		return
	}

	now := time.Now().UTC()

	// Auto-create IAM user if it doesn't exist
	if _, err := h.store.GetIAMUser(reqBody.UserID); err != nil {
		user := metadata.IAMUser{
			Name:      reqBody.UserID,
			CreatedAt: now,
		}
		if err := h.store.CreateIAMUser(user); err != nil {
			writeError(w, http.StatusInternalServerError, "failed to create IAM user")
			return
		}
	}

	// Build policy: scoped to specific buckets or full access
	policyName := fmt.Sprintf("key-policy-%s", reqBody.UserID)
	var policyDoc string
	if len(reqBody.Buckets) > 0 {
		// Scoped: only allow access to specified buckets
		resources := make([]string, 0, len(reqBody.Buckets)*2)
		for _, bucket := range reqBody.Buckets {
			resources = append(resources, fmt.Sprintf("arn:aws:s3:::%s", bucket))
			resources = append(resources, fmt.Sprintf("arn:aws:s3:::%s/*", bucket))
		}
		doc, _ := json.Marshal(map[string]interface{}{
			"Version": "2012-10-17",
			"Statement": []map[string]interface{}{
				{
					"Effect":   "Allow",
					"Action":   []string{"s3:*"},
					"Resource": resources,
				},
			},
		})
		policyDoc = string(doc)
	} else {
		// No buckets specified: full S3 access
		doc, _ := json.Marshal(map[string]interface{}{
			"Version": "2012-10-17",
			"Statement": []map[string]interface{}{
				{
					"Effect":   "Allow",
					"Action":   []string{"s3:*"},
					"Resource": []string{"*"},
				},
			},
		})
		policyDoc = string(doc)
	}

	// Create or update the policy
	if existing, err := h.store.GetIAMPolicy(policyName); err != nil {
		// Policy doesn't exist, create it
		policy := metadata.IAMPolicy{
			Name:      policyName,
			CreatedAt: now,
			Document:  policyDoc,
		}
		if err := h.store.CreateIAMPolicy(policy); err != nil {
			writeError(w, http.StatusInternalServerError, "failed to create policy")
			return
		}
	} else {
		// Policy exists, update the document
		existing.Document = policyDoc
		if err := h.store.UpdateIAMPolicy(*existing); err != nil {
			writeError(w, http.StatusInternalServerError, "failed to update policy")
			return
		}
	}

	// Attach policy to user
	iamUser, _ := h.store.GetIAMUser(reqBody.UserID)
	hasPol := false
	for _, p := range iamUser.PolicyARNs {
		if p == policyName {
			hasPol = true
			break
		}
	}
	if !hasPol {
		iamUser.PolicyARNs = append(iamUser.PolicyARNs, policyName)
		if err := h.store.UpdateIAMUser(*iamUser); err != nil {
			writeError(w, http.StatusInternalServerError, "failed to attach policy")
			return
		}
	}

	// Create the access key
	key := metadata.AccessKey{
		AccessKey: accessKey,
		SecretKey: secretKey,
		CreatedAt: now,
		UserID:    reqBody.UserID,
	}

	if err := h.store.CreateAccessKey(key); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to create access key")
		return
	}

	writeJSON(w, http.StatusCreated, keyCreateResponse{
		AccessKey: accessKey,
		SecretKey: secretKey,
		CreatedAt: now.Format(time.RFC3339),
	})
}

func (h *APIHandler) handleDeleteKey(w http.ResponseWriter, _ *http.Request, accessKey string) {
	if accessKey == h.cfg.Auth.AdminAccessKey {
		writeError(w, http.StatusForbidden, "cannot delete admin key")
		return
	}

	key, err := h.store.GetAccessKey(accessKey)
	if err != nil {
		writeError(w, http.StatusNotFound, "key not found")
		return
	}

	if err := h.store.DeleteAccessKey(accessKey); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to delete key")
		return
	}

	// Clean up auto-created IAM policy and user if no other keys reference this user
	if key.UserID != "" {
		hasOtherKeys := false
		if allKeys, err := h.store.ListAccessKeys(); err == nil {
			for _, k := range allKeys {
				if k.AccessKey != accessKey && k.UserID == key.UserID {
					hasOtherKeys = true
					break
				}
			}
		}
		if !hasOtherKeys {
			policyName := fmt.Sprintf("key-policy-%s", key.UserID)
			_ = h.store.DeleteIAMPolicy(policyName)
			_ = h.store.DeleteIAMUser(key.UserID)
		}
	}

	w.WriteHeader(http.StatusNoContent)
}

func maskSecret(secret string) string {
	if len(secret) <= 8 {
		return "****"
	}
	return secret[:4] + "****" + secret[len(secret)-4:]
}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
