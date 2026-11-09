package iam

import (
	"math/rand/v2"
	"regexp"
	"strings"
	"testing"
)

func TestEvaluate_AllowAll(t *testing.T) {
	policies := []Policy{{
		Version: "2012-10-17",
		Statement: []Statement{{
			Effect:   "Allow",
			Action:   []string{"s3:*"},
			Resource: []string{"*"},
		}},
	}}
	if !Evaluate(policies, "s3:GetObject", "arn:aws:s3:::mybucket/file.txt") {
		t.Error("expected Allow for s3:* / *")
	}
}

func TestEvaluate_DefaultDeny(t *testing.T) {
	policies := []Policy{{
		Version:   "2012-10-17",
		Statement: []Statement{},
	}}
	if Evaluate(policies, "s3:GetObject", "arn:aws:s3:::mybucket/file.txt") {
		t.Error("expected default Deny with no statements")
	}
}

func TestEvaluate_ExplicitDenyWins(t *testing.T) {
	policies := []Policy{{
		Version: "2012-10-17",
		Statement: []Statement{
			{Effect: "Allow", Action: []string{"s3:*"}, Resource: []string{"*"}},
			{Effect: "Deny", Action: []string{"s3:DeleteObject"}, Resource: []string{"*"}},
		},
	}}
	if Evaluate(policies, "s3:DeleteObject", "arn:aws:s3:::mybucket/file.txt") {
		t.Error("expected Deny to override Allow")
	}
	if !Evaluate(policies, "s3:GetObject", "arn:aws:s3:::mybucket/file.txt") {
		t.Error("expected Allow for non-denied action")
	}
}

func TestEvaluate_ResourcePrefix(t *testing.T) {
	policies := []Policy{{
		Version: "2012-10-17",
		Statement: []Statement{{
			Effect:   "Allow",
			Action:   []string{"s3:GetObject"},
			Resource: []string{"arn:aws:s3:::mybucket/*"},
		}},
	}}
	if !Evaluate(policies, "s3:GetObject", "arn:aws:s3:::mybucket/dir/file.txt") {
		t.Error("expected Allow for matching resource prefix")
	}
	if Evaluate(policies, "s3:GetObject", "arn:aws:s3:::otherbucket/file.txt") {
		t.Error("expected Deny for non-matching resource")
	}
}

func TestEvaluate_ActionWildcard(t *testing.T) {
	policies := []Policy{{
		Version: "2012-10-17",
		Statement: []Statement{{
			Effect:   "Allow",
			Action:   []string{"s3:Get*"},
			Resource: []string{"*"},
		}},
	}}
	if !Evaluate(policies, "s3:GetObject", "*") {
		t.Error("expected Allow for s3:Get*")
	}
	if Evaluate(policies, "s3:PutObject", "*") {
		t.Error("expected Deny for s3:PutObject against s3:Get*")
	}
}

func TestMatchWildcard(t *testing.T) {
	tests := []struct {
		pattern, value string
		want           bool
	}{
		{"*", "anything", true},
		{"s3:*", "s3:GetObject", true},
		{"s3:Get*", "s3:GetObject", true},
		{"s3:Get*", "s3:PutObject", false},
		{"arn:aws:s3:::bucket/*", "arn:aws:s3:::bucket/key", true},
		// The bucket's objects, not the bucket itself (as on AWS): a grant
		// on bucket/* used to reach bucket configuration.
		{"arn:aws:s3:::bucket/*", "arn:aws:s3:::bucket", false},
		{"arn:aws:s3:::bucket", "arn:aws:s3:::bucket", true},
		{"arn:aws:s3:::bucket", "arn:aws:s3:::other", false},
		// A '*' in the value (S3 keys may hold one) where the pattern has
		// its '*': still a wildcard there, not a literal match.
		{"arn:aws:s3:::b/private/*", "arn:aws:s3:::b/private/*x", true},
		{"arn:aws:s3:::b/a*", "arn:aws:s3:::b/a*b/c", true},
		{"arn:aws:s3:::*", "arn:aws:s3:::*/*", true},
		{"arn:aws:s3:::b/*.txt", "arn:aws:s3:::b/*.txt.exe", false},
		{"arn:aws:s3:::b/x?z", "arn:aws:s3:::b/x*z", true},
		// '?' is one character, not one byte.
		{"arn:aws:s3:::b/?", "arn:aws:s3:::b/é", true},
		{"arn:aws:s3:::b/??", "arn:aws:s3:::b/é", false},
		{"arn:aws:s3:::b/r?sum?.pdf", "arn:aws:s3:::b/résumé.pdf", true},
		// A byte that is not valid UTF-8 is its own character, compared as
		// that byte.
		{"a\xff*", "a\xfe", false},
		{"a\xff*", "a\xffz", true},
		{"a?b", "a\xffb", true},
		{"*é", "xxé", true},
	}
	for _, tt := range tests {
		got := matchWildcard(tt.pattern, tt.value)
		if got != tt.want {
			t.Errorf("matchWildcard(%q, %q) = %v, want %v", tt.pattern, tt.value, got, tt.want)
		}
	}
}

// Regression (5.0.11): a Deny on "b/private/*" did not apply to a key with a
// '*' right after the prefix ("private/*x"), so an Allow on "b/*" granted it.
// The pattern's '*' was matched as a literal against the key's '*'.
func TestDenyAppliesToKeysContainingAStar(t *testing.T) {
	policies := []Policy{{Statement: []Statement{
		{Effect: "Allow", Action: []string{"s3:GetObject"}, Resource: []string{"arn:aws:s3:::b/*"}},
		{Effect: "Deny", Action: []string{"s3:GetObject"}, Resource: []string{"arn:aws:s3:::b/private/*"}},
	}}}
	for _, key := range []string{"private/x", "private/*x", "private/*", "private/**/y"} {
		if Evaluate(policies, "s3:GetObject", "arn:aws:s3:::b/"+key) {
			t.Errorf("%s: allowed despite the Deny on b/private/*", key)
		}
	}
	if !Evaluate(policies, "s3:GetObject", "arn:aws:s3:::b/public/*x") {
		t.Error("public/*x: refused, the Allow on b/* covers it")
	}
}

func TestEvaluate_MultiplePolicies(t *testing.T) {
	policies := []Policy{
		{Statement: []Statement{{Effect: "Allow", Action: []string{"s3:GetObject"}, Resource: []string{"*"}}}},
		{Statement: []Statement{{Effect: "Allow", Action: []string{"s3:PutObject"}, Resource: []string{"*"}}}},
	}
	if !Evaluate(policies, "s3:GetObject", "*") {
		t.Error("expected Allow from first policy")
	}
	if !Evaluate(policies, "s3:PutObject", "*") {
		t.Error("expected Allow from second policy")
	}
	if Evaluate(policies, "s3:DeleteObject", "*") {
		t.Error("expected Deny for action not in either policy")
	}
}

// Matching a long key must not allocate in proportion to it: anonymous
// requests reach the matcher through public bucket policies (5.0.11 round 4).
func TestMatchWildcardDoesNotAllocate(t *testing.T) {
	key := "arn:aws:s3:::pubb/" + strings.Repeat("ké", 300<<10)
	allocs := testing.AllocsPerRun(5, func() {
		if !matchWildcard("arn:aws:s3:::pubb/*", key) || matchWildcard("arn:aws:s3:::pubb/*x?z*", key) {
			t.Fatal("wrong match")
		}
	})
	if allocs > 0 {
		t.Errorf("%.0f allocations per match of a %d byte key, want none", allocs, len(key))
	}
}

// The matcher agrees with a regular expression built from the pattern ('*'
// any run of characters, '?' one character) on random patterns and values
// that hold '*', '?', '/' and a two-byte character.
func TestMatchWildcardAgreesWithARegexp(t *testing.T) {
	alphabet := []string{"a", "b", "*", "?", "/", "é"}
	rng := rand.New(rand.NewPCG(1, 2))
	word := func(n int) string {
		var b strings.Builder
		for i := 0; i < n; i++ {
			b.WriteString(alphabet[rng.IntN(len(alphabet))])
		}
		return b.String()
	}
	for i := 0; i < 200000; i++ {
		pattern, value := word(rng.IntN(7)), word(rng.IntN(9))
		var re strings.Builder
		re.WriteString("(?s)^")
		for _, c := range pattern {
			switch c {
			case '*':
				re.WriteString(".*")
			case '?':
				re.WriteString(".")
			default:
				re.WriteString(regexp.QuoteMeta(string(c)))
			}
		}
		re.WriteString("$")
		want := regexp.MustCompile(re.String()).MatchString(value)
		if got := matchWildcard(pattern, value); got != want {
			t.Fatalf("matchWildcard(%q, %q) = %v, the regexp says %v", pattern, value, got, want)
		}
	}
}
