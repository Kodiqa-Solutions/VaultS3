package api

import (
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"time"
)

// forwardUpload streams a single uploaded file to the node that owns it (by hash
// ring), so dashboard-uploaded object data lands on the same node an S3 GET will
// proxy to. The owner stores it locally and records its metadata (which then
// replicates via Raft). Authenticated with an internally-minted admin token.
func (h *APIHandler) forwardUpload(ownerAddr, bucket, prefix, filename, contentType string, body io.Reader) (int64, error) {
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	go func() {
		hdr := make(textproto.MIMEHeader)
		hdr.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename=%q`, filename))
		if contentType != "" {
			hdr.Set("Content-Type", contentType)
		}
		part, err := mw.CreatePart(hdr)
		if err != nil {
			pw.CloseWithError(err)
			return
		}
		if _, err := io.Copy(part, body); err != nil {
			pw.CloseWithError(err)
			return
		}
		mw.Close()
		pw.Close()
	}()

	// The owner is reached on its API port, which speaks TLS when the server
	// does. This used to be plain http whatever the configuration, so every
	// forwarded dashboard upload failed on a TLS cluster.
	scheme := "http"
	if h.cfg != nil && h.cfg.Server.TLS.Enabled {
		scheme = "https"
	}
	u := fmt.Sprintf("%s://%s/api/v1/buckets/%s/upload", scheme, ownerAddr, url.PathEscape(bucket))
	if prefix != "" {
		u += "?prefix=" + url.QueryEscape(prefix)
	}
	req, err := http.NewRequest(http.MethodPost, u, pr)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	if tok, err := h.jwtService().Generate("admin", time.Hour); err == nil {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := (&http.Client{Timeout: 10 * time.Minute}).Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return 0, fmt.Errorf("owner upload returned %d: %s", resp.StatusCode, string(raw))
	}
	var res []uploadResult
	if err := json.NewDecoder(resp.Body).Decode(&res); err == nil && len(res) > 0 {
		return res[0].Size, nil
	}
	return 0, nil
}
