package api

import (
	"archive/zip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"mime"
	"net/http"
	"net/http/httptest"
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

	if err := h.consoleDeleteObject(bucket, key); err != nil {
		writeError(w, deleteErrorStatus(err), err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Why a console delete did not happen. The status each maps to is what the
// single delete answers, and the message is what the bulk delete reports for
// the key.
var (
	errObjectNotFound = errors.New("not found")
)

// deleteLockedError means object lock refused the delete.
type deleteLockedError struct{ reason string }

func (e *deleteLockedError) Error() string { return "refused: " + e.reason }

// deleteNotRecordedError means a metadata write failed, so the delete must be
// reported as NOT done even if bytes are already gone: metadata is
// authoritative (issue #34).
type deleteNotRecordedError struct {
	op  string
	err error
}

func (e *deleteNotRecordedError) Error() string { return e.op + " failed: " + e.err.Error() }
func (e *deleteNotRecordedError) Unwrap() error { return e.err }

func deleteErrorStatus(err error) int {
	var locked *deleteLockedError
	switch {
	case errors.Is(err, errObjectNotFound):
		return http.StatusNotFound
	case errors.As(err, &locked):
		return http.StatusForbidden
	}
	return http.StatusInternalServerError
}

// consoleDeleteObject deletes one key the way the S3 DeleteObject does with no
// version id named, and is the only delete the dashboard performs, so the
// single and the bulk delete cannot drift apart again. The bulk delete used to
// remove the plain file and the metadata outright: on a versioned bucket that
// destroyed the object instead of writing a delete marker, and it ignored
// legal hold and retention altogether.
//
// On a versioned bucket a delete marker is written and nothing is destroyed,
// which is also why object lock does not apply there, as on S3. Otherwise the
// data goes, after the lock is checked.
func (h *APIHandler) consoleDeleteObject(bucket, key string) error {
	meta, err := h.store.GetObjectMeta(bucket, key)
	if err != nil || meta == nil || meta.DeleteMarker {
		return errObjectNotFound
	}
	versioning, _ := h.store.GetBucketVersioning(bucket)
	if versioning == "Enabled" || meta.VersionID != "" {
		return h.writeConsoleDeleteMarker(bucket, key, meta)
	}

	if reason := objectLockReason(meta); reason != "" {
		return &deleteLockedError{reason: reason}
	}
	if err := h.engine.DeleteObject(bucket, key); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("delete data: %w", err)
	}
	if err := h.store.DeleteObjectMeta(bucket, key); err != nil {
		return &deleteNotRecordedError{op: "DeleteObjectMeta", err: err}
	}
	if h.onReplication != nil {
		h.onReplication("s3:ObjectRemoved:Delete", bucket, key, 0, "", "")
	}
	return nil
}

// writeConsoleDeleteMarker hides the object behind a delete marker and keeps
// every version. Each write is checked: these used to be discarded, so a
// failed marker reported success while the object stayed listed.
func (h *APIHandler) writeConsoleDeleteMarker(bucket, key string, current *metadata.ObjectMeta) error {
	if err := h.demoteLatest(current); err != nil {
		return &deleteNotRecordedError{op: "demote previous version", err: err}
	}
	dm := metadata.ObjectMeta{
		Bucket: bucket, Key: key, VersionID: genVersionID(),
		DeleteMarker: true, IsLatest: true, LastModified: time.Now().UTC().Unix(),
	}
	if err := h.store.PutObjectVersion(dm); err != nil {
		return &deleteNotRecordedError{op: "write delete marker", err: err}
	}
	if err := h.store.PutObjectMeta(dm); err != nil {
		return &deleteNotRecordedError{op: "write delete marker", err: err}
	}
	if h.onReplication != nil {
		h.onReplication("s3:ObjectRemoved:Delete", bucket, key, 0, "", dm.VersionID)
	}
	return nil
}

// demoteLatest records the current latest object as a non-latest version
// before something replaces it. An object written before its bucket was
// versioned carries no version id, and it used to be skipped here, so the new
// latest pointer overwrote the only record naming it and its bytes became an
// orphan for the reclaim scan to delete. It is adopted as the "null" version
// instead, the way the S3 delete path does, and its bytes stay at the plain
// object path where getVersionData looks for them.
func (h *APIHandler) demoteLatest(current *metadata.ObjectMeta) error {
	if current == nil {
		return nil
	}
	old := *current
	if old.VersionID == "" {
		if old.DeleteMarker {
			return nil
		}
		old.VersionID = nullVersionID
	}
	old.IsLatest = false
	return h.store.PutObjectVersion(old)
}

// nullVersionID is the version id S3 gives an object stored before its bucket
// was versioned. Its bytes live at the plain object path, not under .vs/.
const nullVersionID = "null"

// objectLockReason says why object lock forbids destroying a version, or ""
// when nothing does. It mirrors the S3 path's check.
func objectLockReason(meta *metadata.ObjectMeta) string {
	if meta.LegalHold {
		return "object is under legal hold"
	}
	if meta.RetentionMode != "" && meta.RetentionUntil > 0 && time.Now().UTC().Unix() < meta.RetentionUntil {
		return fmt.Sprintf("object is under %s retention until %s", meta.RetentionMode,
			time.Unix(meta.RetentionUntil, 0).UTC().Format(time.RFC3339))
	}
	return ""
}

// getLatestObject returns a reader for an object's current content, resolving the
// latest version when the bucket is versioned (data then lives under .vs/, not
// at the plain key path).
func (h *APIHandler) getLatestObject(bucket, key string) (io.ReadCloser, int64, *metadata.ObjectMeta, error) {
	meta, _ := h.store.GetObjectMeta(bucket, key)
	if meta != nil && meta.DeleteMarker {
		return nil, 0, meta, errObjectNotFound
	}
	versionID := ""
	if meta != nil {
		versionID = meta.VersionID
	}
	r, sz, err := h.getVersionData(bucket, key, versionID)
	return r, sz, meta, err
}

// getVersionData opens the bytes of one version. The "null" version names an
// object stored before its bucket was versioned, whose bytes are at the plain
// object path rather than under .vs/, so a lookup as a version finds nothing.
func (h *APIHandler) getVersionData(bucket, key, versionID string) (io.ReadCloser, int64, error) {
	if versionID == "" {
		return h.engine.GetObject(bucket, key)
	}
	r, sz, err := h.engine.GetObjectVersion(bucket, key, versionID)
	if err != nil && versionID == nullVersionID {
		return h.engine.GetObject(bucket, key)
	}
	return r, sz, err
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
	user, _ := h.authenticateUser(r)
	var results []uploadResult
	var anyFailed, anyDenied bool

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

		// The route gate checked PutObject on the bucket wildcard, which a Deny
		// on one prefix does not touch, so a user allowed b/* but denied
		// b/secret/* could still write under secret/. Each file is checked under
		// its own key, prefix included.
		if err := h.authorizeConsoleAction(r, user, consoleAction{action: "s3:PutObject", bucket: bucket, key: key}); err != nil {
			part.Close()
			results = append(results, uploadResult{Key: key, Error: err.Error()})
			anyDenied = true
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
			meta := metadata.ObjectMeta{
				Bucket: bucket, Key: key, ContentType: ct, ETag: etag, Size: written,
				LastModified: now, VersionID: versionID, IsLatest: true,
			}
			if err := h.recordNewLatestVersion(meta); err != nil {
				// The metadata writes used to be discarded and the file reported
				// stored. Bytes with no record are an orphan, which the reclaim
				// scan later deletes, so the upload is undone and reported failed.
				if derr := h.engine.DeleteObjectVersion(bucket, key, versionID); derr != nil {
					slog.Warn("could not remove the bytes of an upload whose metadata failed", "bucket", bucket, "key", key, "error", derr)
				}
				fail(key, err)
				continue
			}
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
			if err := h.store.PutObjectMeta(metadata.ObjectMeta{
				Bucket: bucket, Key: key, ContentType: ct, ETag: etag, Size: written, LastModified: now,
			}); err != nil {
				// The plain path may have held an older object whose bytes are now
				// overwritten, so they cannot be restored. Removing the new bytes
				// at least keeps an object with no record from lingering, and the
				// failure is reported instead of a success.
				if derr := h.engine.DeleteObject(bucket, key); derr != nil {
					slog.Warn("could not remove the bytes of an upload whose metadata failed", "bucket", bucket, "key", key, "error", derr)
				}
				fail(key, fmt.Errorf("record metadata: %w", err))
				continue
			}
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
	// (each failed entry carries an `error`), and were already logged above. A
	// file refused by policy alone answers 403.
	status := http.StatusOK
	switch {
	case anyFailed:
		status = http.StatusInternalServerError
	case anyDenied:
		status = http.StatusForbidden
	}
	writeJSON(w, status, results)
}

// recordNewLatestVersion writes the metadata of a freshly written version and
// makes it the latest. The previous latest is demoted first, so two versions
// never both claim to be latest. A failure after the demotion puts the previous
// latest back, so the object keeps answering with its old content.
func (h *APIHandler) recordNewLatestVersion(meta metadata.ObjectMeta) error {
	current, _ := h.store.GetObjectMeta(meta.Bucket, meta.Key)
	if err := h.demoteLatest(current); err != nil {
		return fmt.Errorf("demote previous version: %w", err)
	}
	restore := func() {
		if current == nil || current.VersionID == "" {
			return
		}
		prev := *current
		prev.IsLatest = true
		if err := h.store.PutObjectVersion(prev); err != nil {
			slog.Warn("could not restore the previous latest version", "bucket", meta.Bucket, "key", meta.Key, "error", err)
		}
	}
	if err := h.store.PutObjectVersion(meta); err != nil {
		restore()
		return fmt.Errorf("record version: %w", err)
	}
	if err := h.store.PutObjectMeta(meta); err != nil {
		restore()
		if derr := h.store.DeleteObjectVersion(meta.Bucket, meta.Key, meta.VersionID); derr != nil {
			slog.Warn("could not remove the record of a version whose latest pointer failed", "bucket", meta.Bucket, "key", meta.Key, "error", derr)
		}
		return fmt.Errorf("record latest pointer: %w", err)
	}
	return nil
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
	user, _ := h.authenticateUser(r)

	for _, key := range req.Keys {
		// The route gate checked DeleteObject on the bucket wildcard only, so a
		// Deny on a prefix inside the bucket never applied here.
		if err := h.authorizeConsoleAction(r, user, consoleAction{action: "s3:DeleteObject", bucket: bucket, key: key}); err != nil {
			results = append(results, deleteResult{Key: key, Error: err.Error()})
			continue
		}
		// In a cluster the object's data lives on the node that owns it, and the
		// single delete has always been sent there. The bulk delete removed only
		// what this node held.
		if handled, status, msg := h.proxyConsoleRequest(r, http.MethodDelete, bucket, "objects", key, nil); handled {
			switch {
			case status == http.StatusNoContent || status == http.StatusOK:
				results = append(results, deleteResult{Key: key, Deleted: true})
			case status == http.StatusNotFound:
				results = append(results, deleteResult{Key: key, Error: errObjectNotFound.Error()})
			default:
				results = append(results, deleteResult{Key: key, Error: fmt.Sprintf("owner node answered %d: %s", status, msg)})
			}
			continue
		}
		if err := h.consoleDeleteObject(bucket, key); err != nil {
			results = append(results, deleteResult{Key: key, Error: err.Error()})
			continue
		}
		results = append(results, deleteResult{Key: key, Deleted: true})
	}

	writeJSON(w, http.StatusOK, results)
}

// proxyConsoleRequest sends one per-object console request for a bulk route to
// the node that owns the object, with the caller's own session, so the owner
// authorizes it exactly as if the caller had asked it directly. It reports
// whether the request was handled elsewhere. When body is non-nil the owner's
// response body is streamed into it as it arrives, otherwise up to 1 KiB of it
// comes back as msg.
func (h *APIHandler) proxyConsoleRequest(r *http.Request, method, bucket, sub, key string, body *zipEntryWriter) (handled bool, status int, msg string) {
	if h.clusterProxy == nil {
		return false, 0, ""
	}
	req, err := http.NewRequestWithContext(r.Context(), method, "/", nil)
	if err != nil {
		return false, 0, ""
	}
	req.URL.Path = "/api/v1/buckets/" + bucket + "/" + sub + "/" + key
	req.Host = r.Host
	req.RemoteAddr = r.RemoteAddr
	if auth := r.Header.Get("Authorization"); auth != "" {
		req.Header.Set("Authorization", auth)
	} else if tok := r.URL.Query().Get("token"); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	if body != nil {
		if !h.clusterProxy(body, req, bucket, key) {
			return false, 0, ""
		}
		return true, body.statusCode(), body.errorText()
	}
	rec := httptest.NewRecorder()
	if !h.clusterProxy(rec, req, bucket, key) {
		return false, 0, ""
	}
	text := rec.Body.String()
	if len(text) > 1024 {
		text = text[:1024]
	}
	return true, rec.Code, strings.TrimSpace(text)
}

// zipEntryWriter receives one proxied download and streams it into the
// archive. The entry is created only once the owner answers 200, so a failed
// read leaves no empty file in the zip, and the start of an error answer is
// kept to explain the failure.
type zipEntryWriter struct {
	zw     *zip.Writer
	name   string
	header http.Header
	code   int
	entry  io.Writer
	errBuf []byte
	err    error
}

func (z *zipEntryWriter) Header() http.Header {
	if z.header == nil {
		z.header = http.Header{}
	}
	return z.header
}

func (z *zipEntryWriter) WriteHeader(code int) {
	if z.code != 0 {
		return
	}
	z.code = code
	if code == http.StatusOK {
		z.entry, z.err = z.zw.Create(z.name)
	}
}

func (z *zipEntryWriter) Write(p []byte) (int, error) {
	if z.code == 0 {
		z.WriteHeader(http.StatusOK)
	}
	if z.code != http.StatusOK {
		if room := 1024 - len(z.errBuf); room > 0 {
			if len(p) < room {
				room = len(p)
			}
			z.errBuf = append(z.errBuf, p[:room]...)
		}
		return len(p), nil
	}
	if z.err != nil {
		return 0, z.err
	}
	n, err := z.entry.Write(p)
	if err != nil {
		z.err = err
	}
	return n, err
}

func (z *zipEntryWriter) statusCode() int {
	if z.code == 0 {
		return http.StatusOK
	}
	return z.code
}

func (z *zipEntryWriter) errorText() string {
	if z.err != nil {
		return z.err.Error()
	}
	return strings.TrimSpace(string(z.errBuf))
}

// zipErrorsName is the archive entry that lists what could not be included.
const zipErrorsName = "errors.txt"

// handleDownloadZip streams multiple objects as a zip archive.
//
// Keys come from repeated ?key= parameters, or from ?keys= joined with commas,
// which cannot carry a key that itself contains a comma. Every key that could
// not be included is listed with its reason in an errors.txt entry. The archive
// used to skip such keys in silence and still answer 200, so a download that
// was missing files looked complete, which on a cluster was every key held by
// another node.
func (h *APIHandler) handleDownloadZip(w http.ResponseWriter, r *http.Request, bucket string) {
	if !h.store.BucketExists(bucket) {
		writeError(w, http.StatusNotFound, "bucket not found")
		return
	}

	keys := r.URL.Query()["key"]
	if keysParam := r.URL.Query().Get("keys"); keysParam != "" {
		keys = append(keys, strings.Split(keysParam, ",")...)
	}
	if len(keys) == 0 {
		writeError(w, http.StatusBadRequest, "no keys provided")
		return
	}
	if len(keys) > 1000 {
		writeError(w, http.StatusBadRequest, "max 1000 keys per request")
		return
	}
	user, _ := h.authenticateUser(r)

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

	var skipped []string
	skip := func(key, reason string) {
		skipped = append(skipped, key+": "+reason)
	}
	seen := make(map[string]bool, len(keys))
	for _, key := range keys {
		if seen[key] {
			continue
		}
		seen[key] = true
		// Validate key to prevent zip slip
		if err := validateObjectKey(key); err != nil {
			skip(key, err.Error())
			continue
		}
		if key == zipErrorsName {
			skip(key, "the name is reserved for this list")
			continue
		}
		// The route gate checked GetObject on the bucket wildcard only.
		if err := h.authorizeConsoleAction(r, user, consoleAction{action: "s3:GetObject", bucket: bucket, key: key}); err != nil {
			skip(key, err.Error())
			continue
		}
		entry := &zipEntryWriter{zw: zw, name: key}
		if handled, status, msg := h.proxyConsoleRequest(r, http.MethodGet, bucket, "download", key, entry); handled {
			if status != http.StatusOK || entry.err != nil {
				skip(key, fmt.Sprintf("owner node answered %d: %s", status, msg))
			}
			continue
		}
		reader, _, _, err := h.getLatestObject(bucket, key)
		if err != nil {
			skip(key, "not found or unreadable on this node: "+err.Error())
			continue
		}
		fw, err := zw.Create(key)
		if err != nil {
			reader.Close()
			skip(key, err.Error())
			continue
		}
		if _, err := io.Copy(fw, reader); err != nil {
			skip(key, "read failed part way, the entry is incomplete: "+err.Error())
		}
		reader.Close()
	}
	if len(skipped) > 0 {
		if fw, err := zw.Create(zipErrorsName); err == nil {
			fmt.Fprintf(fw, "%d of the requested objects are missing from this archive:\n", len(skipped))
			for _, line := range skipped {
				fmt.Fprintln(fw, line)
			}
		}
	}
}
