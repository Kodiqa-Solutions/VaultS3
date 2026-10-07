package iam

import "testing"

// An operator the evaluator does not implement counted as "condition false",
// which skipped a Deny written with it, so the Deny protected nothing.
func TestUnknownOperatorStillLetsADenyBlock(t *testing.T) {
	pols := []Policy{{Statement: []Statement{
		{Effect: "Allow", Action: []string{"s3:*"}, Resource: []string{"*"}},
		{Effect: "Deny", Action: []string{"s3:GetObject"}, Resource: []string{"*"},
			Condition: map[string]map[string][]string{"NumericLessThan": {"s3:max-keys": {"10"}}}},
	}}}
	if EvaluateWithContext(pols, "s3:GetObject", "arn:aws:s3:::b/k", map[string]string{"s3:max-keys": "5"}) {
		t.Error("a Deny with an unimplemented operator did not block")
	}
}
