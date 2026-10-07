package s3

import (
	"io"
	"mime"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
	"github.com/Kodiqa-Solutions/VaultS3/internal/storage"
)

// serveWebsite handles static website requests for website-enabled buckets.
func (h *Handler) serveWebsite(w http.ResponseWriter, r *http.Request, bucket, key string) {
	cfg, err := h.store.GetWebsiteConfig(bucket)
	if err != nil {
		writeS3Error(w, "NoSuchWebsiteConfiguration", "No website configuration", http.StatusNotFound)
		return
	}

	// Resolve index document for root or directory paths
	resolvedKey := key
	if resolvedKey == "" || strings.HasSuffix(resolvedKey, "/") {
		resolvedKey += cfg.IndexDocument
	}

	meta, reader, size, status := h.openWebsiteObject(bucket, resolvedKey)
	// On a cluster the page can be known here while its bytes live on a peer.
	if status == http.StatusServiceUnavailable && h.objects.serveFromDataHolder(w, r, bucket, resolvedKey) {
		return
	}
	if status == http.StatusForbidden {
		http.Error(w, "403 Forbidden", http.StatusForbidden)
		return
	}
	if status != http.StatusOK {
		// Object not found — try error document
		if cfg.ErrorDocument != "" {
			h.serveErrorDocument(w, bucket, cfg.ErrorDocument)
			return
		}
		http.Error(w, "404 Not Found", http.StatusNotFound)
		return
	}
	defer reader.Close()

	w.Header().Set("Content-Type", websiteContentType(meta, resolvedKey, "application/octet-stream"))
	w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	w.WriteHeader(http.StatusOK)
	io.Copy(w, reader)
}

// serveErrorDocument serves the custom error document with 404 status.
func (h *Handler) serveErrorDocument(w http.ResponseWriter, bucket, errorDoc string) {
	meta, reader, size, status := h.openWebsiteObject(bucket, errorDoc)
	if status != http.StatusOK {
		http.Error(w, "404 Not Found", http.StatusNotFound)
		return
	}
	defer reader.Close()

	w.Header().Set("Content-Type", websiteContentType(meta, errorDoc, "text/html"))
	w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	w.WriteHeader(http.StatusNotFound)
	io.Copy(w, reader)
}

// openWebsiteObject opens the object a website request names, through the
// metadata store like every other read. status is 200 when it can be served,
// 403 for an object the website cannot serve, 503 for an object whose bytes are
// not on this node, and 404 otherwise.
//
// It used to open the plain path on the local engine directly. That served a
// deleted object whose bytes lingered, served the bytes of an object hidden by a
// delete marker, found nothing at all in a versioned bucket (whose bytes live
// under .vs/, so every page was a 404), and handed out an SSE-C object's stored
// ciphertext as if it were the page.
func (h *Handler) openWebsiteObject(bucket, key string) (*metadata.ObjectMeta, storage.ReadSeekCloser, int64, int) {
	meta, _ := h.store.GetObjectMetaConsistent(bucket, key)
	if meta == nil || meta.DeleteMarker {
		return nil, nil, 0, http.StatusNotFound
	}
	// An anonymous website visitor cannot present the customer key, so an SSE-C
	// object is never servable here.
	if meta.SSECustomerKeyMD5 != "" {
		return nil, nil, 0, http.StatusForbidden
	}
	reader, size, err := h.objects.readObjectData(bucket, key, meta.VersionID)
	if err != nil {
		return nil, nil, 0, http.StatusServiceUnavailable
	}
	return meta, reader, size, http.StatusOK
}

// websiteContentType is the object's own content type when it has a useful
// one, otherwise a guess from the extension.
func websiteContentType(meta *metadata.ObjectMeta, key, fallback string) string {
	if meta != nil && meta.ContentType != "" && meta.ContentType != "application/octet-stream" {
		return meta.ContentType
	}
	if ct := mime.TypeByExtension(filepath.Ext(key)); ct != "" {
		return ct
	}
	return fallback
}
