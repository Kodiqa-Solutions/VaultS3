package iam

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// Policy represents an IAM policy document.
type Policy struct {
	Version   string        `json:"Version"`
	Statement statementList `json:"Statement"`
}

// statementList accepts both forms AWS allows for Statement: an array of
// statements and a single statement object. The single-object form is valid
// IAM and appears in AWS examples, but this field used to be a plain slice, so
// such a policy failed to unmarshal and the identity it was attached to was
// denied everything, with nothing in the policy itself looking wrong.
type statementList []Statement

func (s *statementList) UnmarshalJSON(b []byte) error {
	trimmed := bytes.TrimSpace(b)
	if len(trimmed) > 0 && trimmed[0] == '{' {
		var one Statement
		if err := json.Unmarshal(trimmed, &one); err != nil {
			return err
		}
		*s = statementList{one}
		return nil
	}
	var many []Statement
	if err := json.Unmarshal(trimmed, &many); err != nil {
		return err
	}
	*s = many
	return nil
}

// Statement represents a single policy statement.
type Statement struct {
	Effect      string         `json:"Effect"`
	Action      stringOrSlice  `json:"Action"`
	Resource    stringOrSlice  `json:"Resource"`
	Condition   ConditionBlock `json:"Condition,omitempty"`
	NotAction   stringOrSlice  `json:"NotAction,omitempty"`
	NotResource stringOrSlice  `json:"NotResource,omitempty"`
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

// ConditionBlock is a statement's Condition: operator, then condition key, then
// the values it is compared with. AWS lets a single value be written as a bare
// string ("aws:SourceIp": "203.0.113.0/24") rather than a one-element array, and
// most AWS examples do exactly that. This was a plain map of slices, so the
// bare-string form failed to parse and the whole policy failed with it, which
// denies the identity everything rather than applying the condition.
type ConditionBlock map[string]map[string][]string

func (c *ConditionBlock) UnmarshalJSON(b []byte) error {
	var raw map[string]map[string]conditionValues
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	out := make(ConditionBlock, len(raw))
	for op, kvs := range raw {
		inner := make(map[string][]string, len(kvs))
		for k, v := range kvs {
			inner[k] = []string(v)
		}
		out[op] = inner
	}
	*c = out
	return nil
}

// conditionValues is one condition key's values. Besides the string and array
// forms, AWS accepts a bare boolean or number ("aws:SecureTransport": false,
// "s3:max-keys": 10), which compare as their JSON text.
type conditionValues []string

func (v *conditionValues) UnmarshalJSON(b []byte) error {
	trimmed := bytes.TrimSpace(b)
	if len(trimmed) > 0 && trimmed[0] == '[' {
		var many []json.RawMessage
		if err := json.Unmarshal(trimmed, &many); err != nil {
			return err
		}
		out := make([]string, 0, len(many))
		for _, m := range many {
			one, err := conditionScalar(m)
			if err != nil {
				return err
			}
			out = append(out, one)
		}
		*v = out
		return nil
	}
	one, err := conditionScalar(trimmed)
	if err != nil {
		return err
	}
	*v = conditionValues{one}
	return nil
}

func conditionScalar(b json.RawMessage) (string, error) {
	b = bytes.TrimSpace(b)
	if len(b) > 0 && b[0] == '"' {
		var s string
		err := json.Unmarshal(b, &s)
		return s, err
	}
	var any interface{}
	if err := json.Unmarshal(b, &any); err != nil {
		return "", err
	}
	switch any.(type) {
	case bool, float64:
		return string(b), nil
	}
	return "", fmt.Errorf("iam: a condition value must be a string, number or boolean, got %s", b)
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
