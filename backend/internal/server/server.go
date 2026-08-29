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
)

// Server wires the routes.
type Server struct {
	cfg    *config.Config
	mgr    *download.Manager
	log    *slog.Logger
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
func New(cfg *config.Config, mgr *download.Manager, log *slog.Logger, assets fs.FS) *Server {
	s := &Server{cfg: cfg, mgr: mgr, log: log, static: spaHandler(assets)}
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
	mux.HandleFunc("POST /api/downloads", s.handleAdd)
	mux.HandleFunc("POST /api/settings", s.handleSettings)
	mux.HandleFunc("POST /api/clear", s.handleClear)
	mux.HandleFunc("POST /api/jobs/{jobID}/cancel", s.handleJobCancel)
	mux.HandleFunc("POST /api/jobs/{jobID}/retry", s.handleJobRetry)
	mux.HandleFunc("DELETE /api/jobs/{jobID}", s.handleJobRemove)
	mux.HandleFunc("POST /api/jobs/{jobID}/items/{itemID}/cancel", s.handleItemCancel)
	mux.HandleFunc("POST /api/jobs/{jobID}/items/{itemID}/retry", s.handleItemRetry)

	mux.Handle("/", s.static)

	return s.recoverPanics(s.logRequests(mux))
}

// spaHandler serves the built frontend, falling back to index.html so
// client-side routes resolve on a hard refresh.
func spaHandler(assets fs.FS) http.Handler {
	files := http.FileServerFS(assets)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
		if name == "" || name == "." {
			name = "index.html"
		}

		if _, err := fs.Stat(assets, name); err != nil {
			// Unknown path: hand the SPA its entry point.
			serveIndex(w, r, assets)
			return
		}

		// Vite fingerprints everything under /assets, so those are
		// immutable; the entry document must never be cached.
		if strings.HasPrefix(name, "assets/") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "no-cache")
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
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	http.ServeContent(w, r, "index.html", time.Time{}, bytes.NewReader(body))
}

// logRequests records API calls at debug level, skipping the SSE stream,
// which is long-lived and would only ever log once.
func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Every request counts as someone being there, including the ones
		// too dull to log — a page load is the clearest sign of all.
		s.seen()
		if !strings.HasPrefix(r.URL.Path, "/api/") || r.URL.Path == "/api/events" {
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
