package storage

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAStagedCopyRoundTripsAndIsNotStoredInTheClear(t *testing.T) {
	dir := t.TempDir()
	body := strings.Repeat("secret object bytes ", 1000)
	s, err := StageCopy(dir, strings.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatal(err)
	}
	files, _ := filepath.Glob(filepath.Join(dir, StagingDirName, "*"))
	if len(files) != 1 {
		t.Fatalf("%d staged files", len(files))
	}
	raw, _ := os.ReadFile(files[0])
	if bytes.Contains(raw, []byte("secret object")) {
		t.Error("the staged file holds the bytes in the clear")
	}
	for i := 0; i < 2; i++ { // readable more than once
		r, err := s.Reader()
		if err != nil {
			t.Fatal(err)
		}
		got, _ := io.ReadAll(r)
		if string(got) != body {
			t.Fatalf("read %d: %d bytes back, want %d", i, len(got), len(body))
		}
	}
	s.Close()
	if _, err := os.Stat(files[0]); !os.IsNotExist(err) {
		t.Error("Close left the staged file")
	}
	if _, err := StageCopy(dir, strings.NewReader("short"), 10); err == nil {
		t.Error("a copy shorter than announced was accepted")
	}
	if files, _ := filepath.Glob(filepath.Join(dir, StagingDirName, "*")); len(files) != 0 {
		t.Errorf("a failed copy left %d files", len(files))
	}
}
