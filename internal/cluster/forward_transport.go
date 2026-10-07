package cluster

import (
	"crypto/tls"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"syscall"
	"time"
)

// forwardRetries is how many times a single hop is attempted before the caller is
// told it failed. Two is enough for the case this exists for: a pooled connection
// the peer has already closed, where the first attempt dies instantly and the
// second gets a fresh connection.
const forwardRetries = 2

// forwardRetryDelay spaces out attempts so a peer that is momentarily out of
// accept-queue space (the failure mode that connection churn produces) has a beat
// to recover, without adding real latency to the request.
const forwardRetryDelay = 25 * time.Millisecond

// newForwardTransport builds the transport for one peer's reverse proxy.
//
// Two pools, deliberately:
//
//   - reads carry ResponseHeaderTimeout, so a hung or OOM-looping owner fails fast
//     instead of parking the client (issue #37). It bounds time-to-first-byte only,
//     so streaming a large object after the headers is unaffected.
//
//   - writes must NOT carry it. An upload's response headers do not arrive until
//     the whole body has been sent and stored, so a 10s header timeout was really a
//     "no PUT may take longer than 10 seconds" rule: any forwarded upload slower
//     than that (a large object, a slow client, a busy peer) was cut off and became
//     a 502 Bad Gateway at the client, which is one of the failures reported in
//     issue #42. A write's setup is still bounded by the dial timeout.
//
// Pool sizes are generous for the same reason as InterNodeTransport: the previous
// 16 idle connections per host meant a busy node reopened a TCP connection for most
// forwarded requests, and the resulting socket churn is what makes connects to that
// pod fail intermittently under load.
func newForwardTransport() http.RoundTripper {
	dial := (&net.Dialer{Timeout: 2 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	pool := func(headerTimeout time.Duration) *http.Transport {
		return &http.Transport{
			DialContext:           dial,
			ResponseHeaderTimeout: headerTimeout,
			IdleConnTimeout:       90 * time.Second,
			MaxIdleConns:          1024,
			MaxIdleConnsPerHost:   256,
			// Peers on a TLS cluster commonly use self-signed certificates, as
			// for every other node-to-node call (see InterNodeTransport).
			TLSClientConfig:     &tls.Config{InsecureSkipVerify: true},
			TLSHandshakeTimeout: 5 * time.Second,
		}
	}
	return &forwardTransport{
		read:  pool(10 * time.Second),
		write: pool(0),
	}
}

// forwardTransport picks the right pool for the request and retries a hop that
// failed at the connection level.
type forwardTransport struct {
	read  *http.Transport
	write *http.Transport
}

func (f *forwardTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	base := f.read
	if hasBody(req) {
		base = f.write
	}

	// A retry may only reuse the body if not one byte of it has been read yet;
	// otherwise the peer would receive a truncated upload. Tracking reads is what
	// makes retrying a PUT safe: a hop that failed to dial never touched the body.
	var body *trackedBody
	if req.Body != nil && req.Body != http.NoBody {
		body = &trackedBody{ReadCloser: req.Body}
		req.Body = body
	}

	var resp *http.Response
	var err error
	for attempt := 1; ; attempt++ {
		resp, err = base.RoundTrip(req)
		if err == nil || attempt >= forwardRetries {
			return resp, err
		}
		if !retryableForwardError(err) {
			return resp, err
		}
		if body != nil && body.read {
			return resp, err // upload already in flight, replaying would corrupt it
		}
		slog.Debug("proxy: retrying failed hop",
			"method", req.Method, "path", req.URL.Path, "attempt", attempt, "error", err)
		time.Sleep(forwardRetryDelay)
	}
}

func hasBody(req *http.Request) bool {
	return req.Body != nil && req.Body != http.NoBody && req.ContentLength != 0
}

// retryableForwardError reports whether an error means the peer never processed
// the request, so sending it again is safe. Connection setup and teardown
// failures qualify; a timeout waiting on a response the peer may well be acting
// on does not.
func retryableForwardError(err error) bool {
	if err == nil {
		return false
	}
	// The peer closed a pooled connection as we reused it, or reset/refused the
	// connection outright — the request never reached application code.
	if errors.Is(err, io.EOF) || errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.EPIPE) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		// Only a dial timeout is safe to repeat; a response-header timeout means the
		// peer has the request and may already have applied it.
		var opErr *net.OpError
		return errors.As(err, &opErr) && opErr.Op == "dial"
	}
	var opErr *net.OpError
	return errors.As(err, &opErr) && opErr.Op == "dial"
}

// trackedBody notes whether the request body was read, and swallows Close so a
// retry still has a usable body. The body belongs to the inbound request, which
// the HTTP server closes on its own.
type trackedBody struct {
	io.ReadCloser
	read bool
}

func (t *trackedBody) Read(p []byte) (int, error) {
	n, err := t.ReadCloser.Read(p)
	if n > 0 {
		t.read = true
	}
	return n, err
}

func (t *trackedBody) Close() error { return nil }
