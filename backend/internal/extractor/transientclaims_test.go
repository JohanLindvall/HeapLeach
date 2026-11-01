// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

func collisionProbeServer(t *testing.T, target string, status int, contentType string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// An unrelated download host has none of the compatible APIs or
		// canonical pages the registry may ask about during recognition.
		if r.URL.RequestURI() != target {
			http.NotFound(w, r)
			return
		}
		calls.Add(1)
		if r.Method != http.MethodGet {
			t.Errorf("unexpected probe method %s", r.Method)
		}
		w.Header().Set(httpx.HeaderContentType, contentType)
		w.Header().Set(httpx.HeaderRetryAfter, "0")
		w.WriteHeader(status)
		fmt.Fprint(w, "Unavailable")
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

func TestSubmittedTransientPathCollisionQueuesDirect(t *testing.T) {
	for _, tc := range []struct {
		path, contentType string
		status            int
	}{
		{"/download-token", "application/octet-stream", http.StatusServiceUnavailable},
		{"/d/synthetic", "application/octet-stream", http.StatusServiceUnavailable},
		{"/v/synthetic", "application/octet-stream", http.StatusServiceUnavailable},
		{"/v/123", "application/octet-stream", http.StatusServiceUnavailable},
		{"/file/synthetic", "application/octet-stream", http.StatusServiceUnavailable},
		{"/d/synthetic", "text/html", http.StatusBadGateway},
		{"/v/synthetic", "text/html", http.StatusRequestTimeout},
		{"/file/synthetic", "application/octet-stream", http.StatusTooManyRequests},
		{"/v/synthetic", "text/html", http.StatusNotImplemented},
	} {
		t.Run(fmt.Sprintf("%s/%d", tc.path, tc.status), func(t *testing.T) {
			target := tc.path + "?sig=a%2Bb&part=2&part=1"
			srv, calls := collisionProbeServer(t, target, tc.status, tc.contentType)
			client := testClient()
			defer client.CloseIdleConnections()
			reg := NewRegistry(&config.Config{}, client)
			link := srv.URL + target
			res, ex, err := reg.Extract(t.Context(), link, Options{})
			if err != nil {
				t.Fatal(err)
			}
			if ex != reg.fallback || len(res.Files) != 1 || res.Files[0].URL != link {
				t.Fatalf("got %+v via %v; want the unchanged submitted URL queued directly", res, ex)
			}
			if got := res.Files[0].Headers[httpx.HeaderReferer]; got != srv.URL+"/" {
				t.Errorf("Referer = %q, want the origin", got)
			}
			want := int32(2)
			if tc.status == http.StatusNotImplemented {
				want = 1 // The probe does not retry this status.
			}
			if got := calls.Load(); got != want {
				t.Errorf("got %d reads of the submitted URL, want only the %d probe attempts", got, want)
			}
		})
	}
}

func TestTransientPathCollisionKeepsFailureBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name, mode, contentType string
		status                  int
	}{
		{"followed source", "strict", "application/octet-stream", http.StatusServiceUnavailable},
		{"nested source", "nested", "application/octet-stream", http.StatusServiceUnavailable},
		{"discovery alias", "discovery", "application/octet-stream", http.StatusServiceUnavailable},
		{"claimed site", "claimed", "text/html", http.StatusServiceUnavailable},
		{"forbidden page", "", "text/html", http.StatusForbidden},
		{"missing page", "", "text/html", http.StatusNotFound},
		{"removed page", "", "text/html", http.StatusGone},
		{"permanent binary refusal", "", "application/octet-stream", http.StatusNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := collisionProbeServer(t, "/v/123", tc.status, tc.contentType)
			client := testClient()
			defer client.CloseIdleConnections()
			reg := NewRegistry(&config.Config{}, client)
			u := mustParse(t, srv.URL+"/v/123")
			var opts Options
			switch tc.mode {
			case "strict":
				opts.strict = true
			case "nested":
				opts.ancestors = []string{"https://source.example.test/collection"}
			case "discovery":
				reg.AliasHosts("vids.st", u.Hostname())
			case "claimed":
				aliasClaims(reg, "vids.st", u.Hostname())
			}
			res, ex, err := reg.Extract(t.Context(), u.String(), opts)
			if res != nil || !httpx.HasStatus(err, tc.status) {
				t.Fatalf("got %+v via %v, %v; want the original %d failure", res, ex, err, tc.status)
			}
		})
	}
}

func TestTransientPathCollisionChecksRedirectDestination(t *testing.T) {
	for _, destination := range []string{"/v/123", "/unavailable"} {
		for _, claim := range []string{"none", "destination", "submitted"} {
			t.Run(fmt.Sprintf("%s/claim=%s", destination, claim), func(t *testing.T) {
				var targets atomic.Int32
				var target string
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if strings.HasPrefix(r.URL.Path, "/api/") {
						http.NotFound(w, r)
						return
					}
					if r.URL.Path == "/d/synthetic" {
						http.Redirect(w, r, target, http.StatusFound)
						return
					}
					if r.URL.Path != destination {
						t.Errorf("unexpected request %s", r.URL)
					}
					targets.Add(1)
					w.Header().Set(httpx.HeaderContentType, "application/octet-stream")
					w.WriteHeader(http.StatusServiceUnavailable)
				}))
				defer srv.Close()
				// Both addresses are loopback, with distinct hostnames so
				// either end of the redirect can stand in for a known site.
				target = strings.Replace(srv.URL, "127.0.0.1", "localhost", 1) + destination
				client := testClient()
				defer client.CloseIdleConnections()
				reg := NewRegistry(&config.Config{}, client)
				switch claim {
				case "destination":
					aliasClaims(reg, "vids.st", "localhost")
				case "submitted":
					// This broadcaster claims its non-item pages last,
					// after recognition and the generic fallback.
					aliasClaims(reg, "svtplay", mustParse(t, srv.URL).Hostname())
				}
				link := srv.URL + "/d/synthetic"
				res, _, err := reg.Extract(t.Context(), link, Options{})
				if claim != "none" {
					if res != nil || !httpx.HasStatus(err, http.StatusServiceUnavailable) {
						t.Fatalf("got %+v, %v; want the known site's failure", res, err)
					}
				} else if err != nil || res == nil || len(res.Files) != 1 || res.Files[0].URL != link {
					t.Fatalf("got %+v, %v; want the original submitted URL queued", res, err)
				}
				if got := targets.Load(); got != 2 {
					t.Errorf("target read %d times, want two probe attempts", got)
				}
			})
		}
	}
}

// Force the fallback decision without asking a remote service API. The
// embedded extractor retains its real name and URL shape.
type unidentifiedProbeSite struct{ Extractor }

func (unidentifiedProbeSite) Recognise(context.Context, *Page) bool { return false }

func TestTransientPathCollisionKeepsServiceSpecificForms(t *testing.T) {
	for _, manifest := range []bool{false, true} {
		t.Run(fmt.Sprintf("manifest=%t", manifest), func(t *testing.T) {
			path := "/d/00000000-0000-4000-8000-000000000001"
			if manifest {
				path = "/master.m3u8"
			}
			srv, _ := collisionProbeServer(t, path, http.StatusServiceUnavailable, "application/octet-stream")
			client := testClient()
			defer client.CloseIdleConnections()
			reg := NewRegistry(&config.Config{}, client)
			var site Extractor = NewGofile(&config.Config{}, client)
			if manifest {
				site = NewHLSDirect(client)
			}
			reg.extractors = []Extractor{unidentifiedProbeSite{site}}
			res, _, err := reg.Extract(t.Context(), srv.URL+path, Options{})
			if res != nil || !httpx.HasStatus(err, http.StatusServiceUnavailable) {
				t.Fatalf("got %+v, %v; want the failure of a form valid on this host", res, err)
			}
		})
	}
}

func TestTransientPathCollisionWithoutResponse(t *testing.T) {
	for _, timeout := range []bool{false, true} {
		t.Run(fmt.Sprintf("timeout=%t", timeout), func(t *testing.T) {
			release := make(chan struct{})
			srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-release }))
			defer srv.Close()
			defer close(release)
			if !timeout {
				srv.Close()
			}
			client := httpx.New("test-agent", "en-US", 0, 100*time.Millisecond)
			defer client.CloseIdleConnections()
			reg := NewRegistry(&config.Config{}, client)
			link := srv.URL + "/v/synthetic"
			res, _, err := reg.Extract(t.Context(), link, Options{})
			if err != nil || res == nil || len(res.Files) != 1 || res.Files[0].URL != link {
				t.Fatalf("got %+v, %v; want the file queued after the transient transport failure", res, err)
			}
		})
	}
}

func TestTransientPathCollisionPreservesCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		cancel()
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	client := testClient()
	defer client.CloseIdleConnections()
	res, _, err := NewRegistry(&config.Config{}, client).Extract(ctx, srv.URL+"/v/synthetic", Options{})
	if res != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("got %+v, %v; want cancellation", res, err)
	}
}

func TestTransientPathCollisionChecksFailedRedirectTransport(t *testing.T) {
	for _, claimed := range []bool{false, true} {
		t.Run(fmt.Sprintf("claimed=%t", claimed), func(t *testing.T) {
			unavailable := httptest.NewServer(http.NotFoundHandler())
			target := strings.Replace(unavailable.URL, "127.0.0.1", "localhost", 1) + "/v/123"
			unavailable.Close()
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/d/synthetic" {
					http.NotFound(w, r)
					return
				}
				calls.Add(1)
				http.Redirect(w, r, target, http.StatusFound)
			}))
			defer srv.Close()
			client := testClient()
			defer client.CloseIdleConnections()
			reg := NewRegistry(&config.Config{}, client)
			if claimed {
				aliasClaims(reg, "vids.st", "localhost")
			}
			link := srv.URL + "/d/synthetic"
			res, _, err := reg.Extract(t.Context(), link, Options{})
			if claimed {
				if res != nil || err == nil {
					t.Fatalf("got %+v, %v; want the known destination's transport failure", res, err)
				}
			} else if err != nil || res == nil || len(res.Files) != 1 || res.Files[0].URL != link {
				t.Fatalf("got %+v, %v; want the original submitted URL queued", res, err)
			}
			if got := calls.Load(); got != 2 {
				t.Errorf("got %d reads of the redirect, want two probe attempts", got)
			}
		})
	}
}
