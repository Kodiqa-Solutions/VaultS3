package migrate

import "testing"

// Keys listed by the migration source went straight to the storage engine, so a
// source listing "../victim/a.txt" wrote into another bucket, and one listing a
// key under .vs/ or .ec/ overwrote the engine's own files.
func TestUnsafeSourceKeysAreRefused(t *testing.T) {
	for key, bad := range map[string]bool{
		"a.txt": false, "dir/a.txt": false, "a..b": false, ".hidden": false, "dir/.vs/x": false,
		"../victim/a.txt": true, "a/../../b": true, "/abs": true, "": true, "a\x00b": true,
		".vs/k/v1": true, ".ec/k/shard-00": true, ".multipart/u/part-1": true, ".vaults3-tmp-1": true,
	} {
		if got := unsafeKey(key) != ""; got != bad {
			t.Errorf("unsafeKey(%q) refused=%v, want %v", key, got, bad)
		}
	}
}
