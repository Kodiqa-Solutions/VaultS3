package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"strconv"
	"strings"
)

func runKey(args []string) {
	if len(args) == 0 {
		fmt.Println(`Usage: vaults3-cli key <subcommand>

Subcommands:
  list                                         List access keys
  create <user> --bucket <name> [--bucket ..]  Issue a key with access to these buckets
  create <user> --all-buckets                  Issue a key with access to every bucket
  create <user> --user-policies                Issue a key limited to the user's own policies
  delete <access-key>                          Delete an access key

The server generates the access key and secret key. The secret is printed once,
when the key is created, and cannot be shown again.`)
		os.Exit(1)
	}

	requireCreds()

	switch args[0] {
	case "list", "ls":
		keyList()
	case "create":
		keyCreate(args[1:])
	case "delete", "rm":
		if len(args) < 2 {
			fatal("key delete requires an access key")
		}
		keyDelete(args[1])
	default:
		fatal("unknown key subcommand: " + args[0])
	}
}

func keyList() {
	resp, err := apiRequest("GET", "/keys", nil)
	if err != nil {
		fatal(err.Error())
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		fatal(fmt.Sprintf("HTTP %d: %s", resp.StatusCode, string(body)))
	}

	var keys []struct {
		AccessKey string `json:"accessKey"`
		CreatedAt string `json:"createdAt"`
		IsAdmin   bool   `json:"isAdmin"`
		UserID    string `json:"userId"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&keys); err != nil {
		fatal("parse response: " + err.Error())
	}

	headers := []string{"ACCESS KEY", "USER", "CREATED"}
	var rows [][]string
	for _, k := range keys {
		user := k.UserID
		if k.IsAdmin {
			user = "(built-in admin)"
		}
		rows = append(rows, []string{k.AccessKey, orDash(user), orDash(k.CreatedAt)})
	}
	printTable(headers, rows)
}

// keyCreate issues an access key for a user, creating the user if it does not
// exist yet.
//
// With nothing named the API decides for itself: every bucket for a user with
// no policies, the user's own policies otherwise. Here the caller has to say
// which, so nobody issues a key that can reach every bucket by accident.
func keyCreate(args []string) {
	if len(args) < 1 || strings.HasPrefix(args[0], "-") {
		fatal("key create requires a user name")
	}
	user := args[0]

	buckets := []string{}
	all, userOnly := false, false
	rest := args[1:]
	for i := 0; i < len(rest); i++ {
		arg := rest[i]
		switch {
		case arg == "--all-buckets":
			all = true
		case arg == "--user-policies":
			userOnly = true
		case arg == "--bucket":
			if i+1 >= len(rest) || strings.HasPrefix(rest[i+1], "-") {
				fatal("--bucket needs a bucket name")
			}
			i++
			buckets = append(buckets, rest[i])
		case strings.HasPrefix(arg, "--bucket="):
			name := strings.TrimPrefix(arg, "--bucket=")
			if name == "" {
				fatal("--bucket needs a bucket name")
			}
			buckets = append(buckets, name)
		default:
			fatal("unknown argument for key create: " + arg)
		}
	}
	modes := 0
	for _, set := range []bool{len(buckets) > 0, all, userOnly} {
		if set {
			modes++
		}
	}
	if modes > 1 {
		fatal("use only one of --bucket, --all-buckets and --user-policies")
	}
	if modes == 0 {
		fatal("say what the key can reach: --bucket <name> (repeatable), --all-buckets, or --user-policies")
	}

	// A server older than 4.4.79 would take this request and do something else:
	// it ignores --user-policies and grants every bucket, and any new key
	// rewrites what the user's existing keys can reach. Undoing that afterwards
	// is no better, because deleting a key there deletes its user too. So ask
	// first. The oldest node counts, since every node authorizes the key.
	if v, ok := oldestServerVersion(); ok && versionBefore(v, 4, 4, 79) {
		fatal(fmt.Sprintf("the server runs %s, and key create needs 4.4.79 or later. On older servers a key "+
			"can get more access than asked for, and a new key changes what the user's other keys can reach. "+
			"Upgrade the server first", v))
	}

	req := map[string]interface{}{"userId": user}
	switch {
	case all:
		req["allBuckets"] = true
	case userOnly:
		req["userPoliciesOnly"] = true
	default:
		req["buckets"] = buckets
	}
	data, _ := json.Marshal(req)
	resp, err := apiRequest("POST", "/keys", bytes.NewReader(data))
	if err != nil {
		fatal(err.Error())
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 && resp.StatusCode != 201 {
		body, _ := io.ReadAll(resp.Body)
		fatal(fmt.Sprintf("HTTP %d: %s", resp.StatusCode, string(body)))
	}
	var created struct {
		AccessKey string `json:"accessKey"`
		SecretKey string `json:"secretKey"`
		Access    string `json:"access"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		fatal("parse response: " + err.Error())
	}
	if created.AccessKey == "" || created.SecretKey == "" {
		fatal("the server answered without a key")
	}

	// Report what the server granted, not what was asked for.
	var grants string
	switch created.Access {
	case "buckets":
		grants = strings.Join(buckets, ", ") + ", plus anything the user's own policies allow"
	case "all":
		grants = "every bucket"
	case "user":
		grants = "exactly what the user's own policies allow"
	default:
		// Only a server whose version could not be read gets this far without
		// saying. If it predates 4.4.79 it ignored --user-policies.
		grants = "not reported by this server"
		if userOnly {
			grants += ". WARNING: a server older than 4.4.79 ignores --user-policies and grants every bucket"
		}
	}
	fmt.Printf("Access key for user '%s'. It can reach: %s\n\n", user, grants)
	fmt.Printf("Access key: %s\n", created.AccessKey)
	fmt.Printf("Secret key: %s\n\n", created.SecretKey)
	fmt.Println("Store the secret key now. It is not shown again.")
}

func keyDelete(accessKeyID string) {
	resp, err := apiRequest("DELETE", "/keys/"+url.PathEscape(accessKeyID), nil)
	if err != nil {
		fatal(err.Error())
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case 200, 204:
		fmt.Printf("Access key '%s' deleted.\n", accessKeyID)
	case 404:
		fatal(fmt.Sprintf("access key '%s' not found", accessKeyID))
	default:
		body, _ := io.ReadAll(resp.Body)
		fatal(fmt.Sprintf("HTTP %d: %s", resp.StatusCode, string(body)))
	}
}

// oldestServerVersion reports the lowest version among the nodes that answer,
// and false when no node reports a version that can be compared, such as "dev"
// or "main".
func oldestServerVersion() (string, bool) {
	resp, err := apiRequest("GET", "/cluster/info", nil)
	if err != nil {
		return "", false
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", false
	}
	var ci struct {
		Nodes []struct {
			Reachable bool   `json:"reachable"`
			Version   string `json:"version"`
		} `json:"nodes"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&ci); err != nil {
		return "", false
	}
	oldest, found := "", false
	for _, n := range ci.Nodes {
		if _, ok := parseVersion(n.Version); !ok {
			continue
		}
		if !found || versionLess(n.Version, oldest) {
			oldest, found = n.Version, true
		}
	}
	return oldest, found
}

// parseVersion reads "v4.4.79", "4.4.79" or "v4.4.79-local" as [4 4 79].
func parseVersion(v string) ([3]int, bool) {
	var out [3]int
	v = strings.TrimPrefix(v, "v")
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v = v[:i]
	}
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return out, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			return out, false
		}
		out[i] = n
	}
	return out, true
}

func versionLess(a, b string) bool {
	pa, _ := parseVersion(a)
	pb, _ := parseVersion(b)
	for i := range pa {
		if pa[i] != pb[i] {
			return pa[i] < pb[i]
		}
	}
	return false
}

func versionBefore(v string, major, minor, patch int) bool {
	return versionLess(v, fmt.Sprintf("%d.%d.%d", major, minor, patch))
}
