package storage

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The security assessment listed path traversal protection at the S3, API,
// versioning, copy-source and filesystem layers. Unit tests cover the cases
// somebody thought of. This covers the ones nobody thought of.
//
// The invariant is deliberately stated against the DATA DIRECTORY, not against
// the bucket directory. Checking containment against a path that itself contains
// attacker-controlled input proves nothing: if the bucket name escapes, the
// bucket directory escapes with it and a prefix comparison between the two still
// succeeds while the resolved path sits outside the store.
func FuzzObjectPathStaysInsideDataDir(f *testing.F) {
	seeds := []struct{ bucket, key string }{
		{"b", "a.txt"},
		{"b", "nested/dir/a.txt"},
		{"b", "../escape"},
		{"b", "../../escape"},
		{"b", "a/../../escape"},
		{"b", "./a"},
		{"b", ""},
		{"b", "/abs"},
		{"b", "a//b"},
		{"b", ".."},
		{"b", "dir/"},
		{"b", "a\\..\\b"},
		{"", "k"},
		{"..", "k"},
		{"../evil", "k"},
		{"a/../..", "k"},
	}
	for _, s := range seeds {
		f.Add(s.bucket, s.key)
	}

	root := f.TempDir()
	dataDir := filepath.Join(root, "data")
	fs, err := NewFileSystem(dataDir)
	if err != nil {
		f.Fatal(err)
	}
	// Compare against the cleaned absolute data dir so the assertion does not
	// depend on how the caller spelled the path.
	absData, err := filepath.Abs(dataDir)
	if err != nil {
		f.Fatal(err)
	}
	absData = filepath.Clean(absData)

	f.Fuzz(func(t *testing.T, bucket, key string) {
		got := fs.ObjectPath(bucket, key)

		abs, err := filepath.Abs(got)
		if err != nil {
			t.Fatalf("ObjectPath(%q, %q) = %q, not a resolvable path: %v", bucket, key, got, err)
		}
		abs = filepath.Clean(abs)

		if abs != absData && !strings.HasPrefix(abs, absData+string(filepath.Separator)) {
			t.Fatalf("path escaped the data dir\n  bucket:  %q\n  key:     %q\n  result:  %q\n  dataDir: %q",
				bucket, key, abs, absData)
		}
	})
}

// Same invariant for the version path, which builds a deeper path (.vs/key/id)
// from three attacker-influenced segments instead of two.
func FuzzVersionPathStaysInsideDataDir(f *testing.F) {
	seeds := []struct{ bucket, key, version string }{
		{"b", "a.txt", "v1"},
		{"b", "a.txt", ".."},
		{"b", "..", "v1"},
		{"b", "a", "../../../etc/passwd"},
		{"b", "../..", "v"},
		{"../evil", "a", "v"},
		{"b", "", ""},
	}
	for _, s := range seeds {
		f.Add(s.bucket, s.key, s.version)
	}

	root := f.TempDir()
	dataDir := filepath.Join(root, "data")
	fs, err := NewFileSystem(dataDir)
	if err != nil {
		f.Fatal(err)
	}
	absData, err := filepath.Abs(dataDir)
	if err != nil {
		f.Fatal(err)
	}
	absData = filepath.Clean(absData)

	f.Fuzz(func(t *testing.T, bucket, key, version string) {
		got := fs.versionPath(bucket, key, version)

		abs, err := filepath.Abs(got)
		if err != nil {
			t.Fatalf("versionPath(%q, %q, %q) = %q: %v", bucket, key, version, got, err)
		}
		abs = filepath.Clean(abs)

		if abs != absData && !strings.HasPrefix(abs, absData+string(filepath.Separator)) {
			t.Fatalf("version path escaped the data dir\n  bucket:  %q\n  key:     %q\n  version: %q\n  result:  %q\n  dataDir: %q",
				bucket, key, version, abs, absData)
		}
	})
}

// CreateBucketDir and DeleteBucketDir take a bucket name and act on the
// filesystem with it. The S3 API validates bucket names, but two callers do not:
// internal/migrate creates buckets from names listed by a remote S3 endpoint the
// user pointed at, and internal/replication creates them from a peer's change
// feed. Before the containment fix, CreateBucketDir("../../pwned") created a
// directory outside the data dir and DeleteBucketDir would have recursively
// removed one.
func TestBucketDirOperationsRefuseToEscapeTheDataDir(t *testing.T) {
	escapes := []string{
		"../pwned",
		"../../pwned",
		"a/../../pwned",
		"..",
		"../",
	}

	for _, bucket := range escapes {
		t.Run(bucket, func(t *testing.T) {
			root := t.TempDir()
			dataDir := filepath.Join(root, "data")
			fs, err := NewFileSystem(dataDir)
			if err != nil {
				t.Fatal(err)
			}

			if err := fs.CreateBucketDir(bucket); err == nil {
				t.Errorf("CreateBucketDir(%q) succeeded, want a refusal", bucket)
			}
			if err := fs.DeleteBucketDir(bucket); err == nil {
				t.Errorf("DeleteBucketDir(%q) succeeded, want a refusal", bucket)
			}

			// Nothing may have appeared beside the data dir.
			entries, err := os.ReadDir(root)
			if err != nil {
				t.Fatal(err)
			}
			for _, e := range entries {
				if e.Name() != "data" {
					t.Errorf("bucket %q created %q next to the data dir", bucket, e.Name())
				}
			}
		})
	}
}

// An absolute-looking bucket name is NOT an escape. filepath.Join treats a
// leading separator as an ordinary component, so "/etc" resolves to
// <dataDir>/etc, which is contained. Pinned because it looks alarming and a
// future reader might "fix" it into a rejection and break real bucket names.
func TestAbsoluteLookingBucketNameIsContainedNotAnEscape(t *testing.T) {
	root := t.TempDir()
	dataDir := filepath.Join(root, "data")
	fs, err := NewFileSystem(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := fs.CreateBucketDir("/etc"); err != nil {
		t.Fatalf("CreateBucketDir(\"/etc\") = %v, want it contained under the data dir", err)
	}
	if st, err := os.Stat(filepath.Join(dataDir, "etc")); err != nil || !st.IsDir() {
		t.Fatalf("expected <dataDir>/etc to exist: %v", err)
	}
	if _, err := os.Stat("/etc/../etc"); err != nil {
		t.Skip("no /etc on this platform")
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != "data" {
			t.Errorf("created %q next to the data dir", e.Name())
		}
	}
}

// The ordinary case must keep working: a normal bucket is created and removed.
func TestBucketDirOperationsStillWorkForNormalNames(t *testing.T) {
	root := t.TempDir()
	dataDir := filepath.Join(root, "data")
	fs, err := NewFileSystem(dataDir)
	if err != nil {
		t.Fatal(err)
	}

	if err := fs.CreateBucketDir("my-bucket"); err != nil {
		t.Fatalf("CreateBucketDir: %v", err)
	}
	if st, err := os.Stat(filepath.Join(dataDir, "my-bucket")); err != nil || !st.IsDir() {
		t.Fatalf("bucket dir was not created: %v", err)
	}
	if err := fs.DeleteBucketDir("my-bucket"); err != nil {
		t.Fatalf("DeleteBucketDir: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "my-bucket")); !os.IsNotExist(err) {
		t.Fatal("bucket dir was not removed")
	}
}

// IsSafeBucketName is the check callers use when a bucket name came from
// somewhere other than our own validated API. It must reject traversal without
// imposing the full S3 naming rule, because a migration should not fail over a
// source bucket being two characters long.
func TestIsSafeBucketName(t *testing.T) {
	unsafe := []string{"", ".", "..", "../evil", "a/b", "a/../..", "./a", "/abs", "a/"}
	for _, name := range unsafe {
		if IsSafeBucketName(name) {
			t.Errorf("IsSafeBucketName(%q) = true, want false", name)
		}
	}
	// Short and dotted names are legal here even though CreateBucket would
	// reject some of them. This is a safety check, not the S3 naming rule.
	safe := []string{"b", "ab", "my-bucket", "my.bucket", "bucket123", "UPPER"}
	for _, name := range safe {
		if !IsSafeBucketName(name) {
			t.Errorf("IsSafeBucketName(%q) = false, want true", name)
		}
	}
}

// Whatever IsSafeBucketName accepts must actually stay inside the data dir. This
// ties the predicate to the guarantee instead of letting them drift apart.
func FuzzIsSafeBucketNameAgreesWithContainment(f *testing.F) {
	for _, s := range []string{"b", "my-bucket", "..", "../evil", "a/b", ".", "", "a/../..", "UPPER", "a."} {
		f.Add(s)
	}
	root := f.TempDir()
	dataDir := filepath.Join(root, "data")
	fs, err := NewFileSystem(dataDir)
	if err != nil {
		f.Fatal(err)
	}
	absData, _ := filepath.Abs(dataDir)
	absData = filepath.Clean(absData)

	f.Fuzz(func(t *testing.T, bucket string) {
		if !IsSafeBucketName(bucket) {
			return // rejected, nothing to guarantee
		}
		abs, err := filepath.Abs(fs.bucketPath(bucket))
		if err != nil {
			t.Fatalf("bucketPath(%q): %v", bucket, err)
		}
		if !strings.HasPrefix(filepath.Clean(abs), absData+string(filepath.Separator)) {
			t.Fatalf("IsSafeBucketName(%q) said safe but path escaped: %q", bucket, abs)
		}
	})
}
