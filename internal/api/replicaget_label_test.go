package api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
	"github.com/Kodiqa-Solutions/VaultS3/internal/storage"
)

// overwriteAfterOpen stands in for a client overwrite on the source node that
// lands between replica-get's openLocalCopy and its CopyIndex Lookup.
type overwriteAfterOpen struct {
	storage.Engine
	before func() // runs before the file is opened
	once   func() // runs after
}

func (e *overwriteAfterOpen) GetObject(bucket, key string) (storage.ReadSeekCloser, int64, error) {
	if e.before != nil {
		f := e.before
		e.before = nil
		f()
	}
	rc, n, err := e.Engine.GetObject(bucket, key)
	if e.once != nil {
		f := e.once
		e.once = nil
		f()
	}
	return rc, n, err
}

func TestReplicaGetNeverLabelsBytesWithAnotherWritesETag(t *testing.T) {
	for _, overwriteBeforeOpen := range []bool{false, true} {
		h, store := newTestAPI(t)
		idx, err := metadata.OpenCopyIndex(filepath.Join(t.TempDir(), "c.db"))
		if err != nil {
			t.Fatal(err)
		}
		h.SetCopyIndex(idx)
		_ = store.CreateBucket("rb")
		inner := h.engine
		if _, _, err := inner.PutObject("rb", "k", strings.NewReader("AAAA"), 4); err != nil {
			t.Fatal(err)
		}
		idx.Record("rb", "k", "", `"A"`, 4)
		// A client overwrite on this node through the server's engine: the
		// record is dropped before the write, then set to the new object.
		overwrite := func() {
			_, _, _ = storage.NewForgetOnWrite(inner, idx).PutObject("rb", "k", strings.NewReader("BBBB"), 4)
			idx.Record("rb", "k", "", `"B"`, 4)
		}
		ow := &overwriteAfterOpen{Engine: inner}
		if overwriteBeforeOpen {
			ow.before = overwrite
		} else {
			ow.once = overwrite
		}
		h.engine = ow
		req := httptest.NewRequest(http.MethodGet, "/cluster/replica-get?bucket=rb&key=k", nil)
		req.Header.Set(clusterSecretHeader, "s")
		rr := httptest.NewRecorder()
		h.ClusterReplicaGetHandler("s")(rr, req)
		body, _ := io.ReadAll(rr.Body)
		label := rr.Header().Get(CopyETagHeader)
		if (label == `"A"` && string(body) != "AAAA") || (label == `"B"` && string(body) != "BBBB") {
			t.Errorf("overwrite before open %v: bytes %q sent labelled %s", overwriteBeforeOpen, body, label)
		}
		_ = idx.Close()
	}
}
