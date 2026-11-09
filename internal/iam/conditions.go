package iam

import (
	"net"
	"strings"
	"time"
)

// EvaluateWithContext checks policies with additional context for condition evaluation.
func EvaluateWithContext(policies []Policy, action, resource string, ctx map[string]string) bool {
	allowed, _ := EvaluateDetailed(policies, action, resource, ctx)
	return allowed
}

// EvaluateDetailed is EvaluateWithContext plus the reason a request was refused:
// explicitDeny distinguishes "a statement said Deny" from "nothing said Allow".
// Both refuse, but only the first is a decision the operator actually wrote, and
// an external authorizer running in authoritative mode must not be able to
// overturn one (explicit Deny wins, issue #41).
//
// The two functions share this one body deliberately. The Action/Resource
// matching rules already exist in exactly one place for the same reason: a
// second, subtly different copy is how a Deny stops protecting anything.
func EvaluateDetailed(policies []Policy, action, resource string, ctx map[string]string) (allowed, explicitDeny bool) {
	hasAllow := false

	for _, pol := range policies {
		for _, stmt := range pol.Statement {
			// NotAction: matches if action is NOT in the list
			if len(stmt.NotAction) > 0 {
				if matchesAny(stmt.NotAction, action) {
					continue // NotAction matched means this statement doesn't apply
				}
			} else if !matchesAny(stmt.Action, action) {
				continue
			}

			// Resource and NotResource may name policy variables
			// (${aws:username}), resolved from ctx. One that cannot be
			// resolved leaves the statement undetermined: a Deny still
			// applies, an Allow does not grant. Variables were never
			// substituted, so a Deny written with one never applied and a
			// home-folder Allow granted the literal "${aws:username}" folder
			// to every user holding it.
			deny := stmt.Effect == "Deny"
			if len(stmt.NotResource) > 0 {
				if m, known := matchesAnyVars(stmt.NotResource, resource, ctx); !known {
					if !deny {
						continue
					}
				} else if m {
					continue
				}
			} else if m, known := matchesAnyVars(stmt.Resource, resource, ctx); !known {
				if !deny {
					continue
				}
			} else if !m {
				continue
			}

			// Evaluate conditions. Undetermined ones resolve conservatively:
			// a Deny still blocks, an Allow does not grant.
			if len(stmt.Condition) > 0 {
				ok, determined := evaluateConditions(stmt.Condition, ctx)
				switch {
				case !determined:
					if stmt.Effect == "Deny" {
						return false, true
					}
					continue
				case !ok:
					continue
				}
			}

			if stmt.Effect == "Deny" {
				return false, true
			}
			if stmt.Effect == "Allow" {
				hasAllow = true
			}
		}
	}

	return hasAllow, false
}

// evaluateConditions checks all condition operators.
// evaluateConditions reports whether every condition holds, and whether that
// could be determined at all.
//
// A condition on a key the caller supplied no value for is UNDETERMINED, not
// false. The difference decides the safe answer: an Allow that depends on an
// unproven condition must not grant, and a Deny that depends on one must still
// block. Collapsing both to "false" would have let a conditional Deny be
// bypassed simply by evaluating it without context.
func evaluateConditions(conditions map[string]map[string][]string, ctx map[string]string) (ok, determined bool) {
	for operator, kvs := range conditions {
		// An operator this evaluator does not implement cannot be shown to hold
		// or to fail. It used to count as failed, which is right for an Allow but
		// let a Deny written with such an operator never apply at all.
		if !knownOperator(operator) {
			return false, false
		}
		for key, values := range kvs {
			ctxVal, present := contextValue(ctx, key)
			if !present {
				return false, false
			}
			values, resolved := substituteValues(values, ctx)
			if !resolved {
				return false, false
			}
			if !evaluateOperator(operator, ctxVal, values) {
				return false, true
			}
		}
	}
	return true, true
}

// ConditionsHold evaluates a statement's Condition block against a request
// context. determined is false when the answer cannot be known, because a key
// has no value in ctx or an operator is not implemented. A caller must then
// refuse an Allow and enforce a Deny. It is the evaluator IAM policies use,
// shared so that bucket policies cannot decide conditions differently.
func ConditionsHold(conditions map[string]map[string][]string, ctx map[string]string) (ok, determined bool) {
	return evaluateConditions(conditions, ctx)
}

// ConditionsHoldMissingFalse evaluates a Condition block the way AWS does
// for a bucket policy: a key the request has no value for makes a positive
// operator false and a negated one (StringNotEquals, StringNotLike,
// NotIpAddress) true, an ...IfExists operator holds, and Null tests whether
// the key is missing. supported is false for an operator this evaluator does
// not implement; the caller then ignores the statement.
//
// It is for a bucket policy Deny on a signed request. Treating a missing key
// as "cannot tell, so deny", as an IAM user policy Deny is treated, refused
// every request to a bucket whose policy denied "aws:SecureTransport": "false"
// with a condition on a key some requests lack (aws:Referer, s3:prefix), and
// an unknown operator locked the bucket for everyone.
func ConditionsHoldMissingFalse(conditions map[string]map[string][]string, ctx map[string]string) (holds, supported bool) {
	for operator, kvs := range conditions {
		base, ifExists := strings.CutSuffix(operator, "IfExists")
		if base != "Null" && !knownOperator(base) {
			return false, false
		}
		for key, values := range kvs {
			ctxVal, present := contextValue(ctx, key)
			if base == "Null" {
				want := len(values) > 0 && strings.EqualFold(values[0], "true")
				if want == present {
					return false, true
				}
				continue
			}
			if !present {
				if ifExists || negatedOperator(base) {
					continue
				}
				return false, true
			}
			resolved, ok := substituteValues(values, ctx)
			if !ok {
				// A variable with no value matches nothing.
				if negatedOperator(base) {
					continue
				}
				return false, true
			}
			if !evaluateOperator(base, ctxVal, resolved) {
				return false, true
			}
		}
	}
	return true, true
}

// contextValue looks a condition key up in the request context. AWS
// condition keys are not case-sensitive: "aws:securetransport" is
// aws:SecureTransport. Looked up exactly, a Deny on it never matched.
func contextValue(ctx map[string]string, key string) (string, bool) {
	if v, ok := ctx[key]; ok {
		return v, true
	}
	for k, v := range ctx {
		if strings.EqualFold(k, key) {
			return v, true
		}
	}
	return "", false
}

func negatedOperator(operator string) bool {
	switch operator {
	case "StringNotEquals", "StringNotLike", "NotIpAddress":
		return true
	}
	return false
}

func knownOperator(operator string) bool {
	switch operator {
	case "StringEquals", "StringNotEquals", "StringLike", "StringNotLike", "StringEqualsIgnoreCase",
		"IpAddress", "NotIpAddress", "DateLessThan", "DateGreaterThan", "Bool":
		return true
	}
	return false
}

func evaluateOperator(operator, actual string, expected []string) bool {
	switch operator {
	case "StringEquals":
		return stringEquals(actual, expected)
	case "StringNotEquals":
		return !stringEquals(actual, expected)
	case "StringLike":
		return stringLike(actual, expected)
	case "StringNotLike":
		return !stringLike(actual, expected)
	case "StringEqualsIgnoreCase":
		return stringEqualsIgnoreCase(actual, expected)
	case "IpAddress":
		return ipAddress(actual, expected)
	case "NotIpAddress":
		return !ipAddress(actual, expected)
	case "DateLessThan":
		return dateLessThan(actual, expected)
	case "DateGreaterThan":
		return dateGreaterThan(actual, expected)
	case "Bool":
		return stringEquals(actual, expected)
	default:
		return false // unknown operator = deny
	}
}

func stringEquals(actual string, expected []string) bool {
	for _, v := range expected {
		if actual == v {
			return true
		}
	}
	return false
}

func stringEqualsIgnoreCase(actual string, expected []string) bool {
	for _, v := range expected {
		if strings.EqualFold(actual, v) {
			return true
		}
	}
	return false
}

func stringLike(actual string, expected []string) bool {
	for _, v := range expected {
		if matchWildcard(v, actual) {
			return true
		}
	}
	return false
}

func ipAddress(actual string, cidrs []string) bool {
	ip := net.ParseIP(actual)
	if ip == nil {
		return false
	}
	for _, cidr := range cidrs {
		if strings.Contains(cidr, "/") {
			_, network, err := net.ParseCIDR(cidr)
			if err != nil {
				continue
			}
			if network.Contains(ip) {
				return true
			}
		} else {
			if actual == cidr {
				return true
			}
		}
	}
	return false
}

func dateLessThan(actual string, expected []string) bool {
	t, err := time.Parse(time.RFC3339, actual)
	if err != nil {
		return false
	}
	for _, v := range expected {
		threshold, err := time.Parse(time.RFC3339, v)
		if err != nil {
			continue
		}
		if t.Before(threshold) {
			return true
		}
	}
	return false
}

func dateGreaterThan(actual string, expected []string) bool {
	t, err := time.Parse(time.RFC3339, actual)
	if err != nil {
		return false
	}
	for _, v := range expected {
		threshold, err := time.Parse(time.RFC3339, v)
		if err != nil {
			continue
		}
		if t.After(threshold) {
			return true
		}
	}
	return false
}

// SubstitutePolicyVariables replaces ${aws:*} variables in policy strings.
// expandVars substitutes the policy variables in pattern from ctx. ok is false
// when one has no value, when a value holds a wildcard character (this
// matcher has no escape, so "*" from a user name would match anything), or
// for the escape variables ${*}, ${?} and ${$}, which it cannot express.
func expandVars(pattern string, ctx map[string]string) (string, bool) {
	var b strings.Builder
	for {
		i := strings.Index(pattern, "${")
		if i < 0 {
			b.WriteString(pattern)
			return b.String(), true
		}
		j := strings.IndexByte(pattern[i:], '}')
		if j < 0 {
			return "", false
		}
		name := pattern[i+2 : i+j]
		v, present := ctx[name]
		if !present || v == "" || strings.ContainsAny(v, "*?") {
			return "", false
		}
		b.WriteString(pattern[:i])
		b.WriteString(v)
		pattern = pattern[i+j+1:]
	}
}

// matchesAnyVars is matchesAny for patterns that may name policy variables.
// known is false when a pattern that could decide the answer has a variable
// that cannot be resolved.
func matchesAnyVars(patterns []string, value string, ctx map[string]string) (matched, known bool) {
	known = true
	for _, p := range patterns {
		if !strings.Contains(p, "${") {
			if MatchWildcard(p, value) {
				return true, true
			}
			continue
		}
		exp, ok := expandVars(p, ctx)
		if !ok {
			known = false
			continue
		}
		if MatchWildcard(exp, value) {
			return true, true
		}
	}
	return false, known
}

// substituteValues resolves policy variables in condition values.
func substituteValues(values []string, ctx map[string]string) ([]string, bool) {
	var out []string
	for i, v := range values {
		if !strings.Contains(v, "${") {
			continue
		}
		if out == nil {
			out = append([]string(nil), values...)
		}
		exp, ok := expandVars(v, ctx)
		if !ok {
			return nil, false
		}
		out[i] = exp
	}
	if out == nil {
		return values, true
	}
	return out, true
}

func SubstitutePolicyVariables(s string, ctx map[string]string) string {
	result := s
	for k, v := range ctx {
		result = strings.ReplaceAll(result, "${"+k+"}", v)
	}
	return result
}
