// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

// A successful preview must not prevent a request for the fallback's own
// representation. Vary on a redirect is as significant as on its destination.
func TestDirectHonorsOriginRefererRepresentations(t *testing.T) {
	for _, tc := range []struct {
		name, vary string
		redirect   bool
		refusal    int
	}{
		{name: "referer", vary: "Referer"},
		{name: "wildcard", vary: "*"},
		{name: "mixed case", vary: "Accept-Language, rEfErEr"},
		{name: "redirect variant", vary: "Referer", redirect: true},
		{name: "already invited", vary: "Referer", refusal: http.StatusForbidden},
		{name: "navigation not found", vary: "Referer", refusal: http.StatusNotFound},
		{name: "navigation gone", vary: "Referer", refusal: http.StatusGone},
		{name: "refusal wildcard", vary: "*", refusal: http.StatusNotFound},
		{name: "refusal mixed case", vary: "Accept-Language, rEfErEr", refusal: http.StatusGone},
		{name: "refusal redirect variant", vary: "Referer", redirect: true, refusal: http.StatusNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var pages, invited, entries, mediaReads atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/generic/entry" && tc.redirect {
					entries.Add(1)
					w.Header().Set(httpx.HeaderVary, tc.vary)
					http.Redirect(w, r, "/landing", http.StatusFound)
					return
				}
				if r.URL.Path != "/landing" {
					mediaReads.Add(1)
					http.NotFound(w, r)
					return
				}
				pages.Add(1)
				w.Header().Set(httpx.HeaderContentType, "text/html")
				if !tc.redirect {
					w.Header().Set(httpx.HeaderVary, tc.vary)
				}
				file := "preview.mp4"
				if r.Referer() == "http://"+r.Host+"/" {
					invited.Add(1)
					file = "full-recording.mp4"
				} else if tc.refusal != 0 {
					w.WriteHeader(tc.refusal)
				}
				fmt.Fprintf(w, `<html><title>Example Recording</title><video src="/media/%s"></video></html>`, file)
			}))
			t.Cleanup(srv.Close)
			client := testClient()
			t.Cleanup(client.CloseIdleConnections)
			source := srv.URL + "/landing"
			if tc.redirect {
				source = srv.URL + "/generic/entry"
			}
			res, _, err := NewRegistry(&config.Config{}, client).Extract(t.Context(), source, Options{})
			if err != nil || res == nil || len(res.Files) != 1 || res.Files[0].URL != srv.URL+"/media/full-recording.mp4" {
				t.Fatalf("result=%+v error=%v; want the origin-Referer recording", res, err)
			}
			if pages.Load() != 2 || invited.Load() != 1 || mediaReads.Load() != 0 || tc.redirect && entries.Load() != 2 {
				t.Fatalf("pages=%d invited=%d entries=%d media=%d; want one navigation and one origin-Referer acquisition", pages.Load(), invited.Load(), entries.Load(), mediaReads.Load())
			}
		})
	}
}

func TestDirectRefererRefusalKeepsFailureBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name, vary, mode           string
		navigation, reader, status int
		reads, invited             int32
	}{
		{"no variant", "", "", 404, 200, 404, 1, 0},
		{"gone without variant", "", "", 410, 200, 410, 1, 0},
		{"unrelated variant", "Accept-Language", "", 410, 200, 410, 1, 0},
		{"strict source", "Referer", "strict", 404, 200, 404, 1, 0},
		{"nested source", "Referer", "nested", 410, 200, 410, 1, 0},
		{"claimed source", "Referer", "claimed", 404, 200, 404, 1, 0},
		{"claimed destination", "Referer", "destination", 410, 200, 410, 1, 0},
		{"already invited", "Referer", "", 403, 404, 404, 2, 1},
		{"reader not found", "Referer", "", 404, 404, 404, 2, 1},
		{"reader gone", "Referer", "", 410, 410, 410, 2, 1},
		{"reader unavailable", "Referer", "", 404, 503, 503, 3, 2},
		{"reader canceled", "Referer", "cancel", 404, 200, 0, 2, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var reads, invited atomic.Int32
			var srv *httptest.Server
			srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/generic/entry" {
					w.Header().Set(httpx.HeaderVary, tc.vary)
					http.Redirect(w, r, strings.Replace(srv.URL, "127.0.0.1", "localhost", 1)+"/landing", http.StatusFound)
					return
				}
				if r.URL.Path != "/landing" {
					http.NotFound(w, r)
					return
				}
				reads.Add(1)
				w.Header().Set(httpx.HeaderContentType, "text/html")
				w.Header().Set(httpx.HeaderVary, tc.vary)
				status := tc.navigation
				if r.Referer() == srv.URL+"/" {
					invited.Add(1)
					status = tc.reader
					if tc.mode == "cancel" {
						cancel()
					}
				}
				w.WriteHeader(status)
				fmt.Fprint(w, `<html><video src="/media/full-recording.mp4"></video></html>`)
			}))
			t.Cleanup(srv.Close)
			client := testClient()
			t.Cleanup(client.CloseIdleConnections)
			reg := NewRegistry(&config.Config{}, client)
			source := srv.URL + "/landing"
			var opts Options
			switch tc.mode {
			case "strict":
				opts.strict = true
			case "nested":
				opts.ancestors = []string{"https://source.example.test/collection"}
			case "claimed":
				aliasClaims(reg, "svtplay", mustParse(t, srv.URL).Hostname())
			case "destination":
				aliasClaims(reg, "svtplay", "localhost")
				source = srv.URL + "/generic/entry"
			}
			res, _, err := reg.Extract(ctx, source, opts)
			if res != nil || tc.status != 0 && !httpx.HasStatus(err, tc.status) || tc.mode == "cancel" && !errors.Is(err, context.Canceled) {
				t.Errorf("result=%+v error=%v; want the reader's failure", res, err)
			}
			if reads.Load() != tc.reads || invited.Load() != tc.invited {
				t.Errorf("reads=%d invited=%d; want %d and %d", reads.Load(), invited.Load(), tc.reads, tc.invited)
			}
		})
	}
}

func TestDirectRefererRefusalRecognisesTheReaderResponse(t *testing.T) {
	var reads atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/landing/" {
			// Other recognisers may check a separate canonical path.
			http.NotFound(w, r)
			return
		}
		reads.Add(1)
		w.Header().Set(httpx.HeaderContentType, "text/html")
		w.Header().Set(httpx.HeaderVary, httpx.HeaderReferer)
		if r.Referer() != "http://"+r.Host+"/" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		fmt.Fprint(w, `<html><title>Index of /landing/</title><h1>Index of /landing/</h1><pre><a href="recording.mp4">recording.mp4</a> 02-Jan-2024 03:04 12345</pre></html>`)
	}))
	t.Cleanup(srv.Close)
	client := testClient()
	t.Cleanup(client.CloseIdleConnections)
	res, ex, err := NewRegistry(&config.Config{}, client).Extract(t.Context(), srv.URL+"/landing/", Options{})
	if err != nil || res == nil || len(res.Files) != 1 || res.Files[0].URL != srv.URL+"/landing/recording.mp4" || ex.Name() != "autoindex" {
		t.Fatalf("result=%+v extractor=%v error=%v; want the recognised listing", res, ex, err)
	}
	if reads.Load() != 2 {
		t.Errorf("read %d pages; want one navigation and one reader request", reads.Load())
	}
}

func TestDirectRefererRefusalRetainsSourceSessionAndRoute(t *testing.T) {
	var directReads, routedReads atomic.Int32
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		directReads.Add(1)
		http.Error(w, "request lost its route", http.StatusForbidden)
	}))
	t.Cleanup(origin.Close)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/landing" {
			t.Errorf("unexpected routed request %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		if r.UserAgent() != "fixture-route-agent" {
			t.Errorf("request lost its route agent: %q", r.UserAgent())
		}
		if cookie, err := r.Cookie("source"); err != nil || cookie.Value != "source-session" {
			t.Error("request lost its source cookie jar")
		}
		w.Header().Set(httpx.HeaderContentType, "text/html")
		w.Header().Set(httpx.HeaderVary, httpx.HeaderReferer)
		if routedReads.Add(1) == 1 {
			http.SetCookie(w, &http.Cookie{Name: "gate", Value: "established", Path: "/"})
			w.WriteHeader(http.StatusGone)
			return
		}
		if r.Referer() != origin.URL+"/" {
			t.Errorf("retry Referer = %q, want the submitted origin", r.Referer())
		}
		if cookie, err := r.Cookie("gate"); err != nil || cookie.Value != "established" {
			t.Error("retry lost the cookie established by navigation")
		}
		fmt.Fprint(w, `<html><video src="/media/recording.mp4"></video></html>`)
	}))
	t.Cleanup(proxy.Close)
	client := testClient()
	t.Cleanup(client.CloseIdleConnections)
	route, err := client.ThroughProxy(proxy.URL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(route.CloseIdleConnections)
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	u := mustParse(t, origin.URL+"/landing")
	jar.SetCookies(u, []*http.Cookie{{Name: "source", Value: "source-session", Path: "/"}})
	ctx := httpx.WithCookieJar(httpx.WithRouteAgent(t.Context(), "fixture-route", route, nil, "fixture-route-agent"), jar)
	res, _, err := NewRegistry(&config.Config{}, client).Extract(ctx, u.String(), Options{})
	if err != nil || res == nil || len(res.Files) != 1 || res.Files[0].URL != origin.URL+"/media/recording.mp4" {
		t.Fatalf("result=%+v error=%v; want the reader's recording", res, err)
	}
	if directReads.Load() != 0 || routedReads.Load() != 2 {
		t.Errorf("direct=%d routed=%d; want exactly two routed requests", directReads.Load(), routedReads.Load())
	}
}

// A page that declares no Referer variant can be single-use. Its successful
// navigation response, including redirects, remains sufficient for extraction.
func TestDirectKeepsSingleUseNavigationMedia(t *testing.T) {
	for _, redirect := range []bool{false, true} {
		t.Run(fmt.Sprintf("redirect=%t", redirect), func(t *testing.T) {
			var pages, entries atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/generic/entry" && redirect {
					entries.Add(1)
					http.Redirect(w, r, "/landing", http.StatusFound)
					return
				}
				if r.URL.Path != "/landing" || pages.Add(1) != 1 {
					http.Error(w, "single-use page already spent", http.StatusGone)
					return
				}
				w.Header().Set(httpx.HeaderContentType, "text/html")
				fmt.Fprint(w, `<html><video src="https://media.example.test/full-recording.mp4"></video></html>`)
			}))
			t.Cleanup(srv.Close)
			client := testClient()
			t.Cleanup(client.CloseIdleConnections)
			source := srv.URL + "/landing"
			if redirect {
				source = srv.URL + "/generic/entry"
			}
			res, _, err := NewRegistry(&config.Config{}, client).Extract(t.Context(), source, Options{})
			if err != nil || res == nil || len(res.Files) != 1 || res.Files[0].URL != "https://media.example.test/full-recording.mp4" || pages.Load() != 1 || redirect && entries.Load() != 1 {
				t.Fatalf("result=%+v error=%v pages=%d entries=%d; navigation response must remain reusable", res, err, pages.Load(), entries.Load())
			}
		})
	}
}

func TestDirectRefusesTheWrongRefererPreviewWhenItsOwnReadFails(t *testing.T) {
	var pages atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pages.Add(1)
		w.Header().Set(httpx.HeaderContentType, "text/html")
		w.Header().Set(httpx.HeaderVary, httpx.HeaderReferer)
		if r.Referer() != "" {
			http.Error(w, "recording unavailable", http.StatusNotFound)
			return
		}
		fmt.Fprint(w, `<html><video src="https://media.example.test/preview.mp4"></video></html>`)
	}))
	t.Cleanup(srv.Close)
	client := testClient()
	t.Cleanup(client.CloseIdleConnections)
	res, _, err := NewRegistry(&config.Config{}, client).Extract(t.Context(), srv.URL+"/landing", Options{})
	if res != nil || !errors.Is(err, errPlainPage) || pages.Load() != 2 {
		t.Fatalf("result=%+v error=%v reads=%d; the wrong preview must not become the fallback", res, err, pages.Load())
	}
}
