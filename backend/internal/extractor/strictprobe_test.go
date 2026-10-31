// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

// A followed link is navigation only when its page was actually read. Failed
// probes must keep the typed error that source retries and route recovery use.
func TestStrictProbePreservesFailures(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		short  bool
	}{
		{"server-error", http.StatusServiceUnavailable, false},
		{"forbidden", http.StatusForbidden, false},
		{"not-found", http.StatusNotFound, false},
		{"gone", http.StatusGone, false},
		{"interrupted-body", http.StatusOK, true},
		{"unrecognised-page", http.StatusOK, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set(httpx.HeaderContentType, "text/html")
				w.Header().Set("X-Test-Reason", "synthetic refusal")
				if tc.short {
					w.Header().Set("Content-Length", "4096")
				}
				w.WriteHeader(tc.status)
				fmt.Fprint(w, "<html>Unavailable</html>")
			}))
			defer srv.Close()
			client := testClient()
			defer client.CloseIdleConnections()
			reg := &Registry{client: client, extractors: []Extractor{NewDoodStream(client)}, fallback: NewDirect(client)}
			_, _, err := reg.follow(t.Context(), srv.URL+"/d/synthetic", Options{})
			switch {
			case tc.short:
				if !errors.Is(err, io.ErrUnexpectedEOF) {
					t.Fatalf("wanted the body read failure, got %v", err)
				}
			case tc.status >= http.StatusBadRequest && tc.status != http.StatusNotFound && tc.status != http.StatusGone:
				status, ok := errors.AsType[*httpx.StatusError](err)
				if !ok || status.Code != tc.status || status.Header.Get("X-Test-Reason") != "synthetic refusal" {
					t.Fatalf("wanted HTTP %d and its headers, got %v", tc.status, err)
				}
			case !errors.Is(err, errUnrecognised):
				t.Fatalf("unsupported page: %v", err)
			}
		})
	}
}

func TestStrictProbeBoundsHTTPErrorBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(httpx.HeaderContentType, "text/html")
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, strings.Repeat("x", 2*config.ErrorBodySample))
	}))
	defer srv.Close()
	client := testClient()
	defer client.CloseIdleConnections()
	reg := &Registry{client: client, fallback: NewDirect(client)}
	_, _, err := reg.follow(t.Context(), srv.URL+"/d/synthetic", Options{})
	status, ok := errors.AsType[*httpx.StatusError](err)
	if !ok || status.Code != http.StatusForbidden || len(status.Body) != config.ErrorBodySample {
		t.Fatalf("wanted a bounded status error, got %v", err)
	}
}

func TestStrictProbeRetainsFailureDuringChallengeRecovery(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(httpx.HeaderContentType, "text/html")
		fmt.Fprint(w, doodGateFixture)
	}))
	defer origin.Close()
	client := testClient()
	defer client.CloseIdleConnections()
	route := func(id string, handler http.HandlerFunc) context.Context {
		proxy := httptest.NewServer(handler)
		t.Cleanup(proxy.Close)
		routed, err := client.ThroughProxy(proxy.URL)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(routed.CloseIdleConnections)
		return httpx.WithRoute(t.Context(), id, routed, nil)
	}
	failed := route("failed", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "temporarily unavailable", http.StatusServiceUnavailable)
	})
	working := route("working", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(httpx.HeaderContentType, "text/html")
		if strings.HasPrefix(r.URL.Path, "/pass_md5/") {
			fmt.Fprint(w, "https://cdn.example.test/media/")
			return
		}
		fmt.Fprint(w, `<html><title>Invented Clip</title><script>fetch('/pass_md5/session/synthetic')</script></html>`)
	})
	player := NewDoodStream(client)
	reg := &Registry{client: client, extractors: []Extractor{player}, fallback: NewDirect(client)}
	var recovered bool
	ctx := WithChallengeRecovery(t.Context(), func(_ context.Context, read func(context.Context) (*Result, error), cause error) (*Result, error) {
		if _, ok := errors.AsType[*httpx.ChallengeError](cause); !ok {
			t.Fatalf("expected a player challenge, got %v", cause)
		}
		if _, err := read(failed); !httpx.HasStatus(err, http.StatusServiceUnavailable) {
			t.Fatalf("recovery lost the failed route's response: %v", err)
		}
		recovered = true
		return read(working)
	})
	res, ex, err := reg.follow(ctx, origin.URL+"/d/synthetic", Options{})
	if err != nil || !recovered || ex != player || len(res.Files) != 1 || res.Files[0].Refresh == nil {
		t.Fatalf("recovered=%v extractor=%v result=%+v err=%v", recovered, ex, res, err)
	}
}
