// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

// Some hosts answer an uninvited navigation with a successful but empty
// page. The fallback may ask that page once with its origin as Referer,
// while useful answers and a probe already retried with that Referer stand.
func TestDirectRetriesAnEmptyProbeWithTheOriginsReferer(t *testing.T) {
	const empty = `<html><title>Open from the site</title></html>`
	const playable = `<html><title>Example clip</title><video src="/media/example.mp4"></video></html>`
	for _, tc := range []struct {
		name                    string
		firstStatus, nextStatus int
		firstType, nextType     string
		firstBody, nextBody     string
		wantPath                string
		requests                int32
	}{
		{"successful empty gate", 200, 200, "text/html", "text/html", empty, playable, "/media/example.mp4", 2},
		{"useful page", 200, 200, "text/html", "text/html", playable, playable, "/media/example.mp4", 1},
		{"media response", 200, 200, "video/mp4", "video/mp4", "media", "media", "/feature", 1},
		{"still empty", 200, 200, "text/html", "text/html", empty, empty, "", 2},
		{"unauthorized then empty", 401, 200, "text/html", "text/html", empty, empty, "", 2},
		{"forbidden then empty", 403, 200, "text/html", "text/html", empty, empty, "", 2},
		{"retry becomes media", 200, 200, "text/html", "video/mp4", empty, "media", "/feature", 2},
		{"retry is refused", 200, 404, "text/html", "text/html", empty, empty, "", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var reads, mediaReads atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/feature" {
					mediaReads.Add(1)
					http.NotFound(w, r)
					return
				}
				if r.Method != http.MethodGet {
					t.Errorf("page request method = %s, want GET", r.Method)
				}
				status, kind, body := tc.firstStatus, tc.firstType, tc.firstBody
				if reads.Add(1) == 1 {
					if r.Referer() != "" {
						t.Errorf("initial probe Referer = %q, want none", r.Referer())
					}
				} else {
					want := "http://" + r.Host + "/"
					if r.Referer() != want {
						t.Errorf("retry Referer = %q, want %q", r.Referer(), want)
					}
					status, kind, body = tc.nextStatus, tc.nextType, tc.nextBody
				}
				w.Header().Set(httpx.HeaderContentType, kind)
				w.WriteHeader(status)
				_, _ = io.WriteString(w, body)
			}))
			t.Cleanup(srv.Close)
			client := testClient()
			t.Cleanup(client.CloseIdleConnections)
			reg := markedRegistry(client, markedSite{})
			res, ex, err := reg.Extract(t.Context(), srv.URL+"/feature", Options{})
			if ex != reg.fallback {
				t.Errorf("extractor = %v, want the generic fallback", ex)
			}
			if tc.wantPath == "" {
				if res != nil || !errors.Is(err, errPlainPage) {
					t.Errorf("got %+v, %v; want the original empty-page refusal", res, err)
				}
			} else if err != nil || res == nil || len(res.Files) != 1 || res.Files[0].URL != srv.URL+tc.wantPath {
				t.Errorf("got %+v, %v; want %s", res, err, srv.URL+tc.wantPath)
			} else if tc.name == "retry becomes media" {
				if res.Files[0].Headers[httpx.HeaderReferer] != srv.URL+"/" {
					t.Errorf("media lost the working Referer: %v", res.Files[0].Headers)
				}
				for name, value := range httpx.Navigate {
					if res.Files[0].Headers[name] != value {
						t.Errorf("media lost request header %s=%q: %v", name, value, res.Files[0].Headers)
					}
				}
			}
			if got := reads.Load(); got != tc.requests {
				t.Errorf("page GETs = %d, want %d", got, tc.requests)
			}
			if got := mediaReads.Load(); got != 0 {
				t.Errorf("media requests = %d, want none during extraction", got)
			}
		})
	}
}

// A prefix known to be incomplete is not a successful empty gate, and a
// cancelled extraction must not spend a further request checking a Referer.
func TestDirectDoesNotRetryAnOversizedOrCancelledEmptyProbe(t *testing.T) {
	for _, oversized := range []bool{true, false} {
		name := "cancelled"
		if oversized {
			name = "oversized"
		}
		t.Run(name, func(t *testing.T) {
			var reads atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				reads.Add(1)
				w.Header().Set(httpx.HeaderContentType, "text/html")
				_, _ = io.WriteString(w, `<html><video src="/media/example.mp4"></video></html>`)
			}))
			t.Cleanup(srv.Close)
			u := mustParse(t, srv.URL+"/feature")
			p := &Page{
				URL: u, Final: u, Status: http.StatusOK,
				Header: http.Header{httpx.HeaderContentType: {"text/html"}},
				Body:   `<html><title>An empty page</title></html>`,
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			wantErr := errPlainPage
			if oversized {
				p.Err = httpx.ErrTooLarge
			} else {
				cancel()
				wantErr = context.Canceled
			}
			res, err := NewDirect(testClient()).Extract(withPage(ctx, p), u, Options{})
			if res != nil || !errors.Is(err, wantErr) || reads.Load() != 0 {
				t.Errorf("got %+v, %v after %d requests; want %v without another request", res, err, reads.Load(), wantErr)
			}
		})
	}
}

// Trying another Referer keeps the same source session and selected route,
// including cookies the gate established and the route's browser identity.
func TestDirectRefererRetryRetainsSourceSessionAndRoute(t *testing.T) {
	t.Run("HTML player", func(t *testing.T) { testDirectRefererRetryRoute(t, false) })
	t.Run("media response", func(t *testing.T) { testDirectRefererRetryRoute(t, true) })
}

func testDirectRefererRetryRoute(t *testing.T, media bool) {
	t.Helper()
	var directReads, routeReads, mediaReads atomic.Int32
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		directReads.Add(1)
		http.Error(w, "request lost its route", http.StatusForbidden)
	}))
	t.Cleanup(origin.Close)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/feature" {
			mediaReads.Add(1)
			http.NotFound(w, r)
			return
		}
		if r.UserAgent() != "fixture-route-agent" {
			t.Errorf("User-Agent = %q, want the route's agent", r.UserAgent())
		}
		if cookie, err := r.Cookie("source"); err != nil || cookie.Value != "source-session" {
			t.Error("request lost its source cookie jar")
		}
		w.Header().Set(httpx.HeaderContentType, "text/html")
		if routeReads.Add(1) == 1 {
			if r.Referer() != "" {
				t.Errorf("initial Referer = %q, want none", r.Referer())
			}
			http.SetCookie(w, &http.Cookie{Name: "gate", Value: "established", Path: "/"})
			_, _ = io.WriteString(w, `<html><title>Open from the site</title></html>`)
			return
		}
		if r.Referer() != origin.URL+"/" {
			t.Errorf("retry Referer = %q, want the origin", r.Referer())
		}
		if cookie, err := r.Cookie("gate"); err != nil || cookie.Value != "established" {
			t.Error("retry lost the cookie established by the gate")
		}
		if media {
			w.Header().Set(httpx.HeaderContentType, "video/mp4")
			return
		}
		_, _ = io.WriteString(w, `<html><video src="/media/example.mp4"></video></html>`)
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
	u := mustParse(t, origin.URL+"/feature")
	jar.SetCookies(u, []*http.Cookie{{Name: "source", Value: "source-session", Path: "/"}})
	ctx := httpx.WithCookieJar(httpx.WithRouteAgent(t.Context(), "fixture-route", route, nil, "fixture-route-agent"), jar)
	res, _, err := markedRegistry(client, markedSite{}).Extract(ctx, u.String(), Options{})
	want := origin.URL + "/media/example.mp4"
	if media {
		want = u.String()
	}
	if err != nil || res == nil || len(res.Files) != 1 || res.Files[0].URL != want {
		t.Errorf("got %+v, %v; want the gated media", res, err)
	} else if media {
		if res.Files[0].Headers[httpx.HeaderReferer] != origin.URL+"/" ||
			res.Files[0].Headers[httpx.HeaderCookie] != "" || res.Files[0].Headers[httpx.HeaderUserAgent] != "" {
			t.Errorf("file headers lost the Referer or froze session/route identity: %v", res.Files[0].Headers)
		}
	}
	if routeReads.Load() != 2 || directReads.Load() != 0 || mediaReads.Load() != 0 {
		t.Errorf("requests: routed=%d direct=%d media=%d; want two routed page reads only", routeReads.Load(), directReads.Load(), mediaReads.Load())
	}
}

// Cancellation while the extra request is outstanding remains cancellation,
// rather than becoming the empty-page error from the earlier response.
func TestDirectRefererRetryPreservesCancellation(t *testing.T) {
	var reads atomic.Int32
	retrying := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := reads.Add(1)
		if n == 1 {
			w.Header().Set(httpx.HeaderContentType, "text/html")
			_, _ = io.WriteString(w, `<html><title>Open from the site</title></html>`)
			return
		}
		if n == 2 {
			close(retrying)
		}
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	client := testClient()
	t.Cleanup(client.CloseIdleConnections)
	type answer struct {
		res *Result
		err error
	}
	done := make(chan answer, 1)
	go func() {
		res, _, err := markedRegistry(client, markedSite{}).Extract(ctx, srv.URL+"/feature", Options{})
		done <- answer{res, err}
	}()
	select {
	case <-retrying:
	case got := <-done:
		t.Fatalf("extraction completed before retrying: %+v, %v", got.res, got.err)
	case <-time.After(5 * time.Second):
		t.Fatal("retry did not reach the server")
	}
	cancel()
	select {
	case got := <-done:
		if got.res != nil || !errors.Is(got.err, context.Canceled) {
			t.Errorf("got %+v, %v; want cancellation", got.res, got.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled extraction did not finish")
	}
	if reads.Load() != 2 {
		t.Errorf("page GETs = %d, want 2", reads.Load())
	}
}
