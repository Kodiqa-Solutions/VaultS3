package cluster

import (
	"crypto/tls"
	"net"
	"net/http"
	"sync/atomic"
	"time"
)

// InterNodeTransport is the ONE HTTP transport every node-to-node call shares:
// metadata writes forwarded to the leader, replica data pushes, delete reaps,
// health probes and read-index queries.
//
// Sharing matters because the connection pool lives in the transport, not the
// client. Each of these call sites used to build its own http.Client per request
// (or per component), which meant they all fell back to the default pool of
// MaxIdleConnsPerHost = 2. Above that trickle of concurrency every inter-node
// call opened a fresh TCP connection and closed it again, so a node under load
// left thousands of sockets in TIME_WAIT — measured at ~1180 on a 3-node cluster
// doing 255 writes/s, against only 55 established. That churn is what turns into
// intermittent connect failures and resets at the gateway hop under sustained
// load (issue #42): ephemeral ports and conntrack entries run out, and a client's
// next connect to that pod fails for no reason it can see.
//
// The pool is sized for the fan-in of a busy cluster: every follower forwards
// each metadata write to the single leader, so the leader is the hot host and
// per-host is the limit that matters.
//
// Inter-node TLS is commonly self-signed and these calls are authenticated by the
// cluster secret, so certificate verification is skipped for them.
var InterNodeTransport = &http.Transport{
	DialContext:           (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
	MaxIdleConns:          1024,
	MaxIdleConnsPerHost:   256,
	IdleConnTimeout:       90 * time.Second,
	TLSHandshakeTimeout:   5 * time.Second,
	ExpectContinueTimeout: 1 * time.Second,
	TLSClientConfig:       &tls.Config{InsecureSkipVerify: true},
}

// InterNodeClient returns a client for inter-node calls with the given overall
// timeout, sharing the pooled transport above. Use timeout 0 for requests that
// stream a body (an overall timeout would cap the transfer, not just the setup).
func InterNodeClient(timeout time.Duration) *http.Client {
	return &http.Client{Transport: InterNodeTransport, Timeout: timeout}
}

// interNodeScheme is the scheme every node-to-node call uses. A node with TLS
// enabled serves its API, and every /cluster/ endpoint on it, over HTTPS only,
// so it has to be reached that way. The control plane used to hard-code http://,
// which with TLS on either failed outright or, behind something that accepted
// plain HTTP, sent the cluster secret across the wire in clear text on every
// forwarded write, read-index query, shard call and join.
//
// It is process-wide because every node in a cluster serves the same scheme and
// the calls are spread over several types. The server sets it once at startup.
var interNodeScheme atomic.Value

// SetInterNodeScheme sets the scheme for node-to-node calls: "https" when the
// server has TLS enabled, "http" otherwise. Anything else is ignored.
func SetInterNodeScheme(scheme string) {
	if scheme == "http" || scheme == "https" {
		interNodeScheme.Store(scheme)
	}
}

// InterNodeScheme returns the scheme node-to-node calls use.
func InterNodeScheme() string {
	if s, ok := interNodeScheme.Load().(string); ok {
		return s
	}
	return "http"
}

// interNodeURL builds the URL of a path on a peer's API address.
func interNodeURL(addr, path string) string {
	return InterNodeScheme() + "://" + addr + path
}
