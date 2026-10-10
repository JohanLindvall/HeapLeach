// SPDX-License-Identifier: MIT

package server

import (
	"net/http"
	"slices"
	"strconv"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/proxy"
)

func (s *Server) handleGetSettings(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, s.mgr.CurrentSettings())
}

func (s *Server) handleProxies(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	values := r.URL.Query()
	q := proxy.Query{Search: values.Get("search"), Status: values.Get("status"), Sort: values.Get("sort")}
	for key, dst := range map[string]*int{"offset": &q.Offset, "limit": &q.Limit} {
		if raw := values.Get(key); raw != "" {
			n, err := strconv.Atoi(raw)
			if err != nil || n < 0 || (key == "limit" && (n == 0 || n > config.ProxyMaxPageSize)) {
				writeError(w, http.StatusBadRequest, "invalid proxy list "+key)
				return
			}
			*dst = n
		}
	}
	if !slices.Contains([]string{"", "all", "available", "active", "busy", "cooling", "untested", "finishing"}, q.Status) ||
		!slices.Contains([]string{"", "score", "throughput", "success", "address"}, q.Sort) {
		writeError(w, http.StatusBadRequest, "invalid proxy list filter or sort")
		return
	}
	writeJSON(w, http.StatusOK, s.mgr.ProxyPage(q))
}
