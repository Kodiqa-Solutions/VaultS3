package metadata

import (
	"encoding/json"
	"path/filepath"
	"testing"
)

// CompleteMultipartUpload builds the finished object's metadata from this record,
// so anything that does not survive being written and read back is a header
// silently dropped on every object uploaded in parts.
func TestMultipartUploadRecordRoundTrips(t *testing.T) {
	s, err := NewStore(filepath.Join(t.TempDir(), "meta.db"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer s.Close()

	want := MultipartUpload{
		UploadID:           "u1",
		Bucket:             "b",
		Key:                "k",
		ContentType:        "text/csv",
		CreatedAt:          1234,
		Tags:               map[string]string{"run": "nightly batch"},
		UserMetadata:       map[string]string{"owner": "analytics"},
		ContentEncoding:    "gzip",
		ContentDisposition: `attachment; filename="r.csv"`,
		CacheControl:       "max-age=99",
		ContentLanguage:    "en-GB",
		WebsiteRedirect:    "/elsewhere",
	}
	if err := s.CreateMultipartUpload(want); err != nil {
		t.Fatalf("CreateMultipartUpload: %v", err)
	}

	got, err := s.GetMultipartUpload("u1")
	if err != nil {
		t.Fatalf("GetMultipartUpload: %v", err)
	}

	for _, f := range []struct{ name, got, want string }{
		{"ContentType", got.ContentType, want.ContentType},
		{"ContentEncoding", got.ContentEncoding, want.ContentEncoding},
		{"ContentDisposition", got.ContentDisposition, want.ContentDisposition},
		{"CacheControl", got.CacheControl, want.CacheControl},
		{"ContentLanguage", got.ContentLanguage, want.ContentLanguage},
		{"WebsiteRedirect", got.WebsiteRedirect, want.WebsiteRedirect},
		{"Tags[run]", got.Tags["run"], want.Tags["run"]},
		{"UserMetadata[owner]", got.UserMetadata["owner"], want.UserMetadata["owner"]},
	} {
		if f.got != f.want {
			t.Errorf("%s: got %q, want %q", f.name, f.got, f.want)
		}
	}
}

// The record is stored as JSON, which is what lets an upload started before an
// upgrade complete afterwards. A record written by a build that did not know these
// fields has to read back as zero values, not as an error, or every in-flight
// upload breaks the moment the server restarts into a newer version.
func TestMultipartUploadRecordReadsOldFormat(t *testing.T) {
	const oldFormat = `{"upload_id":"u1","bucket":"b","key":"k","content_type":"text/csv","created_at":1234}`

	var got MultipartUpload
	if err := json.Unmarshal([]byte(oldFormat), &got); err != nil {
		t.Fatalf("a record from an older build must still unmarshal: %v", err)
	}
	if got.UploadID != "u1" || got.ContentType != "text/csv" {
		t.Errorf("old fields lost: %+v", got)
	}
	if got.Tags != nil || got.UserMetadata != nil {
		t.Errorf("new fields should be nil on an old record, got tags=%v meta=%v", got.Tags, got.UserMetadata)
	}
	if got.ContentEncoding != "" || got.WebsiteRedirect != "" {
		t.Errorf("new string fields should be empty on an old record: %+v", got)
	}
}

// And the other direction, which is what a rolling upgrade does: a record written
// by a newer build is read by a node still running the older one. encoding/json
// ignores fields it does not know, so that node sees the record it expects rather
// than failing to decode it.
func TestMultipartUploadRecordIsReadableByAnOlderBuild(t *testing.T) {
	blob, err := json.Marshal(MultipartUpload{
		UploadID: "u1", Bucket: "b", Key: "k", ContentType: "text/csv", CreatedAt: 1234,
		Tags: map[string]string{"run": "x"}, ContentEncoding: "gzip",
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	// The struct as the older build declared it.
	var old struct {
		UploadID    string `json:"upload_id"`
		Bucket      string `json:"bucket"`
		Key         string `json:"key"`
		ContentType string `json:"content_type"`
		CreatedAt   int64  `json:"created_at"`
	}
	if err := json.Unmarshal(blob, &old); err != nil {
		t.Fatalf("an older build must still decode a newer record: %v", err)
	}
	if old.UploadID != "u1" || old.ContentType != "text/csv" || old.CreatedAt != 1234 {
		t.Errorf("older build decoded the wrong values: %+v", old)
	}
}
