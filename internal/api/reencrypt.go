package api

import (
	"fmt"
	"net/http"
	"time"

	"github.com/Kodiqa-Solutions/VaultS3/internal/storage"
)

// Objects encrypted before 4.4.53 were sealed as one AES-GCM message, so a read
// of one costs its own size in latency and memory and cannot be streamed: the
// authentication tag covers the whole message and sits at the end, so releasing
// plaintext before verifying it would mean serving unauthenticated bytes.
// Rotating keys does not help, because rotation mints a new key version without
// rewriting any bodies.
//
// This walks the object index and rewrites those objects in the current chunked
// format. It reports by default and only rewrites with ?apply=true, because it
// modifies stored data.

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

func (h *APIHandler) handleReencrypt(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "use POST")
		return
	}
	// Only an engine that can still hold the old format has anything to do here.
	lf, ok := h.engine.(storage.LegacyFormatEngine)
	if !ok {
		writeJSON(w, http.StatusOK, reencryptReport{
			DryRun:   true,
			ByBucket: map[string]int{},
			Errors:   []string{"encryption is not enabled on this server, so no object can be in the legacy format"},
		})
		return
	}

	apply := r.URL.Query().Get("apply") == "true"
	onlyBucket := r.URL.Query().Get("bucket")
	start := time.Now()
	rep := reencryptReport{DryRun: !apply, ByBucket: map[string]int{}}

	buckets, err := h.store.ListBuckets()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "list buckets: "+err.Error())
		return
	}
	for _, b := range buckets {
		if onlyBucket != "" && b.Name != onlyBucket {
			continue
		}
		// Page through the bucket rather than materialising its whole index: a
		// migration is exactly the case where a bucket may hold millions of keys.
		startAfter := ""
		for {
			objects, truncated, err := h.store.ListLatestObjects(b.Name, "", startAfter, 1000)
			if err != nil {
				rep.Errors = append(rep.Errors, fmt.Sprintf("%s: list: %v", b.Name, err))
				break
			}
			if len(objects) == 0 {
				break
			}
			for _, obj := range objects {
				rep.Scanned++
				legacy, err := lf.IsLegacyObject(b.Name, obj.Key)
				if err != nil {
					rep.Errors = append(rep.Errors, fmt.Sprintf("%s/%s: %v", b.Name, obj.Key, err))
					continue
				}
				if !legacy {
					continue
				}
				rep.Legacy++
				rep.ByBucket[b.Name]++
				rep.Bytes += obj.Size
				if len(rep.Samples) < 20 {
					rep.Samples = append(rep.Samples, reencryptSample{Bucket: b.Name, Key: obj.Key, Bytes: obj.Size})
				}
				if !apply {
					continue
				}
				// One at a time on purpose. Reading a legacy object materialises it,
				// which is the very cost being removed, so fanning out would multiply
				// exactly the memory this migration exists to stop paying.
				if err := lf.RewriteObject(b.Name, obj.Key); err != nil {
					rep.Errors = append(rep.Errors, fmt.Sprintf("%s/%s: rewrite: %v", b.Name, obj.Key, err))
					continue
				}
				rep.Rewritten++
			}
			startAfter = objects[len(objects)-1].Key
			if !truncated {
				break
			}
		}
	}
	rep.TookMs = time.Since(start).Milliseconds()
	writeJSON(w, http.StatusOK, rep)
}
