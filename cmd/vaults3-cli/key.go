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
  create ... --force                           Issue it even when a node's version cannot be confirmed
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
	// The API creates a missing user, and a name holding '/' could then never
	// be addressed again, so it could not be deleted from the CLI.
	refuseUnaddressable(user)

	buckets := []string{}
	all, userOnly, force := false, false, false
	rest := args[1:]
	for i := 0; i < len(rest); i++ {
		arg := rest[i]
		switch {
		case arg == "--force":
			force = true
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
	vc := checkServerVersions()
	if vc.oldest != "" && versionBefore(vc.oldest, 4, 4, 79) {
		fatal(fmt.Sprintf("the server runs %s, and key create needs 4.4.79 or later. On older servers a key "+
			"can get more access than asked for, and a new key changes what the user's other keys can reach. "+
			"Upgrade the server first", vc.oldest))
	}
	// A node whose version cannot be read is exactly the risky case: the
	// :latest image reports "main", a source build reports "dev", and a node
	// that does not answer reports nothing, yet each of them may be older than
	// 4.4.79. Treating that as a pass let the check wave through the servers it
	// exists for, so it needs an explicit --force.
	if len(vc.unknown) > 0 && !force {
		fatal(fmt.Sprintf("cannot confirm every node runs 4.4.79 or later: %s. On an older server a key "+
			"can get more access than asked for, and a new key changes what the user's other keys can reach. "+
			"Check the version on each node, then re-run with --force", strings.Join(vc.unknown, ", ")))
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
		// Only a server whose version could not be read, accepted with
		// --force, gets this far without saying. If it predates 4.4.79 it
		// ignored --user-policies.
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

// versionCheck is what the cluster said about its versions: the lowest
// version a node reported, and a description of every node, or of the whole
// request, whose version could not be learned.
type versionCheck struct {
	oldest  string
	unknown []string
}

// checkServerVersions asks the cluster which version each node runs. A node
// that is unreachable or reports a version that cannot be compared, such as
// "dev" or "main", is listed as unknown, and so is the whole cluster when the
// question cannot be asked at all.
func checkServerVersions() versionCheck {
	var vc versionCheck
	resp, err := apiRequest("GET", "/cluster/info", nil)
	if err != nil {
		vc.unknown = append(vc.unknown, "the version request failed ("+err.Error()+")")
		return vc
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		vc.unknown = append(vc.unknown, fmt.Sprintf("the version request returned HTTP %d", resp.StatusCode))
		return vc
	}
	var ci struct {
		Nodes []struct {
			NodeID    string `json:"nodeId"`
			Address   string `json:"address"`
			Reachable bool   `json:"reachable"`
			Version   string `json:"version"`
		} `json:"nodes"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&ci); err != nil {
		vc.unknown = append(vc.unknown, "the version answer could not be read ("+err.Error()+")")
		return vc
	}
	if len(ci.Nodes) == 0 {
		vc.unknown = append(vc.unknown, "the server listed no nodes")
	}
	for _, n := range ci.Nodes {
		name := n.NodeID
		if name == "" {
			name = n.Address
		}
		if name == "" {
			name = "this node"
		}
		if !n.Reachable {
			vc.unknown = append(vc.unknown, fmt.Sprintf("node %s is unreachable", name))
			continue
		}
		if _, ok := parseVersion(n.Version); !ok {
			v := n.Version
			if v == "" {
				v = "nothing"
			}
			vc.unknown = append(vc.unknown, fmt.Sprintf("node %s reports version %q", name, v))
			continue
		}
		if vc.oldest == "" || versionLess(n.Version, vc.oldest) {
			vc.oldest = n.Version
		}
	}
	return vc
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
