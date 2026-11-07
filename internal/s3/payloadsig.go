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

// smallBodyLimit is the largest body checked in full before the handler runs.
// Every configuration body (policy, lifecycle, CORS, tagging, delete list,
// multipart completion) is far smaller. Such handlers may stop reading at the
// end of the XML, so a check made only at EOF would never run for them.
// Larger bodies (object uploads) are checked as they stream.
const smallBodyLimit = 1 << 20

var errPayloadHashMismatch = &reqError{"XAmzContentSHA256Mismatch",
	"The provided 'x-amz-content-sha256' header does not match what was computed.", http.StatusBadRequest}

// isHexSHA256 reports whether v is a SHA-256 written as 64 hex digits, as
// opposed to one of the UNSIGNED-PAYLOAD or STREAMING-* markers.
func isHexSHA256(v string) bool {
	if len(v) != 64 {
		return false
	}
	_, err := hex.DecodeString(v)
	return err == nil
}

// verifyPayloadHash checks a body against the SHA-256 its X-Amz-Content-Sha256
// header names. A small body is read and checked now and replaced by its
// bytes, and a mismatch is returned. A larger one is wrapped so the read that
// reaches its end fails on a mismatch, which makes the storage engine abandon
// the write.
func verifyPayloadHash(r *http.Request) error {
	want := strings.ToLower(r.Header.Get("X-Amz-Content-Sha256"))
	if !isHexSHA256(want) || r.Body == nil || r.Body == http.NoBody {
		return nil
	}
	if r.ContentLength >= 0 && r.ContentLength <= smallBodyLimit {
		body, err := io.ReadAll(io.LimitReader(r.Body, smallBodyLimit+1))
		if err != nil {
			return err
		}
		sum := sha256.Sum256(body)
		if hex.EncodeToString(sum[:]) != want {
			return errPayloadHashMismatch
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		return nil
	}
	r.Body = &hashCheckedBody{src: r.Body, want: want, h: sha256.New()}
	return nil
}

// hashCheckedBody fails the read that reaches EOF when the body's SHA-256 is
// not the one promised.
type hashCheckedBody struct {
	src  io.ReadCloser
	want string
	h    hash.Hash
	done bool
}

func (b *hashCheckedBody) Read(p []byte) (int, error) {
	n, err := b.src.Read(p)
	b.h.Write(p[:n])
	if err == io.EOF && !b.done {
		b.done = true
		if hex.EncodeToString(b.h.Sum(nil)) != b.want {
			return n, errPayloadHashMismatch
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
