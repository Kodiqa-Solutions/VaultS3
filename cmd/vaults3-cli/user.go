package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
)

func runUser(args []string) {
	if len(args) == 0 {
		fmt.Println(`Usage: vaults3-cli user <subcommand>

Subcommands:
  list                                   List IAM users
  create <name>                          Create IAM user
  delete <name>                          Delete IAM user
  attach-policy <user> <policy>          Attach policy to user`)
		os.Exit(1)
	}

	requireCreds()

	switch args[0] {
	case "list", "ls":
		userList()
	case "create":
		userCreate(args[1:])
	case "delete", "rm":
		if len(args) < 2 {
			fatal("user delete requires a username")
		}
		userDelete(args[1])
	case "attach-policy":
		if len(args) < 3 {
			fatal("user attach-policy requires: <user> <policy>")
		}
		userAttachPolicy(args[1], args[2])
	default:
		fatal("unknown user subcommand: " + args[0])
	}
}

// userPath is the API path for one user. The name has to be escaped: the API
// routes on the decoded path, so an unescaped '?' ended the path and a '#' began
// a fragment, and the request named a DIFFERENT user. "user delete 'a?b'"
// deleted the user "a" and then reported "a?b" as deleted.
func userPath(name string) string {
	return "/iam/users/" + url.PathEscape(name)
}

// refuseUnaddressable stops a name the API can never route. It splits the decoded
// path on '/', so escaping cannot help: a user whose name contains one could be
// created but never deleted or given a policy again, from the CLI or the
// dashboard.
func refuseUnaddressable(name string) {
	if strings.Contains(name, "/") {
		fatal(fmt.Sprintf("user name %q contains '/', which the API cannot address", name))
	}
}

// userList prints the IAM users. The keys decoded here are the ones the API
// actually writes (iamUserResponse in internal/api/iam.go). This used to read
// user_id, user_name and policies, none of which the API sends, so every user
// listed with a blank name and no policies (issue #62).
func userList() {
	resp, err := apiRequest("GET", "/iam/users", nil)
	if err != nil {
		fatal(err.Error())
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		fatal(fmt.Sprintf("HTTP %d: %s", resp.StatusCode, string(body)))
	}

	var users []struct {
		Name       string   `json:"name"`
		CreatedAt  string   `json:"createdAt"`
		PolicyARNs []string `json:"policyArns"`
		Groups     []string `json:"groups"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&users); err != nil {
		fatal("parse response: " + err.Error())
	}

	if len(users) == 0 {
		fmt.Println("No IAM users found.")
		return
	}

	headers := []string{"NAME", "POLICIES", "GROUPS", "CREATED"}
	var rows [][]string
	for _, u := range users {
		rows = append(rows, []string{
			u.Name,
			orDash(strings.Join(u.PolicyARNs, ", ")),
			orDash(strings.Join(u.Groups, ", ")),
			u.CreatedAt,
		})
	}
	printTable(headers, rows)
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// userCreate creates an IAM user.
//
// It sent the name as user_name while the API reads name, so it failed with
// "name is required" every time (issue #62). It also accepted --access-key and
// --secret-key, which the API never read: the server generates every access key
// itself and has no way to take one the caller chose. Those flags are refused
// now instead of being silently dropped, so nobody leaves believing a user has
// credentials it does not.
func userCreate(args []string) {
	if len(args) < 1 || strings.HasPrefix(args[0], "-") {
		fatal("user create requires a username")
	}
	name := args[0]
	refuseUnaddressable(name)

	for _, arg := range args[1:] {
		if strings.HasPrefix(arg, "--access-key") || strings.HasPrefix(arg, "--secret-key") {
			fatal("user create cannot set an access key or secret key: the server generates them. " +
				"Issue a key for this user from the dashboard, under Access Keys")
		}
		fatal("unknown argument for user create: " + arg)
	}

	data, _ := json.Marshal(map[string]string{"name": name})
	resp, err := apiRequest("POST", "/iam/users", bytes.NewReader(data))
	if err != nil {
		fatal(err.Error())
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 && resp.StatusCode != 201 {
		body, _ := io.ReadAll(resp.Body)
		fatal(fmt.Sprintf("HTTP %d: %s", resp.StatusCode, string(body)))
	}
	fmt.Printf("User '%s' created.\n", name)
}

// userDelete deletes an IAM user. The API answers 204 whether or not the user
// existed, the same as it does for groups and policies, so on its own this
// printed "deleted" for a name that was never there. Look the user up first so
// a typo, or a create that silently failed, is reported instead of hidden.
func userDelete(name string) {
	refuseUnaddressable(name)
	check, err := apiRequest("GET", userPath(name), nil)
	if err != nil {
		fatal(err.Error())
	}
	check.Body.Close()
	if check.StatusCode == 404 {
		fatal(fmt.Sprintf("user '%s' not found", name))
	}

	resp, err := apiRequest("DELETE", userPath(name), nil)
	if err != nil {
		fatal(err.Error())
	}
	defer resp.Body.Close()

	if resp.StatusCode == 200 || resp.StatusCode == 204 {
		fmt.Printf("User '%s' deleted.\n", name)
	} else {
		body, _ := io.ReadAll(resp.Body)
		fatal(fmt.Sprintf("HTTP %d: %s", resp.StatusCode, string(body)))
	}
}

// userAttachPolicy attaches a policy to a user. It sent the policy as
// policy_name while the API reads policyName, so it failed every time.
func userAttachPolicy(userName, policyName string) {
	refuseUnaddressable(userName)
	data, _ := json.Marshal(map[string]string{"policyName": policyName})
	resp, err := apiRequest("POST", userPath(userName)+"/policies", bytes.NewReader(data))
	if err != nil {
		fatal(err.Error())
	}
	defer resp.Body.Close()

	if resp.StatusCode == 200 || resp.StatusCode == 204 {
		fmt.Printf("Policy '%s' attached to user '%s'.\n", policyName, userName)
	} else {
		body, _ := io.ReadAll(resp.Body)
		fatal(fmt.Sprintf("HTTP %d: %s", resp.StatusCode, string(body)))
	}
}
