package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Kodiqa-Solutions/VaultS3/internal/api"
	"github.com/Kodiqa-Solutions/VaultS3/internal/config"
	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
	"github.com/Kodiqa-Solutions/VaultS3/internal/metrics"
	"github.com/Kodiqa-Solutions/VaultS3/internal/storage"
)

// The CLI and the dashboard API each declared their own JSON shapes, and nothing
// checked that the two agreed. They drifted: the API settled on camelCase and
// the CLI was written in snake_case, and when the API was reshaped for the
// dashboard (issue #10) its second consumer was never updated. By 4.4.77 four
// commands failed outright and two more printed blank or wrong values with no
// error at all (issue #62).
//
// These tests run the real CLI functions against the real API handler, mounted
// exactly as the server mounts it. A field renamed on either side breaks them.
// This file is the only place the CLI imports internal/api, and because it is a
// test the CLI binary does not link the server.

type exitCalled int

// cliServer starts the real dashboard API and points the CLI at it.
func cliServer(t *testing.T, tweak func(*config.Config)) *metadata.Store {
	t.Helper()
	dir := t.TempDir()
	store, err := metadata.NewStore(filepath.Join(dir, "meta.db"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	engine, err := storage.NewFileSystem(filepath.Join(dir, "data"))
	if err != nil {
		t.Fatalf("NewFileSystem: %v", err)
	}

	cfg := &config.Config{}
	cfg.Auth.AdminAccessKey = "admin"
	cfg.Auth.AdminSecretKey = "secret-for-tests"
	cfg.Server.Port = 9000
	if tweak != nil {
		tweak(cfg)
	}

	h := api.NewAPIHandler(store, engine, metrics.NewCollector(store, engine), cfg, api.NewActivityLog())
	mux := http.NewServeMux()
	mux.Handle("/api/v1/", h) // as internal/server mounts it
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	prev := [3]string{endpoint, accessKey, secretKey}
	endpoint, accessKey, secretKey = srv.URL, "admin", "secret-for-tests"
	t.Cleanup(func() { endpoint, accessKey, secretKey = prev[0], prev[1], prev[2] })
	return store
}

// runCLI runs one CLI command and returns what it printed and whether it failed.
func runCLI(t *testing.T, args ...string) (stdout, stderr string, failed bool) {
	t.Helper()
	outR, outW, _ := os.Pipe()
	errR, errW, _ := os.Pipe()
	origOut, origErr, origExit := os.Stdout, os.Stderr, exit
	os.Stdout, os.Stderr = outW, errW
	exit = func(code int) { panic(exitCalled(code)) }

	outC, errC := make(chan string), make(chan string)
	go func() { b, _ := io.ReadAll(outR); outC <- string(b) }()
	go func() { b, _ := io.ReadAll(errR); errC <- string(b) }()

	func() {
		defer func() {
			if r := recover(); r != nil {
				if _, ok := r.(exitCalled); !ok {
					panic(r)
				}
				failed = true
			}
		}()
		runUserOrReplication(args)
	}()

	outW.Close()
	errW.Close()
	os.Stdout, os.Stderr, exit = origOut, origErr, origExit
	return <-outC, <-errC, failed
}

func runUserOrReplication(args []string) {
	switch args[0] {
	case "user":
		runUser(args[1:])
	case "replication":
		runReplication(args[1:])
	case "bucket":
		runBucket(args[1:])
	default:
		panic("test helper does not route " + args[0])
	}
}

func TestUserCreateSendsTheNameTheAPIReads(t *testing.T) {
	store := cliServer(t, nil)

	out, errOut, failed := runCLI(t, "user", "create", "alice")
	if failed {
		t.Fatalf("user create failed: %s", errOut)
	}
	if !strings.Contains(out, "alice") {
		t.Errorf("unexpected output: %q", out)
	}
	if _, err := store.GetIAMUser("alice"); err != nil {
		t.Errorf("the CLI reported success but the user does not exist: %v", err)
	}
}

func TestUserCreateRefusesKeyFlagsItCannotHonour(t *testing.T) {
	store := cliServer(t, nil)

	for _, args := range [][]string{
		{"user", "create", "bob", "--access-key=AK", "--secret-key=SK"},
		{"user", "create", "bob", "--access-key", "AK"},
		{"user", "create", "bob", "--secret-key=SK"},
	} {
		_, errOut, failed := runCLI(t, args...)
		if !failed {
			t.Errorf("%v: expected a refusal, the flags would be silently dropped", args[3:])
		}
		if !strings.Contains(errOut, "server generates") {
			t.Errorf("%v: refusal does not say why: %q", args[3:], errOut)
		}
	}
	// Refused means nothing was created either.
	if _, err := store.GetIAMUser("bob"); err == nil {
		t.Error("a refused create still created the user")
	}
}

// Asserting only that the command failed is not enough: on the released code it
// failed too, with the unrelated 400, so a bare "failed" passed against the bug.
// Assert the reason, and that no user was created.
func TestUserCreateRejectsUnknownArguments(t *testing.T) {
	store := cliServer(t, nil)
	_, errOut, failed := runCLI(t, "user", "create", "carol", "--with-keys")
	if !failed {
		t.Fatal("an unrecognised argument was silently ignored")
	}
	if !strings.Contains(errOut, "unknown argument") {
		t.Errorf("failed for the wrong reason: %q", errOut)
	}
	if _, err := store.GetIAMUser("carol"); err == nil {
		t.Error("the user was created despite the bad argument")
	}
}

func TestUserListShowsTheFieldsTheAPIWrites(t *testing.T) {
	store := cliServer(t, nil)
	if err := store.CreateIAMPolicy(metadata.IAMPolicy{
		Name: "ReadOnly", CreatedAt: time.Now(), Document: `{"Version":"2012-10-17","Statement":[]}`,
	}); err != nil {
		t.Fatal(err)
	}
	if _, errOut, failed := runCLI(t, "user", "create", "alice"); failed {
		t.Fatalf("create: %s", errOut)
	}
	if _, errOut, failed := runCLI(t, "user", "attach-policy", "alice", "ReadOnly"); failed {
		t.Fatalf("attach-policy: %s", errOut)
	}

	out, errOut, failed := runCLI(t, "user", "list")
	if failed {
		t.Fatalf("list: %s", errOut)
	}
	// Before the fix this printed a row of dashes: the name and policies columns
	// read keys the API does not send.
	for _, want := range []string{"alice", "ReadOnly"} {
		if !strings.Contains(out, want) {
			t.Errorf("list does not show %q:\n%s", want, out)
		}
	}
}

func TestUserAttachPolicySendsTheKeyTheAPIReads(t *testing.T) {
	store := cliServer(t, nil)
	if err := store.CreateIAMPolicy(metadata.IAMPolicy{
		Name: "ReadOnly", CreatedAt: time.Now(), Document: `{"Version":"2012-10-17","Statement":[]}`,
	}); err != nil {
		t.Fatal(err)
	}
	if _, errOut, failed := runCLI(t, "user", "create", "alice"); failed {
		t.Fatalf("create: %s", errOut)
	}

	if _, errOut, failed := runCLI(t, "user", "attach-policy", "alice", "ReadOnly"); failed {
		t.Fatalf("attach-policy failed: %s", errOut)
	}
	u, err := store.GetIAMUser("alice")
	if err != nil {
		t.Fatal(err)
	}
	if len(u.PolicyARNs) != 1 || u.PolicyARNs[0] != "ReadOnly" {
		t.Errorf("policy not attached, user has %v", u.PolicyARNs)
	}
}

func TestUserDeleteReportsAUserThatDoesNotExist(t *testing.T) {
	store := cliServer(t, nil)

	// The API answers 204 for a missing user, so the CLI has to check.
	_, errOut, failed := runCLI(t, "user", "delete", "ghost")
	if !failed {
		t.Error("deleting a user that never existed reported success")
	}
	if !strings.Contains(errOut, "not found") {
		t.Errorf("unexpected error: %q", errOut)
	}

	// And a real delete still works.
	if _, errOut, failed := runCLI(t, "user", "create", "alice"); failed {
		t.Fatalf("create: %s", errOut)
	}
	if _, errOut, failed := runCLI(t, "user", "delete", "alice"); failed {
		t.Fatalf("delete of an existing user failed: %s", errOut)
	}
	if _, err := store.GetIAMUser("alice"); err == nil {
		t.Error("user still exists after delete")
	}
}

func TestReplicationStatusDecodesTheAPIShape(t *testing.T) {
	cliServer(t, func(c *config.Config) {
		c.Replication.Enabled = true
		c.Replication.Peers = []config.ReplicationPeer{{Name: "dr-site", URL: "http://dr.example:9000"}}
	})

	out, errOut, failed := runCLI(t, "replication", "status")
	if failed {
		t.Fatalf("replication status failed: %s", errOut)
	}
	for _, want := range []string{"dr-site", "http://dr.example:9000"} {
		if !strings.Contains(out, want) {
			t.Errorf("status does not show %q:\n%s", want, out)
		}
	}
}

func TestReplicationStatusWhenDisabled(t *testing.T) {
	cliServer(t, nil)
	out, errOut, failed := runCLI(t, "replication", "status")
	if failed {
		t.Fatalf("replication status failed: %s", errOut)
	}
	if !strings.Contains(out, "not enabled") {
		t.Errorf("unexpected output: %q", out)
	}
}

func TestReplicationQueueShowsRetriesAndDate(t *testing.T) {
	store := cliServer(t, nil)
	created := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	if err := store.EnqueueReplication(metadata.ReplicationEvent{
		Type: "put", Bucket: "b", Key: "k", Peer: "dr-site",
		RetryCount: 3, CreatedAt: created.Unix(),
	}); err != nil {
		t.Fatal(err)
	}

	out, errOut, failed := runCLI(t, "replication", "queue")
	if failed {
		t.Fatalf("replication queue failed: %s", errOut)
	}
	// Before the fix every event listed with 0 retries and a 1970 date.
	if !strings.Contains(out, "2026-03-04") {
		t.Errorf("queue does not show the event's date:\n%s", out)
	}
	if strings.Contains(out, "1970") {
		t.Errorf("queue shows the zero date:\n%s", out)
	}
	if !hasField(out, "3") {
		t.Errorf("queue does not show the retry count of 3:\n%s", out)
	}
}

// hasField reports whether any whitespace-separated field equals want.
func hasField(out, want string) bool {
	for _, f := range strings.Fields(out) {
		if f == want {
			return true
		}
	}
	return false
}

// The user name goes into the URL path, and the API routes on the decoded path.
// Unescaped, a '?' ended the path and a '#' began a fragment, so the request
// named a DIFFERENT user: "user delete 'a?b'" deleted the user "a" and then
// reported "a?b" as deleted. Both users exist here so the wrong one is visible.
func TestUserDeleteActsOnTheNamedUserOnly(t *testing.T) {
	store := cliServer(t, nil)
	for _, n := range []string{"a", "a?b", "c", "c#d", "e f"} {
		if err := store.CreateIAMUser(metadata.IAMUser{Name: n, CreatedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}

	for _, target := range []string{"a?b", "c#d", "e f"} {
		if _, errOut, failed := runCLI(t, "user", "delete", target); failed {
			t.Errorf("delete %q failed: %s", target, errOut)
		}
		if _, err := store.GetIAMUser(target); err == nil {
			t.Errorf("delete %q reported success but the user still exists", target)
		}
	}
	for _, bystander := range []string{"a", "c"} {
		if _, err := store.GetIAMUser(bystander); err != nil {
			t.Errorf("deleting a different user removed %q", bystander)
		}
	}
}

func TestUserAttachPolicyActsOnTheNamedUserOnly(t *testing.T) {
	store := cliServer(t, nil)
	if err := store.CreateIAMPolicy(metadata.IAMPolicy{
		Name: "ReadOnly", CreatedAt: time.Now(), Document: `{"Version":"2012-10-17","Statement":[]}`,
	}); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"a", "a?b"} {
		if err := store.CreateIAMUser(metadata.IAMUser{Name: n, CreatedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}

	if _, errOut, failed := runCLI(t, "user", "attach-policy", "a?b", "ReadOnly"); failed {
		t.Fatalf("attach failed: %s", errOut)
	}
	target, _ := store.GetIAMUser("a?b")
	bystander, _ := store.GetIAMUser("a")
	if len(target.PolicyARNs) != 1 {
		t.Errorf("policy not attached to the named user: %v", target.PolicyARNs)
	}
	if len(bystander.PolicyARNs) != 0 {
		t.Errorf("policy attached to a DIFFERENT user: %v", bystander.PolicyARNs)
	}
}

// The API splits the decoded path on '/', so a name containing one can never be
// addressed again, by the CLI or by the dashboard, and escaping cannot help.
// Refuse to create one, and say why instead of answering "not found" for a user
// that exists.
func TestUserNamesWithASlashAreRefused(t *testing.T) {
	store := cliServer(t, nil)

	_, errOut, failed := runCLI(t, "user", "create", "team/alice")
	if !failed || !strings.Contains(errOut, "'/'") {
		t.Errorf("create of a name with '/' was not refused with a reason: failed=%v %q", failed, errOut)
	}
	if _, err := store.GetIAMUser("team/alice"); err == nil {
		t.Error("an unaddressable user was created")
	}

	if err := store.CreateIAMUser(metadata.IAMUser{Name: "team/bob", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"user", "delete", "team/bob"},
		{"user", "attach-policy", "team/bob", "ReadOnly"},
	} {
		_, errOut, failed := runCLI(t, args...)
		if !failed || !strings.Contains(errOut, "'/'") {
			t.Errorf("%v: expected a refusal naming the '/', got failed=%v %q", args[1], failed, errOut)
		}
	}
}

// Bucket names cannot legally contain '?', but the CLI sent whatever it was
// given, so a typo like "bucket info 'photos?x'" answered with the details of
// the bucket "photos" as if they belonged to the name typed.
func TestBucketInfoActsOnTheNamedBucketOnly(t *testing.T) {
	store := cliServer(t, nil)
	if err := store.CreateBucket("photos"); err != nil {
		t.Fatal(err)
	}
	out, _, failed := runCLI(t, "bucket", "info", "photos?x")
	if !failed {
		t.Errorf("info for a bucket that does not exist succeeded, showing:\n%s", out)
	}
	if strings.Contains(out, `"photos"`) {
		t.Errorf("answered with a different bucket's details:\n%s", out)
	}
}
