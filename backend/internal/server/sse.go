// SPDX-License-Identifier: MIT

package server

import (
	"compress/gzip"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

// handleEvents starts with a complete snapshot, then streams changed rows.
// The manager replaces dropped patches with complete snapshots.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set(httpx.HeaderContentType, "text/event-stream; charset=utf-8")
	h.Set(httpx.HeaderCacheControl, "no-cache, no-transform")
	h.Set("Connection", "keep-alive")
	// Tell nginx and friends not to buffer the stream.
	h.Set(httpx.HeaderAccelBuffering, "no")

	rc := http.NewResponseController(w)
	// The stream is long-lived; the per-write deadlines still apply.
	_ = rc.SetWriteDeadline(time.Time{})

	// Frames contain state snapshots and patches, mostly the same
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

	// The whole of the current state comes back with the subscription, so
	// the page renders without waiting for the first change — and so that
	// registering and being given something to merge into are one act.
	// Every frame after it carries only what moved.
	events, initial, unsubscribe := s.mgr.Subscribe()
	defer unsubscribe()

	// No first frame means the manager has closed, and this stream has
	// nothing to carry: end it rather than hold a shutdown to its deadline.
	// An error page cannot be written here either way — the headers have
	// gone out and they promise a compressed event stream.
	if initial == nil {
		return
	}
	if !writeSSE(out, gz, rc, "data: ", initial) {
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
			if !writeSSE(out, gz, rc, "data: ", msg) {
				return
			}
		case <-ticker.C:
			if !writeSSE(out, gz, rc, ": ", []byte("keep-alive")) {
				return
			}
		}
	}
}

// writeSSE emits one SSE line, a message ("data: ") or a comment (": "),
// reporting whether the client is still there. A message's payload is
// compact JSON, which never contains a newline, so it fits a single line.
func writeSSE(out io.Writer, gz *gzip.Writer, rc *http.ResponseController, prefix string, payload []byte) bool {
	_ = rc.SetWriteDeadline(time.Now().Add(config.SSEWriteTimeout))
	if _, err := fmt.Fprintf(out, "%s%s\n\n", prefix, payload); err != nil {
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
	// Negotiation affects both variants, including the uncompressed one.
	w.Header().Add(httpx.HeaderVary, httpx.HeaderAcceptEncoding)
	if !acceptsGzip(r) {
		return w, nil
	}
	h := w.Header()
	h.Set(httpx.HeaderContentEncoding, "gzip")
	gz := gzip.NewWriter(w)
	return gz, gz
}

// acceptsGzip reports whether the client will take a compressed reply.
func acceptsGzip(r *http.Request) bool {
	for part := range strings.SplitSeq(r.Header.Get(httpx.HeaderAcceptEncoding), ",") {
		name, params, _ := strings.Cut(part, ";")
		if !strings.EqualFold(strings.TrimSpace(name), "gzip") {
			continue
		}
		// "gzip;q=0" names gzip in order to refuse it.
		if q, ok := strings.CutPrefix(strings.ReplaceAll(params, " ", ""), "q="); ok {
			if v, err := strconv.ParseFloat(q, 64); err == nil && v == 0 {
				return false
			}
		}
		return true
	}
	return false
}
