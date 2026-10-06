package main

import (
	"regexp"
	"strings"
	"testing"

	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
)

var secretLine = regexp.MustCompile(`(?m)^Secret key: (\S+)$`)
var accessLine = regexp.MustCompile(`(?m)^Access key: (\S+)$`)

// A user created from the CLI had no way to get credentials from the CLI
// (issue #62). key create prints the pair the server generated, and it must be
// the pair the server stored.
func TestKeyCreatePrintsTheCredentialsTheServerStored(t *testing.T) {
	store := cliServer(t, nil)
	if err := store.CreateBucket("bkt-one"); err != nil {
		t.Fatal(err)
	}

	out, errOut, failed := runCLI(t, "key", "create", "alice", "--bucket", "bkt-one")
	if failed {
		t.Fatalf("key create failed: %s", errOut)
	}
	ak, sk := accessLine.FindStringSubmatch(out), secretLine.FindStringSubmatch(out)
	if ak == nil || sk == nil {
		t.Fatalf("no credentials in the output: %q", out)
	}
	k, err := store.GetAccessKey(ak[1])
	if err != nil {
		t.Fatalf("printed access key %s does not exist: %v", ak[1], err)
	}
	if k.SecretKey != sk[1] {
		t.Errorf("printed secret does not match the stored one")
	}
	if k.UserID != "alice" {
		t.Errorf("key belongs to %q, want alice", k.UserID)
	}
	p, err := store.GetIAMPolicy(k.PolicyName)
	if err != nil {
		t.Fatalf("key has no policy: %v", err)
	}
	if !strings.Contains(p.Document, "bkt-one") || strings.Contains(p.Document, `"*"`) {
		t.Errorf("key grant is %s, want bkt-one only", p.Document)
	}
}

// The API gives a key every bucket when none are named. The CLI must not take
// that by accident: no scope is a refusal, and nothing is created.
func TestKeyCreateRefusesWithoutAScope(t *testing.T) {
	store := cliServer(t, nil)

	_, errOut, failed := runCLI(t, "key", "create", "alice")
	if !failed {
		t.Fatal("a key was issued without saying which buckets it can reach")
	}
	if !strings.Contains(errOut, "--all-buckets") {
		t.Errorf("refused for the wrong reason: %q", errOut)
	}
	if keys, _ := store.ListAccessKeys(); len(keys) != 0 {
		t.Errorf("a refused create still issued %d key(s)", len(keys))
	}
}

func TestKeyCreateAllBucketsIsExplicit(t *testing.T) {
	store := cliServer(t, nil)

	out, errOut, failed := runCLI(t, "key", "create", "alice", "--all-buckets")
	if failed {
		t.Fatalf("key create --all-buckets failed: %s", errOut)
	}
	if !strings.Contains(out, "every bucket") {
		t.Errorf("output does not say the key reaches every bucket: %q", out)
	}
	keys, _ := store.ListAccessKeys()
	if len(keys) != 1 {
		t.Fatalf("want one key, got %d", len(keys))
	}
	p, _ := store.GetIAMPolicy(keys[0].PolicyName)
	if p == nil || !strings.Contains(p.Document, `"*"`) {
		t.Errorf("--all-buckets key grant is %v", p)
	}
}

func TestKeyCreateRejectsConflictingAndUnknownArguments(t *testing.T) {
	store := cliServer(t, nil)
	if err := store.CreateBucket("bkt-one"); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		args   []string
		reason string
	}{
		{[]string{"key", "create", "alice", "--bucket", "bkt-one", "--all-buckets"}, "only one of"},
		{[]string{"key", "create", "alice", "--all-buckets", "--user-policies"}, "only one of"},
		{[]string{"key", "create", "alice", "--user-policies"}, "no policies"},
		{[]string{"key", "create", "alice", "--bucket"}, "needs a bucket name"},
		{[]string{"key", "create", "alice", "--bucket="}, "needs a bucket name"},
		{[]string{"key", "create", "alice", "--read-only"}, "unknown argument"},
		{[]string{"key", "create", "--bucket", "bkt-one"}, "requires a user name"},
		{[]string{"key", "create", "alice", "--bucket", "no-such-bucket"}, "does not exist"},
	} {
		_, errOut, failed := runCLI(t, c.args...)
		if !failed {
			t.Errorf("%v: accepted", c.args[2:])
			continue
		}
		if !strings.Contains(errOut, c.reason) {
			t.Errorf("%v: refused for the wrong reason: %q", c.args[2:], errOut)
		}
	}
	if keys, _ := store.ListAccessKeys(); len(keys) != 0 {
		t.Errorf("refused creates still issued %d key(s)", len(keys))
	}
}

func TestKeyListShowsEachKeysUser(t *testing.T) {
	cliServer(t, nil)
	out, errOut, failed := runCLI(t, "key", "create", "alice", "--all-buckets")
	if failed {
		t.Fatalf("create: %s", errOut)
	}
	ak := accessLine.FindStringSubmatch(out)[1]

	out, errOut, failed = runCLI(t, "key", "list")
	if failed {
		t.Fatalf("key list failed: %s", errOut)
	}
	var keyRow, adminRow string
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, ak) {
			keyRow = line
		}
		if strings.HasPrefix(line, "admin ") {
			adminRow = line
		}
	}
	if !strings.Contains(keyRow, "alice") {
		t.Errorf("the key's row does not name its user: %q\n%s", keyRow, out)
	}
	if !strings.Contains(adminRow, "built-in admin") {
		t.Errorf("the admin key is not labelled: %q\n%s", adminRow, out)
	}
}

func TestKeyDeleteRemovesTheKeyAndReportsAMissingOne(t *testing.T) {
	store := cliServer(t, nil)
	out, _, _ := runCLI(t, "key", "create", "alice", "--all-buckets")
	ak := accessLine.FindStringSubmatch(out)[1]

	if _, errOut, failed := runCLI(t, "key", "delete", ak); failed {
		t.Fatalf("key delete failed: %s", errOut)
	}
	if _, err := store.GetAccessKey(ak); err == nil {
		t.Error("the CLI reported the key deleted but it still exists")
	}

	_, errOut, failed := runCLI(t, "key", "delete", ak)
	if !failed || !strings.Contains(errOut, "not found") {
		t.Errorf("deleting a missing key: failed=%v %q", failed, errOut)
	}
}

// The reporter's flow: create a user, give it credentials, later revoke them.
// Revoking the last key must not take the user with it.
func TestUserThenKeyThenRevokeKeepsTheUser(t *testing.T) {
	store := cliServer(t, nil)
	if err := store.CreateBucket("bkt-one"); err != nil {
		t.Fatal(err)
	}
	out, errOut, failed := runCLI(t, "user", "create", "alice")
	if failed {
		t.Fatalf("user create: %s", errOut)
	}
	if !strings.Contains(out, "key create alice") {
		t.Errorf("user create does not say how to get credentials: %q", out)
	}
	out, errOut, failed = runCLI(t, "key", "create", "alice", "--bucket", "bkt-one")
	if failed {
		t.Fatalf("key create: %s", errOut)
	}
	ak := accessLine.FindStringSubmatch(out)[1]
	if _, errOut, failed := runCLI(t, "key", "delete", ak); failed {
		t.Fatalf("key delete: %s", errOut)
	}
	if _, err := store.GetIAMUser("alice"); err != nil {
		t.Errorf("alice was created on purpose and was deleted with her key: %v", err)
	}
}

// The documented flow: a user with a read-only policy, then a key. The key must
// be limited to that policy, and the CLI must say so.
func TestKeyCreateUserPoliciesLimitsTheKeyToThem(t *testing.T) {
	store := cliServer(t, nil)
	if _, errOut, failed := runCLI(t, "user", "create", "alice"); failed {
		t.Fatalf("user create: %s", errOut)
	}
	if err := store.CreateIAMPolicy(metadata.IAMPolicy{Name: "ro", Document: `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":["s3:GetObject"],"Resource":["*"]}]}`}); err != nil {
		t.Fatal(err)
	}
	if _, errOut, failed := runCLI(t, "user", "attach-policy", "alice", "ro"); failed {
		t.Fatalf("attach: %s", errOut)
	}

	out, errOut, failed := runCLI(t, "key", "create", "alice", "--user-policies")
	if failed {
		t.Fatalf("key create --user-policies failed: %s", errOut)
	}
	if !strings.Contains(out, "exactly what the user's own policies allow") {
		t.Errorf("output does not say the key is limited to the user's policies: %q", out)
	}
	k, err := store.GetAccessKey(accessLine.FindStringSubmatch(out)[1])
	if err != nil {
		t.Fatal(err)
	}
	if k.PolicyName != "" {
		t.Errorf("a key limited to the user's policies was given a grant of its own: %s", k.PolicyName)
	}
}

// A bucket grant is added to the user's own policies, not intersected with
// them. The output must not claim the key reaches only the named buckets.
func TestKeyCreateBucketOutputDoesNotOverclaimTheLimit(t *testing.T) {
	store := cliServer(t, nil)
	if err := store.CreateBucket("bkt-one"); err != nil {
		t.Fatal(err)
	}
	out, errOut, failed := runCLI(t, "key", "create", "alice", "--bucket", "bkt-one")
	if failed {
		t.Fatalf("key create: %s", errOut)
	}
	if !strings.Contains(out, "bkt-one, plus anything the user's own policies allow") {
		t.Errorf("output: %q", out)
	}
}
