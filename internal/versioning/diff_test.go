package versioning

import (
	"strings"
	"testing"

	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
)

func TestComputeDiff_Identical(t *testing.T) {
	a := []string{"line1", "line2", "line3"}
	b := []string{"line1", "line2", "line3"}

	result := computeDiff(a, b)
	for _, dl := range result {
		if dl.Type != "equal" {
			t.Errorf("expected all equal, got %s for %q", dl.Type, dl.Line)
		}
	}
	if len(result) != 3 {
		t.Errorf("expected 3 lines, got %d", len(result))
	}
}

func TestComputeDiff_AllAdded(t *testing.T) {
	a := []string{}
	b := []string{"new1", "new2"}

	result := computeDiff(a, b)
	if len(result) != 2 {
		t.Fatalf("expected 2 lines, got %d", len(result))
	}
	for _, dl := range result {
		if dl.Type != "add" {
			t.Errorf("expected add, got %s", dl.Type)
		}
	}
}

func TestComputeDiff_AllRemoved(t *testing.T) {
	a := []string{"old1", "old2"}
	b := []string{}

	result := computeDiff(a, b)
	if len(result) != 2 {
		t.Fatalf("expected 2 lines, got %d", len(result))
	}
	for _, dl := range result {
		if dl.Type != "remove" {
			t.Errorf("expected remove, got %s", dl.Type)
		}
	}
}

func TestComputeDiff_Mixed(t *testing.T) {
	a := []string{"line1", "line2", "line3"}
	b := []string{"line1", "changed", "line3", "added"}

	result := computeDiff(a, b)

	types := make(map[string]int)
	for _, dl := range result {
		types[dl.Type]++
	}

	if types["equal"] != 2 {
		t.Errorf("expected 2 equal lines, got %d", types["equal"])
	}
	if types["add"] != 2 {
		t.Errorf("expected 2 add lines, got %d", types["add"])
	}
	if types["remove"] != 1 {
		t.Errorf("expected 1 remove line, got %d", types["remove"])
	}
}

func TestComputeDiff_Empty(t *testing.T) {
	result := computeDiff([]string{}, []string{})
	if len(result) != 0 {
		t.Errorf("expected 0 lines, got %d", len(result))
	}
}

func TestComputeDiff_SingleLineChange(t *testing.T) {
	a := []string{"hello"}
	b := []string{"world"}

	result := computeDiff(a, b)
	if len(result) != 2 {
		t.Fatalf("expected 2 lines (remove + add), got %d", len(result))
	}

	hasRemove := false
	hasAdd := false
	for _, dl := range result {
		if dl.Type == "remove" && dl.Line == "hello" {
			hasRemove = true
		}
		if dl.Type == "add" && dl.Line == "world" {
			hasAdd = true
		}
	}
	if !hasRemove || !hasAdd {
		t.Errorf("expected remove hello + add world, got %+v", result)
	}
}

// --- isTextType tests ---

func TestIsTextType(t *testing.T) {
	textTypes := []string{
		"text/plain", "text/html", "text/css",
		"application/json", "application/xml",
		"application/javascript", "application/yaml",
	}
	for _, ct := range textTypes {
		if !isTextType(ct) {
			t.Errorf("expected %q to be text type", ct)
		}
	}

	binaryTypes := []string{
		"application/octet-stream", "image/png", "video/mp4",
		"application/pdf", "application/zip",
	}
	for _, ct := range binaryTypes {
		if isTextType(ct) {
			t.Errorf("expected %q to be binary type", ct)
		}
	}
}

// --- buildMetaDiff tests ---

func TestBuildMetaDiff_NoDiff(t *testing.T) {
	a := &metadata.ObjectMeta{ContentType: "text/plain", Size: 100, ETag: "abc"}
	b := &metadata.ObjectMeta{ContentType: "text/plain", Size: 100, ETag: "abc"}

	diff := buildMetaDiff(a, b)
	if len(diff) != 0 {
		t.Errorf("expected no diffs, got %d", len(diff))
	}
}

func TestBuildMetaDiff_AllChanged(t *testing.T) {
	a := &metadata.ObjectMeta{ContentType: "text/plain", Size: 100, ETag: "abc"}
	b := &metadata.ObjectMeta{ContentType: "text/html", Size: 200, ETag: "def"}

	diff := buildMetaDiff(a, b)
	if len(diff) != 3 {
		t.Errorf("expected 3 diffs, got %d", len(diff))
	}
	if diff["content_type"][0] != "text/plain" || diff["content_type"][1] != "text/html" {
		t.Errorf("content_type diff wrong: %v", diff["content_type"])
	}
}

func TestBuildMetaDiff_Tags(t *testing.T) {
	a := &metadata.ObjectMeta{Tags: map[string]string{"env": "dev", "old": "yes"}}
	b := &metadata.ObjectMeta{Tags: map[string]string{"env": "prod", "new": "yes"}}

	diff := buildMetaDiff(a, b)
	if diff["tag:env"][0] != "dev" || diff["tag:env"][1] != "prod" {
		t.Errorf("tag:env diff wrong: %v", diff["tag:env"])
	}
	if diff["tag:old"][0] != "yes" || diff["tag:old"][1] != "" {
		t.Errorf("tag:old diff wrong: %v", diff["tag:old"])
	}
	if diff["tag:new"][0] != "" || diff["tag:new"][1] != "yes" {
		t.Errorf("tag:new diff wrong: %v", diff["tag:new"])
	}
}

// --- readLines tests ---

func TestReadLines(t *testing.T) {
	input := "line1\nline2\nline3"
	lines := readLines(strings.NewReader(input))
	if len(lines) != 3 {
		t.Errorf("expected 3 lines, got %d", len(lines))
	}
	if lines[0] != "line1" || lines[2] != "line3" {
		t.Errorf("unexpected lines: %v", lines)
	}
}

func TestReadLines_Empty(t *testing.T) {
	lines := readLines(strings.NewReader(""))
	if len(lines) != 0 {
		t.Errorf("expected 0 lines, got %d", len(lines))
	}
}
