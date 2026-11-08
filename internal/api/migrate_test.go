package api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Kodiqa-Solutions/VaultS3/internal/migrate"
)

// Regression (issue found 2026-11-08): a source on a private address was
// answered only with "could not connect", so an operator had no way to learn
// why a MinIO on their own network failed or what to change. It now gets a
// 400 that names the setting, from both the connection test and the start.
// A failure at an allowed source still says nothing about the source's reply.
func TestMigrationNamesTheSettingForAPrivateSource(t *testing.T) {
	src := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		io.WriteString(w, "INTERNAL-SERVICE-SECRET")
	}))
	t.Cleanup(src.Close)
	body := `{"endpoint":"` + src.URL + `","accessKey":"k","secretKey":"s"}`
	call := func(h *APIHandler, handler func(*APIHandler, http.ResponseWriter, *http.Request)) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		handler(h, rec, httptest.NewRequest(http.MethodPost, "/api/v1/migrate", strings.NewReader(body)))
		return rec
	}
	handlers := map[string]func(*APIHandler, http.ResponseWriter, *http.Request){
		"test":  (*APIHandler).handleMigrateTest,
		"start": (*APIHandler).handleMigrateStart,
	}

	h := &APIHandler{}
	h.SetMigrator(migrate.NewManager(nil, nil)) // private sources refused, the default
	for name, handler := range handlers {
		rec := call(h, handler)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "VAULTS3_ALLOW_PRIVATE_MIGRATION_SOURCES") {
			t.Errorf("%s, private source refused: %d %s, want 400 naming the setting", name, rec.Code, rec.Body)
		}
	}

	allowed := migrate.NewManager(nil, nil)
	allowed.AllowPrivateSources(true)
	h.SetMigrator(allowed)
	for name, handler := range handlers {
		rec := call(h, handler)
		if rec.Code != http.StatusBadGateway {
			t.Errorf("%s, source answering 403: %d %s, want 502", name, rec.Code, rec.Body)
		}
		if strings.Contains(rec.Body.String(), "INTERNAL-SERVICE-SECRET") {
			t.Errorf("%s: the answer carries the source's reply: %s", name, rec.Body)
		}
	}
}

// Regression (review of the private-source fix): the start answer passed the
// raw error through, which quoted the source's reply when it did not parse
// and told a closed port from an open one.
func TestMigrationStartDoesNotQuoteTheSource(t *testing.T) {
	src := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "<x>&INTERNAL_SECRET_TOKEN;</x>")
	}))
	t.Cleanup(src.Close)
	m := migrate.NewManager(nil, nil)
	m.AllowPrivateSources(true)
	h := &APIHandler{}
	h.SetMigrator(m)
	for _, endpoint := range []string{src.URL, "http://127.0.0.1:1"} {
		for name, handler := range map[string]func(http.ResponseWriter, *http.Request){
			"start": h.handleMigrateStart, "test": h.handleMigrateTest,
		} {
			rec := httptest.NewRecorder()
			handler(rec, httptest.NewRequest(http.MethodPost, "/api/v1/migrate",
				strings.NewReader(`{"endpoint":"`+endpoint+`","accessKey":"k","secretKey":"s"}`)))
			if rec.Code != http.StatusBadGateway || strings.Contains(rec.Body.String(), "INTERNAL_SECRET_TOKEN") ||
				strings.Contains(rec.Body.String(), "refused") {
				t.Errorf("%s %s: %d %s, want 502 saying nothing about the source", name, endpoint, rec.Code, rec.Body)
			}
		}
	}
	rec := httptest.NewRecorder()
	h.handleMigrateStart(rec, httptest.NewRequest(http.MethodPost, "/api/v1/migrate", strings.NewReader(`{"endpoint":"ftp://x/"}`)))
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "ftp") {
		t.Errorf("bad endpoint: %d %s, want 400 naming the scheme", rec.Code, rec.Body)
	}
}
