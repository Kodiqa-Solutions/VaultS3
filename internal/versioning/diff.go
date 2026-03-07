package versioning

import (
	"bufio"
	"fmt"
	"io"
	"strings"

	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
	"github.com/Kodiqa-Solutions/VaultS3/internal/storage"
)

type DiffLine struct {
	Type string `json:"type"` // "add", "remove", "equal"
	Line string `json:"line"`
	Num  int    `json:"num,omitempty"`
}

type DiffResult struct {
	Type     string               `json:"type"` // "text" or "binary"
	Lines    []DiffLine           `json:"lines,omitempty"`
	MetaDiff map[string][2]string `json:"meta_diff,omitempty"`
	SizeA    int64                `json:"size_a"`
	SizeB    int64                `json:"size_b"`
}

// Diff compares two versions of an object.
func Diff(store *metadata.Store, engine storage.Engine, bucket, key, versionA, versionB string) (*DiffResult, error) {
	metaA, err := store.GetObjectVersion(bucket, key, versionA)
	if err != nil {
		return nil, fmt.Errorf("version %s not found: %w", versionA, err)
	}
	metaB, err := store.GetObjectVersion(bucket, key, versionB)
	if err != nil {
		return nil, fmt.Errorf("version %s not found: %w", versionB, err)
	}

	result := &DiffResult{
		SizeA:    metaA.Size,
		SizeB:    metaB.Size,
		MetaDiff: buildMetaDiff(metaA, metaB),
	}

	// Check if text content type
	if !isTextType(metaA.ContentType) || !isTextType(metaB.ContentType) {
		result.Type = "binary"
		return result, nil
	}

	// Read both versions
	readerA, _, err := engine.GetObjectVersion(bucket, key, versionA)
	if err != nil {
		return nil, fmt.Errorf("read version %s: %w", versionA, err)
	}
	defer readerA.Close()

	readerB, _, err := engine.GetObjectVersion(bucket, key, versionB)
	if err != nil {
		return nil, fmt.Errorf("read version %s: %w", versionB, err)
	}
	defer readerB.Close()

	linesA := readLines(readerA)
	linesB := readLines(readerB)

	result.Type = "text"
	result.Lines = computeDiff(linesA, linesB)
	return result, nil
}

func isTextType(ct string) bool {
	if strings.HasPrefix(ct, "text/") {
		return true
	}
	textTypes := []string{
		"application/json", "application/xml", "application/javascript",
		"application/yaml", "application/x-yaml", "application/toml",
	}
	for _, t := range textTypes {
		if ct == t {
			return true
		}
	}
	return false
}

func readLines(r io.Reader) []string {
	var lines []string
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	return lines
}

func buildMetaDiff(a, b *metadata.ObjectMeta) map[string][2]string {
	diff := make(map[string][2]string)

	if a.ContentType != b.ContentType {
		diff["content_type"] = [2]string{a.ContentType, b.ContentType}
	}
	if a.Size != b.Size {
		diff["size"] = [2]string{fmt.Sprintf("%d", a.Size), fmt.Sprintf("%d", b.Size)}
	}
	if a.ETag != b.ETag {
		diff["etag"] = [2]string{a.ETag, b.ETag}
	}

	// Diff tags
	allTags := make(map[string]bool)
	for k := range a.Tags {
		allTags[k] = true
	}
	for k := range b.Tags {
		allTags[k] = true
	}
	for k := range allTags {
		va, vb := a.Tags[k], b.Tags[k]
		if va != vb {
			diff["tag:"+k] = [2]string{va, vb}
		}
	}

	return diff
}

// computeDiff produces a simple unified diff using LCS.
func computeDiff(a, b []string) []DiffLine {
	// Build LCS table
	m, n := len(a), len(b)

	// For very large files, limit diff to first 5000 lines
	if m > 5000 {
		a = a[:5000]
		m = 5000
	}
	if n > 5000 {
		b = b[:5000]
		n = 5000
	}

	lcs := make([][]int, m+1)
	for i := range lcs {
		lcs[i] = make([]int, n+1)
	}
	for i := m - 1; i >= 0; i-- {
		for j := n - 1; j >= 0; j-- {
			if a[i] == b[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else if lcs[i+1][j] >= lcs[i][j+1] {
				lcs[i][j] = lcs[i+1][j]
			} else {
				lcs[i][j] = lcs[i][j+1]
			}
		}
	}

	// Trace back to produce diff
	var result []DiffLine
	i, j := 0, 0
	lineNum := 1
	for i < m || j < n {
		if i < m && j < n && a[i] == b[j] {
			result = append(result, DiffLine{Type: "equal", Line: a[i], Num: lineNum})
			i++
			j++
			lineNum++
		} else if j < n && (i >= m || lcs[i][j+1] >= lcs[i+1][j]) {
			result = append(result, DiffLine{Type: "add", Line: b[j], Num: lineNum})
			j++
			lineNum++
		} else if i < m {
			result = append(result, DiffLine{Type: "remove", Line: a[i]})
			i++
		}
	}

	return result
}
