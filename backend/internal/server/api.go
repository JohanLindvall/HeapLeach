// SPDX-License-Identifier: MIT

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
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

// handleHealth is a liveness probe for container orchestration.
func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	// The version lets a deploy tool confirm that the process answering is
	// the build it just started, not the one it replaced.
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "version": s.cfg.Version})
}

// handleState returns the current snapshot, for the initial page load and
// as a fallback when EventSource is unavailable.
func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	// The one response here big enough to be worth compressing, and the
	// one a browser asks for repeatedly: the polling fallback fetches it
	// every couple of seconds, and on a long queue it is most of a
	// megabyte of very repetitive JSON.
	writeJSONMaybeCompressed(w, r, http.StatusOK, s.mgr.Snapshot())
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
	var req download.Settings
	if !decodeJSON(w, r, &req) {
		return
	}
	if err := s.mgr.ApplySettings(req); err != nil {
		writeManagerError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.mgr.Snapshot())
}

// handleClear forgets every finished job.
func (s *Server) handleClear(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]int{"removed": s.mgr.ClearFinished()})
}

// handleJobLimits replaces a job's own limits; a field left out has none.
func (s *Server) handleJobLimits(w http.ResponseWriter, r *http.Request) {
	var req download.JobLimit
	if !decodeJSON(w, r, &req) {
		return
	}
	writeDone(w, s.mgr.SetJobLimit(r.PathValue("jobID"), req))
}

// jobAction applies a manager method to the job named in the path.
func jobAction(fn func(string) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeDone(w, fn(r.PathValue("jobID")))
	}
}

// itemAction applies a manager method to the item named in the path.
func itemAction(fn func(string, string) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeDone(w, fn(r.PathValue("jobID"), r.PathValue("itemID")))
	}
}

// writeDone answers an action that returns nothing but whether it worked.
func writeDone(w http.ResponseWriter, err error) {
	if err != nil {
		writeManagerError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func writeManagerError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, download.ErrClosed):
		writeError(w, http.StatusServiceUnavailable, err.Error())
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
	err := dec.Decode(out)
	problem := "invalid request body: "
	if err == nil {
		// Anything after the value, a second one included, is refused.
		if err = dec.Decode(new(any)); err == io.EOF {
			return true
		}
		problem += "expected a single JSON value"
	} else {
		problem += err.Error()
	}
	if _, tooLarge := errors.AsType[*http.MaxBytesError](err); tooLarge {
		writeError(w, http.StatusRequestEntityTooLarge, "request body is too large")
	} else {
		writeError(w, http.StatusBadRequest, problem)
	}
	return false
}

// jsonHeaders marks a reply as JSON of the moment: live state must never be
// served stale by an intermediary.
func jsonHeaders(h http.Header) {
	h.Set(httpx.HeaderContentType, "application/json; charset=utf-8")
	h.Set(httpx.HeaderContentTypeOptions, "nosniff")
	h.Set(httpx.HeaderCacheControl, "no-store")
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	jsonHeaders(w.Header())
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
	jsonHeaders(h)
	h.Add(httpx.HeaderVary, httpx.HeaderAcceptEncoding)

	if len(payload) < compressibleJSON || !acceptsGzip(r) {
		h.Set(httpx.HeaderContentLength, strconv.Itoa(len(payload)))
		w.WriteHeader(status)
		_, _ = w.Write(payload)
		return
	}

	h.Set(httpx.HeaderContentEncoding, "gzip")
	w.WriteHeader(status)
	gz := gzip.NewWriter(w)
	defer gz.Close()
	_, _ = gz.Write(payload)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
