package iam

import (
	"encoding/json"
	"testing"
)

// AWS IAM accepts Action and Resource as either a bare string or an array, and
// most AWS documentation examples use the bare string. VaultS3 typed them as
// []string, so the bare-string form failed to unmarshal, and the loader that
// reads a user's policies discards anything it cannot parse without logging.
// The result was a policy that was accepted, stored, listed back intact, and
// then ignored at every authorization decision.
func TestPolicyAcceptsBareStringActionAndResource(t *testing.T) {
	cases := []struct{ name, doc string }{
		{"bare strings", `{"Version":"2012-10-17","Statement":[
			{"Effect":"Deny","Action":"s3:DeleteObjectVersion","Resource":"arn:aws:s3:::b/*"}]}`},
		{"arrays", `{"Version":"2012-10-17","Statement":[
			{"Effect":"Deny","Action":["s3:DeleteObjectVersion"],"Resource":["arn:aws:s3:::b/*"]}]}`},
		{"mixed", `{"Version":"2012-10-17","Statement":[
			{"Effect":"Deny","Action":["s3:DeleteObjectVersion"],"Resource":"arn:aws:s3:::b/*"}]}`},
	}
	for _, c := range cases {
		var p Policy
		if err := json.Unmarshal([]byte(c.doc), &p); err != nil {
			t.Fatalf("%s: policy failed to parse, so it would be silently ignored: %v", c.name, err)
		}
		if len(p.Statement) != 1 {
			t.Fatalf("%s: got %d statements, want 1", c.name, len(p.Statement))
		}
		st := p.Statement[0]
		if len(st.Action) != 1 || st.Action[0] != "s3:DeleteObjectVersion" {
			t.Errorf("%s: Action = %v, want [s3:DeleteObjectVersion]", c.name, st.Action)
		}
		if len(st.Resource) != 1 || st.Resource[0] != "arn:aws:s3:::b/*" {
			t.Errorf("%s: Resource = %v, want [arn:aws:s3:::b/*]", c.name, st.Resource)
		}
	}
}

// The decisive one: a Deny written in the bare-string form must actually deny.
func TestBareStringDenyIsEnforced(t *testing.T) {
	var allow, deny Policy
	json.Unmarshal([]byte(`{"Statement":[{"Effect":"Allow","Action":"s3:*","Resource":"*"}]}`), &allow)
	json.Unmarshal([]byte(`{"Statement":[{"Effect":"Deny","Action":"s3:DeleteObjectVersion","Resource":"arn:aws:s3:::b/*"}]}`), &deny)

	if Evaluate([]Policy{allow, deny}, "s3:DeleteObjectVersion", "arn:aws:s3:::b/k.txt") {
		t.Error("a bare-string Deny did not block the action, so the policy protects nothing")
	}
	if !Evaluate([]Policy{allow, deny}, "s3:DeleteObject", "arn:aws:s3:::b/k.txt") {
		t.Error("the Deny leaked onto an unrelated action")
	}
}
