package metadata

import (
	"path/filepath"
	"testing"
)

func newPolicyStore(t *testing.T) *Store {
	t.Helper()
	s, err := NewStore(filepath.Join(t.TempDir(), "meta.db"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	if err := s.CreateBucket("photos"); err != nil {
		t.Fatalf("CreateBucket: %v", err)
	}
	return s
}

func setPolicy(t *testing.T, s *Store, policy string) {
	t.Helper()
	if err := s.PutBucketPolicy("photos", []byte(policy)); err != nil {
		t.Fatalf("PutBucketPolicy: %v", err)
	}
}

// TestPublicReadPrincipalFormats covers issue #41: the AWS standard object form
// of Principal ({"AWS": "*"} and {"AWS": ["*"]}) must be recognised as public,
// not just the shorthand string form.
func TestPublicReadPrincipalFormats(t *testing.T) {
	cases := []struct {
		name      string
		principal string
		want      bool
	}{
		{"string wildcard", `"*"`, true},
		{"aws object wildcard", `{"AWS": "*"}`, true},
		{"aws object wildcard list", `{"AWS": ["*"]}`, true},
		{"aws object list with wildcard among others", `{"AWS": ["arn:aws:iam::123456789012:root", "*"]}`, true},
		{"lowercase aws key", `{"aws": "*"}`, true},
		// Must NOT be treated as public — these name specific principals.
		{"specific account", `{"AWS": "arn:aws:iam::123456789012:root"}`, false},
		{"specific user list", `{"AWS": ["arn:aws:iam::123456789012:user/bob"]}`, false},
		{"service principal", `{"Service": "cloudtrail.amazonaws.com"}`, false},
		{"canonical user", `{"CanonicalUser": "abc123"}`, false},
		{"empty", `{}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newPolicyStore(t)
			setPolicy(t, s, `{
			  "Version": "2012-10-17",
			  "Statement": [{
			    "Effect": "Allow",
			    "Principal": `+tc.principal+`,
			    "Action": ["s3:GetObject"],
			    "Resource": ["arn:aws:s3:::photos/*"]
			  }]
			}`)
			if got := s.IsObjectPublicRead("photos", "cat.jpg"); got != tc.want {
				t.Fatalf("IsObjectPublicRead = %v, want %v (principal %s)", got, tc.want, tc.principal)
			}
		})
	}
}

// TestPublicReadActionForms checks action matching, including wildcards and the
// single-string form.
func TestPublicReadActionForms(t *testing.T) {
	cases := []struct {
		action string
		want   bool
	}{
		{`"s3:GetObject"`, true},
		{`["s3:GetObject"]`, true},
		{`"s3:*"`, true},
		{`"s3:Get*"`, true},
		{`["s3:PutObject", "s3:GetObject"]`, true},
		{`"s3:PutObject"`, false},
		{`["s3:ListBucket"]`, false}, // listing is not object read
		{`"*"`, true},
	}
	for _, tc := range cases {
		t.Run(tc.action, func(t *testing.T) {
			s := newPolicyStore(t)
			setPolicy(t, s, `{
			  "Statement": [{
			    "Effect": "Allow",
			    "Principal": {"AWS": "*"},
			    "Action": `+tc.action+`,
			    "Resource": ["arn:aws:s3:::photos/*"]
			  }]
			}`)
			if got := s.IsObjectPublicRead("photos", "cat.jpg"); got != tc.want {
				t.Fatalf("action %s: got %v want %v", tc.action, got, tc.want)
			}
		})
	}
}

// TestPublicListIsSeparateFromRead is the security boundary for the "also support
// s3:ListBucket" request on issue #41: granting only s3:ListBucket must make the
// listing public WITHOUT making every object readable, and vice versa.
func TestPublicListIsSeparateFromRead(t *testing.T) {
	t.Run("list only", func(t *testing.T) {
		s := newPolicyStore(t)
		setPolicy(t, s, `{
		  "Statement": [{
		    "Effect": "Allow",
		    "Principal": {"AWS": "*"},
		    "Action": ["s3:ListBucket"],
		    "Resource": ["arn:aws:s3:::photos"]
		  }]
		}`)
		if !s.IsBucketPublicList("photos") {
			t.Fatal("s3:ListBucket should make the bucket publicly listable")
		}
		if s.IsObjectPublicRead("photos", "cat.jpg") {
			t.Fatal("SECURITY: s3:ListBucket must NOT make objects publicly readable")
		}
	})

	t.Run("read only", func(t *testing.T) {
		s := newPolicyStore(t)
		setPolicy(t, s, `{
		  "Statement": [{
		    "Effect": "Allow",
		    "Principal": {"AWS": "*"},
		    "Action": ["s3:GetObject"],
		    "Resource": ["arn:aws:s3:::photos/*"]
		  }]
		}`)
		if !s.IsObjectPublicRead("photos", "cat.jpg") {
			t.Fatal("s3:GetObject should make objects publicly readable")
		}
		if s.IsBucketPublicList("photos") {
			t.Fatal("SECURITY: s3:GetObject must NOT make the bucket publicly listable")
		}
	})
}

// TestPublicReadExplicitDenyWins verifies AWS evaluation order: an explicit Deny
// overrides an Allow, so a bucket with both is not public.
func TestPublicReadExplicitDenyWins(t *testing.T) {
	s := newPolicyStore(t)
	setPolicy(t, s, `{
	  "Statement": [
	    {"Effect": "Allow", "Principal": {"AWS": "*"}, "Action": ["s3:GetObject"], "Resource": ["arn:aws:s3:::photos/*"]},
	    {"Effect": "Deny",  "Principal": {"AWS": "*"}, "Action": ["s3:GetObject"], "Resource": ["arn:aws:s3:::photos/*"]}
	  ]
	}`)
	if s.IsObjectPublicRead("photos", "cat.jpg") {
		t.Fatal("SECURITY: an explicit Deny must override the Allow")
	}
}

// TestPublicReadResourceMustMatchObject stops a policy written for another bucket,
// or for another prefix of this one, from making photos/cat.jpg public. The
// Resource is matched as a full object ARN, exactly as AWS evaluates it.
func TestPublicReadResourceMustMatchObject(t *testing.T) {
	cases := []struct {
		resource string
		want     bool
	}{
		{`["arn:aws:s3:::photos/*"]`, true},
		{`["*"]`, true},
		{`["arn:aws:s3:::*"]`, true},
		{`["arn:aws:s3:::photos/cat.jpg"]`, true},
		// A bare bucket ARN does not cover the objects in it, in AWS or here.
		{`["arn:aws:s3:::photos"]`, false},
		// SECURITY: a Resource scoped to one prefix publishes that prefix only.
		{`"arn:aws:s3:::photos/public/*"`, false},
		{`["arn:aws:s3:::other-bucket/*"]`, false},
		{`["arn:aws:s3:::photos-archive/*"]`, false},
		// A statement with no Resource at all grants nothing.
		{`[]`, false},
	}
	for _, tc := range cases {
		t.Run(tc.resource, func(t *testing.T) {
			s := newPolicyStore(t)
			setPolicy(t, s, `{
			  "Statement": [{
			    "Effect": "Allow",
			    "Principal": {"AWS": "*"},
			    "Action": ["s3:GetObject"],
			    "Resource": `+tc.resource+`
			  }]
			}`)
			if got := s.IsObjectPublicRead("photos", "cat.jpg"); got != tc.want {
				t.Fatalf("resource %s: got %v want %v", tc.resource, got, tc.want)
			}
		})
	}
}

// TestPublicAccessBlockOverridesPolicy verifies the Public Access Block setting is
// actually enforced: it was previously stored and reported but never consulted, so
// enabling it did not stop anonymous access.
func TestPublicAccessBlockOverridesPolicy(t *testing.T) {
	for _, field := range []string{"BlockPublicPolicy", "RestrictPublicBuckets"} {
		t.Run(field, func(t *testing.T) {
			s := newPolicyStore(t)
			setPolicy(t, s, `{
			  "Statement": [{
			    "Effect": "Allow",
			    "Principal": {"AWS": "*"},
			    "Action": ["s3:GetObject"],
			    "Resource": ["arn:aws:s3:::photos/*"]
			  }]
			}`)
			if !s.IsObjectPublicRead("photos", "cat.jpg") {
				t.Fatal("precondition: bucket should be public before the block")
			}

			cfg := PublicAccessBlockConfig{}
			if field == "BlockPublicPolicy" {
				cfg.BlockPublicPolicy = true
			} else {
				cfg.RestrictPublicBuckets = true
			}
			if err := s.PutPublicAccessBlock("photos", cfg); err != nil {
				t.Fatalf("PutPublicAccessBlock: %v", err)
			}
			if s.IsObjectPublicRead("photos", "cat.jpg") {
				t.Fatalf("SECURITY: %s must block anonymous read access", field)
			}
		})
	}
}

// TestNoPolicyIsNotPublic is the default-deny guard.
func TestNoPolicyIsNotPublic(t *testing.T) {
	s := newPolicyStore(t)
	if s.IsObjectPublicRead("photos", "cat.jpg") || s.IsBucketPublicList("photos") {
		t.Fatal("a bucket with no policy must not be public")
	}
	setPolicy(t, s, `{not valid json`)
	if s.IsObjectPublicRead("photos", "cat.jpg") {
		t.Fatal("a malformed policy must not be treated as public")
	}
}

// TestPublicReadIsScopedToTheResourcePrefix is the regression test for the
// reported disclosure: a policy publishing photos/public/* made every object in
// the bucket anonymously readable, because the key was never evaluated.
func TestPublicReadIsScopedToTheResourcePrefix(t *testing.T) {
	s := newPolicyStore(t)
	setPolicy(t, s, `{
	  "Version": "2012-10-17",
	  "Statement": [{
	    "Effect": "Allow",
	    "Principal": "*",
	    "Action": "s3:GetObject",
	    "Resource": "arn:aws:s3:::photos/public/*"
	  }]
	}`)
	if !s.IsObjectPublicRead("photos", "public/ok.txt") {
		t.Fatal("the published prefix should be anonymously readable")
	}
	for _, key := range []string{
		"secret/credentials.env",
		"private.txt",
		"public-but-not-really/x", // a sibling key sharing the prefix string
		"decoy/public/ok.txt",     // the prefix must anchor at the start
	} {
		if s.IsObjectPublicRead("photos", key) {
			t.Fatalf("SECURITY: %q is outside the published prefix but reads as public", key)
		}
	}

	// The bucket-wide summary still reports the grant, since GetBucketACL has to
	// show that something in the bucket is public. It is not an access decision.
	if !s.HasPublicReadPolicy("photos") {
		t.Fatal("HasPublicReadPolicy should report a bucket with a scoped public policy")
	}

	// Listing is a different permission and this policy does not grant it.
	if s.IsBucketPublicList("photos") {
		t.Fatal("SECURITY: an s3:GetObject grant must not make the bucket listable")
	}
}

// TestStatementWithNoResourceGrantsNothing covers the second reported issue: an
// empty Resource used to match every bucket and key, so a malformed policy
// published the bucket instead of being ignored.
func TestStatementWithNoResourceGrantsNothing(t *testing.T) {
	s := newPolicyStore(t)
	setPolicy(t, s, `{
	  "Statement": [{
	    "Effect": "Allow",
	    "Principal": "*",
	    "Action": ["s3:GetObject", "s3:ListBucket"]
	  }]
	}`)
	if s.IsObjectPublicRead("photos", "cat.jpg") {
		t.Fatal("SECURITY: a statement with no Resource must not publish objects")
	}
	if s.IsBucketPublicList("photos") {
		t.Fatal("SECURITY: a statement with no Resource must not publish the listing")
	}
	if s.HasPublicReadPolicy("photos") {
		t.Fatal("SECURITY: a statement with no Resource must not count as public")
	}
}
