package api

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/Kodiqa-Solutions/VaultS3/internal/iam"
	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
)

// AWS limits for IAM names. Users and groups take up to 64 characters,
// policies up to 128, all from the same set.
const (
	maxIAMUserNameLen   = 64
	maxIAMPolicyNameLen = 128
)

var iamNameRe = regexp.MustCompile(`^[\w+=,.@-]+$`)

// validateIAMName applies the AWS naming rule to a new user, group or policy.
// Names used to be taken as given, so one containing "/" was created and then
// could never be addressed again, since the API splits its paths on it, and one
// of any length was stored. It applies only at creation: an existing object
// with another name must stay deletable.
func validateIAMName(kind, name string, max int) error {
	if name == "" {
		return fmt.Errorf("%s name is required", kind)
	}
	if len(name) > max {
		return fmt.Errorf("%s name must be at most %d characters", kind, max)
	}
	if !iamNameRe.MatchString(name) {
		return fmt.Errorf("%s name may contain only letters, digits and + = , . @ _ -", kind)
	}
	return nil
}

// validatePolicyDocument refuses a document the authorizer could not use. It
// used to check only that the text was JSON, so a document that did not decode
// into a policy was stored and attached, and from then on every user holding it
// was denied everything, because a policy that fails to parse fails the whole
// identity closed. Decoding into iam.Policy here means this check accepts
// exactly what the authorizer accepts.
func validatePolicyDocument(doc string) error {
	var pol iam.Policy
	if err := json.Unmarshal([]byte(doc), &pol); err != nil {
		return fmt.Errorf("document is not a valid IAM policy: %v", err)
	}
	if len(pol.Statement) == 0 {
		return fmt.Errorf("document has no Statement")
	}
	for i, st := range pol.Statement {
		if st.Effect != "Allow" && st.Effect != "Deny" {
			return fmt.Errorf("statement %d: Effect must be Allow or Deny, got %q", i+1, st.Effect)
		}
	}
	return nil
}

// IAM Users

type iamUserResponse struct {
	Name         string   `json:"name"`
	CreatedAt    string   `json:"createdAt"`
	PolicyARNs   []string `json:"policyArns"`
	Groups       []string `json:"groups"`
	AllowedCIDRs []string `json:"allowedCidrs"`
}

func (h *APIHandler) handleListIAMUsers(w http.ResponseWriter, _ *http.Request) {
	users, err := h.store.ListIAMUsers()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list users")
		return
	}

	items := make([]iamUserResponse, 0, len(users))
	for _, u := range users {
		if isSTSSyntheticUser(u) {
			continue
		}
		policyARNs := u.PolicyARNs
		if policyARNs == nil {
			policyARNs = []string{}
		}
		groups := u.Groups
		if groups == nil {
			groups = []string{}
		}
		cidrs := u.AllowedCIDRs
		if cidrs == nil {
			cidrs = []string{}
		}
		items = append(items, iamUserResponse{
			Name:         u.Name,
			CreatedAt:    u.CreatedAt.Format(time.RFC3339),
			PolicyARNs:   policyARNs,
			Groups:       groups,
			AllowedCIDRs: cidrs,
		})
	}
	writeJSON(w, http.StatusOK, items)
}

func (h *APIHandler) handleCreateIAMUser(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if err := readJSON(r, &req); err != nil || req.Name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	if err := validateIAMName("user", req.Name, maxIAMUserNameLen); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	user := metadata.IAMUser{
		Name:      req.Name,
		CreatedAt: time.Now().UTC(),
	}
	if err := h.store.CreateIAMUser(user); err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}

	writeJSON(w, http.StatusCreated, iamUserResponse{
		Name:         user.Name,
		CreatedAt:    user.CreatedAt.Format(time.RFC3339),
		PolicyARNs:   []string{},
		Groups:       []string{},
		AllowedCIDRs: []string{},
	})
}

func (h *APIHandler) handleGetIAMUser(w http.ResponseWriter, _ *http.Request, name string) {
	user, err := h.store.GetIAMUser(name)
	if err != nil {
		writeError(w, http.StatusNotFound, "user not found")
		return
	}

	policyARNs := user.PolicyARNs
	if policyARNs == nil {
		policyARNs = []string{}
	}
	userGroups := user.Groups
	if userGroups == nil {
		userGroups = []string{}
	}
	cidrs := user.AllowedCIDRs
	if cidrs == nil {
		cidrs = []string{}
	}
	writeJSON(w, http.StatusOK, iamUserResponse{
		Name:         user.Name,
		CreatedAt:    user.CreatedAt.Format(time.RFC3339),
		PolicyARNs:   policyARNs,
		Groups:       userGroups,
		AllowedCIDRs: cidrs,
	})
}

func (h *APIHandler) handleDeleteIAMUser(w http.ResponseWriter, _ *http.Request, name string) {
	// A user's access keys go with it. Left behind, they were refused only while
	// no user had the name: a new user created with it later brought them back,
	// each with the grant it was issued with. Sessions issued from the user go
	// too. Keys are removed first, so a failure part way leaves the user in place
	// to delete again rather than keys without a user.
	keys, err := h.store.ListAccessKeys()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list the user's access keys")
		return
	}
	for _, k := range keys {
		if k.UserID != name && k.SourceUserID != name {
			continue
		}
		if err := h.store.DeleteAccessKey(k.AccessKey); err != nil {
			writeError(w, http.StatusInternalServerError, "failed to delete the user's access key "+k.AccessKey)
			return
		}
		h.deleteSTSArtifacts(k)
		if k.PolicyName != "" {
			_ = h.store.DeleteIAMPolicy(k.PolicyName)
		}
	}
	_ = h.store.DeleteIAMPolicy(legacyKeyPolicyName(name))
	if err := h.store.DeleteIAMUser(name); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to delete user")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *APIHandler) handleAttachUserPolicy(w http.ResponseWriter, r *http.Request, userName string) {
	var req struct {
		PolicyName string `json:"policyName"`
	}
	if err := readJSON(r, &req); err != nil || req.PolicyName == "" {
		writeError(w, http.StatusBadRequest, "policyName is required")
		return
	}

	user, err := h.store.GetIAMUser(userName)
	if err != nil {
		writeError(w, http.StatusNotFound, "user not found")
		return
	}

	// Check policy exists
	if _, err := h.store.GetIAMPolicy(req.PolicyName); err != nil {
		writeError(w, http.StatusNotFound, "policy not found")
		return
	}

	// Avoid duplicates
	for _, p := range user.PolicyARNs {
		if p == req.PolicyName {
			w.WriteHeader(http.StatusNoContent)
			return
		}
	}

	user.PolicyARNs = append(user.PolicyARNs, req.PolicyName)
	if err := h.store.UpdateIAMUser(*user); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *APIHandler) handleDetachUserPolicy(w http.ResponseWriter, _ *http.Request, userName, policyName string) {
	user, err := h.store.GetIAMUser(userName)
	if err != nil {
		writeError(w, http.StatusNotFound, "user not found")
		return
	}

	filtered := make([]string, 0, len(user.PolicyARNs))
	for _, p := range user.PolicyARNs {
		if p != policyName {
			filtered = append(filtered, p)
		}
	}
	user.PolicyARNs = filtered

	if err := h.store.UpdateIAMUser(*user); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *APIHandler) handleAddUserToGroup(w http.ResponseWriter, r *http.Request, userName string) {
	var req struct {
		GroupName string `json:"groupName"`
	}
	if err := readJSON(r, &req); err != nil || req.GroupName == "" {
		writeError(w, http.StatusBadRequest, "groupName is required")
		return
	}

	user, err := h.store.GetIAMUser(userName)
	if err != nil {
		writeError(w, http.StatusNotFound, "user not found")
		return
	}

	if _, err := h.store.GetIAMGroup(req.GroupName); err != nil {
		writeError(w, http.StatusNotFound, "group not found")
		return
	}

	for _, g := range user.Groups {
		if g == req.GroupName {
			w.WriteHeader(http.StatusNoContent)
			return
		}
	}

	user.Groups = append(user.Groups, req.GroupName)
	if err := h.store.UpdateIAMUser(*user); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *APIHandler) handleRemoveUserFromGroup(w http.ResponseWriter, _ *http.Request, userName, groupName string) {
	user, err := h.store.GetIAMUser(userName)
	if err != nil {
		writeError(w, http.StatusNotFound, "user not found")
		return
	}

	filtered := make([]string, 0, len(user.Groups))
	for _, g := range user.Groups {
		if g != groupName {
			filtered = append(filtered, g)
		}
	}
	user.Groups = filtered

	if err := h.store.UpdateIAMUser(*user); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// IAM Groups

type iamGroupResponse struct {
	Name       string   `json:"name"`
	CreatedAt  string   `json:"createdAt"`
	PolicyARNs []string `json:"policyArns"`
}

func (h *APIHandler) handleListIAMGroups(w http.ResponseWriter, _ *http.Request) {
	groups, err := h.store.ListIAMGroups()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list groups")
		return
	}

	items := make([]iamGroupResponse, 0, len(groups))
	for _, g := range groups {
		policyARNs := g.PolicyARNs
		if policyARNs == nil {
			policyARNs = []string{}
		}
		items = append(items, iamGroupResponse{
			Name:       g.Name,
			CreatedAt:  g.CreatedAt.Format(time.RFC3339),
			PolicyARNs: policyARNs,
		})
	}
	writeJSON(w, http.StatusOK, items)
}

func (h *APIHandler) handleCreateIAMGroup(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if err := readJSON(r, &req); err != nil || req.Name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	if err := validateIAMName("group", req.Name, maxIAMUserNameLen); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	group := metadata.IAMGroup{
		Name:      req.Name,
		CreatedAt: time.Now().UTC(),
	}
	if err := h.store.CreateIAMGroup(group); err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}

	writeJSON(w, http.StatusCreated, iamGroupResponse{
		Name:       group.Name,
		CreatedAt:  group.CreatedAt.Format(time.RFC3339),
		PolicyARNs: []string{},
	})
}

func (h *APIHandler) handleDeleteIAMGroup(w http.ResponseWriter, _ *http.Request, name string) {
	// Members are taken out of the group first. The name used to stay on every
	// member, so a group created later with the same name silently granted its
	// policies to people nobody had added to it. Done before the group goes, so
	// a failure part way leaves the group in place to delete again.
	users, err := h.store.ListIAMUsers()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "could not list the group's members, retry")
		return
	}
	for _, u := range users {
		kept := make([]string, 0, len(u.Groups))
		for _, g := range u.Groups {
			if g != name {
				kept = append(kept, g)
			}
		}
		if len(kept) == len(u.Groups) {
			continue
		}
		u.Groups = kept
		if err := h.store.UpdateIAMUser(u); err != nil {
			writeError(w, http.StatusInternalServerError, "failed to remove user "+u.Name+" from the group")
			return
		}
	}
	if err := h.store.DeleteIAMGroup(name); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to delete group")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *APIHandler) handleAttachGroupPolicy(w http.ResponseWriter, r *http.Request, groupName string) {
	var req struct {
		PolicyName string `json:"policyName"`
	}
	if err := readJSON(r, &req); err != nil || req.PolicyName == "" {
		writeError(w, http.StatusBadRequest, "policyName is required")
		return
	}

	group, err := h.store.GetIAMGroup(groupName)
	if err != nil {
		writeError(w, http.StatusNotFound, "group not found")
		return
	}

	if _, err := h.store.GetIAMPolicy(req.PolicyName); err != nil {
		writeError(w, http.StatusNotFound, "policy not found")
		return
	}

	for _, p := range group.PolicyARNs {
		if p == req.PolicyName {
			w.WriteHeader(http.StatusNoContent)
			return
		}
	}

	group.PolicyARNs = append(group.PolicyARNs, req.PolicyName)
	if err := h.store.UpdateIAMGroup(*group); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *APIHandler) handleDetachGroupPolicy(w http.ResponseWriter, _ *http.Request, groupName, policyName string) {
	group, err := h.store.GetIAMGroup(groupName)
	if err != nil {
		writeError(w, http.StatusNotFound, "group not found")
		return
	}

	filtered := make([]string, 0, len(group.PolicyARNs))
	for _, p := range group.PolicyARNs {
		if p != policyName {
			filtered = append(filtered, p)
		}
	}
	group.PolicyARNs = filtered

	if err := h.store.UpdateIAMGroup(*group); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// IAM Policies

type iamPolicyResponse struct {
	Name      string `json:"name"`
	CreatedAt string `json:"createdAt"`
	Document  string `json:"document"`
}

func (h *APIHandler) handleListIAMPolicies(w http.ResponseWriter, _ *http.Request) {
	policies, err := h.store.ListIAMPolicies()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list policies")
		return
	}

	items := make([]iamPolicyResponse, 0, len(policies))
	for _, p := range policies {
		items = append(items, iamPolicyResponse{
			Name:      p.Name,
			CreatedAt: p.CreatedAt.Format(time.RFC3339),
			Document:  p.Document,
		})
	}
	writeJSON(w, http.StatusOK, items)
}

func (h *APIHandler) handleCreateIAMPolicy(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name     string `json:"name"`
		Document string `json:"document"`
	}
	if err := readJSON(r, &req); err != nil || req.Name == "" || req.Document == "" {
		writeError(w, http.StatusBadRequest, "name and document are required")
		return
	}

	if err := validateIAMName("policy", req.Name, maxIAMPolicyNameLen); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := validatePolicyDocument(req.Document); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	policy := metadata.IAMPolicy{
		Name:      req.Name,
		CreatedAt: time.Now().UTC(),
		Document:  req.Document,
	}
	if err := h.store.CreateIAMPolicy(policy); err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}

	writeJSON(w, http.StatusCreated, iamPolicyResponse{
		Name:      policy.Name,
		CreatedAt: policy.CreatedAt.Format(time.RFC3339),
		Document:  policy.Document,
	})
}

func (h *APIHandler) handleDeleteIAMPolicy(w http.ResponseWriter, _ *http.Request, name string) {
	// An access key's own policy is removed with the key. Deleting it on its own
	// would leave the key listed and working for signing while it can reach
	// nothing, with no sign of why.
	// A key list that cannot be read is not an empty one: the check used to be
	// skipped then, which deleted a key's own policy whenever the store hiccuped.
	keys, err := h.store.ListAccessKeys()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "could not check whether an access key owns this policy, retry")
		return
	}
	for _, k := range keys {
		if k.PolicyName == name {
			writeError(w, http.StatusConflict,
				fmt.Sprintf("policy %q belongs to access key %s, delete the key instead", name, k.AccessKey))
			return
		}
	}
	if err := h.store.DeleteIAMPolicy(name); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to delete policy")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// IP Restrictions

func (h *APIHandler) handleSetIPRestrictions(w http.ResponseWriter, r *http.Request, userName string) {
	var req struct {
		AllowedCIDRs []string `json:"allowedCidrs"`
	}
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	// A CIDR that does not parse used to be stored as given and then skipped by
	// the matcher, so a typo in the only entry left the user locked out of
	// everything, and one among several silently allowed less than intended.
	for _, c := range req.AllowedCIDRs {
		if _, _, err := net.ParseCIDR(strings.TrimSpace(c)); err != nil {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("%q is not a CIDR such as 203.0.113.0/24", c))
			return
		}
	}

	user, err := h.store.GetIAMUser(userName)
	if err != nil {
		writeError(w, http.StatusNotFound, "user not found")
		return
	}

	user.AllowedCIDRs = req.AllowedCIDRs
	if err := h.store.UpdateIAMUser(*user); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
