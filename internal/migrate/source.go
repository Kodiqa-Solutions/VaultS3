// Package migrate imports data from any S3-compatible source (MinIO, AWS S3,
// Garage, ...) into VaultS3 — the "migrate off MinIO" path. It speaks the S3
// REST API directly (SigV4-signed, path-style) so it needs no AWS SDK and keeps
// VaultS3 a single binary.
package migrate

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// Source is a client for a remote S3-compatible endpoint (path-style).
type Source struct {
	endpoint  string // e.g. http://minio.example.com:9000
	accessKey string
	secretKey string
	region    string
	client    *http.Client
}

// NewSource creates a source client. region defaults to "us-east-1".
func NewSource(endpoint, accessKey, secretKey, region string, timeoutSecs int) *Source {
	return newSource(endpoint, accessKey, secretKey, region, timeoutSecs, false)
}

// newSourceAllowingPrivate is NewSource for an operator who has opted into
// migrating from a source on a private network.
func newSourceAllowingPrivate(endpoint, accessKey, secretKey, region string, timeoutSecs int) *Source {
	return newSource(endpoint, accessKey, secretKey, region, timeoutSecs, true)
}

func newSource(endpoint, accessKey, secretKey, region string, timeoutSecs int, allowPrivate bool) *Source {
	endpoint = strings.TrimRight(endpoint, "/")
	if region == "" {
		region = "us-east-1"
	}
	if timeoutSecs <= 0 {
		timeoutSecs = 60
	}
	return &Source{
		endpoint:  endpoint,
		accessKey: accessKey,
		secretKey: secretKey,
		region:    region,
		// No overall http.Client.Timeout: it caps the WHOLE request including
		// reading the response body, so a large object (tens of GB) that takes
		// longer than the timeout to download fails with "context deadline
		// exceeded ... while reading body" (issue #26). Instead we bound the parts
		// that indicate a dead or stalled source — connect, TLS, and time-to-first
		// -byte — while letting a legitimately long body stream for as long as it
		// needs. timeoutSecs is used as the response-header (time-to-first-byte)
		// budget.
		client: &http.Client{
			Transport: &http.Transport{
				// Copy object bytes verbatim. Without this, Go transparently gunzips a
				// response with Content-Encoding: gzip — which would store DECODED bytes
				// while we record Content-Encoding: gzip (corruption) and also strips the
				// header we want to preserve (issue #13).
				DisableCompression: true,
				// Guarded at dial time so a hostname cannot resolve to something
				// harmless during validation and to loopback a moment later.
				DialContext:           dialContextGuarded(&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}, allowPrivate),
				TLSHandshakeTimeout:   15 * time.Second,
				ResponseHeaderTimeout: time.Duration(timeoutSecs) * time.Second,
				ExpectContinueTimeout: 5 * time.Second,
				IdleConnTimeout:       90 * time.Second,
			},
		},
	}
}

// ObjectInfo is a listed source object.
type ObjectInfo struct {
	Key          string
	Size         int64
	ETag         string
	LastModified int64 // source's original modified time (unix); 0 if unparsable
}

// ObjectData is a fetched source object: its body plus the metadata worth
// carrying over to VaultS3 so a migration is a faithful copy, not a same-day
// re-upload (issue #13). The caller must Close Body.
type ObjectData struct {
	Body               io.ReadCloser
	ContentType        string
	Size               int64 // -1 if unknown
	LastModified       int64 // from the source's Last-Modified header; 0 if absent
	UserMetadata       map[string]string
	ContentEncoding    string
	ContentDisposition string
	CacheControl       string
	ContentLanguage    string
}

type listAllMyBucketsResult struct {
	Buckets struct {
		Bucket []struct {
			Name string `xml:"Name"`
		} `xml:"Bucket"`
	} `xml:"Buckets"`
}

// ListBuckets returns the bucket names on the source.
func (s *Source) ListBuckets() ([]string, error) {
	body, err := s.get(s.endpoint + "/")
	if err != nil {
		return nil, err
	}
	defer body.Close()
	data, _ := io.ReadAll(io.LimitReader(body, 16<<20))

	var res listAllMyBucketsResult
	if err := xml.Unmarshal(data, &res); err != nil {
		return nil, fmt.Errorf("parse ListBuckets response: %w", err)
	}
	names := make([]string, 0, len(res.Buckets.Bucket))
	for _, b := range res.Buckets.Bucket {
		names = append(names, b.Name)
	}
	return names, nil
}

type listBucketResult struct {
	IsTruncated           bool   `xml:"IsTruncated"`
	NextContinuationToken string `xml:"NextContinuationToken"`
	Contents              []struct {
		Key          string `xml:"Key"`
		Size         int64  `xml:"Size"`
		ETag         string `xml:"ETag"`
		LastModified string `xml:"LastModified"`
	} `xml:"Contents"`
}

// ListObjects returns one page of objects (ListObjectsV2). Pass the returned
// next token to page; an empty next token means the listing is complete.
func (s *Source) ListObjects(bucket, continuationToken string) (objs []ObjectInfo, next string, err error) {
	u := fmt.Sprintf("%s/%s?list-type=2&max-keys=1000", s.endpoint, url.PathEscape(bucket))
	if continuationToken != "" {
		u += "&continuation-token=" + url.QueryEscape(continuationToken)
	}
	body, err := s.get(u)
	if err != nil {
		return nil, "", err
	}
	defer body.Close()
	data, _ := io.ReadAll(io.LimitReader(body, 64<<20))

	var res listBucketResult
	if err := xml.Unmarshal(data, &res); err != nil {
		return nil, "", fmt.Errorf("parse ListObjectsV2 response: %w", err)
	}
	for _, c := range res.Contents {
		var lm int64
		// S3 lists timestamps in ISO-8601 (RFC3339); keep 0 if a source deviates.
		if t, err := time.Parse(time.RFC3339, c.LastModified); err == nil {
			lm = t.Unix()
		}
		objs = append(objs, ObjectInfo{Key: c.Key, Size: c.Size, ETag: c.ETag, LastModified: lm})
	}
	if res.IsTruncated {
		next = res.NextContinuationToken
	}
	return objs, next, nil
}

// GetObject opens an object's body for streaming and captures the metadata worth
// preserving (modified time, user metadata, content headers). The caller must
// Close the returned Body.
func (s *Source) GetObject(bucket, key string) (*ObjectData, error) {
	// Strict AWS URI-encoding so the wire path and the SigV4 canonical URI agree.
	// Go's default path escaping leaves sub-delimiters like '&' and '$' literal,
	// which made the source's signature differ from the server's → 403
	// SignatureDoesNotMatch for keys containing them (issue #9).
	full := s.endpoint + uriEncodePath("/"+bucket+"/"+key)
	req, err := http.NewRequest(http.MethodGet, full, nil)
	if err != nil {
		return nil, err
	}
	s.sign(req)
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err // network error — retryable
	}
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		return nil, &httpError{StatusCode: resp.StatusCode,
			msg: fmt.Sprintf("source GET %s/%s returned %d: %s", bucket, key, resp.StatusCode, string(raw))}
	}

	var lm int64
	if t, err := http.ParseTime(resp.Header.Get("Last-Modified")); err == nil {
		lm = t.Unix()
	}
	// User metadata travels as x-amz-meta-<name> response headers.
	var userMeta map[string]string
	for name, vals := range resp.Header {
		if len(vals) > 0 && len(name) > len("X-Amz-Meta-") && strings.EqualFold(name[:len("X-Amz-Meta-")], "X-Amz-Meta-") {
			if userMeta == nil {
				userMeta = make(map[string]string)
			}
			userMeta[strings.ToLower(name[len("X-Amz-Meta-"):])] = vals[0]
		}
	}

	return &ObjectData{
		Body:               resp.Body,
		ContentType:        resp.Header.Get("Content-Type"),
		Size:               resp.ContentLength,
		LastModified:       lm,
		UserMetadata:       userMeta,
		ContentEncoding:    resp.Header.Get("Content-Encoding"),
		ContentDisposition: resp.Header.Get("Content-Disposition"),
		CacheControl:       resp.Header.Get("Cache-Control"),
		ContentLanguage:    resp.Header.Get("Content-Language"),
	}, nil
}

// isAbsent reports whether err means "the source has no such config" (HTTP 404,
// e.g. NoSuchBucketPolicy / NoSuchTagSet) — which is normal, not a failure.
func isAbsent(err error) bool {
	var he *httpError
	if errors.As(err, &he) {
		return he.StatusCode == http.StatusNotFound
	}
	return false
}

// GetBucketPolicy fetches the source bucket's policy document (JSON). Returns
// (nil, nil) when the bucket has no policy attached.
func (s *Source) GetBucketPolicy(bucket string) ([]byte, error) {
	body, err := s.get(fmt.Sprintf("%s/%s?policy", s.endpoint, url.PathEscape(bucket)))
	if err != nil {
		if isAbsent(err) {
			return nil, nil
		}
		return nil, err
	}
	defer body.Close()
	data, err := io.ReadAll(io.LimitReader(body, 1<<20)) // policies are small
	if err != nil {
		return nil, err
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, nil
	}
	return data, nil
}

type sourceTagging struct {
	TagSet struct {
		Tags []struct {
			Key   string `xml:"Key"`
			Value string `xml:"Value"`
		} `xml:"Tag"`
	} `xml:"TagSet"`
}

// GetBucketTagging fetches the source bucket's tag set. Returns (nil, nil) when
// the bucket has no tags.
func (s *Source) GetBucketTagging(bucket string) (map[string]string, error) {
	body, err := s.get(fmt.Sprintf("%s/%s?tagging", s.endpoint, url.PathEscape(bucket)))
	if err != nil {
		if isAbsent(err) {
			return nil, nil
		}
		return nil, err
	}
	defer body.Close()
	data, _ := io.ReadAll(io.LimitReader(body, 1<<20))
	var t sourceTagging
	if err := xml.Unmarshal(data, &t); err != nil {
		return nil, fmt.Errorf("parse bucket tagging: %w", err)
	}
	if len(t.TagSet.Tags) == 0 {
		return nil, nil
	}
	tags := make(map[string]string, len(t.TagSet.Tags))
	for _, tag := range t.TagSet.Tags {
		tags[tag.Key] = tag.Value
	}
	return tags, nil
}

// httpError carries the HTTP status so the migrator can distinguish transient
// failures (5xx, 429, and network errors) from permanent ones (4xx).
type httpError struct {
	StatusCode int
	msg        string
}

func (e *httpError) Error() string { return e.msg }

// Retryable reports whether the failure is worth retrying.
func (e *httpError) Retryable() bool {
	return e.StatusCode >= 500 || e.StatusCode == http.StatusTooManyRequests
}

// get performs a signed GET and returns the body for 2xx, else an error.
func (s *Source) get(rawURL string) (io.ReadCloser, error) {
	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	s.sign(req)
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		return nil, &httpError{StatusCode: resp.StatusCode,
			msg: fmt.Sprintf("source request to %s returned %d: %s", rawURL, resp.StatusCode, string(raw))}
	}
	return resp.Body, nil
}

// --- SigV4 signing (path-style, UNSIGNED-PAYLOAD; adapted from internal/replication) ---

func (s *Source) sign(req *http.Request) {
	now := time.Now().UTC()
	dateStr := now.Format("20060102")
	amzDate := now.Format("20060102T150405Z")

	req.Header.Set("X-Amz-Date", amzDate)
	const payloadHash = "UNSIGNED-PAYLOAD"
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)

	signedHeaders := "host;x-amz-content-sha256;x-amz-date"
	host := req.Host
	if host == "" {
		host = req.URL.Host
	}
	canonHeaders := fmt.Sprintf("host:%s\nx-amz-content-sha256:%s\nx-amz-date:%s\n", host, payloadHash, amzDate)

	canonicalQuery := ""
	if req.URL.RawQuery != "" {
		params := req.URL.Query()
		keys := make([]string, 0, len(params))
		for k := range params {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var parts []string
		for _, k := range keys {
			for _, v := range params[k] {
				parts = append(parts, uriEncode(k)+"="+uriEncode(v))
			}
		}
		canonicalQuery = strings.Join(parts, "&")
	}

	// Canonical URI must use strict AWS encoding. Go's EscapedPath() leaves '&',
	// '$' and other sub-delimiters literal, which mismatches the server (issue #9).
	uri := uriEncodePath(req.URL.Path)
	if uri == "" {
		uri = "/"
	}

	canonicalRequest := fmt.Sprintf("%s\n%s\n%s\n%s\n%s\n%s",
		req.Method, uri, canonicalQuery, canonHeaders, signedHeaders, payloadHash)

	scope := fmt.Sprintf("%s/%s/s3/aws4_request", dateStr, s.region)
	hash := sha256.Sum256([]byte(canonicalRequest))
	stringToSign := fmt.Sprintf("AWS4-HMAC-SHA256\n%s\n%s\n%s", amzDate, scope, hex.EncodeToString(hash[:]))

	signingKey := deriveKey(s.secretKey, dateStr, s.region, "s3")
	sig := hex.EncodeToString(hmacSHA256(signingKey, []byte(stringToSign)))

	req.Header.Set("Authorization", fmt.Sprintf(
		"AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		s.accessKey, scope, signedHeaders, sig))
}

func deriveKey(secret, dateStr, region, service string) []byte {
	kDate := hmacSHA256([]byte("AWS4"+secret), []byte(dateStr))
	kRegion := hmacSHA256(kDate, []byte(region))
	kService := hmacSHA256(kRegion, []byte(service))
	return hmacSHA256(kService, []byte("aws4_request"))
}

func hmacSHA256(key, data []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write(data)
	return h.Sum(nil)
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

// uriEncodePath strictly URI-encodes each segment of a path while preserving the
// '/' separators — the AWS SigV4 canonical-URI rule. Used for both the request
// line and the signature so they agree even for keys with '&', '$', spaces, etc.
func uriEncodePath(p string) string {
	segs := strings.Split(p, "/")
	for i, seg := range segs {
		segs[i] = uriEncode(seg)
	}
	return strings.Join(segs, "/")
}
