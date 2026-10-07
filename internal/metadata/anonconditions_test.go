package metadata

import (
	"encoding/json"
	"testing"
)

// A bucket policy's Condition was dropped while parsing, so a conditional Allow
// granted to everyone: a grant limited to one IP range published the object to
// the whole internet.
func TestAnonymousPolicyEvaluatesConditions(t *testing.T) {
	s := newPolicyStore(t)
	from := func(ip string) map[string]string { return map[string]string{"aws:SourceIp": ip} }

	// Single-string value, as AWS documentation writes it.
	setPolicy(t, s, `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":"*","Action":"s3:GetObject",
		"Resource":"arn:aws:s3:::photos/*","Condition":{"IpAddress":{"aws:SourceIp":"203.0.113.0/24"}}}]}`)
	if s.IsObjectPublicRead("photos", "cat.jpg", from("198.51.100.7")) {
		t.Error("granted to an address outside the condition's range")
	}
	if !s.IsObjectPublicRead("photos", "cat.jpg", from("203.0.113.9")) {
		t.Error("refused an address inside the condition's range")
	}
	if s.IsObjectPublicRead("photos", "cat.jpg", nil) {
		t.Error("granted with no context to decide the condition on")
	}

	// A Deny whose condition cannot be decided must still block.
	setPolicy(t, s, `{"Version":"2012-10-17","Statement":[
		{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::photos/*"},
		{"Effect":"Deny","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::photos/*",
		 "Condition":{"NumericGreaterThan":{"s3:max-keys":["10"]}}}]}`)
	if s.IsObjectPublicRead("photos", "cat.jpg", from("203.0.113.9")) {
		t.Error("a Deny with an operator the evaluator does not know was skipped")
	}

	// Listing under a prefix condition needs the prefix in the request.
	setPolicy(t, s, `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":"*","Action":"s3:ListBucket",
		"Resource":"arn:aws:s3:::photos","Condition":{"StringLike":{"s3:prefix":["public/*"]}}}]}`)
	if s.IsBucketPublicList("photos", from("203.0.113.9")) {
		t.Error("a listing with no prefix was granted by a prefix-conditioned policy")
	}
	if !s.IsBucketPublicList("photos", map[string]string{"aws:SourceIp": "203.0.113.9", "s3:prefix": "public/x"}) {
		t.Error("a listing inside the allowed prefix was refused")
	}
}

// Snowball imports and POST uploads stored LastModified in nanoseconds. Reading
// such a record repairs it, so the objects already stored become usable.
func TestObjectMetaRepairsANanosecondLastModified(t *testing.T) {
	var m ObjectMeta
	if err := json.Unmarshal([]byte(`{"bucket":"b","key":"k","last_modified":1790000000123456789}`), &m); err != nil {
		t.Fatal(err)
	}
	if m.LastModified != 1790000000 {
		t.Errorf("LastModified %d, want 1790000000", m.LastModified)
	}
	if err := json.Unmarshal([]byte(`{"last_modified":1790000000}`), &m); err != nil || m.LastModified != 1790000000 {
		t.Errorf("a seconds value was changed: %d %v", m.LastModified, err)
	}
}
