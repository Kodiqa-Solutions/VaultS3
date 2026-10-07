package iam

import (
	"encoding/json"
	"testing"
)

// AWS lets a condition carry one value as a bare string. That form failed to
// parse, and a policy that fails to parse denies its identity everything, so an
// IP-scoped Allow copied from the AWS documentation locked the user out instead
// of scoping them.
func TestConditionAcceptsABareStringValue(t *testing.T) {
	doc := `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"s3:GetObject",
		"Resource":"*","Condition":{"IpAddress":{"aws:SourceIp":"203.0.113.0/24"}}}]}`
	var p Policy
	if err := json.Unmarshal([]byte(doc), &p); err != nil {
		t.Fatalf("a bare-string condition value failed to parse: %v", err)
	}
	inside := map[string]string{"aws:SourceIp": "203.0.113.9"}
	outside := map[string]string{"aws:SourceIp": "198.51.100.9"}
	if !EvaluateWithContext([]Policy{p}, "s3:GetObject", "arn:aws:s3:::b/k", inside) {
		t.Error("the Allow did not grant from inside the range")
	}
	if EvaluateWithContext([]Policy{p}, "s3:GetObject", "arn:aws:s3:::b/k", outside) {
		t.Error("the Allow granted from outside the range, so the condition was dropped rather than parsed")
	}
}

// A bare boolean is also valid ("aws:SecureTransport": false) and is the usual
// way the deny-plain-HTTP policy is written.
func TestConditionAcceptsABareBoolean(t *testing.T) {
	doc := `{"Statement":[{"Effect":"Allow","Action":"s3:*","Resource":"*"},
		{"Effect":"Deny","Action":"s3:*","Resource":"*","Condition":{"Bool":{"aws:SecureTransport":false}}}]}`
	var p Policy
	if err := json.Unmarshal([]byte(doc), &p); err != nil {
		t.Fatalf("a bare boolean condition value failed to parse: %v", err)
	}
	if EvaluateWithContext([]Policy{p}, "s3:GetObject", "arn:aws:s3:::b/k", map[string]string{"aws:SecureTransport": "false"}) {
		t.Error("the Deny on plain HTTP did not apply")
	}
	if !EvaluateWithContext([]Policy{p}, "s3:GetObject", "arn:aws:s3:::b/k", map[string]string{"aws:SecureTransport": "true"}) {
		t.Error("the Deny on plain HTTP applied to a TLS request")
	}
}

// A single statement object instead of an array is valid IAM.
func TestStatementMayBeASingleObject(t *testing.T) {
	doc := `{"Version":"2012-10-17","Statement":{"Effect":"Deny","Action":"s3:DeleteObject","Resource":"arn:aws:s3:::b/*"}}`
	var p Policy
	if err := json.Unmarshal([]byte(doc), &p); err != nil {
		t.Fatalf("a single-object Statement failed to parse: %v", err)
	}
	if len(p.Statement) != 1 || p.Statement[0].Effect != "Deny" {
		t.Fatalf("parsed %+v, want the one Deny statement", p.Statement)
	}
	allow := Policy{Statement: []Statement{{Effect: "Allow", Action: []string{"s3:*"}, Resource: []string{"*"}}}}
	if Evaluate([]Policy{allow, p}, "s3:DeleteObject", "arn:aws:s3:::b/k") {
		t.Error("the single-object Deny did not block")
	}
}

// A condition value of the wrong shape is still refused, so the loader can
// refuse the identity rather than guess.
func TestConditionRejectsAnObjectValue(t *testing.T) {
	doc := `{"Statement":[{"Effect":"Allow","Action":"s3:*","Resource":"*","Condition":{"StringEquals":{"aws:username":{"x":1}}}}]}`
	var p Policy
	if err := json.Unmarshal([]byte(doc), &p); err == nil {
		t.Error("an object as a condition value parsed, it should be refused")
	}
}
