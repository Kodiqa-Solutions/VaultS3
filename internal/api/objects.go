package api

import (
	"archive/zip"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
	"github.com/Kodiqa-Solutions/VaultS3/internal/search"
)

type objectListItem struct {
	Key          string `json:"key"`
	Size         int64  `json:"size"`
	LastModified string `json:"lastModified"`
	ContentType  string `json:"contentType"`
	IsPrefix     bool   `json:"isPrefix"` // true = "folder"
}

type objectListResponse struct {
	Objects   []objectListItem `json:"objects"`
	Truncated bool             `json:"truncated"`
	Prefix    string           `json:"prefix"`
	// NextStartAfter is the continuation cursor (the last flat object key in this
	// page). Pass it back as ?startAfter= to fetch the next page. It is the last
	// *flat* key rather than the last displayed item, so folder roll-ups don't
	// corrupt the cursor.
	NextStartAfter string `json:"nextStartAfter,omitempty"`
}

type uploadResult struct {
	Key         string `json:"key"`
	Size        int64  `json:"size"`
	ContentType string `json:"contentType"`
	Error       string `json:"error,omitempty"` // set when this file failed to store
}

func (h *APIHandler) handleListObjects(w http.ResponseWriter, r *http.Request, bucket string) {
	if !h.store.BucketExists(bucket) {
		writeError(w, http.StatusNotFound, "bucket not found")
		return
	}

	prefix := r.URL.Query().Get("prefix")
	startAfter := r.URL.Query().Get("startAfter")
	maxKeys := 200
	if mk := r.URL.Query().Get("maxKeys"); mk != "" {
		if v, err := strconv.Atoi(mk); err == nil && v > 0 && v <= 1000 {
			maxKeys = v
		}
	}

	// Server-side folder collapsing: the store returns folders (common prefixes)
	// directly and seeks past their contents, so a folder level shows up to maxKeys
	// FOLDERS per page regardless of how many objects each holds (issue #16
	// follow-up — folder-heavy buckets used to show only a handful per page).
	objects, prefixes, truncated, nextStartAfter, err := h.store.ListLatestObjectsDelimited(bucket, prefix, "/", startAfter, maxKeys)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list objects")
		return
	}

	writeJSON(w, http.StatusOK, objectListResponse{
		Objects:        toObjectListItems(objects, prefixes),
		Truncated:      truncated,
		Prefix:         prefix,
		NextStartAfter: nextStartAfter,
	})
}

func folderItem(folder metadata.CommonPrefixInfo) objectListItem {
	item := objectListItem{Key: folder.Prefix, IsPrefix: true}
	// Surface the folder's date (its directory marker or first child) so the
	// browser shows a real date instead of a blank (issue #35).
	if folder.LastModified > 0 {
		item.LastModified = time.Unix(folder.LastModified, 0).UTC().Format(time.RFC3339)
	}
	return item
}

func fileItem(obj metadata.ObjectMeta) objectListItem {
	return objectListItem{
		Key:          obj.Key,
		Size:         obj.Size,
		LastModified: time.Unix(obj.LastModified, 0).UTC().Format(time.RFC3339),
		ContentType:  obj.ContentType,
	}
}

// toObjectListItems renders a delimited listing the way the file browser shows
// it: folders first, then files.
func toObjectListItems(objects []metadata.ObjectMeta, prefixes []metadata.CommonPrefixInfo) []objectListItem {
	items := make([]objectListItem, 0, len(prefixes)+len(objects))
	for _, folder := range prefixes {
		items = append(items, folderItem(folder))
	}
	for _, obj := range objects {
		items = append(items, fileItem(obj))
	}
	return items
}

// searchScanPageSize is how many flat keys one store call walks while filtering
// a folder; searchMaxScanPages bounds a single request so a folder with a
// million children cannot pin a handler. The response's cursor lets the client
// continue where the scan stopped.
const (
	searchScanPageSize = 1000
	searchMaxScanPages = 20
)

// handleSearchObjects filters ONE folder level of a bucket — the direct
// children of ?prefix=, folders and files — against ?q=, using the same query
// grammar as the global search (case-insensitive substrings, AND of terms,
// tag:k=v, type:x and etag:x filters) and the same fields (name, content type,
// tags). Matching is on the child's own name, not the full key: in a/ the query
// "aa" finds a/aa-1 and a/aa-2 but not a/bb-1/aa.pdf.
//
// It walks the metadata store with the delimited listing cursor rather than
// the in-memory search index, so it is complete regardless of the index's
// entry cap and needs no admin privilege: the route is authorised as a listing
// of the bucket.
//
// The response is the same shape as the listing (objectListResponse).
// Truncated means the scan stopped before the folder's end — because the page
// filled OR because the per-request scan budget ran out — and NextStartAfter is
// where to resume; a page can therefore come back with few (even zero)
// matches and still be truncated.
func (h *APIHandler) handleSearchObjects(w http.ResponseWriter, r *http.Request, bucket string) {
	if !h.store.BucketExists(bucket) {
		writeError(w, http.StatusNotFound, "bucket not found")
		return
	}

	q := search.ParseQuery(r.URL.Query().Get("q"))
	if q.IsEmpty() {
		writeError(w, http.StatusBadRequest, "query parameter 'q' is required")
		return
	}
	prefix := r.URL.Query().Get("prefix")
	cursor := r.URL.Query().Get("startAfter")
	maxKeys := 200
	if mk := r.URL.Query().Get("maxKeys"); mk != "" {
		if v, err := strconv.Atoi(mk); err == nil && v > 0 && v <= 1000 {
			maxKeys = v
		}
	}

	items := []objectListItem{}
	more := false // whether the folder has keys beyond the cursor when the scan stops
scan:
	for pages := 0; pages < searchMaxScanPages; pages++ {
		objects, prefixes, pageMore, next, err := h.store.ListLatestObjectsDelimited(bucket, prefix, "/", cursor, searchScanPageSize)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to list objects")
			return
		}
		// Walk folders and files together in key order so that, when the page
		// fills mid-way, the cursor can point just past the last item returned
		// and nothing between it and the store's own cursor is skipped.
		candidates := mergeChildren(prefix, prefixes, objects)
		for i, c := range candidates {
			if !q.Match(c.text, c.contentType, c.etag, c.tags) {
				continue
			}
			items = append(items, c.item)
			if len(items) >= maxKeys {
				more = pageMore || i+1 < len(candidates)
				cursor = c.resumeAfter
				break scan
			}
		}
		more, cursor = pageMore, next
		if !more {
			break
		}
	}
	resp := objectListResponse{Objects: items, Truncated: more, Prefix: prefix}
	if more {
		resp.NextStartAfter = cursor
	}
	writeJSON(w, http.StatusOK, resp)
}

// searchCandidate is one direct child of the folder being filtered, with the
// haystack the query is matched against and the cursor that resumes after it.
type searchCandidate struct {
	item        objectListItem
	text        string // search.SearchText of the child's own name, type and tags
	contentType string
	etag        string
	tags        map[string]string
	resumeAfter string // startAfter that continues just past this child
}

// mergeChildren interleaves a page's folders and files back into key order.
// Both slices come sorted from the store; a folder sorts by its prefix. Folders
// have no content type, ETag or tags, so a type:/etag:/tag: filter never
// matches one — only files carry those. Plain terms see the child's name (not
// the full key), its content type and its tags, the same fields the global
// search index matches, so "prod" or "pdf" behaves the same in both places.
func mergeChildren(prefix string, prefixes []metadata.CommonPrefixInfo, objects []metadata.ObjectMeta) []searchCandidate {
	out := make([]searchCandidate, 0, len(prefixes)+len(objects))
	i, j := 0, 0
	for i < len(prefixes) || j < len(objects) {
		if j >= len(objects) || (i < len(prefixes) && prefixes[i].Prefix < objects[j].Key) {
			f := prefixes[i]
			i++
			name := strings.TrimSuffix(strings.TrimPrefix(f.Prefix, prefix), "/")
			out = append(out, searchCandidate{
				item:        folderItem(f),
				text:        search.SearchText("", name, "", nil),
				resumeAfter: f.Prefix + "\xff", // past every key under the folder
			})
			continue
		}
		o := objects[j]
		j++
		out = append(out, searchCandidate{
			item:        fileItem(o),
			text:        search.SearchText("", strings.TrimPrefix(o.Key, prefix), o.ContentType, o.Tags),
			contentType: o.ContentType,
			etag:        o.ETag,
			tags:        o.Tags,
			resumeAfter: o.Key,
		})
	}
	return out
}

func (h *APIHandler) handleDeleteObject(w http.ResponseWriter, r *http.Request, bucket, key string) {
	if !h.store.BucketExists(bucket) {
		writeError(w, http.StatusNotFound, "bucket not found")
		return
	}
	// In a cluster, delete the object data on the node that owns it.
	if h.clusterProxy != nil && h.clusterProxy(w, r, bucket, key) {
		return
	}

	meta, err := h.store.GetObjectMeta(bucket, key)
	if err != nil || meta == nil || meta.DeleteMarker {
		writeError(w, http.StatusNotFound, "object not found")
		return
	}

	if versioning, _ := h.store.GetBucketVersioning(bucket); versioning == "Enabled" || meta.VersionID != "" {
		// Versioned bucket: write a delete marker instead of erasing data. The
		// object disappears from listings but its versions are kept, so it stays
		// snapshot/restore-able (S3 versioned-delete semantics).
		old := *meta
		old.IsLatest = false
		h.store.PutObjectVersion(old)

		dm := metadata.ObjectMeta{
			Bucket: bucket, Key: key, VersionID: genVersionID(),
			DeleteMarker: true, IsLatest: true, LastModified: time.Now().UTC().Unix(),
		}
		h.store.PutObjectVersion(dm)
		h.store.PutObjectMeta(dm)
		if h.onReplication != nil {
			h.onReplication("s3:ObjectRemoved:Delete", bucket, key, 0, "", dm.VersionID)
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// Non-versioned: hard delete.
	if err := h.engine.DeleteObject(bucket, key); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to delete object")
		return
	}
	h.store.DeleteObjectMeta(bucket, key)
	if h.onReplication != nil {
		h.onReplication("s3:ObjectRemoved:Delete", bucket, key, 0, "", "")
	}
	w.WriteHeader(http.StatusNoContent)
}

// getLatestObject returns a reader for an object's current content, resolving the
// latest version when the bucket is versioned (data then lives under .vs/, not
// at the plain key path).
func (h *APIHandler) getLatestObject(bucket, key string) (io.ReadCloser, int64, *metadata.ObjectMeta, error) {
	meta, _ := h.store.GetObjectMeta(bucket, key)
	if meta != nil && meta.VersionID != "" {
		r, sz, err := h.engine.GetObjectVersion(bucket, key, meta.VersionID)
		return r, sz, meta, err
	}
	r, sz, err := h.engine.GetObject(bucket, key)
	return r, sz, meta, err
}

func (h *APIHandler) handleDownload(w http.ResponseWriter, r *http.Request, bucket, key string) {
	if !h.store.BucketExists(bucket) {
		writeError(w, http.StatusNotFound, "bucket not found")
		return
	}
	// In a cluster, the object's data lives on the node that owns it — fetch from there.
	if h.clusterProxy != nil && h.clusterProxy(w, r, bucket, key) {
		return
	}

	reader, size, meta, err := h.getLatestObject(bucket, key)
	if err != nil {
		writeError(w, http.StatusNotFound, "object not found")
		return
	}
	defer reader.Close()

	// Set content type from metadata
	ct := "application/octet-stream"
	if meta != nil && meta.ContentType != "" {
		ct = meta.ContentType
	}

	// Extract filename from key
	filename := filepath.Base(key)

	w.Header().Set("Content-Type", ct)
	w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	// Sanitize filename: remove quotes and control characters to prevent header injection
	safeName := strings.Map(func(r rune) rune {
		if r == '"' || r == '\\' || r < 32 {
			return '_'
		}
		return r
	}, filename)
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, safeName))
	io.Copy(w, reader)
}

func (h *APIHandler) handleUpload(w http.ResponseWriter, r *http.Request, bucket string) {
	if !h.store.BucketExists(bucket) {
		writeError(w, http.StatusNotFound, "bucket not found")
		return
	}

	// Stream each file part straight to storage. ParseMultipartForm buffered the
	// whole request body to a temp file first, which fails for very large uploads
	// when the temp dir fills (issue #26). A MultipartReader streams part by part,
	// so a 100GB file needs no temp space and is not copied twice.
	mr, err := r.MultipartReader()
	if err != nil {
		writeError(w, http.StatusBadRequest, "failed to read upload")
		return
	}

	prefix := r.URL.Query().Get("prefix")
	versioning, _ := h.store.GetBucketVersioning(bucket)
	var results []uploadResult
	var anyFailed bool

	// fail records a per-file failure: it logs the real reason (uploads used to
	// swallow write errors silently and still return 200, so a full disk or a
	// permission error surfaced only as a blank "upload failed" with no logs —
	// issue #26) and reports it back to the client.
	fail := func(key string, err error) {
		slog.Error("dashboard upload failed", "bucket", bucket, "key", key, "error", err)
		results = append(results, uploadResult{Key: key, Error: err.Error()})
		anyFailed = true
	}

	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			writeError(w, http.StatusBadRequest, "failed to read upload part")
			return
		}
		// part.FileName() applies filepath.Base, which strips the directory and
		// flattens folder uploads. Read the raw filename from Content-Disposition
		// so a relative path (webkitRelativePath) is preserved as the object key.
		// validateObjectKey below blocks any ".." traversal.
		filename := part.FileName()
		if _, params, perr := mime.ParseMediaType(part.Header.Get("Content-Disposition")); perr == nil {
			if raw := params["filename"]; raw != "" {
				filename = strings.TrimLeft(strings.ReplaceAll(raw, "\\", "/"), "/")
			}
		}
		if filename == "" {
			part.Close() // a plain form field, not a file
			continue
		}

		key := prefix + filename
		if err := validateObjectKey(key); err != nil {
			part.Close()
			continue
		}

		// Detect content type up front.
		ct := part.Header.Get("Content-Type")
		if ct == "" || ct == "application/octet-stream" {
			if detected := mime.TypeByExtension(filepath.Ext(key)); detected != "" {
				ct = detected
			} else {
				ct = "application/octet-stream"
			}
		}

		// In a cluster, place each file on the node that owns its key (by hash
		// ring) so the data lands where an S3 GET will look for it. The owner
		// stores it and records the metadata (which replicates via Raft).
		if h.clusterOwner != nil {
			if ownerAddr, remote := h.clusterOwner(bucket, key); remote {
				written, ferr := h.forwardUpload(ownerAddr, bucket, prefix, filename, ct, part)
				part.Close()
				if ferr != nil {
					fail(key, ferr)
					continue
				}
				results = append(results, uploadResult{Key: key, Size: written, ContentType: ct})
				continue
			}
		}

		now := time.Now().UTC().Unix()
		var written int64
		var etag string

		// size -1: the part is streamed, so its length is unknown up front; the
		// engine reports the actual bytes written.
		if versioning == "Enabled" {
			// Versioned bucket: write a new version so the object has history
			// (and is snapshot/restore-able), mirroring the S3 PutObject path.
			versionID := genVersionID()
			written, etag, err = h.engine.PutObjectVersion(bucket, key, versionID, part, -1)
			part.Close()
			if err != nil {
				fail(key, err)
				continue
			}
			if old, e := h.store.GetObjectMeta(bucket, key); e == nil && old.VersionID != "" {
				old.IsLatest = false
				h.store.PutObjectVersion(*old)
			}
			meta := metadata.ObjectMeta{
				Bucket: bucket, Key: key, ContentType: ct, ETag: etag, Size: written,
				LastModified: now, VersionID: versionID, IsLatest: true,
			}
			h.store.PutObjectVersion(meta)
			h.store.PutObjectMeta(meta)
			if h.onReplication != nil {
				h.onReplication("s3:ObjectCreated:Put", bucket, key, written, etag, versionID)
			}
		} else {
			written, etag, err = h.engine.PutObject(bucket, key, part, -1)
			part.Close()
			if err != nil {
				fail(key, err)
				continue
			}
			h.store.PutObjectMeta(metadata.ObjectMeta{
				Bucket: bucket, Key: key, ContentType: ct, ETag: etag, Size: written, LastModified: now,
			})
			if h.onReplication != nil {
				h.onReplication("s3:ObjectCreated:Put", bucket, key, written, etag, "")
			}
		}

		results = append(results, uploadResult{
			Key:         key,
			Size:        written,
			ContentType: ct,
		})
	}

	if results == nil {
		results = []uploadResult{}
	}
	// If any file failed to store, return 5xx so the dashboard shows a real failure
	// instead of a silent "success". Per-file reasons ride along in the results
	// (each failed entry carries an `error`), and were already logged above.
	status := http.StatusOK
	if anyFailed {
		status = http.StatusInternalServerError
	}
	writeJSON(w, status, results)
}

// handleBulkDelete deletes multiple objects at once.
func (h *APIHandler) handleBulkDelete(w http.ResponseWriter, r *http.Request, bucket string) {
	if !h.store.BucketExists(bucket) {
		writeError(w, http.StatusNotFound, "bucket not found")
		return
	}

	var req struct {
		Keys []string `json:"keys"`
	}
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if len(req.Keys) == 0 {
		writeError(w, http.StatusBadRequest, "no keys provided")
		return
	}
	if len(req.Keys) > 1000 {
		writeError(w, http.StatusBadRequest, "max 1000 keys per request")
		return
	}

	type deleteResult struct {
		Key     string `json:"key"`
		Deleted bool   `json:"deleted"`
		Error   string `json:"error,omitempty"`
	}
	var results []deleteResult

	for _, key := range req.Keys {
		if !h.engine.ObjectExists(bucket, key) {
			results = append(results, deleteResult{Key: key, Error: "not found"})
			continue
		}
		if err := h.engine.DeleteObject(bucket, key); err != nil {
			results = append(results, deleteResult{Key: key, Error: err.Error()})
			continue
		}
		h.store.DeleteObjectMeta(bucket, key)
		if h.onReplication != nil {
			h.onReplication("s3:ObjectRemoved:Delete", bucket, key, 0, "", "")
		}
		results = append(results, deleteResult{Key: key, Deleted: true})
	}

	writeJSON(w, http.StatusOK, results)
}

// handleDownloadZip streams multiple objects as a zip archive.
func (h *APIHandler) handleDownloadZip(w http.ResponseWriter, r *http.Request, bucket string) {
	if !h.store.BucketExists(bucket) {
		writeError(w, http.StatusNotFound, "bucket not found")
		return
	}

	keysParam := r.URL.Query().Get("keys")
	if keysParam == "" {
		writeError(w, http.StatusBadRequest, "no keys provided")
		return
	}
	keys := strings.Split(keysParam, ",")
	if len(keys) > 1000 {
		writeError(w, http.StatusBadRequest, "max 1000 keys per request")
		return
	}

	// Sanitize bucket name for Content-Disposition header
	safeBucket := strings.Map(func(r rune) rune {
		if r == '"' || r == '\\' || r < 32 {
			return '_'
		}
		return r
	}, bucket)
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s-files.zip"`, safeBucket))

	zw := zip.NewWriter(w)
	defer zw.Close()

	for _, key := range keys {
		// Validate key to prevent zip slip
		if err := validateObjectKey(key); err != nil {
			continue
		}
		reader, _, _, err := h.getLatestObject(bucket, key)
		if err != nil {
			continue
		}
		fw, err := zw.Create(key)
		if err != nil {
			reader.Close()
			continue
		}
		io.Copy(fw, reader)
		reader.Close()
	}
}
