// SPDX-License-Identifier: MIT

// Package server exposes the download manager over HTTP: a small JSON API,
// a server-sent-events stream for live progress, and the embedded frontend.
package server

import (
	"bytes"
	"io/fs"
	"log/slog"
	"net/http"
	"path"
	"strings"
	"sync/atomic"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/download"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
	"github.com/JohanLindvall/HeapLeach/internal/logstore"
)

// Server wires the routes.
type Server struct {
	cfg    *config.Config
	mgr    *download.Manager
	log    *slog.Logger
	logs   *logstore.Buffer
	static http.Handler

	// lastSeen is when a browser last said anything, as Unix nanoseconds.
	// streams counts the event streams open right now.
	//
	// Together these answer "is anyone there", which a bare desktop run
	// needs in order to know when to stop. They are two signals rather than
	// one because they fail in opposite directions: an open tab makes no
	// requests once its stream is established, so lastSeen alone would call
	// a watching browser absent — and a stream can be held by a machine
	// nobody is sitting at, so the request time is what proves recency.
	lastSeen atomic.Int64
	streams  atomic.Int64
}

// New builds the HTTP handler set.
func New(cfg *config.Config, mgr *download.Manager, log *slog.Logger, assets fs.FS, logs *logstore.Buffer) *Server {
	s := &Server{cfg: cfg, mgr: mgr, log: log, logs: logs, static: spaHandler(assets)}
	// Counted from startup rather than from zero, so a process that is
	// never visited still gets its full grace period before deciding
	// nobody is coming.
	s.lastSeen.Store(time.Now().UnixNano())
	return s
}

// Idle reports whether nothing is downloading and no browser is watching,
// and for how long that has been true.
//
// A stream open right now means someone is there, whatever the clock says.
func (s *Server) Idle() (bool, time.Duration) {
	if s.mgr.Busy() || s.streams.Load() > 0 {
		return false, 0
	}
	return true, time.Since(time.Unix(0, s.lastSeen.Load()))
}

// seen records that a browser is alive.
func (s *Server) seen() { s.lastSeen.Store(time.Now().UnixNano()) }

// Handler returns the root mux.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/health", s.handleHealth)
	mux.HandleFunc("GET /api/state", s.handleState)
	mux.HandleFunc("GET /api/events", s.handleEvents)
	mux.HandleFunc("GET /api/logs", s.handleLogs)
	mux.HandleFunc("POST /api/downloads", s.handleAdd)
	mux.HandleFunc("POST /api/settings", s.handleSettings)
	mux.HandleFunc("GET /api/settings", s.handleGetSettings)
	mux.HandleFunc("GET /api/proxies", s.handleProxies)
	mux.HandleFunc("POST /api/clear", s.handleClear)
	mux.HandleFunc("POST /api/jobs/{jobID}/cancel", s.handleJobCancel)
	mux.HandleFunc("POST /api/jobs/{jobID}/retry", s.handleJobRetry)
	mux.HandleFunc("DELETE /api/jobs/{jobID}", s.handleJobRemove)
	mux.HandleFunc("POST /api/jobs/{jobID}/limits", s.handleJobLimits)
	mux.HandleFunc("POST /api/jobs/{jobID}/items/{itemID}/cancel", s.handleItemCancel)
	mux.HandleFunc("POST /api/jobs/{jobID}/items/{itemID}/retry", s.handleItemRetry)

	// Keep the SPA fallback outside the API mux. Otherwise an unknown API
	// path, or the wrong method on a real endpoint, returns HTML with 200.
	root := http.NewServeMux()
	root.Handle("/api/", mux)
	root.Handle("/", s.static)

	// A page on another site can still send a simple POST here without a
	// preflight — enough to move the download directory and queue a file
	// into it. Refuse cross-origin state changes outright.
	guarded := http.NewCrossOriginProtection().Handler(root)

	return s.recoverPanics(s.logRequests(securityHeaders(guarded)))
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(httpx.HeaderContentTypeOptions, "nosniff")
		w.Header().Set(httpx.HeaderFrameOptions, "DENY")
		w.Header().Set(httpx.HeaderContentSecurityPolicy, "frame-ancestors 'none'; base-uri 'self'; object-src 'none'")
		w.Header().Set(httpx.HeaderReferrerPolicy, "no-referrer")
		next.ServeHTTP(w, r)
	})
}

// spaHandler serves the built frontend, falling back to index.html so
// client-side routes resolve on a hard refresh.
func spaHandler(assets fs.FS) http.Handler {
	files := http.FileServerFS(assets)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set(httpx.HeaderAllow, "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		name := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
		if name == "" || name == "." {
			name = "index.html"
		}

		if _, err := fs.Stat(assets, name); err != nil {
			if strings.HasPrefix(name, "assets/") {
				http.NotFound(w, r)
				return
			}
			// Unknown path: hand the SPA its entry point.
			serveIndex(w, r, assets)
			return
		}

		// Vite fingerprints everything under /assets, so those are
		// immutable; the entry document must never be cached.
		if strings.HasPrefix(name, "assets/") {
			w.Header().Set(httpx.HeaderCacheControl, "public, max-age=31536000, immutable")
		} else {
			w.Header().Set(httpx.HeaderCacheControl, "no-cache")
		}
		files.ServeHTTP(w, r)
	})
}

func serveIndex(w http.ResponseWriter, r *http.Request, assets fs.FS) {
	body, err := fs.ReadFile(assets, "index.html")
	if err != nil {
		http.Error(w, "frontend not built", http.StatusNotFound)
		return
	}
	w.Header().Set(httpx.HeaderContentType, "text/html; charset=utf-8")
	w.Header().Set(httpx.HeaderCacheControl, "no-cache")
	http.ServeContent(w, r, "index.html", time.Time{}, bytes.NewReader(body))
}

// logRequests skips the long-lived SSE stream and the log reader: observing
// logs must not itself keep generating more logs.
func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Every request counts as someone being there, including the ones
		// too dull to log — a page load is the clearest sign of all.
		s.seen()
		if !strings.HasPrefix(r.URL.Path, "/api/") || r.URL.Path == "/api/events" || r.URL.Path == "/api/logs" {
			next.ServeHTTP(w, r)
			return
		}
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		s.log.Debug("api", "method", r.Method, "path", r.URL.Path,
			"status", rec.status, "dur", time.Since(start).Round(time.Millisecond))
	})
}

// recoverPanics keeps one bad request from taking the process down.
func (s *Server) recoverPanics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				s.log.Error("panic serving request", "path", r.URL.Path, "panic", v)
				http.Error(w, "internal error", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// statusRecorder captures the response status for logging.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }
