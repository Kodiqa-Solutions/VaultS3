package iam

import "testing"

// The S3 evaluator matched Action, Resource and Effect and silently ignored
// Condition, NotAction and NotResource, so a policy scoped to one IP range was
// treated as unconditional and a conditional Deny did not deny (security
// assessment finding 12). Evaluate now delegates to the full evaluator.
func TestEvaluateHonoursConditions(t *testing.T) {
	scoped := []Policy{{Statement: []Statement{{
		Effect:    "Allow",
		Action:    []string{"s3:GetObject"},
		Resource:  []string{"arn:aws:s3:::b/*"},
		Condition: map[string]map[string][]string{"IpAddress": {"aws:SourceIp": {"10.99.99.99/32"}}},
	}}}}

	// From the permitted address the Allow applies.
	if !EvaluateWithContext(scoped, "s3:GetObject", "arn:aws:s3:::b/k", map[string]string{"aws:SourceIp": "10.99.99.99"}) {
		t.Error("a request from the permitted address was denied")
	}
	// From anywhere else it must not.
	if EvaluateWithContext(scoped, "s3:GetObject", "arn:aws:s3:::b/k", map[string]string{"aws:SourceIp": "127.0.0.1"}) {
		t.Error("the source-IP condition was ignored: access granted from a non-matching address")
	}
	// With no context at all, an Allow that depends on an unproven condition
	// must not grant.
	if Evaluate(scoped, "s3:GetObject", "arn:aws:s3:::b/k") {
		t.Error("a conditional Allow granted access with no context to prove the condition")
	}
}

// A conditional Deny is a control an operator relies on. It must still block
// when the condition cannot be evaluated, or it could be bypassed by evaluating
// without context.
func TestConditionalDenyStillDeniesWithoutContext(t *testing.T) {
	policies := []Policy{
		{Statement: []Statement{{Effect: "Allow", Action: []string{"s3:GetObject"}, Resource: []string{"arn:aws:s3:::b/*"}}}},
		{Statement: []Statement{{
			Effect:    "Deny",
			Action:    []string{"s3:GetObject"},
			Resource:  []string{"arn:aws:s3:::b/*"},
			Condition: map[string]map[string][]string{"IpAddress": {"aws:SourceIp": {"127.0.0.1/32"}}},
		}}},
	}
	if EvaluateWithContext(policies, "s3:GetObject", "arn:aws:s3:::b/k", map[string]string{"aws:SourceIp": "127.0.0.1"}) {
		t.Error("the conditional Deny did not deny the address it names")
	}
	if Evaluate(policies, "s3:GetObject", "arn:aws:s3:::b/k") {
		t.Error("the conditional Deny was skipped when no context was supplied")
	}
}

// NotAction and NotResource were never inspected at all.
func TestNotActionAndNotResourceAreHonoured(t *testing.T) {
	notAction := []Policy{{Statement: []Statement{{
		Effect:    "Allow",
		NotAction: []string{"s3:DeleteObject"},
		Resource:  []string{"arn:aws:s3:::b/*"},
	}}}}
	if !Evaluate(notAction, "s3:GetObject", "arn:aws:s3:::b/k") {
		t.Error("NotAction did not allow an action outside its list")
	}
	if Evaluate(notAction, "s3:DeleteObject", "arn:aws:s3:::b/k") {
		t.Error("NotAction allowed the action it excludes")
	}

	notResource := []Policy{{Statement: []Statement{{
		Effect:      "Allow",
		Action:      []string{"s3:GetObject"},
		NotResource: []string{"arn:aws:s3:::secret/*"},
	}}}}
	if !Evaluate(notResource, "s3:GetObject", "arn:aws:s3:::public/k") {
		t.Error("NotResource did not allow a resource outside its list")
	}
	if Evaluate(notResource, "s3:GetObject", "arn:aws:s3:::secret/k") {
		t.Error("NotResource allowed the resource it excludes")
	}
}
