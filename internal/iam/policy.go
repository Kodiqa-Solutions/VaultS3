package iam

import (
	"bytes"
	"encoding/json"
	"strings"
)

// Policy represents an IAM policy document.
type Policy struct {
	Version   string      `json:"Version"`
	Statement []Statement `json:"Statement"`
}

// Statement represents a single policy statement.
type Statement struct {
	Effect      string                         `json:"Effect"`
	Action      stringOrSlice                  `json:"Action"`
	Resource    stringOrSlice                  `json:"Resource"`
	Condition   map[string]map[string][]string `json:"Condition,omitempty"`
	NotAction   stringOrSlice                  `json:"NotAction,omitempty"`
	NotResource stringOrSlice                  `json:"NotResource,omitempty"`
}

// stringOrSlice accepts both forms AWS IAM allows for Action and Resource: a
// bare string ("s3:GetObject") and an array (["s3:GetObject"]). Most AWS
// documentation examples use the bare string, so policies are routinely written
// that way and pasted into other S3 implementations.
//
// These fields used to be plain []string, so a bare string failed to unmarshal.
// The caller that loads a user's policies discards a policy it cannot parse
// without logging anything, so such a policy was accepted at creation, stored,
// listed back intact, and then silently ignored at every authorization decision.
// A Deny written that way protected nothing, which is the same failure shape as
// conditions being ignored (security assessment finding 12).
type stringOrSlice []string

func (s *stringOrSlice) UnmarshalJSON(b []byte) error {
	trimmed := bytes.TrimSpace(b)
	if len(trimmed) > 0 && trimmed[0] == '"' {
		var one string
		if err := json.Unmarshal(trimmed, &one); err != nil {
			return err
		}
		*s = stringOrSlice{one}
		return nil
	}
	var many []string
	if err := json.Unmarshal(trimmed, &many); err != nil {
		return err
	}
	*s = many
	return nil
}

// Evaluate checks all statements against an action and resource.
// Returns true if access is allowed, false if denied.
// Logic: explicit Deny wins, then explicit Allow, else default deny.
// Evaluate checks policies with no request context.
//
// It delegates to EvaluateWithContext so that NotAction, NotResource and
// Condition are honoured rather than silently ignored, which is what this used
// to do: a policy that allowed access only from one IP range, or that denied an
// action under a condition, was evaluated as unconditional. Operators who
// believed they had locked access down had no such protection (security
// assessment finding 12).
//
// With no context, a statement carrying a Condition cannot be shown to apply, so
// EvaluateWithContext fails it closed: an Allow that depends on an unproven
// condition does not grant, and a Deny that depends on one does not block a
// request the caller could not have proven anyway. Callers that can supply
// context should use EvaluateWithContext directly.
func Evaluate(policies []Policy, action, resource string) bool {
	return EvaluateWithContext(policies, action, resource, nil)
}

// matchesAny checks if the value matches any of the patterns.
func matchesAny(patterns []string, value string) bool {
	for _, p := range patterns {
		if matchWildcard(p, value) {
			return true
		}
	}
	return false
}

// MatchWildcard matches an IAM-style pattern against a value, so bucket-policy
// evaluation (which lives in the metadata store) uses exactly the same matching
// rules as identity-policy evaluation instead of a second, subtly different copy.
func MatchWildcard(pattern, value string) bool { return matchWildcard(pattern, value) }

// matchWildcard matches a pattern against a value.
// Supports "*" (matches any sequence of characters) and "?" (matches any single character)
// at any position in the pattern.
func matchWildcard(pattern, value string) bool {
	// Fast path for common cases
	if pattern == "*" {
		return true
	}
	if !strings.ContainsAny(pattern, "*?") {
		return pattern == value
	}
	// DP-based wildcard matching
	return wildcardMatch(pattern, value)
}

// wildcardMatch implements full wildcard matching with * and ? at any position.
// Special case: "prefix/*" also matches "prefix" itself (for resource ARN matching).
func wildcardMatch(pattern, value string) bool {
	// Special case: arn:aws:s3:::bucket/* should match arn:aws:s3:::bucket
	if strings.HasSuffix(pattern, "/*") {
		base := strings.TrimSuffix(pattern, "/*")
		if value == base {
			return true
		}
	}
	p, v := 0, 0
	starP, starV := -1, -1
	for v < len(value) {
		if p < len(pattern) && (pattern[p] == '?' || pattern[p] == value[v]) {
			p++
			v++
		} else if p < len(pattern) && pattern[p] == '*' {
			starP = p
			starV = v
			p++
		} else if starP >= 0 {
			starV++
			v = starV
			p = starP + 1
		} else {
			return false
		}
	}
	for p < len(pattern) && pattern[p] == '*' {
		p++
	}
	return p == len(pattern)
}
