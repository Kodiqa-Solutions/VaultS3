package s3

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// Regression (5.0.14 security review): lock headers on a write were applied
// as given, so a user allowed only s3:PutObject locked an object in
// COMPLIANCE mode until 9999 and nobody, the admin included, could delete it.
// They now take the permission the lock actions take, and a valid mode and
// date.
func TestLockHeadersTakeTheLockPermission(t *testing.T) {
	e := newFixEnv(t)
	e.admin(t, http.MethodPut, "/lkb", "").must(t, "bucket")
	far := "9999-12-31T00:00:00Z"
	putOnly, sk := e.addUser(t, "uploader", allow(`"s3:PutObject","s3:GetObject"`, `"arn:aws:s3:::lkb/*"`))
	for _, hdrs := range [][]string{
		{"X-Amz-Object-Lock-Mode", "COMPLIANCE", "X-Amz-Object-Lock-Retain-Until-Date", far},
		{"X-Amz-Object-Lock-Legal-Hold", "ON"},
	} {
		r := e.as(t, putOnly, sk, http.MethodPut, "/lkb/pinned", "x", hdrs...)
		if r.code != http.StatusForbidden {
			t.Errorf("lock %v with PutObject only: %d %s", hdrs, r.code, r.body)
		}
	}
	// Nothing was locked: the admin can still delete what the user wrote.
	e.as(t, putOnly, sk, http.MethodPut, "/lkb/plain", "x").must(t, "a plain put")
	expect(t, "admin delete", e.admin(t, http.MethodDelete, "/lkb/plain", ""), http.StatusNoContent, "")

	// Multipart: refused when the upload begins.
	r := e.as(t, putOnly, sk, http.MethodPost, "/lkb/mp?uploads", "", "X-Amz-Object-Lock-Mode", "COMPLIANCE", "X-Amz-Object-Lock-Retain-Until-Date", far)
	if r.code != http.StatusForbidden {
		t.Errorf("multipart with lock headers, PutObject only: %d %s", r.code, r.body)
	}

	// With the permission it works, and bad values are refused.
	locker, lsk := e.addUser(t, "locker", allow(`"s3:PutObject","s3:PutObjectRetention","s3:PutObjectLegalHold"`, `"arn:aws:s3:::lkb/*"`))
	soon := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	e.as(t, locker, lsk, http.MethodPut, "/lkb/ok", "x", "X-Amz-Object-Lock-Mode", "GOVERNANCE", "X-Amz-Object-Lock-Retain-Until-Date", soon).must(t, "a permitted lock")
	for _, c := range []struct {
		hdrs []string
		why  string
	}{
		{[]string{"X-Amz-Object-Lock-Mode", "FOREVER", "X-Amz-Object-Lock-Retain-Until-Date", soon}, "unknown mode"},
		{[]string{"X-Amz-Object-Lock-Mode", "COMPLIANCE", "X-Amz-Object-Lock-Retain-Until-Date", "tomorrow"}, "bad date"},
		{[]string{"X-Amz-Object-Lock-Mode", "COMPLIANCE", "X-Amz-Object-Lock-Retain-Until-Date", "2001-01-01T00:00:00Z"}, "date in the past"},
		{[]string{"X-Amz-Object-Lock-Mode", "COMPLIANCE"}, "mode without a date"},
		{[]string{"X-Amz-Object-Lock-Legal-Hold", "MAYBE"}, "bad legal hold"},
	} {
		r := e.as(t, locker, lsk, http.MethodPut, "/lkb/bad", "x", c.hdrs...)
		if r.code != http.StatusBadRequest || !strings.Contains(r.body, "InvalidArgument") {
			t.Errorf("%s: %d %s", c.why, r.code, r.body)
		}
	}
}
