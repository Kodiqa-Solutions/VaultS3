package s3

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"hash"
	"io"
	"net/http"
	"strings"
)

// Payload integrity.
//
// A SigV4 signature covers the request's headers, including
// X-Amz-Content-Sha256, the SHA-256 of the body the client meant to send. It
// does not cover the body itself: the server has to compare the bytes that
// arrive with that hash, and for a signed streaming upload check each chunk's
// own signature. VaultS3 did neither, so on a plain HTTP connection anyone on
// the path could replace the body of a correctly signed upload. These checks
// are what AWS does: XAmzContentSHA256Mismatch for a body that does not match
// its hash, SignatureDoesNotMatch for a chunk or trailer whose signature is
// wrong.

// Payload markers that X-Amz-Content-Sha256 may carry instead of a hash.
const (
	signedStreaming        = "STREAMING-AWS4-HMAC-SHA256-PAYLOAD"
	signedStreamingTrailer = "STREAMING-AWS4-HMAC-SHA256-PAYLOAD-TRAILER"
)

// verifiedBodyLimit is the largest body read and checked in full before a
// non-upload handler runs: the largest that handler itself accepts. They must
// be checked up front because most stop reading at the closing XML tag, so a
// check made at EOF would never run, and the tampered request would be carried
// out. Reading more than the handler would accept only costs memory.
func verifiedBodyLimit(r *http.Request) int64 {
	q := r.URL.Query()
	switch {
	case r.Method == http.MethodPost && q.Has("delete"):
		return maxDeleteRequestBody
	case r.Method == http.MethodPost && q.Has("uploadId"):
		return maxCompleteBodySize
	default:
		return 256 << 10 // every configuration handler's own limit or more
	}
}

// maxVerifiedBody is the largest of those limits.
const maxVerifiedBody = maxCompleteBodySize

var (
	errPayloadHashMismatch = &reqError{"XAmzContentSHA256Mismatch",
		"The provided 'x-amz-content-sha256' header does not match what was computed.", http.StatusBadRequest}
	errVerifiedBodyTooLarge = &reqError{"MaxMessageLengthExceeded",
		"Your request was too big.", http.StatusBadRequest}
)

// isHexSHA256 reports whether v is a SHA-256 written as 64 hex digits, as
// opposed to one of the UNSIGNED-PAYLOAD or STREAMING-* markers.
func isHexSHA256(v string) bool {
	if len(v) != 64 {
		return false
	}
	_, err := hex.DecodeString(v)
	return err == nil
}

// streamsToEngine reports whether a request's body is an object's data that
// goes to the storage engine (PutObject, including a Snowball archive and
// CopyObject's empty body, or UploadPart), where it may be checked as it
// streams, or a browser form upload, whose handler reads the body to its end
// (where the check runs) before it stores anything, and which may be far
// larger than any configuration body. The query parameters of a presigned URL (X-Amz-*) and the
// x-id that SDKs add do not change what the request is.
func streamsToEngine(r *http.Request, key string) bool {
	// A bucket-level POST with a form body is PostUpload unless it is a
	// multi-object delete, as in the dispatcher.
	if r.Method == http.MethodPost && key == "" && !r.URL.Query().Has("delete") &&
		strings.Contains(r.Header.Get("Content-Type"), "multipart/form-data") {
		return true
	}
	if r.Method != http.MethodPut || key == "" {
		return false
	}
	// The same decision the dispatcher makes (handler.go, object
	// sub-resources): these four go to configuration handlers, checked
	// before anything else. Every other PUT on a key is PutObject or, with
	// an uploadId, UploadPart, whatever other query parameters it carries.
	q := r.URL.Query()
	for _, sub := range []string{"legal-hold", "retention", "acl", "tagging"} {
		if q.Has(sub) {
			return false
		}
	}
	return true
}

// verifyPayloadHash checks a body against the SHA-256 its X-Amz-Content-Sha256
// header names. An object upload or part is wrapped so the read that reaches
// its end fails on a mismatch, which happens inside the storage engine's own
// read, before anything is saved. Every other body is read and checked in full
// now, up to maxVerifiedBody, whatever Content-Length says, and replaced by its
// bytes. An empty body must match the hash of an empty body.
func verifyPayloadHash(r *http.Request, streamable bool) error {
	// Only a request signed with an Authorization header has a hash the
	// signature vouches for. An anonymous or presigned request's header is
	// whatever the sender wrote, so reading its body to compare would prove
	// nothing and only cost memory. Only PUT and POST bodies are read.
	if r.Header.Get("Authorization") == "" || (r.Method != http.MethodPut && r.Method != http.MethodPost) {
		return nil
	}
	// A non-upload body sent as aws-chunked is read in full now, through the
	// decoder, so a chunk or trailer signature that does not match refuses the
	// request before its handler acts on bytes it would otherwise decode
	// before the last chunk's signature was checked.
	want := strings.ToLower(r.Header.Get("X-Amz-Content-Sha256"))
	if want == "" {
		// Authentication signed a request without the header as one with an
		// empty body (buildCanonicalRequest), so that is what it vouches for.
		want = emptyPayloadSHA256
	}
	if _, chunked := r.Body.(*awsChunkedBody); chunked && !streamable {
		limit := verifiedBodyLimit(r)
		body, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
		if err != nil {
			return err // a chunk or trailer signature that does not match
		}
		if int64(len(body)) > limit {
			return errVerifiedBodyTooLarge
		}
		// A body that is also covered by a signed hash must match it: the
		// framing may have been added on the way.
		if isHexSHA256(want) {
			sum := sha256.Sum256(body)
			if hex.EncodeToString(sum[:]) != want {
				return errPayloadHashMismatch
			}
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		return nil
	}
	if !isHexSHA256(want) {
		return nil
	}
	if r.Body == nil || r.Body == http.NoBody {
		if want != emptyPayloadSHA256 {
			return errPayloadHashMismatch
		}
		return nil
	}
	if streamable {
		if r.ContentLength == 0 && want != emptyPayloadSHA256 {
			return errPayloadHashMismatch
		}
		// The storage engine's read checks the hash, and putDigests refuses
		// any byte past the declared length, so a length of 0 cannot hide a
		// body.
		r.Body = &hashCheckedBody{src: r.Body, want: want, h: sha256.New()}
		return nil
	}
	// Read in full whatever the declared length says: an unsigned
	// X-Amz-Decoded-Content-Length of 0 must not hide a body from the check.
	limit := verifiedBodyLimit(r)
	body, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil {
		return err
	}
	if int64(len(body)) > limit {
		return errVerifiedBodyTooLarge
	}
	sum := sha256.Sum256(body)
	if hex.EncodeToString(sum[:]) != want {
		return errPayloadHashMismatch
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	return nil
}

// hashCheckedBody fails the read that reaches EOF when the body's SHA-256 is
// not the one promised.
type hashCheckedBody struct {
	src    io.ReadCloser
	want   string
	h      hash.Hash
	done   bool
	failed error // sticky, so a reader that lost the first report sees it again
}

func (b *hashCheckedBody) Read(p []byte) (int, error) {
	if b.failed != nil {
		return 0, b.failed
	}
	n, err := b.src.Read(p)
	b.h.Write(p[:n])
	if err == io.EOF && !b.done {
		b.done = true
		if hex.EncodeToString(b.h.Sum(nil)) != b.want {
			b.failed = errPayloadHashMismatch
			return n, b.failed
		}
	}
	return n, err
}

func (b *hashCheckedBody) Close() error { return b.src.Close() }

// chunkSigner holds what checking a signed streaming upload needs: the signing
// key, the request time and scope, and the signature the next chunk must
// chain from (the request's own signature for the first chunk).
type chunkSigner struct {
	key     []byte
	amzDate string
	scope   string
	prev    string
}

var errChunkSignature = &reqError{"SignatureDoesNotMatch",
	"The request signature we calculated does not match the signature you provided for a chunk of the body.", http.StatusForbidden}

// chunk checks the signature of one chunk, given the SHA-256 of its data, and
// moves the chain on.
func (c *chunkSigner) chunk(dataSum []byte, got string) error {
	sts := "AWS4-HMAC-SHA256-PAYLOAD\n" + c.amzDate + "\n" + c.scope + "\n" + c.prev + "\n" +
		emptyPayloadSHA256 + "\n" + hex.EncodeToString(dataSum)
	return c.check(sts, got)
}

// trailer checks the trailer signature over the trailer's checksum lines,
// each canonicalised as "name:value\n".
func (c *chunkSigner) trailer(canonical string, got string) error {
	sum := sha256.Sum256([]byte(canonical))
	sts := "AWS4-HMAC-SHA256-TRAILER\n" + c.amzDate + "\n" + c.scope + "\n" + c.prev + "\n" + hex.EncodeToString(sum[:])
	return c.check(sts, got)
}

func (c *chunkSigner) check(stringToSign, got string) error {
	want := hex.EncodeToString(hmacSHA256(c.key, []byte(stringToSign)))
	if !hmac.Equal([]byte(want), []byte(strings.ToLower(got))) {
		return errChunkSignature
	}
	c.prev = want
	return nil
}

// chunkSignerFor returns the chunk signer for a request Authenticate already
// accepted with an Authorization header. That check verified the header and
// its signature, so this only re-derives the signing key to continue the
// chain.
func (a *Authenticator) chunkSignerFor(r *http.Request) (*chunkSigner, error) {
	authHeader := r.Header.Get("Authorization")
	parts := strings.SplitN(authHeader, " ", 2)
	if len(parts) != 2 || !strings.HasPrefix(authHeader, "AWS4-HMAC-SHA256") {
		return nil, errors.New("signed streaming needs an Authorization header")
	}
	params := parseAuthParams(parts[1])
	cred := strings.Split(params["Credential"], "/")
	if len(cred) != 5 || params["Signature"] == "" {
		return nil, errors.New("malformed Authorization header")
	}
	amzDate, _, ok := requestTimestamp(r)
	if !ok {
		return nil, errors.New("no request time")
	}
	_, secret, err := a.resolveIdentity(cred[0], r)
	if err != nil && !isAfterSignature(err) {
		return nil, err
	}
	return &chunkSigner{
		key:     deriveSigningKey(secret, cred[1], cred[2], cred[3]),
		amzDate: amzDate,
		scope:   cred[1] + "/" + cred[2] + "/" + cred[3] + "/aws4_request",
		prev:    strings.ToLower(params["Signature"]),
	}, nil
}
