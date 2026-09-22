package server

import (
	"compress/gzip"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/download"
	"github.com/JohanLindvall/HeapLeach/internal/extractor"
)

// handleHealth is a liveness probe for container orchestration.
func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleState returns the current snapshot, for the initial page load and
// as a fallback when EventSource is unavailable.
func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	// The one response here big enough to be worth compressing, and the
	// one a browser asks for repeatedly: the polling fallback fetches it
	// every couple of seconds, and on a long queue it is most of a
	// megabyte of very repetitive JSON.
	writeJSONMaybeCompressed(w, r, http.StatusOK, s.mgr.SnapshotFor(openJobs(r)))
}

// addRequest is the body of POST /api/downloads.
type addRequest struct {
	// URLs accepts either a newline-separated string or an array.
	URLs urlList `json:"urls"`
	// Password unlocks protected folders, where the host supports them.
	Password string `json:"password"`
}

// addResponse reports which URLs were accepted and which were rejected.
type addResponse struct {
	Accepted []acceptedJob `json:"accepted"`
	Rejected []rejectedURL `json:"rejected"`
}

type acceptedJob struct {
	ID  string `json:"id"`
	URL string `json:"url"`
}

type rejectedURL struct {
	URL   string `json:"url"`
	Error string `json:"error"`
}

// handleAdd queues one or more URLs.
func (s *Server) handleAdd(w http.ResponseWriter, r *http.Request) {
	var req addRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if len(req.URLs) == 0 {
		writeError(w, http.StatusBadRequest, "no URLs supplied")
		return
	}

	resp := addResponse{Accepted: []acceptedJob{}, Rejected: []rejectedURL{}}
	for _, raw := range req.URLs {
		id, err := s.mgr.Add(raw, req.Password)
		if err != nil {
			resp.Rejected = append(resp.Rejected, rejectedURL{URL: raw, Error: err.Error()})
			continue
		}
		resp.Accepted = append(resp.Accepted, acceptedJob{ID: id, URL: raw})
	}

	status := http.StatusAccepted
	if len(resp.Accepted) == 0 {
		status = http.StatusBadRequest
	}
	writeJSON(w, status, resp)
}

// handleSettings updates runtime settings. Every field is optional, so a
// request carries only what changed.
func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Concurrency *int    `json:"concurrency"`
		Streams     *int    `json:"streams"`
		Paused      *bool   `json:"paused"`
		SpeedLimit  *int64  `json:"speedLimit"`
		DownloadDir *string `json:"downloadDir"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.DownloadDir != nil {
		if err := s.mgr.SetDownloadDir(*req.DownloadDir); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	if req.Concurrency != nil {
		if err := s.mgr.SetConcurrency(*req.Concurrency); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	if req.Streams != nil {
		if err := s.mgr.SetStreams(*req.Streams); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	if req.SpeedLimit != nil {
		if err := s.mgr.SetSpeedLimit(*req.SpeedLimit); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	if req.Paused != nil {
		s.mgr.SetPaused(*req.Paused)
	}
	writeJSON(w, http.StatusOK, s.mgr.Snapshot())
}

// handleClear forgets every finished job.
func (s *Server) handleClear(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]int{"removed": s.mgr.ClearFinished()})
}

func (s *Server) handleJobCancel(w http.ResponseWriter, r *http.Request) {
	s.jobAction(w, r, s.mgr.CancelJob)
}

func (s *Server) handleJobRetry(w http.ResponseWriter, r *http.Request) {
	s.jobAction(w, r, s.mgr.RetryJob)
}

func (s *Server) handleJobRemove(w http.ResponseWriter, r *http.Request) {
	s.jobAction(w, r, s.mgr.RemoveJob)
}

func (s *Server) handleItemCancel(w http.ResponseWriter, r *http.Request) {
	s.itemAction(w, r, s.mgr.CancelItem)
}

func (s *Server) handleItemRetry(w http.ResponseWriter, r *http.Request) {
	s.itemAction(w, r, s.mgr.RetryItem)
}

// jobAction applies a manager method to the job named in the path.
func (s *Server) jobAction(w http.ResponseWriter, r *http.Request, fn func(string) error) {
	if err := fn(r.PathValue("jobID")); err != nil {
		writeManagerError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// itemAction applies a manager method to the item named in the path.
func (s *Server) itemAction(w http.ResponseWriter, r *http.Request, fn func(string, string) error) {
	if err := fn(r.PathValue("jobID"), r.PathValue("itemID")); err != nil {
		writeManagerError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func writeManagerError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, download.ErrNotFound):
		writeError(w, http.StatusNotFound, "no such job or item")
	case errors.Is(err, extractor.ErrPasswordRequired):
		writeError(w, http.StatusUnauthorized, err.Error())
	default:
		writeError(w, http.StatusBadRequest, err.Error())
	}
}

// urlList accepts a JSON string (one URL per line) or an array of strings.
type urlList []string

func (l *urlList) UnmarshalJSON(b []byte) error {
	var single string
	if err := json.Unmarshal(b, &single); err == nil {
		*l = splitURLs(single)
		return nil
	}
	var many []string
	if err := json.Unmarshal(b, &many); err != nil {
		return errors.New("urls must be a string or an array of strings")
	}
	var out []string
	for _, v := range many {
		out = append(out, splitURLs(v)...)
	}
	*l = out
	return nil
}

// splitURLs pulls individual URLs out of pasted text. URLs cannot contain
// whitespace, so splitting on it is safe, and commas are stripped so a
// comma-separated paste works too.
func splitURLs(s string) []string {
	var out []string
	for field := range strings.FieldsSeq(s) {
		field = strings.Trim(field, ",;\"'<>")
		if field != "" {
			out = append(out, field)
		}
	}
	return out
}

// decodeJSON reads a bounded JSON body, reporting failures to the client.
// It returns false when a response has already been written.
func decodeJSON(w http.ResponseWriter, r *http.Request, out any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, config.MaxRequestBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return false
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		writeError(w, http.StatusBadRequest, "invalid request body: expected a single JSON value")
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	// Live state must never be served stale by an intermediary.
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// compressibleJSON is the size above which a reply is worth compressing.
// Below it the header costs more than the saving.
const compressibleJSON = 1 << 10

// writeJSONMaybeCompressed writes a reply, compressing a large one when the
// client asked for that. The body is marshalled up front rather than
// streamed, because whether it is worth compressing is a question about its
// size.
func writeJSONMaybeCompressed(w http.ResponseWriter, r *http.Request, status int, body any) {
	payload, err := json.Marshal(body)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	h := w.Header()
	h.Set("Content-Type", "application/json; charset=utf-8")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Cache-Control", "no-store")

	if len(payload) < compressibleJSON || !acceptsGzip(r) {
		h.Set("Content-Length", strconv.Itoa(len(payload)))
		w.WriteHeader(status)
		_, _ = w.Write(payload)
		return
	}

	h.Set("Content-Encoding", "gzip")
	h.Add("Vary", "Accept-Encoding")
	w.WriteHeader(status)
	gz := gzip.NewWriter(w)
	defer gz.Close()
	_, _ = gz.Write(payload)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
