package replication

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"
)

// signV4 adds AWS Signature V4 headers to an outgoing request.
func signV4(req *http.Request, accessKey, secretKey, region string) {
	now := time.Now().UTC()
	dateStr := now.Format("20060102")
	amzDate := now.Format("20060102T150405Z")

	req.Header.Set("X-Amz-Date", amzDate)

	// Payload hash
	payloadHash := req.Header.Get("X-Amz-Content-Sha256")
	if payloadHash == "" {
		payloadHash = "UNSIGNED-PAYLOAD"
		req.Header.Set("X-Amz-Content-Sha256", payloadHash)
	}

	// Signed headers
	signedHeaderNames := []string{"host", "x-amz-content-sha256", "x-amz-date"}
	for name := range req.Header {
		lower := strings.ToLower(name)
		// Every x-amz-* header is signed too: VaultS3 and AWS refuse one that
		// is not.
		if lower == "x-vaults3-replication" ||
			(strings.HasPrefix(lower, "x-amz-") && lower != "x-amz-content-sha256" && lower != "x-amz-date") {
			signedHeaderNames = append(signedHeaderNames, lower)
		}
	}
	sort.Strings(signedHeaderNames)
	signedHeaders := strings.Join(signedHeaderNames, ";")

	// Canonical headers
	var canonHeaders strings.Builder
	for _, name := range signedHeaderNames {
		value := req.Header.Get(name)
		if name == "host" {
			value = req.Host
			if value == "" {
				value = req.URL.Host
			}
		}
		canonHeaders.WriteString(name)
		canonHeaders.WriteString(":")
		canonHeaders.WriteString(strings.TrimSpace(value))
		canonHeaders.WriteString("\n")
	}

	// Canonical query string
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

	uri := uriEncodePath(req.URL.Path)
	if uri == "" {
		uri = "/"
	}

	canonicalRequest := fmt.Sprintf("%s\n%s\n%s\n%s\n%s\n%s",
		req.Method, uri, canonicalQuery,
		canonHeaders.String(), signedHeaders, payloadHash)

	scope := fmt.Sprintf("%s/%s/s3/aws4_request", dateStr, region)
	hash := sha256.Sum256([]byte(canonicalRequest))
	stringToSign := fmt.Sprintf("AWS4-HMAC-SHA256\n%s\n%s\n%s",
		amzDate, scope, hex.EncodeToString(hash[:]))

	signingKey := deriveKey(secretKey, dateStr, region, "s3")
	sig := hex.EncodeToString(hmacSHA256(signingKey, []byte(stringToSign)))

	authHeader := fmt.Sprintf("AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		accessKey, scope, signedHeaders, sig)
	req.Header.Set("Authorization", authHeader)
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
