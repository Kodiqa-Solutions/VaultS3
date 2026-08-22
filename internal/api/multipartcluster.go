package api

import (
	"net/http"

	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
)

// In-progress multipart state is deliberately node-local (issue #32): every part
// of an upload is written to one node's disk, and replicating the metadata through
// Raft only added a read-after-write lag that 404'd concurrent part uploads.
//
// The cost of that choice is that no single node knows about all uploads, which
// broke two things (issue #47 bug B):
//
//   - ListMultipartUploads is a BUCKET-level request, routed by hash(bucket, "")
//     to one node, while uploads live on the node owning each object KEY. It
//     therefore listed only the ~1/N of uploads whose key hashed to the same node,
//     and the rest were invisible: unlistable, unabortable, parts stuck on disk.
//   - After any hash-ring change an upload stays on its creating node while abort
//     and ListParts route to the key's new owner, which has never seen it and
//     answers NoSuchUpload. The upload is then a permanent phantom: still listed,
//     never removable.
//
// These two handlers give the coordinating node a way to ask its peers, without
// moving multipart state back into Raft.

// ClusterMultipartListHandler serves GET /cluster/multipart-list?bucket=X on the
// cluster channel: this node's own in-progress uploads for that bucket.
func (h *APIHandler) ClusterMultipartListHandler(secret string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !clusterAuthOK(r, secret) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		bucket := r.URL.Query().Get("bucket")
		if bucket == "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		store := h.localStore
		if store == nil {
			store = h.store
		}
		uploads, err := store.ListMultipartUploads(bucket)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		if uploads == nil {
			uploads = []metadata.MultipartUpload{}
		}
		writeJSON(w, http.StatusOK, uploads)
	}
}

// ClusterMultipartFindHandler serves GET /cluster/multipart-find?uploadId=X on
// the cluster channel: 200 when this node holds that upload, 404 otherwise. The
// coordinator uses it to locate an upload before forwarding the client's request,
// so an upload stranded by a ring change stays reachable.
func (h *APIHandler) ClusterMultipartFindHandler(secret string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !clusterAuthOK(r, secret) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		uploadID := r.URL.Query().Get("uploadId")
		if uploadID == "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		store := h.localStore
		if store == nil {
			store = h.store
		}
		upload, err := store.GetMultipartUpload(uploadID)
		if err != nil || upload == nil {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		writeJSON(w, http.StatusOK, upload)
	}
}
