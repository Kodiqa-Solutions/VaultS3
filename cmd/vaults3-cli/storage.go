package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"strconv"
	"strings"
)

func runStorage(args []string) {
	if len(args) == 0 {
		fmt.Println(`Usage: vaults3-cli storage <subcommand>

Subcommands:
  reclaim [--apply] [--min-age=<hours>] [--bucket=<b>] [--local] [--yes]
      Find object data on disk that no metadata refers to any more, and with
      --apply delete it. Reports only by default.

  reencrypt [--apply] [--bucket=<b>]
      Find objects still stored in the pre-4.4.53 whole-object encryption
      format, which cannot be streamed on read, and with --apply rewrite them
      in the current chunked format. Reports only by default.`)
		os.Exit(1)
	}

	requireCreds()

	switch args[0] {
	case "reclaim":
		storageReclaim(args[1:])
	case "reencrypt":
		storageReencrypt(args[1:])
	default:
		fatal("unknown storage subcommand: " + args[0])
	}
}

type reclaimSample struct {
	Path    string `json:"path"`
	Bucket  string `json:"bucket"`
	Key     string `json:"key"`
	Version string `json:"version"`
	Upload  string `json:"upload"`
	Bytes   uint64 `json:"bytes"`
}

type reclaimReport struct {
	Scanned       uint64            `json:"scanned"`
	Orphans       uint64            `json:"orphans"`
	OrphanBytes   uint64            `json:"orphanBytes"`
	SkippedTooNew uint64            `json:"skippedTooNew"`
	Deleted       uint64            `json:"deleted"`
	DeletedBytes  uint64            `json:"deletedBytes"`
	ByBucket      map[string]uint64 `json:"byBucket"`
	Samples       []reclaimSample   `json:"samples"`
	Errors        []string          `json:"errors"`
	DryRun        bool              `json:"dryRun"`
	MinAge        string            `json:"minAge"`
	TookMs        int64             `json:"tookMs"`
}

type reclaimNode struct {
	NodeID    string         `json:"nodeId"`
	Address   string         `json:"address"`
	Reachable bool           `json:"reachable"`
	Error     string         `json:"error"`
	Report    *reclaimReport `json:"report"`
}

type reclaimResponse struct {
	DryRun         bool          `json:"dryRun"`
	MinAge         string        `json:"minAge"`
	NodeCount      int           `json:"nodeCount"`
	ReachableNodes int           `json:"reachableNodes"`
	Nodes          []reclaimNode `json:"nodes"`
	Totals         struct {
		Scanned       uint64 `json:"scanned"`
		Orphans       uint64 `json:"orphans"`
		OrphanBytes   uint64 `json:"orphanBytes"`
		Deleted       uint64 `json:"deleted"`
		DeletedBytes  uint64 `json:"deletedBytes"`
		SkippedTooNew uint64 `json:"skippedTooNew"`
	} `json:"totals"`
}

// storageReclaim frees object data that no metadata refers to any more.
//
// Before issue #47 several delete paths removed the Raft-replicated metadata
// cluster-wide but deleted the data file only on the node serving the request, so
// on an N-node cluster (N-1)/N of every bulk-deleted byte was stranded: not
// listed, not readable, not deletable through S3. Those paths are fixed, but a
// cluster that ran the older builds still holds the orphans and needs this to get
// the space back.
func storageReclaim(args []string) {
	apply, local, assumeYes := false, false, false
	minAgeHours := ""
	var buckets []string

	for _, arg := range args {
		switch {
		case arg == "--apply":
			apply = true
		case arg == "--local":
			local = true
		case arg == "--yes" || arg == "-y":
			assumeYes = true
		case strings.HasPrefix(arg, "--min-age="):
			minAgeHours = strings.TrimPrefix(arg, "--min-age=")
		case strings.HasPrefix(arg, "--bucket="):
			buckets = append(buckets, strings.TrimPrefix(arg, "--bucket="))
		default:
			fatal("unknown flag: " + arg)
		}
	}

	if minAgeHours != "" {
		if v, err := strconv.ParseFloat(minAgeHours, 64); err != nil || v <= 0 {
			fatal("--min-age must be a positive number of hours")
		}
	}

	q := url.Values{}
	if apply {
		q.Set("apply", "true")
	}
	if local {
		q.Set("scope", "local")
	}
	if minAgeHours != "" {
		q.Set("min_age_hours", minAgeHours)
	}
	for _, b := range buckets {
		q.Add("bucket", b)
	}

	// Deleting is irreversible, so an operator confirms it unless they opted out.
	// A dry run needs no confirmation because it changes nothing.
	if apply && !assumeYes {
		fmt.Print("This permanently deletes data files that no metadata refers to. Continue? [y/N]: ")
		var answer string
		fmt.Scanln(&answer)
		if !strings.EqualFold(strings.TrimSpace(answer), "y") {
			fmt.Println("Aborted.")
			return
		}
	}

	path := "/reclaim"
	if enc := q.Encode(); enc != "" {
		path += "?" + enc
	}
	resp, err := apiRequest("POST", path, nil)
	if err != nil {
		fatal("reclaim request failed: " + err.Error())
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		fatal(fmt.Sprintf("reclaim failed (HTTP %d): %s", resp.StatusCode, strings.TrimSpace(string(body))))
	}

	var out reclaimResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		fatal("could not decode response: " + err.Error())
	}

	printReclaim(out, apply)
}

func printReclaim(out reclaimResponse, apply bool) {
	mode := "DRY RUN (nothing was deleted)"
	if apply {
		mode = "APPLIED"
	}
	fmt.Printf("Orphaned object data: %s\n", mode)
	fmt.Printf("  Protection: files newer than %s are never touched\n", out.MinAge)
	if out.NodeCount > 1 {
		fmt.Printf("  Nodes scanned: %d of %d reachable\n", out.ReachableNodes, out.NodeCount)
	}
	fmt.Println()

	fmt.Printf("  Files scanned:    %d\n", out.Totals.Scanned)
	fmt.Printf("  Orphans found:    %d (%s)\n", out.Totals.Orphans, humanBytes(out.Totals.OrphanBytes))
	if out.Totals.SkippedTooNew > 0 {
		fmt.Printf("  Too new to touch: %d\n", out.Totals.SkippedTooNew)
	}
	if apply {
		fmt.Printf("  Reclaimed:        %d (%s)\n", out.Totals.Deleted, humanBytes(out.Totals.DeletedBytes))
	}

	// A partial scan must never read as a complete one: if a node was unreachable,
	// the bytes it holds are simply not in these numbers.
	unreachable := out.NodeCount - out.ReachableNodes
	if unreachable > 0 {
		fmt.Printf("\n  WARNING: %d node(s) did not report, so their orphans are NOT counted above.\n", unreachable)
		for _, n := range out.Nodes {
			if !n.Reachable {
				fmt.Printf("    %s (%s): %s\n", n.NodeID, n.Address, n.Error)
			}
		}
	}

	if out.NodeCount > 1 {
		fmt.Println("\nPer node:")
		rows := [][]string{}
		for _, n := range out.Nodes {
			if n.Report == nil {
				rows = append(rows, []string{n.NodeID, "unreachable", "-", "-"})
				continue
			}
			reclaimed := "-"
			if apply {
				reclaimed = humanBytes(n.Report.DeletedBytes)
			}
			rows = append(rows, []string{
				n.NodeID,
				strconv.FormatUint(n.Report.Orphans, 10),
				humanBytes(n.Report.OrphanBytes),
				reclaimed,
			})
		}
		printTable([]string{"NODE", "ORPHANS", "BYTES", "RECLAIMED"}, rows)
	}

	// Largest offenders, so an operator can sanity-check before running --apply.
	var samples []reclaimSample
	for _, n := range out.Nodes {
		if n.Report != nil {
			samples = append(samples, n.Report.Samples...)
		}
	}
	if len(samples) > 0 {
		fmt.Println("\nLargest orphans:")
		rows := [][]string{}
		for i, s := range samples {
			if i >= 10 {
				break
			}
			what := s.Bucket + "/" + s.Key
			switch {
			case s.Upload != "":
				what = "multipart upload " + s.Upload
			case s.Version != "":
				what += " (version " + s.Version + ")"
			}
			rows = append(rows, []string{what, humanBytes(s.Bytes)})
		}
		printTable([]string{"OBJECT", "SIZE"}, rows)
	}

	var errs []string
	for _, n := range out.Nodes {
		if n.Report != nil {
			errs = append(errs, n.Report.Errors...)
		}
	}
	if len(errs) > 0 {
		fmt.Printf("\n%d path(s) could not be read:\n", len(errs))
		for i, e := range errs {
			if i >= 5 {
				fmt.Printf("  ... and %d more\n", len(errs)-i)
				break
			}
			fmt.Printf("  %s\n", e)
		}
	}

	if !apply && out.Totals.Orphans > 0 {
		fmt.Println("\nRe-run with --apply to delete these and free the space.")
	}
	if out.Totals.Orphans == 0 {
		fmt.Println("\nNothing to reclaim.")
	}
}

type reencryptSample struct {
	Bucket string `json:"bucket"`
	Key    string `json:"key"`
	Bytes  int64  `json:"bytes"`
}

type reencryptReport struct {
	Scanned   int               `json:"scanned"`
	Legacy    int               `json:"legacy"`
	Rewritten int               `json:"rewritten"`
	Bytes     int64             `json:"bytes"`
	ByBucket  map[string]int    `json:"byBucket"`
	Samples   []reencryptSample `json:"samples"`
	Errors    []string          `json:"errors"`
	DryRun    bool              `json:"dryRun"`
	TookMs    int64             `json:"tookMs"`
}

// storageReencrypt migrates objects still sealed in the pre-4.4.53 whole-object
// format. Those cannot be streamed on read, so each one costs its own size in
// latency and memory every time it is fetched, and key rotation does not fix it
// because rotation never rewrites object bodies.
func storageReencrypt(args []string) {
	apply := false
	bucket := ""
	for _, a := range args {
		switch {
		case a == "--apply":
			apply = true
		case strings.HasPrefix(a, "--bucket="):
			bucket = strings.TrimPrefix(a, "--bucket=")
		default:
			fatal("unknown flag: " + a)
		}
	}

	q := url.Values{}
	if apply {
		q.Set("apply", "true")
	}
	if bucket != "" {
		q.Set("bucket", bucket)
	}

	resp, err := apiRequest("POST", "/reencrypt?"+q.Encode(), nil)
	if err != nil {
		fatal("reencrypt request failed: " + err.Error())
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		fatal(fmt.Sprintf("reencrypt failed (HTTP %d): %s", resp.StatusCode, strings.TrimSpace(string(body))))
	}
	var rep reencryptReport
	if err := json.NewDecoder(resp.Body).Decode(&rep); err != nil {
		fatal("decode response: " + err.Error())
	}

	if rep.DryRun {
		fmt.Println("Dry run. Nothing was rewritten. Add --apply to migrate.")
	}
	fmt.Printf("Scanned %d objects, %d still in the old format (%s)\n",
		rep.Scanned, rep.Legacy, humanBytes(uint64(rep.Bytes)))
	if rep.Rewritten > 0 {
		fmt.Printf("Rewritten: %d\n", rep.Rewritten)
	}
	for b, n := range rep.ByBucket {
		fmt.Printf("  %-30s %d\n", b, n)
	}
	for _, s := range rep.Samples {
		fmt.Printf("  e.g. %s/%s (%s)\n", s.Bucket, s.Key, humanBytes(uint64(s.Bytes)))
	}
	for _, e := range rep.Errors {
		fmt.Fprintln(os.Stderr, "  error: "+e)
	}
	fmt.Printf("Took %d ms\n", rep.TookMs)
}
