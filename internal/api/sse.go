package api

import (
	"fmt"
	"net/http"
)

// CloseStreams ends every open live stream (logs, events, trace) and makes new
// ones end at once. The server calls it when it starts shutting down: a stream
// only ends when its client leaves, and a graceful shutdown waits for every
// request, so a single open stream used to hold a restart for the whole
// shutdown timeout and then fail it.
func (h *APIHandler) CloseStreams() {
	h.streamsOnce.Do(func() {
		if h.streamsDone != nil { // nil only on a handler built without NewAPIHandler
			close(h.streamsDone)
		}
	})
}

// serveSSE streams each value from ch to the client as one line, formatted
// with format (which holds the SSE framing), until the client leaves or
// CloseStreams is called.
//
// Headers go out as soon as the stream opens, so a client sees it connected
// instead of waiting, with nothing on the wire, for the first event.
func serveSSE[T any](h *APIHandler, w http.ResponseWriter, r *http.Request, ch <-chan T, format string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming not supported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-h.streamsDone:
			return
		case v := <-ch:
			if _, err := fmt.Fprintf(w, format, v); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}
