package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
)

func runReplication(args []string) {
	if len(args) == 0 {
		fmt.Println(`Usage: vaults3-cli replication <subcommand>

Subcommands:
  status               Show replication peer status
  queue                Show replication queue`)
		os.Exit(1)
	}

	requireCreds()

	switch args[0] {
	case "status":
		replicationStatus()
	case "queue":
		replicationQueue()
	default:
		fatal("unknown replication subcommand: " + args[0])
	}
}

// replicationStatus prints each configured replication peer. The API answers
// {"enabled":..., "peers":[...]} (replicationStatusResponse in
// internal/api/replication.go); it was reshaped from a bare list for the
// dashboard in issue #10 and this command was never updated, so it failed to
// decode on every run.
func replicationStatus() {
	resp, err := apiRequest("GET", "/replication/status", nil)
	if err != nil {
		fatal(err.Error())
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		fatal(fmt.Sprintf("HTTP %d: %s", resp.StatusCode, string(body)))
	}

	var status struct {
		Enabled bool `json:"enabled"`
		Peers   []struct {
			Name        string `json:"name"`
			URL         string `json:"url"`
			QueueDepth  int    `json:"queueDepth"`
			LastSync    string `json:"lastSync"`
			TotalSynced int64  `json:"totalSynced"`
			TotalFailed int64  `json:"totalFailed"`
			LastError   string `json:"lastError"`
		} `json:"peers"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		fatal("parse response: " + err.Error())
	}

	if !status.Enabled {
		fmt.Println("Replication is not enabled.")
		return
	}
	if len(status.Peers) == 0 {
		fmt.Println("No replication peers configured.")
		return
	}

	headers := []string{"PEER", "URL", "QUEUE", "SYNCED", "FAILED", "LAST SYNC", "LAST ERROR"}
	var rows [][]string
	for _, p := range status.Peers {
		lastSync := "never"
		if p.LastSync != "" {
			lastSync = p.LastSync
		}
		lastErr := "-"
		if p.LastError != "" {
			lastErr = p.LastError
			if len(lastErr) > 40 {
				lastErr = lastErr[:40] + "..."
			}
		}
		rows = append(rows, []string{
			p.Name,
			p.URL,
			strconv.Itoa(p.QueueDepth),
			strconv.FormatInt(p.TotalSynced, 10),
			strconv.FormatInt(p.TotalFailed, 10),
			lastSync,
			lastErr,
		})
	}
	printTable(headers, rows)
}

func replicationQueue() {
	resp, err := apiRequest("GET", "/replication/queue?limit=20", nil)
	if err != nil {
		fatal(err.Error())
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		fatal(fmt.Sprintf("HTTP %d: %s", resp.StatusCode, string(body)))
	}

	// The keys the API writes (replicationEventResponse), not the ones the store
	// keeps internally (metadata.ReplicationEvent). Decoding the store's
	// retry_count and created_at found neither, so every queued event listed with
	// 0 retries and a 1970 date.
	var events []struct {
		ID         uint64 `json:"id"`
		Type       string `json:"type"`
		Bucket     string `json:"bucket"`
		Key        string `json:"key"`
		Peer       string `json:"peer"`
		RetryCount int    `json:"retryCount"`
		CreatedAt  string `json:"createdAt"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&events); err != nil {
		fatal("parse response: " + err.Error())
	}

	if len(events) == 0 {
		fmt.Println("Replication queue is empty.")
		return
	}

	headers := []string{"ID", "TYPE", "BUCKET", "KEY", "PEER", "RETRIES", "CREATED"}
	var rows [][]string
	for _, e := range events {
		created := e.CreatedAt
		if created == "" {
			created = "-"
		}
		rows = append(rows, []string{
			strconv.FormatUint(e.ID, 10),
			e.Type,
			e.Bucket,
			e.Key,
			e.Peer,
			strconv.Itoa(e.RetryCount),
			created,
		})
	}
	printTable(headers, rows)
}
