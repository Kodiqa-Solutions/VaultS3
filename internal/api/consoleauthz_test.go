package api

import (
	"net/http"
	"testing"
)

// The console used to authorize on "is this JWT valid" alone, so any session
// could read, overwrite and delete objects in any bucket while the S3 API
// refused the same user the same data (security assessment finding 14). Every
// bucket route must now map to an S3 action so it can be evaluated.
func TestEveryBucketRouteMapsToAnAction(t *testing.T) {
	cases := []struct {
		method, rest string
		wantAction   string
		wantResource string
	}{
		{http.MethodGet, "b", "s3:ListBucket", "arn:aws:s3:::b"},
		{http.MethodDelete, "b", "s3:DeleteBucket", "arn:aws:s3:::b"},
		{http.MethodGet, "b/objects", "s3:ListBucket", "arn:aws:s3:::b"},
		{http.MethodDelete, "b/objects/k.txt", "s3:DeleteObject", "arn:aws:s3:::b/k.txt"},
		{http.MethodGet, "b/download/k.txt", "s3:GetObject", "arn:aws:s3:::b/k.txt"},
		{http.MethodPost, "b/upload", "s3:PutObject", "arn:aws:s3:::b/*"},
		{http.MethodPost, "b/bulk-delete", "s3:DeleteObject", "arn:aws:s3:::b/*"},
		{http.MethodGet, "b/download-zip", "s3:GetObject", "arn:aws:s3:::b/*"},
		{http.MethodPut, "b/versioning", "s3:PutBucketVersioning", "arn:aws:s3:::b"},
		{http.MethodPut, "b/policy", "s3:PutBucketPolicy", "arn:aws:s3:::b"},
	}
	for _, c := range cases {
		act, ok := consoleActionFor(c.method, c.rest)
		if !ok {
			t.Errorf("%s %s maps to no action, so it would be unauthorized", c.method, c.rest)
			continue
		}
		if act.action != c.wantAction || act.resource() != c.wantResource {
			t.Errorf("%s %s -> %s on %s, want %s on %s",
				c.method, c.rest, act.action, act.resource(), c.wantAction, c.wantResource)
		}
	}
}

// A sub-resource nobody mapped must not fall through to "no action required".
// Defaulting to a bucket write means an unmapped route is denied for non-admins
// rather than silently open.
func TestUnknownBucketRouteIsNotUnauthorized(t *testing.T) {
	act, ok := consoleActionFor(http.MethodPost, "b/some-future-subresource")
	if !ok {
		t.Fatal("an unmapped bucket route reported that no authorization applies")
	}
	if act.action != "s3:PutBucketPolicy" || act.bucket != "b" {
		t.Fatalf("unmapped route maps to %s on %s, want a bucket-level write", act.action, act.resource())
	}
}

// A route naming no bucket is handled by the admin allowlist, not here.
func TestRouteWithoutABucketIsNotMapped(t *testing.T) {
	if _, ok := consoleActionFor(http.MethodGet, ""); ok {
		t.Fatal("an empty bucket name produced an action")
	}
}
