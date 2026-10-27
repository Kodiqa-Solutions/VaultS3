package s3

import (
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
)

const corsBrowserOrigin = "https://app.example.com"

// Configure through the S3 API rather than the store: an accepted ExposeHeader
// used to disappear when the XML was decoded, before any object request ran.
func configureBrowserCORS(t *testing.T, ts *httptest.Server) {
	t.Helper()
	resp := doSigned(t, http.MethodPut, ts.URL+"/browser-cors", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("create bucket: %d", resp.StatusCode)
	}
	config := `<CORSConfiguration>
  <CORSRule><AllowedOrigin>https://app.example.com</AllowedOrigin><AllowedMethod>GET</AllowedMethod><AllowedMethod>HEAD</AllowedMethod><ExposeHeader>Content-Range</ExposeHeader></CORSRule>
  <CORSRule><AllowedOrigin>https://app.example.com</AllowedOrigin><AllowedMethod>PUT</AllowedMethod><AllowedMethod>POST</AllowedMethod><AllowedHeader>*</AllowedHeader><ExposeHeader>ETag</ExposeHeader><ExposeHeader>x-amz-version-id</ExposeHeader><MaxAgeSeconds>3600</MaxAgeSeconds></CORSRule>
</CORSConfiguration>`
	resp = doSigned(t, http.MethodPut, ts.URL+"/browser-cors?cors", []byte(config))
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("put CORS: %d", resp.StatusCode)
	}
}

func requireVary(t *testing.T, h http.Header, fields ...string) {
	t.Helper()
	got := make(map[string]int)
	for _, value := range h.Values("Vary") {
		for _, field := range strings.Split(value, ",") {
			got[strings.ToLower(strings.TrimSpace(field))]++
		}
	}
	for _, field := range fields {
		if got[strings.ToLower(field)] != 1 {
			t.Errorf("Vary %q must include %q once", h.Values("Vary"), field)
		}
	}
}

func TestCORSExposeHeadersSurviveS3RoundTrip(t *testing.T) {
	ts := newIntegrationServer(t)
	configureBrowserCORS(t, ts)
	resp := doSigned(t, http.MethodGet, ts.URL+"/browser-cors?cors", nil)
	defer resp.Body.Close()
	var config struct {
		Rules []struct {
			ExposeHeaders []string `xml:"ExposeHeader"`
		} `xml:"CORSRule"`
	}
	if err := xml.NewDecoder(resp.Body).Decode(&config); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || len(config.Rules) != 2 {
		t.Fatalf("get CORS: %d, %+v", resp.StatusCode, config)
	}
	if got := strings.Join(config.Rules[1].ExposeHeaders, ", "); got != "ETag, x-amz-version-id" {
		t.Fatalf("exposed headers after XML/storage round trip = %q", got)
	}
}

func TestCORSExposesMultipartETagToBrowser(t *testing.T) {
	ts := newIntegrationServer(t)
	configureBrowserCORS(t, ts)
	headers := map[string]string{"Origin": corsBrowserOrigin}
	resp := doSignedWithHeaders(t, http.MethodPost, ts.URL+"/browser-cors/large?uploads", nil, headers)
	var init initiateResult
	if err := xml.NewDecoder(resp.Body).Decode(&init); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || init.UploadID == "" {
		t.Fatalf("initiate multipart: %d, %+v", resp.StatusCode, init)
	}
	partURL := fmt.Sprintf("%s/browser-cors/large?uploadId=%s&partNumber=1", ts.URL, init.UploadID)
	resp = doSignedWithHeaders(t, http.MethodPut, partURL, []byte("browser upload"), headers)
	resp.Body.Close()
	etag := resp.Header.Get("ETag")
	if resp.StatusCode != http.StatusOK || etag == "" {
		t.Fatalf("upload part: %d, ETag %q", resp.StatusCode, etag)
	}
	if got := resp.Header.Get("Access-Control-Expose-Headers"); got != "ETag, x-amz-version-id" {
		t.Fatalf("browser cannot read the part ETag: exposed headers = %q", got)
	}
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != corsBrowserOrigin {
		t.Fatalf("allow origin = %q", got)
	}
	requireVary(t, resp.Header, "Origin")

	complete := fmt.Sprintf(`<CompleteMultipartUpload><Part><PartNumber>1</PartNumber><ETag>%s</ETag></Part></CompleteMultipartUpload>`, etag)
	resp = doSignedWithHeaders(t, http.MethodPost, ts.URL+"/browser-cors/large?uploadId="+init.UploadID, []byte(complete), headers)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("complete multipart with the exposed ETag: %d", resp.StatusCode)
	}
	resp = doSignedWithHeaders(t, http.MethodGet, ts.URL+"/browser-cors/large", nil, headers)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || string(body) != "browser upload" {
		t.Fatalf("completed object: %d %q", resp.StatusCode, body)
	}
	if got := resp.Header.Get("Access-Control-Expose-Headers"); got != "Content-Range" {
		t.Fatalf("GET must select its own rule, exposed headers = %q", got)
	}
}

func TestCORSResponsesMatchMethodAndOrigin(t *testing.T) {
	ts := newIntegrationServer(t)
	configureBrowserCORS(t, ts)
	for _, tc := range []struct {
		name, method, origin, expose string
	}{
		{"put uses the second rule", "PUT", corsBrowserOrigin, "ETag, x-amz-version-id"},
		{"head uses the read rule", "HEAD", corsBrowserOrigin, "Content-Range"},
		{"unmatched method", "DELETE", corsBrowserOrigin, ""},
		{"unmatched origin", "GET", "https://other.example.com", ""},
		{"no origin", "GET", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := doSignedWithHeaders(t, tc.method, ts.URL+"/browser-cors/object", []byte("data"), map[string]string{"Origin": tc.origin})
			resp.Body.Close()
			if got := resp.Header.Get("Access-Control-Expose-Headers"); got != tc.expose {
				t.Errorf("exposed headers = %q, want %q", got, tc.expose)
			}
			wantOrigin := ""
			if tc.expose != "" {
				wantOrigin = tc.origin
			}
			if got := resp.Header.Get("Access-Control-Allow-Origin"); got != wantOrigin {
				t.Errorf("allowed origin = %q, want %q", got, wantOrigin)
			}
			requireVary(t, resp.Header, "Origin")
		})
	}
}

func TestCORSPreflightVariesOnRequestHeaders(t *testing.T) {
	ts := newIntegrationServer(t)
	configureBrowserCORS(t, ts)
	for _, tc := range []struct {
		origin, method string
		status         int
	}{
		{corsBrowserOrigin, "PUT", http.StatusOK},
		{corsBrowserOrigin, "DELETE", http.StatusForbidden},
		{"https://other.example.com", "PUT", http.StatusForbidden},
	} {
		req, _ := http.NewRequest(http.MethodOptions, ts.URL+"/browser-cors/object", nil)
		req.Header.Set("Origin", tc.origin)
		req.Header.Set("Access-Control-Request-Method", tc.method)
		req.Header.Set("Access-Control-Request-Headers", "content-type,x-amz-meta-name")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != tc.status {
			t.Errorf("%s %s: status %d, want %d", tc.origin, tc.method, resp.StatusCode, tc.status)
		}
		requireVary(t, resp.Header, "Origin", "Access-Control-Request-Method", "Access-Control-Request-Headers")
		if tc.status == http.StatusOK {
			if resp.Header.Get("Access-Control-Allow-Origin") != tc.origin || resp.Header.Get("Access-Control-Allow-Headers") != "*" || resp.Header.Get("Access-Control-Max-Age") != "3600" {
				t.Errorf("preflight headers: %v", resp.Header)
			}
		} else if resp.Header.Get("Access-Control-Allow-Origin") != "" {
			t.Error("refused preflight allows an origin")
		}
	}
}

func TestCORSLegacyRulePreservesOtherVaryFields(t *testing.T) {
	h := newTestHandler(t)
	if err := h.store.CreateBucket("legacy-cors"); err != nil {
		t.Fatal(err)
	}
	if err := h.store.PutCORSConfig("legacy-cors", metadata.CORSConfig{Rules: []metadata.CORSRule{{AllowedOrigins: []string{"*"}, AllowedMethods: []string{"GET"}}}}); err != nil {
		t.Fatal(err)
	}
	for _, vary := range []string{"Accept-Encoding", "Accept-Encoding, origin", "*"} {
		r := httptest.NewRequest(http.MethodGet, "/legacy-cors/object", nil)
		r.Header.Set("Origin", corsBrowserOrigin)
		w := httptest.NewRecorder()
		w.Header().Set("Vary", vary)
		h.addCORSHeaders(w, r, "legacy-cors")
		if w.Header().Get("Access-Control-Allow-Origin") != corsBrowserOrigin || w.Header().Get("Access-Control-Expose-Headers") != "" {
			t.Errorf("legacy rule changed: %v", w.Header())
		}
		if vary == "*" {
			if got := w.Header().Values("Vary"); len(got) != 1 || got[0] != "*" {
				t.Errorf("Vary: * changed: %v", got)
			}
		} else {
			requireVary(t, w.Header(), "Accept-Encoding", "Origin")
		}
	}
}
