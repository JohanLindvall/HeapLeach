// SPDX-License-Identifier: MIT

package server

import (
	"net/http"
	"strconv"

	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	w.Header().Set(httpx.HeaderCacheControl, "no-store")
	if s.logs == nil {
		writeError(w, http.StatusServiceUnavailable, "Service logs are unavailable.")
		return
	}
	var after uint64
	if raw := r.URL.Query().Get("after"); raw != "" {
		var err error
		after, err = strconv.ParseUint(raw, 10, 64)
		if err != nil {
			writeError(w, http.StatusBadRequest, "Invalid log cursor.")
			return
		}
	}
	writeJSONMaybeCompressed(w, r, http.StatusOK, s.logs.Snapshot(r.URL.Query().Get("session"), after))
}
