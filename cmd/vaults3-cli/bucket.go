package main

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

func runBucket(args []string) {
	if len(args) == 0 {
		fmt.Println(`Usage: vaults3-cli bucket <subcommand>

Subcommands:
  list                List all buckets
  create <name>       Create a bucket
  delete <name>       Delete a bucket
  info <name>         Show bucket details
  durability <name> [--erasure=on|off|default] [--replicas=N|default]
                      Show or set how much protection a bucket's data gets:
                      whether new objects are erasure coded, and how many
                      cluster nodes hold a copy. Omit the flags to show the
                      current settings. Existing objects are not rewritten.`)
		os.Exit(1)
	}

	requireCreds()

	switch args[0] {
	case "list", "ls":
		bucketList()
	case "create":
		if len(args) < 2 {
			fatal("bucket create requires a bucket name")
		}
		bucketCreate(args[1])
	case "delete", "rm":
		if len(args) < 2 {
			fatal("bucket delete requires a bucket name")
		}
		bucketDelete(args[1])
	case "info":
		if len(args) < 2 {
			fatal("bucket info requires a bucket name")
		}
		bucketInfo(args[1])
	case "durability":
		if len(args) < 2 {
			fatal("bucket durability requires a bucket name")
		}
		bucketDurability(args[1], args[2:])
	default:
		fatal("unknown bucket subcommand: " + args[0])
	}
}

func bucketList() {
	resp, err := s3Request("GET", "/", nil)
	if err != nil {
		fatal(err.Error())
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		fatal(fmt.Sprintf("HTTP %d: %s", resp.StatusCode, string(body)))
	}

	var result struct {
		XMLName xml.Name `xml:"ListAllMyBucketsResult"`
		Buckets struct {
			Bucket []struct {
				Name         string `xml:"Name"`
				CreationDate string `xml:"CreationDate"`
			} `xml:"Bucket"`
		} `xml:"Buckets"`
	}
	if err := xml.NewDecoder(resp.Body).Decode(&result); err != nil {
		fatal("parse response: " + err.Error())
	}

	if len(result.Buckets.Bucket) == 0 {
		fmt.Println("No buckets found.")
		return
	}

	headers := []string{"NAME", "CREATED"}
	var rows [][]string
	for _, b := range result.Buckets.Bucket {
		t, _ := time.Parse(time.RFC3339Nano, b.CreationDate)
		rows = append(rows, []string{b.Name, t.Format("2006-01-02 15:04:05")})
	}
	printTable(headers, rows)
}

func bucketCreate(name string) {
	resp, err := s3Request("PUT", bucketPath(name), nil)
	if err != nil {
		fatal(err.Error())
	}
	defer resp.Body.Close()

	if resp.StatusCode == 200 || resp.StatusCode == 204 {
		fmt.Printf("Bucket '%s' created.\n", name)
	} else if resp.StatusCode == 409 {
		fmt.Printf("Bucket '%s' already exists.\n", name)
	} else {
		body, _ := io.ReadAll(resp.Body)
		fatal(fmt.Sprintf("HTTP %d: %s", resp.StatusCode, string(body)))
	}
}

func bucketDelete(name string) {
	resp, err := s3Request("DELETE", bucketPath(name), nil)
	if err != nil {
		fatal(err.Error())
	}
	defer resp.Body.Close()

	if resp.StatusCode == 204 || resp.StatusCode == 200 {
		fmt.Printf("Bucket '%s' deleted.\n", name)
	} else {
		body, _ := io.ReadAll(resp.Body)
		fatal(fmt.Sprintf("HTTP %d: %s", resp.StatusCode, string(body)))
	}
}

func bucketInfo(name string) {
	// Escaped so a stray '?' or '#' cannot cut the name short and answer for a
	// different bucket.
	resp, err := apiRequest("GET", "/buckets/"+url.PathEscape(name), nil)
	if err != nil {
		fatal(err.Error())
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		fatal(fmt.Sprintf("HTTP %d: %s", resp.StatusCode, string(body)))
	}

	var info map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		fatal("parse response: " + err.Error())
	}

	data, _ := json.MarshalIndent(info, "", "  ")
	fmt.Println(string(data))
}

// bucketDurability shows or sets a bucket's erasure-coding and replica-count
// overrides (issue #39). With no flags it reports what is in force and whether
// each value is the bucket's own choice or the server default.
func bucketDurability(name string, flags []string) {
	if len(flags) == 0 {
		resp, err := s3Request("GET", bucketPath(name)+"?durability", nil)
		if err != nil {
			fatal(err.Error())
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != 200 {
			fatal(fmt.Sprintf("HTTP %d: %s", resp.StatusCode, string(body)))
		}
		var d struct {
			ErasureEnabled  bool `json:"erasure_enabled"`
			ReplicaCount    int  `json:"replica_count"`
			ErasureExplicit bool `json:"erasure_explicit"`
			ReplicaExplicit bool `json:"replica_explicit"`
		}
		if err := json.Unmarshal(body, &d); err != nil {
			fatal("parse response: " + err.Error())
		}
		source := func(explicit bool) string {
			if explicit {
				return "set on this bucket"
			}
			return "server default"
		}
		fmt.Printf("Bucket:          %s\n", name)
		fmt.Printf("Erasure coding:  %-5v (%s)\n", d.ErasureEnabled, source(d.ErasureExplicit))
		fmt.Printf("Replica count:   %-5d (%s)\n", d.ReplicaCount, source(d.ReplicaExplicit))
		return
	}

	payload := map[string]interface{}{}
	for _, f := range flags {
		switch {
		case strings.HasPrefix(f, "--erasure="):
			switch v := strings.TrimPrefix(f, "--erasure="); v {
			case "on", "true", "yes":
				payload["erasure_enabled"] = true
			case "off", "false", "no":
				payload["erasure_enabled"] = false
			case "default", "inherit":
				payload["erasure_enabled"] = nil
			default:
				fatal("--erasure must be on, off, or default")
			}
		case strings.HasPrefix(f, "--replicas="):
			v := strings.TrimPrefix(f, "--replicas=")
			if v == "default" || v == "inherit" {
				payload["replica_count"] = nil
				continue
			}
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 {
				fatal("--replicas must be a positive number, or default")
			}
			payload["replica_count"] = n
		default:
			fatal("unknown flag: " + f)
		}
	}

	body, _ := json.Marshal(payload)
	resp, err := s3Request("PUT", bucketPath(name)+"?durability", bytes.NewReader(body))
	if err != nil {
		fatal(err.Error())
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		raw, _ := io.ReadAll(resp.Body)
		fatal(fmt.Sprintf("HTTP %d: %s", resp.StatusCode, string(raw)))
	}
	fmt.Printf("Updated durability for %s. Objects already stored keep their current layout.\n", name)
	bucketDurability(name, nil)
}
