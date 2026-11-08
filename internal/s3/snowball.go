package s3

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
)

// SnowballUpload handles PUT /{bucket}/{key} with x-amz-meta-snowball-auto-extract: true.
// It extracts a TAR archive into individual objects in the bucket.
func (h *ObjectHandler) SnowballUpload(w http.ResponseWriter, r *http.Request, bucket string) {
	if !h.store.BucketExists(bucket) {
		writeS3Error(w, "NoSuchBucket", "Bucket does not exist", http.StatusNotFound)
		return
	}

	// Entries are stored as the archive is read, and tar stops reading at
	// the end-of-archive blocks, before the body's end. So an archive whose
	// body must match its signature (a SHA-256, or signed chunks) is read to
	// its end first, which runs those checks, and only then extracted.
	// Otherwise a tampered archive's entries were stored.
	var body io.Reader = r.Body
	if bodyIsSigned(r) {
		staged, status, code, msg := h.stageSignedArchive(r, bucket)
		if staged == nil {
			writeS3Error(w, code, msg, status)
			return
		}
		defer func() {
			_ = staged.Close()
			_ = os.Remove(staged.Name())
		}()
		body = staged
	}

	tr := tar.NewReader(body)
	var count, refused int

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			writeS3Error(w, "InvalidArgument", fmt.Sprintf("TAR read error: %v", err), http.StatusBadRequest)
			return
		}

		key, why := h.snowballEntryKey(r, bucket, hdr)
		if key == "" {
			if why != "" {
				slog.Warn("snowball: entry refused", "bucket", bucket, "entry", hdr.Name, "reason", why)
				refused++
			}
			continue
		}

		// Check quota
		if !h.checkQuota(w, bucket, hdr.Size) {
			return
		}

		// Each entry is written like any other object, so the import honours the
		// bucket's versioning and object lock. It used to write the plain path
		// directly, replacing locked objects and bypassing versioning.
		meta, err := h.writeObject(newObject{
			bucket:   bucket,
			key:      key,
			body:     tr,
			size:     hdr.Size,
			meta:     metadata.ObjectMeta{ContentType: "application/octet-stream"},
			lockFrom: nil,
		})
		if err != nil {
			// One entry of a batch import: record it and keep going, but never
			// count it as imported.
			slog.Error("snowball: entry not imported", "bucket", bucket, "key", key, "error", err)
			var re *reqError
			if errors.As(err, &re) {
				refused++
			}
			continue
		}

		if h.onNotification != nil {
			h.onNotification("s3:ObjectCreated:Put", bucket, key, meta.Size, meta.ETag, meta.VersionID)
		}
		if h.onSearchUpdate != nil {
			h.onSearchUpdate("put", bucket, key)
		}

		count++
	}

	w.Header().Set("X-Amz-Snowball-Extracted-Count", fmt.Sprintf("%d", count))
	w.Header().Set("X-Amz-Snowball-Refused-Count", fmt.Sprintf("%d", refused))
	w.WriteHeader(http.StatusOK)
}

// snowballEntryKey is the key an archive entry is stored under, or "" when it
// is skipped: why says why it was refused, and is empty for an entry that is
// simply not an object (a directory, a link, the archive's root). Staging and
// extraction both use it, so the quota is charged for exactly the entries
// that are stored.
func (h *ObjectHandler) snowballEntryKey(r *http.Request, bucket string, hdr *tar.Header) (key, why string) {
	// Only regular files become objects. Directories, links and devices
	// carry no data.
	if hdr.Typeflag != tar.TypeReg {
		return "", ""
	}
	// A sparse entry (PAX GNU.sparse records) is reported as a regular file
	// of its full size, and reading it fills the holes with zeros, so a few
	// KiB of archive stored an object of any size, past every size limit.
	// No S3 object is sparse, so these are refused. (The old GNU 'S' type is
	// not TypeReg and never got here.)
	for k := range hdr.PAXRecords {
		if strings.HasPrefix(k, "GNU.sparse.") {
			return "", "sparse entries are not supported"
		}
	}
	// One entry is one object, held to the 5 GiB a single upload may be. The
	// archive branches off before PutObject's own limit.
	if hdr.Size > maxSnowballEntry {
		return "", "entry exceeds the 5GB object size limit"
	}
	// The raw entry name is checked before it is cleaned. path.Clean keeps a
	// leading "..", and nothing checked it afterwards, so an entry named
	// "../victim/a.txt" overwrote another bucket's object. A refused entry is
	// counted and skipped, and the rest of the archive goes on. Cleaning
	// still turns "./a.txt", as `tar -C dir .` writes it, into "a.txt".
	for _, seg := range strings.Split(hdr.Name, "/") {
		if seg == ".." {
			return "", "Key must not contain '..' path segments"
		}
	}
	key = path.Clean(hdr.Name)
	if key == "." {
		return "", ""
	}
	// The cleaned name is the key, so it is the one checked in full: checked
	// raw, "./.vs/x" passed and was stored as .vs/x, in the version store.
	if msg := objectKeyProblem(key); msg != "" {
		return "", msg
	}
	// The request was authorized for the archive's own key. Every entry is a
	// write to a key of its own, so each is authorized on that key.
	if err := h.authorizeEntry(r, "s3:PutObject", formatResource(bucket, key)); err != nil {
		return "", "refused by policy: " + err.Error()
	}
	return key, ""
}

// snowballStagingDir holds signed archives while their checks run. It is on
// the data volume, which is always writable (a container may run with a
// read-only root filesystem, so the system temp directory is not), and its
// leading dot keeps the reclaim scan away from it.
const snowballStagingDir = ".snowball"

// maxSnowballEntry is the largest object one archive entry may become, the
// size limit of a single upload. A variable only so tests can shrink it.
var maxSnowballEntry int64 = 5 << 30

// maxSnowballArchive is the largest archive staged, the size limit of a
// single upload. A variable only so tests can shrink it.
var maxSnowballArchive int64 = 5 << 30

// ClearSnowballStaging removes archives a previous run left staged, when it
// was stopped part way through an upload. Called at startup.
func ClearSnowballStaging(dataDir string) error {
	return os.RemoveAll(filepath.Join(dataDir, snowballStagingDir))
}

// stageSignedArchive copies a signed archive's body to a file on the data
// volume, reading it to its end so its signature checks run. On failure it
// returns nil and the error to answer with.
func (h *ObjectHandler) stageSignedArchive(r *http.Request, bucket string) (*os.File, int, string, string) {
	dir := filepath.Join(h.engine.DataDir(), snowballStagingDir)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		slog.Error("snowball: cannot create staging directory", "dir", dir, "error", err)
		return nil, http.StatusInternalServerError, "InternalError", "Could not stage the archive"
	}
	f, err := os.CreateTemp(dir, "archive-*")
	if err != nil {
		slog.Error("snowball: cannot create staging file", "dir", dir, "error", err)
		return nil, http.StatusInternalServerError, "InternalError", "Could not stage the archive"
	}
	fail := func(status int, code, msg string) (*os.File, int, string, string) {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return nil, status, code, msg
	}

	// Staging must not fill the disk: an archive is held to the 5 GiB a
	// single upload may be. The quota limits what is stored, the entries, not
	// the archive: tar adds a header and padding to each entry, so an archive
	// whose files fit was refused when its own size was held to the quota.
	// The headers are read as the archive arrives, and one whose files cannot
	// fit is refused at once rather than staged in full.
	sw := &stagingWriter{f: f, limit: maxSnowballArchive}
	src := io.TeeReader(r.Body, sw)
	quota := h.quotaByteLimit(bucket)
	var files int64
	tr := tar.NewReader(src)
	for {
		hdr, err := tr.Next() // reads past the previous entry's data
		if err != nil {
			// The end of the archive, or a damaged one, which extraction
			// reports once the body's checks have passed.
			break
		}
		// Only what extraction will store counts: an entry it refuses (a
		// bad name, a key the caller may not write) is never charged.
		if quota < 0 {
			continue
		}
		if key, _ := h.snowballEntryKey(r, bucket, hdr); key != "" {
			if files += hdr.Size; files > quota {
				return fail(http.StatusForbidden, "QuotaExceeded", "Maximum bucket size exceeded")
			}
		}
	}
	// The rest (end blocks, padding) is read to the body's end, where its
	// checks run.
	_, rerr := io.Copy(io.Discard, src)
	switch {
	case sw.tooLarge:
		return fail(http.StatusBadRequest, "EntityTooLarge", "Archive size exceeds the 5GB limit")
	case sw.werr != nil:
		// A full or failing disk is the server's problem, not a bad request.
		slog.Error("snowball: cannot write staging file", "file", f.Name(), "error", sw.werr)
		return fail(http.StatusInternalServerError, "InternalError", "Could not stage the archive")
	case rerr != nil:
		if re := bodyFailure(rerr); re != nil {
			return fail(re.status, re.code, re.msg)
		}
		return fail(http.StatusBadRequest, "IncompleteBody", "The archive could not be read")
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return fail(http.StatusInternalServerError, "InternalError", "Could not stage the archive")
	}
	return f, 0, "", ""
}

// stagingWriter writes a staged archive, up to limit bytes.
type stagingWriter struct {
	f        *os.File
	n, limit int64
	tooLarge bool
	werr     error
}

var errStagingFull = errors.New("staged archive over its size limit")

func (s *stagingWriter) Write(p []byte) (int, error) {
	if s.n+int64(len(p)) > s.limit {
		s.tooLarge = true
		return 0, errStagingFull
	}
	n, err := s.f.Write(p)
	s.n += int64(n)
	if err != nil {
		s.werr = err
	}
	return n, err
}

// bodyIsSigned reports whether a request's body is covered by its signature:
// a SHA-256 in X-Amz-Content-Sha256, signed streaming chunks, or a request
// signed with an Authorization header but no payload hash, which is checked
// as an empty body. It also asks the body itself, so it agrees with what
// verifyPayloadHash set up.
func bodyIsSigned(r *http.Request) bool {
	if bodyIsVerified(r.Body) {
		return true
	}
	ph := r.Header.Get("X-Amz-Content-Sha256")
	if ph == "" && r.Header.Get("Authorization") != "" {
		return true
	}
	return isHexSHA256(ph) || isSignedStreaming(r)
}

// bodyIsVerified reports whether body checks itself as it is read and reports
// a failure only at its end: a body checked against a signed SHA-256, or
// signed chunks. Whoever acts on such a body must read it to the end first.
func bodyIsVerified(body io.Reader) bool {
	switch b := body.(type) {
	case *hashCheckedBody:
		return true
	case *awsChunkedBody:
		return b.signer != nil
	}
	return false
}
