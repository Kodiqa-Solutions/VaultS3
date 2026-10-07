package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"
)

// unsignedPayload is the SigV4 payload hash for a body that is not hashed
// before it is sent. The server takes it as the signed value, as AWS does.
const unsignedPayload = "UNSIGNED-PAYLOAD"

// signV4 signs an HTTP request with AWS Signature V4.
func signV4(req *http.Request, accessKey, secretKey, region string) {
	now := time.Now().UTC()
	dateStr := now.Format("20060102")
	amzDate := now.Format("20060102T150405Z")

	req.Header.Set("X-Amz-Date", amzDate)

	// A caller that streams its body sets the payload hash itself, normally
	// UNSIGNED-PAYLOAD, and the body is then left alone. Hashing it here meant
	// reading all of it into memory, whatever its size.
	bodyHash := req.Header.Get("X-Amz-Content-Sha256")
	if bodyHash == "" {
		var body []byte
		if req.Body != nil && req.Body != http.NoBody {
			body, _ = io.ReadAll(req.Body)
			req.Body = io.NopCloser(bytes.NewReader(body))
		}
		h := sha256.Sum256(body)
		bodyHash = hex.EncodeToString(h[:])
	}
	req.Header.Set("X-Amz-Content-Sha256", bodyHash)

	signedHeaderNames := []string{"host", "x-amz-content-sha256", "x-amz-date"}
	sort.Strings(signedHeaderNames)
	signedHeaders := strings.Join(signedHeaderNames, ";")

	var canonHeaders strings.Builder
	for _, name := range signedHeaderNames {
		val := req.Header.Get(name)
		if name == "host" {
			val = req.Host
			if val == "" {
				val = req.URL.Host
			}
		}
		canonHeaders.WriteString(name + ":" + strings.TrimSpace(val) + "\n")
	}

	canonQuery := ""
	if req.URL.RawQuery != "" {
		qv := req.URL.Query()
		keys := make([]string, 0, len(qv))
		for k := range qv {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var parts []string
		for _, k := range keys {
			for _, v := range qv[k] {
				parts = append(parts, uriEncode(k)+"="+uriEncode(v))
			}
		}
		canonQuery = strings.Join(parts, "&")
	}

	canonReq := fmt.Sprintf("%s\n%s\n%s\n%s\n%s\n%s",
		req.Method, uriEncodePath(req.URL.Path), canonQuery,
		canonHeaders.String(), signedHeaders, bodyHash)

	scope := fmt.Sprintf("%s/%s/s3/aws4_request", dateStr, region)
	hash := sha256.Sum256([]byte(canonReq))
	stringToSign := fmt.Sprintf("AWS4-HMAC-SHA256\n%s\n%s\n%s",
		amzDate, scope, hex.EncodeToString(hash[:]))

	kDate := hmacSign([]byte("AWS4"+secretKey), []byte(dateStr))
	kRegion := hmacSign(kDate, []byte(region))
	kService := hmacSign(kRegion, []byte("s3"))
	kSigning := hmacSign(kService, []byte("aws4_request"))
	sig := hex.EncodeToString(hmacSign(kSigning, []byte(stringToSign)))

	credential := fmt.Sprintf("%s/%s", accessKey, scope)
	req.Header.Set("Authorization",
		fmt.Sprintf("AWS4-HMAC-SHA256 Credential=%s, SignedHeaders=%s, Signature=%s",
			credential, signedHeaders, sig))
}

func hmacSign(key, data []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write(data)
	return h.Sum(nil)
}

// uriEncodePath strictly encodes each path segment (preserving '/') per AWS
// SigV4 rules, so signatures match for keys with '&', '$', spaces, etc.
func uriEncodePath(p string) string {
	segs := strings.Split(p, "/")
	for i, seg := range segs {
		segs[i] = uriEncode(seg)
	}
	return strings.Join(segs, "/")
}

func uriEncode(s string) string {
	var buf strings.Builder
	for _, b := range []byte(s) {
		if (b >= 'A' && b <= 'Z') || (b >= 'a' && b <= 'z') || (b >= '0' && b <= '9') || b == '-' || b == '_' || b == '.' || b == '~' {
			buf.WriteByte(b)
		} else {
			fmt.Fprintf(&buf, "%%%02X", b)
		}
	}
	return buf.String()
}

// s3Request makes a signed S3 API request.
func s3Request(method, path string, body io.Reader) (*http.Response, error) {
	url := strings.TrimRight(endpoint, "/") + path
	var bodyBytes []byte
	if body != nil {
		var err error
		bodyBytes, err = io.ReadAll(body)
		if err != nil {
			return nil, err
		}
	}

	req, err := http.NewRequest(method, url, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, err
	}

	signV4(req, accessKey, secretKey, region)
	return http.DefaultClient.Do(req)
}

// apiToken gets a JWT token from the admin API.
func apiToken() (string, error) {
	url := strings.TrimRight(endpoint, "/") + "/api/v1/auth/login"
	payload := map[string]string{"accessKey": accessKey, "secretKey": secretKey}
	data, _ := json.Marshal(payload)
	resp, err := http.Post(url, "application/json", bytes.NewReader(data))
	if err != nil {
		return "", fmt.Errorf("login failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		switch resp.StatusCode {
		case 403:
			return "", fmt.Errorf("login failed: HTTP 403 — the endpoint %q may not be serving the dashboard API (a 403 means the request fell through to the S3 handler). If you run a split console_port, point --endpoint / VAULTS3_ENDPOINT at the console/dashboard port", endpoint)
		case 401:
			return "", fmt.Errorf("login failed: HTTP 401 — the dashboard API requires the ROOT admin access/secret key (VAULTS3_ACCESS_KEY/SECRET_KEY), not an IAM user key")
		default:
			return "", fmt.Errorf("login failed: HTTP %d", resp.StatusCode)
		}
	}

	var result struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}
	return result.Token, nil
}

// apiRequest makes an authenticated admin API request.
func apiRequest(method, path string, body io.Reader) (*http.Response, error) {
	token, err := apiToken()
	if err != nil {
		return nil, err
	}

	url := strings.TrimRight(endpoint, "/") + "/api/v1" + path
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	return http.DefaultClient.Do(req)
}

// printTable prints data in a formatted table.
func printTable(headers []string, rows [][]string) {
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, strings.Join(headers, "\t"))
	// Underline each header in full. A single "-" per column read as a row of
	// empty values, the same "-" a table prints for an empty field.
	rules := make([]string, len(headers))
	for i, h := range headers {
		rules[i] = strings.Repeat("-", len(h))
	}
	fmt.Fprintln(w, strings.Join(rules, "\t"))
	for _, row := range rows {
		fmt.Fprintln(w, strings.Join(row, "\t"))
	}
	w.Flush()
}

// progressWriter wraps an io.Writer and shows progress.
type progressWriter struct {
	w       io.Writer
	total   int64
	written int64
}

func (pw *progressWriter) Write(p []byte) (int, error) {
	n, err := pw.w.Write(p)
	pw.written += int64(n)
	if pw.total > 0 {
		pct := float64(pw.written) / float64(pw.total) * 100
		fmt.Fprintf(os.Stderr, "\r  Progress: %.1f%% (%s / %s)",
			pct, formatSize(pw.written), formatSize(pw.total))
	} else {
		fmt.Fprintf(os.Stderr, "\r  Uploaded: %s", formatSize(pw.written))
	}
	return n, err
}

func formatSize(b int64) string {
	switch {
	case b >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(b)/(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(b)/(1<<20))
	case b >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(b)/(1<<10))
	default:
		return fmt.Sprintf("%d B", b)
	}
}
