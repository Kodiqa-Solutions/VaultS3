package cluster

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// Regression (5.0.11): an informational status from the other node (100
// Continue, 103 Early Hints) was taken for the answer, so the real status that
// followed was dropped: a 404 the failover should fall through on was sent on,
// as was any error, behind a 1xx the client never asked for.
func TestPeekWriterIgnoresInformationalStatus(t *testing.T) {
	for _, final := range []int{http.StatusNotFound, http.StatusInternalServerError, http.StatusOK} {
		rec := httptest.NewRecorder()
		pw := &peekWriter{ResponseWriter: rec, discardNotFound: true}
		pw.WriteHeader(http.StatusContinue)
		pw.WriteHeader(http.StatusEarlyHints)
		pw.WriteHeader(final)
		switch {
		case final == http.StatusNotFound:
			if !pw.discarded || pw.committed {
				t.Errorf("a 404 after a 1xx was not held for the next node (discarded %v, committed %v)", pw.discarded, pw.committed)
			}
		case !pw.committed || rec.Code != final:
			t.Errorf("final status %d after a 1xx: recorded %d (committed %v)", final, rec.Code, pw.committed)
		}
	}
}

// A holder whose own copy is behind answers 503; the next holder may have
// the current bytes, so the answer is held back like a 404 (5.0.14 soak).
func TestPeekWriterTriesTheNextHolderAfterA503(t *testing.T) {
	rec := httptest.NewRecorder()
	pw := &peekWriter{ResponseWriter: rec, discardNotFound: true}
	pw.WriteHeader(http.StatusServiceUnavailable)
	if !pw.discarded || pw.committed {
		t.Errorf("a 503 was sent on while another holder was left (discarded %v, committed %v)", pw.discarded, pw.committed)
	}
	rec = httptest.NewRecorder()
	pw = &peekWriter{ResponseWriter: rec, discardNotFound: false}
	pw.WriteHeader(http.StatusServiceUnavailable)
	if !pw.committed || rec.Code != http.StatusServiceUnavailable {
		t.Errorf("the last holder's 503 was not sent: %d", rec.Code)
	}
}
