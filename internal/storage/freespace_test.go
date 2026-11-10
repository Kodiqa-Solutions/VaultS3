package storage

import (
	"fmt"
	"runtime"
	"strings"
	"syscall"
	"testing"
)

func TestSpaceGuard(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("free space is not measured on windows")
	}
	dir := t.TempDir()
	g := NewSpaceGuard(1<<62, dir) // more than any disk has
	g.Check()
	if !g.Low() {
		t.Error("a reserve larger than the disk is not low")
	}
	g = NewSpaceGuard(1, dir)
	g.Check()
	if g.Low() {
		t.Error("a 1-byte reserve is low")
	}
	g = NewSpaceGuard(0, dir) // disabled
	g.Check()
	if g.Low() {
		t.Error("a disabled guard is low")
	}
	var none *SpaceGuard
	if none.Low() {
		t.Error("a nil guard is low")
	}
	if !IsNoSpace(fmt.Errorf("write object: %w", syscall.ENOSPC)) || IsNoSpace(fmt.Errorf("other")) {
		t.Error("IsNoSpace")
	}
}

// Every writer of object data is refused below the reserve, not only the S3
// API: batch jobs, migrations, dashboard rollbacks and replica copies write
// through the engine.
func TestTheGuardedEngineRefusesWritesWhenLow(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("free space is not measured on windows")
	}
	dir := t.TempDir()
	fs, err := NewFileSystem(dir)
	if err != nil {
		t.Fatal(err)
	}
	_ = fs.CreateBucketDir("b")
	g := NewSpaceGuard(1<<62, dir)
	g.Check()
	e := NewGuardedEngine(fs, g)
	if _, _, err := e.PutObject("b", "k", strings.NewReader("x"), 1); !IsNoSpace(err) {
		t.Errorf("PutObject while low: %v", err)
	}
	if _, _, err := e.PutObjectVersion("b", "k", "v1", strings.NewReader("x"), 1); !IsNoSpace(err) {
		t.Errorf("PutObjectVersion while low: %v", err)
	}
	if fs.ObjectExists("b", "k") {
		t.Error("a refused write reached the disk")
	}
	if _, ok := EngineAs[*FileSystem](e); !ok {
		t.Error("the wrapped engine cannot be found through the guard")
	}
}
