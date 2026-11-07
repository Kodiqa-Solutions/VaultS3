package server

import (
	"net/http"
	"time"
)

// Connection limits shared by every listener (S3 and dashboard, inter-node,
// separate console). Variables rather than constants only so tests can shrink
// them.
var (
	// readHeaderTimeout bounds how long a client may take to send the request
	// line and headers. Without it a client that sends headers a byte at a
	// time holds a connection and a goroutine open for ever (slowloris), and
	// enough of them exhaust the server's file descriptors.
	readHeaderTimeout = 30 * time.Second

	// idleTimeout closes a keep-alive connection that has sat between
	// requests this long. It never applies while a request is in flight, so
	// a long download or a live stream is not cut.
	idleTimeout = 2 * time.Minute
)

// newHTTPServer builds an http.Server with the limits above.
//
// ReadTimeout and WriteTimeout stay unset on purpose. They cover the whole
// body, so any fixed value would cut off a large upload or download on a slow
// link. Once the headers are read, net/http clears the header deadline, and
// the body is read with no deadline, exactly as before.
func newHTTPServer(addr string, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: readHeaderTimeout,
		IdleTimeout:       idleTimeout,
	}
}
