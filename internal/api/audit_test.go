package api

import (
	"encoding/json"
	"testing"
)

// The dashboard's Audit Trail binds to these exact JSON names. The Source IP
// column rendered "-" for every entry because the client read `sourceIP` while
// the API emits `sourceIp`, and JSON keys are case sensitive. Nothing failed:
// the value was simply undefined, so a security log silently lost the one field
// that says where a denied request came from.
func TestAuditResponseFieldNames(t *testing.T) {
	b, err := json.Marshal(auditResponse{SourceIP: "203.0.113.7", User: "u", Action: "s3:ListBucket"})
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]interface{}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"sourceIp", "user", "action", "resource", "effect", "statusCode", "time"} {
		if _, ok := got[field]; !ok {
			t.Errorf("audit entry has no %q field, the dashboard column bound to it will be empty: %s", field, b)
		}
	}
	if got["sourceIp"] != "203.0.113.7" {
		t.Errorf("sourceIp = %v, want the recorded address", got["sourceIp"])
	}
}
