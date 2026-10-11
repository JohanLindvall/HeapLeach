// SPDX-License-Identifier: MIT

package server

import (
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/JohanLindvall/HeapLeach/internal/logstore"
)

func TestLogsExposeHistoryAndDeltasWithoutLoggingTheirOwnReads(t *testing.T) {
	_, server := newTestParts(t)
	server.log.Debug("player response", "job", "test-job", "status", 200)
	handler := server.Handler()
	read := func(path string, compressed bool) logstore.Page {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		if compressed {
			req.Header.Set("Accept-Encoding", "gzip")
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code != http.StatusOK || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("status=%d headers=%v", w.Code, w.Header())
		}
		var body io.Reader = w.Body
		if w.Header().Get("Content-Encoding") == "gzip" {
			gz, err := gzip.NewReader(body)
			if err != nil {
				t.Fatal(err)
			}
			defer gz.Close()
			body = gz
		}
		var page logstore.Page
		if err := json.NewDecoder(body).Decode(&page); err != nil {
			t.Fatal(err)
		}
		return page
	}
	initial := read("/api/logs", true)
	if len(initial.Entries) != 1 || !strings.Contains(initial.Entries[0].Text, "job=test-job") || initial.Entries[0].Level != "DEBUG" {
		t.Fatalf("history=%+v", initial)
	}
	path := fmt.Sprintf("/api/logs?session=%s&after=%d", initial.Session, initial.Next)
	for range 3 {
		if got := read(path, false); got.Next != initial.Next || got.Reset || len(got.Entries) != 0 {
			t.Fatalf("log reads generated more messages: %+v", got)
		}
	}
	server.log.Warn("player challenged")
	if got := read(path, true); got.Reset || len(got.Entries) != 1 || got.Entries[0].Level != "WARN" {
		t.Fatalf("delta=%+v", got)
	}
}

func TestLogsRejectInvalidCursorsAndStayUnderAPIRouting(t *testing.T) {
	_, server := newTestParts(t)
	handler := server.Handler()
	for _, tc := range []struct {
		method, path string
		status       int
	}{
		{"GET", "/api/logs?after=-1", 400},
		{"GET", "/api/logs?after=bad", 400},
		{"GET", "/api/logs?after=18446744073709551616", 400},
		{"POST", "/api/logs", 405},
	} {
		t.Run(tc.method+tc.path, func(t *testing.T) {
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, httptest.NewRequest(tc.method, tc.path, nil))
			if w.Code != tc.status {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
		})
	}
}
