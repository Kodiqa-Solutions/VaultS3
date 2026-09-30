package s3

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Issue #61: the x-amz-tagging header carries the tag set encoded as URL query
// parameters, so both keys and values arrive percent-encoded. The server stored
// them verbatim, so a tag value with a space came back as "Tag%201%20value" from
// GetObjectTagging. Anything a client had to encode (space, &, =, non-ASCII) was
// corrupted on the way in, and corrupted permanently: the object's stored
// metadata held the encoded form.
func TestParseInlineTagsDecodes(t *testing.T) {
	cases := []struct {
		name   string
		header string
		want   map[string]string
	}{
		// The reporter's exact header.
		{"space in value", "Tag1=Tag%201%20value&tag2=V2",
			map[string]string{"Tag1": "Tag 1 value", "tag2": "V2"}},
		// A '+' means space in a query string, which is how most SDKs encode it.
		{"plus is a space", "k=a+b", map[string]string{"k": "a b"}},
		// The separators themselves have to survive being encoded.
		{"encoded separators", "k=a%26b%3Dc", map[string]string{"k": "a&b=c"}},
		// Keys are encoded by the same rules as values.
		{"encoded key", "my%20key=v", map[string]string{"my key": "v"}},
		{"non-ascii", "city=M%C3%BCnchen", map[string]string{"city": "München"}},
		// Already-unreserved input must pass through untouched, which is the only
		// case the previous code got right.
		{"nothing to decode", "env=prod&team=backend",
			map[string]string{"env": "prod", "team": "backend"}},
		{"empty value", "k=", map[string]string{"k": ""}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPut, "/b/k", nil)
			r.Header.Set("X-Amz-Tagging", tc.header)
			got, err := parseInlineTags(r)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("got %d tags %v, want %d %v", len(got), got, len(tc.want), tc.want)
			}
			for k, v := range tc.want {
				if got[k] != v {
					t.Errorf("tag %q: got %q, want %q", k, got[k], v)
				}
			}
		})
	}
}

// Storing a half-decoded tag set is worse than refusing it, because the object
// keeps the wrong value with no sign anything went wrong.
func TestParseInlineTagsRejectsMalformed(t *testing.T) {
	for _, header := range []string{"k=%ZZ", "%ZZ=v", "k=%2"} {
		r := httptest.NewRequest(http.MethodPut, "/b/k", nil)
		r.Header.Set("X-Amz-Tagging", header)
		if _, err := parseInlineTags(r); err == nil {
			t.Errorf("%q: expected an error, got none", header)
		}
	}
}

// The XML path (PutObjectTagging) already refuses more than 10 tags. The header
// path accepted any number, so the same request was legal one way and not the
// other.
func TestParseInlineTagsEnforcesTagLimit(t *testing.T) {
	var pairs []string
	for i := 0; i < 11; i++ {
		pairs = append(pairs, string(rune('a'+i))+"=v")
	}
	r := httptest.NewRequest(http.MethodPut, "/b/k", nil)
	r.Header.Set("X-Amz-Tagging", strings.Join(pairs, "&"))
	if _, err := parseInlineTags(r); err == nil {
		t.Error("11 tags: expected an error, got none")
	}
}

func TestParseInlineTagsAbsentHeader(t *testing.T) {
	r := httptest.NewRequest(http.MethodPut, "/b/k", nil)
	tags, err := parseInlineTags(r)
	if err != nil || tags != nil {
		t.Errorf("no header: got %v, %v; want nil, nil", tags, err)
	}
}

// The header and the XML body are two spellings of one tag set, so they must not
// disagree about what a given set means. PutObjectTagging fills its map from the
// XML tag list in order, so the last of a repeated key wins; the header path has
// to land on the same value.
func TestParseInlineTagsDuplicateKeyMatchesXMLPath(t *testing.T) {
	r := httptest.NewRequest(http.MethodPut, "/b/k", nil)
	r.Header.Set("X-Amz-Tagging", "k=first&k=second")
	tags, err := parseInlineTags(r)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// What PutObjectTagging would store for the same set, by the same loop it uses.
	viaXML := map[string]string{}
	for _, tag := range []xmlTag{{Key: "k", Value: "first"}, {Key: "k", Value: "second"}} {
		viaXML[tag.Key] = tag.Value
	}

	if tags["k"] != viaXML["k"] {
		t.Errorf("header path stored %q, XML path stores %q", tags["k"], viaXML["k"])
	}
}

// A ';' is not a character S3 allows in a tag, and Go's query parser refuses it as
// a separator, so the request is refused either way. Say which it was: calling it
// invalid encoding sends the caller looking for a percent-escape bug that is not
// there.
func TestParseInlineTagsSemicolonIsNamed(t *testing.T) {
	r := httptest.NewRequest(http.MethodPut, "/b/k", nil)
	r.Header.Set("X-Amz-Tagging", "k=a;b")
	_, err := parseInlineTags(r)
	if err == nil {
		t.Fatal("expected a ';' to be refused")
	}
	var te *tagError
	if !errors.As(err, &te) {
		t.Fatalf("expected a tagError, got %T", err)
	}
	if te.code != "InvalidTag" {
		t.Errorf("code: got %q, want InvalidTag", te.code)
	}
	if !strings.Contains(te.msg, ";") {
		t.Errorf("message does not mention the ';': %q", te.msg)
	}
}
