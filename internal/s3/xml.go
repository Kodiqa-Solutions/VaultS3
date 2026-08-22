package s3

import (
	"encoding/xml"
	"log/slog"
	"net/http"
)

type xmlOwner struct {
	ID          string `xml:"ID"`
	DisplayName string `xml:"DisplayName"`
}

type s3Error struct {
	XMLName xml.Name `xml:"Error"`
	Code    string   `xml:"Code"`
	Message string   `xml:"Message"`
}

func writeS3Error(w http.ResponseWriter, code, message string, status int) {
	writeXML(w, status, s3Error{Code: code, Message: message})
}

func writeXML(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	w.Write([]byte(xml.Header))
	xml.NewEncoder(w).Encode(v)
}

// Object tagging XML types

type taggingRequest struct {
	XMLName xml.Name `xml:"Tagging"`
	TagSet  tagSet   `xml:"TagSet"`
}

type tagSet struct {
	Tags []xmlTag `xml:"Tag"`
}

type xmlTag struct {
	Key   string `xml:"Key"`
	Value string `xml:"Value"`
}

type taggingResponse struct {
	XMLName xml.Name `xml:"Tagging"`
	Xmlns   string   `xml:"xmlns,attr"`
	TagSet  tagSet   `xml:"TagSet"`
}

// Batch delete XML types

type deleteRequest struct {
	XMLName xml.Name       `xml:"Delete"`
	Quiet   bool           `xml:"Quiet"`
	Objects []deleteObject `xml:"Object"`
}

type deleteObject struct {
	Key string `xml:"Key"`
	// VersionId names one version to remove permanently. Empty means "delete the
	// object", which on a versioned bucket writes a delete marker instead.
	VersionID string `xml:"VersionId"`
}

type deleteResult struct {
	XMLName xml.Name        `xml:"DeleteResult"`
	Deleted []deletedObject `xml:"Deleted"`
	Errors  []deleteError   `xml:"Error"`
}

type deletedObject struct {
	Key string `xml:"Key"`
	// VersionId is the version the caller asked to remove, echoed back.
	VersionID string `xml:"VersionId,omitempty"`
	// DeleteMarker and DeleteMarkerVersionId report that the delete wrote a
	// marker rather than removing anything, which is how a client tells a
	// versioned delete from a permanent one.
	DeleteMarker          bool   `xml:"DeleteMarker,omitempty"`
	DeleteMarkerVersionID string `xml:"DeleteMarkerVersionId,omitempty"`
}

type deleteError struct {
	Key       string `xml:"Key"`
	VersionID string `xml:"VersionId,omitempty"`
	Code      string `xml:"Code"`
	Message   string `xml:"Message"`
}

// metaWriteFailed reports a failed metadata write to the client and returns true
// when it has answered the request.
//
// A metadata write can fail for a reason the client must hear about: in a cluster
// it goes through Raft, so losing the leader makes it return an error while the
// object bytes are already on disk. Ignoring that error acknowledged a write the
// cluster never committed, and the object then never listed and its GET returned
// 404 forever while the bytes sat there orphaned (found reviewing issue #50).
//
// 503 SlowDown is the right answer: every mainstream SDK retries it, and the
// orphaned bytes are collected by `vaults3-cli storage reclaim` once they pass
// its minimum age. Acking a write that does not exist is the worse trade.
func metaWriteFailed(w http.ResponseWriter, err error, op, bucket, key string) bool {
	if err == nil {
		return false
	}
	slog.Error("metadata write failed, reporting the write as unsuccessful",
		"op", op, "bucket", bucket, "key", key, "error", err)
	writeS3Error(w, "SlowDown",
		"The write could not be recorded, please retry", http.StatusServiceUnavailable)
	return true
}
