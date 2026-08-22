// Package server exposes the download manager over HTTP: a small JSON API,
// a server-sent-events stream for live progress, and the embedded frontend.
package server

import (
	"io/fs"
	"log/slog"
	"net/http"
	"path"
	"strings"
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
}

// New builds the HTTP handler set.
func New(cfg *config.Config, mgr *download.Manager, log *slog.Logger, assets fs.FS) *Server {
	return &Server{cfg: cfg, mgr: mgr, log: log, static: spaHandler(assets)}
}

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
	http.ServeContent(w, r, "index.html", time.Time{}, strings.NewReader(string(body)))
}

// logRequests records API calls at debug level, skipping the SSE stream,
// which is long-lived and would only ever log once.
func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
