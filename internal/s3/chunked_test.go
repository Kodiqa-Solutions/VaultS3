package s3

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

// awsChunkedEncode frames data the way an AWS SDK does for streaming uploads.
// signed adds a `;chunk-signature=…` extension and a trailer, mirroring
// STREAMING-AWS4-HMAC-SHA256-PAYLOAD-TRAILER; unsigned mirrors
// STREAMING-UNSIGNED-PAYLOAD-TRAILER.
func awsChunkedEncode(data []byte, signed bool) string {
	var b strings.Builder
	ext := ""
	if signed {
		ext = ";chunk-signature=0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	}
	fmt.Fprintf(&b, "%x%s\r\n", len(data), ext)
	b.Write(data)
	b.WriteString("\r\n")
	fmt.Fprintf(&b, "0%s\r\n", ext)
	b.WriteString("x-amz-checksum-crc32:AAAAAA==\r\n\r\n") // trailer
	return b.String()
}

func newChunkedReq(data []byte, signed bool, sha string) *http.Request {
	r, _ := http.NewRequest(http.MethodPut, "/bucket/key", strings.NewReader(awsChunkedEncode(data, signed)))
	r.Header.Set("Content-Encoding", "aws-chunked")
	r.Header.Set("X-Amz-Content-Sha256", sha)
	r.Header.Set("X-Amz-Decoded-Content-Length", fmt.Sprintf("%d", len(data)))
	// Real SDKs announce the trailer they will send. An unannounced checksum
	// in the trailer is refused, since nothing hashed the body for it.
	r.Header.Set("X-Amz-Trailer", "x-amz-checksum-crc32")
	return r
}

func TestMaybeDecodeAwsChunked(t *testing.T) {
	cases := []struct {
		name   string
		size   int
		signed bool
		sha    string
	}{
		{"unsigned_small", 100, false, "STREAMING-UNSIGNED-PAYLOAD-TRAILER"},
		{"unsigned_empty", 0, false, "STREAMING-UNSIGNED-PAYLOAD-TRAILER"},
		{"unsigned_large", 1 << 20, false, "STREAMING-UNSIGNED-PAYLOAD-TRAILER"},
		{"signed", 4096, true, "STREAMING-AWS4-HMAC-SHA256-PAYLOAD-TRAILER"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			data := bytes.Repeat([]byte{0xAB, 0x03, 0xFF, 0x10}, c.size/4)
			for len(data) < c.size {
				data = append(data, 0x7E)
			}
			r := newChunkedReq(data, c.signed, c.sha)
			maybeDecodeAwsChunked(r)
			got, err := io.ReadAll(r.Body)
			if err != nil {
				t.Fatalf("read decoded body: %v", err)
			}
			if !bytes.Equal(got, data) {
				t.Fatalf("decoded body mismatch: got %d bytes, want %d", len(got), len(data))
			}
			if r.ContentLength != int64(len(data)) {
				t.Errorf("ContentLength = %d, want %d", r.ContentLength, len(data))
			}
			if r.Header.Get("Content-Encoding") != "" {
				t.Errorf("Content-Encoding should be cleared, got %q", r.Header.Get("Content-Encoding"))
			}
		})
	}
}

// A normal (non-chunked) body must pass through completely untouched.
func TestMaybeDecodeAwsChunked_Passthrough(t *testing.T) {
	body := []byte("plain object bytes, not chunked")
	r, _ := http.NewRequest(http.MethodPut, "/bucket/key", bytes.NewReader(body))
	r.Header.Set("X-Amz-Content-Sha256", "abc123") // a real hash, not STREAMING-*
	maybeDecodeAwsChunked(r)
	got, _ := io.ReadAll(r.Body)
	if !bytes.Equal(got, body) {
		t.Fatalf("passthrough body altered: got %q", got)
	}
}
