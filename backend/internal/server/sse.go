package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/config"
)

// handleEvents streams state snapshots as server-sent events. Each message
// is the whole state, so a client that misses one simply gets the next.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-cache, no-transform")
	h.Set("Connection", "keep-alive")
	// Tell nginx and friends not to buffer the stream.
	h.Set("X-Accel-Buffering", "no")

	rc := http.NewResponseController(w)
	// The stream is long-lived; the per-write deadlines still apply.
	_ = rc.SetWriteDeadline(time.Time{})

	events, unsubscribe := s.mgr.Subscribe()
	defer unsubscribe()

	// Send the current state at once so the page renders without waiting
	// for the first change.
	payload, err := jsonBytes(s.mgr.Snapshot())
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if !writeEvent(w, rc, payload) {
		return
	}

	ticker := time.NewTicker(config.SSEHeartbeat)
	defer ticker.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case msg, ok := <-events:
			if !ok {
				return // manager shut down
			}
			if !writeEvent(w, rc, msg) {
				return
			}
		case <-ticker.C:
			if _, err := fmt.Fprint(w, ": keep-alive\n\n"); err != nil {
				return
			}
			if rc.Flush() != nil {
				return
			}
		}
	}
}

// writeEvent emits one SSE message, reporting whether the client is still
// there. The payload is compact JSON, which never contains a newline, so it
// fits a single data: line.
func writeEvent(w http.ResponseWriter, rc *http.ResponseController, payload []byte) bool {
	if _, err := fmt.Fprintf(w, "data: %s\n\n", payload); err != nil {
		return false
	}
	return rc.Flush() == nil
}

// jsonBytes marshals compactly for the wire.
func jsonBytes(v any) ([]byte, error) {
	return json.Marshal(v)
}
