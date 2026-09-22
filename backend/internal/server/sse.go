package server

import (
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
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

	// Every frame is a whole state snapshot, which is mostly the same
	// words as the last one — item names, statuses, the same keys a
	// thousand times over. That is exactly what a compressor is for, and
	// on a long queue it is the difference between kilobytes and hundreds
	// of them per frame. Browsers ask for it and decode it without being
	// told anything.
	out, gz := compressed(w, r)
	if gz != nil {
		defer gz.Close()
	}

	// An open stream is a browser watching, which is what keeps a bare run
	// alive; the closing decrement is what eventually lets it stop.
	s.streams.Add(1)
	defer func() {
		s.streams.Add(-1)
		s.seen()
	}()

	// Which jobs this browser has open, so the rest arrive with their
	// items reduced to what the whole-queue views read. The client
	// reconnects with a new list when a card is expanded or collapsed,
	// which is a rare deliberate action and costs one frame.
	open := openJobs(r)

	events, unsubscribe := s.mgr.Subscribe(open)
	defer unsubscribe()

	// Send the current state at once so the page renders without waiting
	// for the first change.
	payload, err := json.Marshal(s.mgr.SnapshotFor(open))
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if !writeEvent(out, gz, rc, payload) {
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
			if !writeEvent(out, gz, rc, msg) {
				return
			}
		case <-ticker.C:
			if _, err := fmt.Fprint(out, ": keep-alive\n\n"); err != nil {
				return
			}
			if gz != nil && gz.Flush() != nil {
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
func writeEvent(out io.Writer, gz *gzip.Writer, rc *http.ResponseController, payload []byte) bool {
	if _, err := fmt.Fprintf(out, "data: %s\n\n", payload); err != nil {
		return false
	}
	// A compressor holds bytes back until it has a block worth emitting,
	// which for a stream means the browser sees nothing until the next
	// frame pushes the last one out. Flushing ends the block; both layers
	// have to be flushed, in this order, or the event stops at whichever
	// was missed.
	if gz != nil {
		if err := gz.Flush(); err != nil {
			return false
		}
	}
	return rc.Flush() == nil
}

// compressed wraps the response in gzip when the client asked for it,
// returning what to write to and the compressor to flush, or a nil
// compressor when the client would rather have it plain.
func compressed(w http.ResponseWriter, r *http.Request) (io.Writer, *gzip.Writer) {
	if !acceptsGzip(r) {
		return w, nil
	}
	h := w.Header()
	h.Set("Content-Encoding", "gzip")
	// The same URL answers both ways, so a cache must key on which.
	h.Add("Vary", "Accept-Encoding")
	gz := gzip.NewWriter(w)
	return gz, gz
}

// openJobs reads the job ids a client renders items for, as a comma-joined
// "open" parameter. Absent means none are open, which is what a freshly
// loaded page shows.
func openJobs(r *http.Request) []string {
	raw := r.URL.Query().Get("open")
	if raw == "" {
		return nil
	}
	return strings.Split(raw, ",")
}

// acceptsGzip reports whether the client will take a compressed reply.
func acceptsGzip(r *http.Request) bool {
	for part := range strings.SplitSeq(r.Header.Get("Accept-Encoding"), ",") {
		if name, _, _ := strings.Cut(part, ";"); strings.EqualFold(strings.TrimSpace(name), "gzip") {
			return true
		}
	}
	return false
}
