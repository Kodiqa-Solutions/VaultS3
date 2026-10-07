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
	// Access says what the key can reach: "buckets" (the ones requested),
	// "all" (every bucket) or "user" (exactly the user's own policies).
	Access string `json:"access"`
}

const (
	keyAccessBuckets = "buckets"
	keyAccessAll     = "all"
	keyAccessUser    = "user"
)

func (h *APIHandler) handleListKeys(w http.ResponseWriter, _ *http.Request) {
	keys, err := h.store.ListAccessKeys()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list keys")
		return
	}

	items := make([]keyListItem, 0, len(keys)+1)

	// Include admin key
	adminAK, adminSK := h.adminCredentials()
	items = append(items, keyListItem{
		AccessKey:    adminAK,
		MaskedSecret: maskSecret(adminSK),
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
		UserID           string   `json:"userId"`
		Buckets          []string `json:"buckets"`
		AllBuckets       bool     `json:"allBuckets"`
		UserPoliciesOnly bool     `json:"userPoliciesOnly"`
	}
	if err := readJSON(r, &reqBody); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if reqBody.UserID == "" {
		writeError(w, http.StatusBadRequest, "userId is required")
		return
	}
	chosen := 0
	for _, set := range []bool{len(reqBody.Buckets) > 0, reqBody.AllBuckets, reqBody.UserPoliciesOnly} {
		if set {
			chosen++
		}
	}
	if chosen > 1 {
		writeError(w, http.StatusBadRequest, "buckets, allBuckets and userPoliciesOnly are alternatives, set one")
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

	// Keys issued before 4.4.79 shared a policy attached to the user. Split it
	// first, so it is neither counted as the user's own policy below nor
	// inherited by the key about to be issued.
	_, getErr := h.store.GetIAMUser(reqBody.UserID)
	userExists := getErr == nil
	if userExists {
		if err := h.splitLegacyKeyPolicy(reqBody.UserID, now); err != nil {
			writeError(w, http.StatusInternalServerError, "failed to convert the user's existing keys: "+err.Error())
			return
		}
	}

	// Decide what the key can reach. With no buckets named, a key used to get
	// every bucket even when its user already had policies of its own, so the
	// documented flow of creating a user, attaching ReadOnlyAccess and issuing a
	// key produced a key that could write anywhere. Such a user's key now gets
	// exactly the user's policies. Every bucket stays the default only for a
	// user with no policies, where the key would otherwise reach nothing.
	access := keyAccessAll
	switch {
	case len(reqBody.Buckets) > 0:
		access = keyAccessBuckets
	case reqBody.AllBuckets:
		access = keyAccessAll
	default:
		var own []metadata.IAMPolicy
		if userExists {
			var err error
			if own, err = h.store.GetUserPolicies(reqBody.UserID); err != nil {
				writeError(w, http.StatusInternalServerError, "failed to read the user's policies")
				return
			}
		}
		if len(own) > 0 {
			access = keyAccessUser
		} else if reqBody.UserPoliciesOnly {
			writeError(w, http.StatusBadRequest, fmt.Sprintf(
				"user %q has no policies, so a key limited to them could reach nothing. Attach a policy first", reqBody.UserID))
			return
		}
	}

	// Auto-create the IAM user if it doesn't exist. It is marked as existing only
	// for its keys, so deleting its last key removes it again. The name gets the
	// same rule as POST /iam/users, which this used to bypass.
	if !userExists {
		if err := validateIAMName("user", reqBody.UserID, maxIAMUserNameLen); err != nil {
			writeError(w, http.StatusBadRequest, "userId: "+err.Error())
			return
		}
		if err := h.store.CreateIAMUser(metadata.IAMUser{
			Name:       reqBody.UserID,
			CreatedAt:  now,
			KeyManaged: true,
		}); err != nil {
			writeError(w, http.StatusInternalServerError, "failed to create IAM user")
			return
		}
	}

	resp := keyCreateResponse{
		AccessKey: accessKey,
		SecretKey: secretKey,
		CreatedAt: now.Format(time.RFC3339),
		Access:    access,
	}
	key := metadata.AccessKey{
		AccessKey: accessKey,
		SecretKey: secretKey,
		CreatedAt: now,
		UserID:    reqBody.UserID,
	}
	if access == keyAccessUser {
		if err := h.store.CreateAccessKey(key); err != nil {
			writeError(w, http.StatusInternalServerError, "failed to create access key")
			return
		}
		writeJSON(w, http.StatusCreated, resp)
		return
	}

	// Build policy: scoped to specific buckets or full access
	var policyDoc string
	if access == keyAccessBuckets {
		// Scoped: only allow access to specified buckets
		resources := make([]string, 0, len(reqBody.Buckets)*2)
		for _, bucket := range reqBody.Buckets {
			resources = append(resources, fmt.Sprintf("arn:aws:s3:::%s", bucket))
			resources = append(resources, fmt.Sprintf("arn:aws:s3:::%s/*", bucket))
		}
		policyDoc = allowS3On(resources)
	} else {
		// No buckets specified: full S3 access
		policyDoc = allowS3On([]string{"*"})
	}

	// The grant belongs to this key alone. It used to be one policy per USER,
	// attached to the user and rewritten on every issue, so issuing a second key
	// silently changed what the first could reach: a key scoped to one bucket
	// lost it when the next was scoped to another, and was widened to every
	// bucket when the next was issued with full access.
	policyName := keyPolicyName(accessKey)
	if err := h.store.CreateIAMPolicy(metadata.IAMPolicy{
		Name:      policyName,
		CreatedAt: now,
		Document:  policyDoc,
	}); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to create policy")
		return
	}

	key.PolicyName = policyName
	if err := h.store.CreateAccessKey(key); err != nil {
		_ = h.store.DeleteIAMPolicy(policyName)
		writeError(w, http.StatusInternalServerError, "failed to create access key")
		return
	}

	writeJSON(w, http.StatusCreated, resp)
}

func (h *APIHandler) handleDeleteKey(w http.ResponseWriter, _ *http.Request, accessKey string) {
	if adminAK, _ := h.adminCredentials(); accessKey == adminAK {
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
	h.deleteSTSArtifacts(*key)

	if key.PolicyName != "" {
		_ = h.store.DeleteIAMPolicy(key.PolicyName)
	}

	// Remove the user with its last key only when the key is the reason it
	// exists. This used to remove the user unconditionally, so a user created on
	// purpose, with policies attached, vanished when its last key was deleted.
	if key.UserID != "" && !h.userHasOtherKeys(key.UserID, accessKey) {
		if user, err := h.store.GetIAMUser(key.UserID); err == nil {
			legacy := legacyKeyPolicyName(key.UserID)
			if keyManaged(user) {
				_ = h.store.DeleteIAMUser(key.UserID)
			} else if detachPolicy(user, legacy) {
				// The shared grant of keys issued before 4.4.79 goes with the
				// last of them. The user and its own policies stay.
				_ = h.store.UpdateIAMUser(*user)
			}
			_ = h.store.DeleteIAMPolicy(legacy)
		}
	}

	w.WriteHeader(http.StatusNoContent)
}

// keyPolicyName names the policy that holds one key's grant.
func keyPolicyName(accessKey string) string {
	return "access-key-" + accessKey
}

// legacyKeyPolicyName is the single per-user policy that keys issued before
// 4.4.79 shared, attached to the user itself.
func legacyKeyPolicyName(user string) string {
	return "key-policy-" + user
}

func allowS3On(resources []string) string {
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
	return string(doc)
}

func (h *APIHandler) userHasOtherKeys(user, except string) bool {
	keys, err := h.store.ListAccessKeys()
	if err != nil {
		// Unknown is not "none": keeping a user is recoverable, removing one is not.
		return true
	}
	for _, k := range keys {
		if k.AccessKey != except && k.UserID == user {
			return true
		}
	}
	return false
}

// keyManaged reports whether a user exists only for its access keys. Users
// created before 4.4.79 carry no marker, so one is recognised by the only shape
// key issuance ever gave it: the shared key policy and nothing else.
func keyManaged(u *metadata.IAMUser) bool {
	if u.KeyManaged {
		return true
	}
	return len(u.PolicyARNs) == 1 && u.PolicyARNs[0] == legacyKeyPolicyName(u.Name) &&
		len(u.Groups) == 0 && len(u.AllowedCIDRs) == 0
}

// detachPolicy removes a policy from a user's list and reports whether it was
// there.
func detachPolicy(u *metadata.IAMUser, name string) bool {
	kept := u.PolicyARNs[:0:0]
	for _, p := range u.PolicyARNs {
		if p != name {
			kept = append(kept, p)
		}
	}
	found := len(kept) != len(u.PolicyARNs)
	u.PolicyARNs = kept
	return found
}

// splitLegacyKeyPolicy moves a user's keys off the shared per-user policy that
// keys issued before 4.4.79 used. Each existing key gets its own copy of that
// policy, so it keeps exactly the access it had, and the shared policy is then
// detached, so the key about to be issued does not inherit it.
//
// The order is what makes a failure part way safe. Until the last step the
// shared policy is still attached, and every key still has at least the access
// it had before.
func (h *APIHandler) splitLegacyKeyPolicy(user string, now time.Time) error {
	u, err := h.store.GetIAMUser(user)
	if err != nil {
		return err
	}
	legacyName := legacyKeyPolicyName(user)
	attached := false
	for _, p := range u.PolicyARNs {
		if p == legacyName {
			attached = true
			break
		}
	}
	if !attached {
		return nil
	}
	// The policy can be listed on the user after it was deleted by hand. The old
	// keys had no grant from it then, so there is nothing to copy, only the stale
	// entry to drop. Failing here instead refused every new key for the user.
	legacy, err := h.store.GetIAMPolicy(legacyName)
	if err != nil {
		// Only a policy that is really gone counts as gone. An unreadable one
		// stops the conversion, so the old keys keep their grant.
		all, lerr := h.store.ListIAMPolicies()
		if lerr != nil {
			return err
		}
		for _, p := range all {
			if p.Name == legacyName {
				return err
			}
		}
		legacy = nil
	}
	keys, err := h.store.ListAccessKeys()
	if err != nil {
		return err
	}
	for _, k := range keys {
		if legacy == nil || k.UserID != user || k.PolicyName != "" {
			continue
		}
		name := keyPolicyName(k.AccessKey)
		pol := metadata.IAMPolicy{Name: name, CreatedAt: now, Document: legacy.Document}
		if err := h.store.CreateIAMPolicy(pol); err != nil {
			// Left behind by an earlier attempt that failed part way.
			if _, gerr := h.store.GetIAMPolicy(name); gerr != nil {
				return err
			}
			if err := h.store.UpdateIAMPolicy(pol); err != nil {
				return err
			}
		}
		k.PolicyName = name
		if err := h.store.CreateAccessKey(k); err != nil {
			return err
		}
	}

	u.KeyManaged = keyManaged(u)
	detachPolicy(u, legacyName)
	if err := h.store.UpdateIAMUser(*u); err != nil {
		return err
	}
	return h.store.DeleteIAMPolicy(legacyName)
}

// maskSecret shows just enough of a secret to tell two apart. It used to show
// the first and last four characters, eight in all, which for the admin secret
// (eight characters is the minimum) was the whole of it. Now at most two from
// each end, and nothing at all for a secret shorter than 16.
func maskSecret(secret string) string {
	if len(secret) < 16 {
		return "****"
	}
	return secret[:2] + "****" + secret[len(secret)-2:]
}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
