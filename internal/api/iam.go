package api

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
)

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
			w.WriteHeader(http.StatusOK)
			return
		}
	}

	user.PolicyARNs = append(user.PolicyARNs, req.PolicyName)
	if err := h.store.UpdateIAMUser(*user); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusOK)
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
			w.WriteHeader(http.StatusOK)
			return
		}
	}

	user.Groups = append(user.Groups, req.GroupName)
	if err := h.store.UpdateIAMUser(*user); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusOK)
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
			w.WriteHeader(http.StatusOK)
			return
		}
	}

	group.PolicyARNs = append(group.PolicyARNs, req.PolicyName)
	if err := h.store.UpdateIAMGroup(*group); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusOK)
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

	// Validate document is valid JSON
	var js json.RawMessage
	if err := json.Unmarshal([]byte(req.Document), &js); err != nil {
		writeError(w, http.StatusBadRequest, "document must be valid JSON")
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
	w.WriteHeader(http.StatusOK)
}
