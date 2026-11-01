// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

// A cookie may be sent only to an intermediate or final redirect hop.
// Matching the submitted URL alone cannot establish session equivalence.
func TestRegistryPageCacheSeparatesRedirectCookieSessions(t *testing.T) {
	for _, crossHost := range []bool{false, true} {
		for _, cookieHop := range []string{"middle", "target"} {
			t.Run(fmt.Sprintf("cross-host=%t/cookie=%s", crossHost, cookieHop), func(t *testing.T) {
				origin := "http://start.example.test"
				middle, target := origin, origin
				if crossHost {
					middle, target = "http://middle.example.test", "http://target.example.test"
				}
				// The proxy answers every request locally; the invented hosts
				// are used only to exercise cookie domain boundaries.
				proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					who := "anonymous"
					if cookie, err := r.Cookie("session"); err == nil {
						who = cookie.Value
					}
					switch r.URL.Path {
					case "/entry":
						if who != "anonymous" {
							t.Error("cookie must not be sent to the submitted URL")
						}
						http.Redirect(w, r, middle+"/middle/feature", http.StatusFound)
					case "/middle/feature":
						http.Redirect(w, r, target+"/target/feature?who="+who, http.StatusFound)
					case "/target/feature":
						if cookieHop == "middle" {
							who = r.URL.Query().Get("who")
						}
						w.Header().Set(httpx.HeaderContentType, "text/html")
						fmt.Fprintf(w, `<html><video src="https://cdn.example.test/%s.mp4"></video></html>`, who)
					default:
						http.NotFound(w, r)
					}
				}))
				t.Cleanup(proxy.Close)
				client := testClient()
				route, err := client.ThroughProxy(proxy.URL)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(route.CloseIdleConnections)
				reg := NewRegistry(&config.Config{}, client)
				cookieURL := mustParse(t, middle+"/middle/feature")
				if cookieHop == "target" {
					cookieURL = mustParse(t, target+"/target/feature")
				}
				for _, value := range []string{"first", "second", "anonymous"} {
					jar, _ := cookiejar.New(nil)
					if value != "anonymous" {
						jar.SetCookies(cookieURL, []*http.Cookie{{Name: "session", Value: value, Path: "/" + cookieHop}})
					}
					ctx := httpx.WithCookieJar(context.Background(), jar)
					u := mustParse(t, origin+"/entry")
					first := reg.pages.get(httpx.WithRoute(ctx, "a", route, nil), client, u)
					for _, id := range []string{"a", "a", "b"} {
						if reg.pages.get(httpx.WithRoute(ctx, id, route, nil), client, u) != first {
							t.Fatal("unchanged session did not share its page across routes")
						}
						registryCookieMedia(t, reg, httpx.WithRoute(ctx, id, route, nil), origin+"/entry", Options{},
							"https://cdn.example.test/"+value+".mp4")
					}
					jar.SetCookies(cookieURL, []*http.Cookie{{Name: "session", Value: "unrelated", Path: "/elsewhere"}})
					registryCookieMedia(t, reg, httpx.WithRoute(ctx, "b", route, nil), origin+"/entry", Options{},
						"https://cdn.example.test/"+value+".mp4")
					if reg.pages.get(httpx.WithRoute(ctx, "b", route, nil), client, u) != first {
						t.Fatal("unrelated cookie invalidated the redirect page")
					}
				}
			})
		}
	}
}

// Cookie updates made by ordinary HTTP responses invalidate both the kept
// page and an already-attached probe, regardless of which jar owns them.
func TestPageCacheFollowsRedirectCookieChanges(t *testing.T) {
	for _, owner := range []string{"client", "route", "source"} {
		t.Run(owner, func(t *testing.T) {
			var reads atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/session":
					cookie := &http.Cookie{Name: "session", Value: r.URL.Query().Get("value"), Path: "/target"}
					if cookie.Value == "" {
						cookie.MaxAge = -1
					}
					http.SetCookie(w, cookie)
				case "/entry":
					http.Redirect(w, r, "/target/feature", http.StatusFound)
				case "/target/feature":
					reads.Add(1)
					who := "anonymous"
					if cookie, err := r.Cookie("session"); err == nil {
						who = cookie.Value
					}
					w.Header().Set(httpx.HeaderContentType, "text/html")
					fmt.Fprintf(w, "<html><title>%s</title></html>", who)
				default:
					http.NotFound(w, r)
				}
			}))
			t.Cleanup(srv.Close)
			client := testClient()
			ctx := context.Background()
			if owner != "client" {
				ctx = httpx.WithRoute(ctx, "route", testClient(), nil)
			}
			if owner == "source" {
				jar, _ := cookiejar.New(nil)
				ctx = httpx.WithCookieJar(ctx, jar)
			}
			u := mustParse(t, srv.URL+"/entry")
			var cache pageCache
			var previous *Page
			for i, value := range []string{"first", "second", ""} {
				if _, _, err := client.Fetch(ctx, srv.URL+"/session?value="+value, nil); err != nil {
					t.Fatal(err)
				}
				if previous != nil {
					attached := withPage(ctx, previous)
					if probed(attached, u) != nil || probed(attached, previous.Final) != nil {
						t.Error("attached redirect probe survived a destination cookie change")
					}
				}
				want := value
				if want == "" {
					want = "anonymous"
				}
				p := cache.get(ctx, client, u)
				if p.Err != nil || p.Title() != want {
					t.Fatalf("title %q, error %v; want %q", p.Title(), p.Err, want)
				}
				if probed(withPage(ctx, p), u) != p || cache.get(ctx, client, u) != p || reads.Load() != int32(i+1) {
					t.Fatal("unchanged redirect session did not reuse its page")
				}
				previous = p
			}
		})
	}
}

// A redirect can establish a cookie for a path it has not visited yet.
// Its original reader may consume that answer once, but another extraction
// must ask again even if the final hop sent the newly established cookie.
func TestPageCacheCookieSettingRedirectIsOnlyItsCallersAnswer(t *testing.T) {
	for _, setAt := range []string{"middle", "target"} {
		t.Run(setAt, func(t *testing.T) {
			var entries atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/entry" {
					setup := entries.Add(1) == 1
					http.Redirect(w, r, fmt.Sprintf("/middle/feature?setup=%t", setup), http.StatusFound)
					return
				}
				setup := r.URL.Query().Get("setup") == "true"
				if setup && r.URL.Path == "/"+setAt+"/feature" {
					http.SetCookie(w, &http.Cookie{Name: "session", Value: "established", Path: "/target"})
				}
				if r.URL.Path == "/middle/feature" {
					http.Redirect(w, r, "/target/feature?"+r.URL.RawQuery, http.StatusFound)
					return
				}
				w.Header().Set(httpx.HeaderContentType, "text/html")
				fmt.Fprintf(w, "<html><title>setup=%t</title></html>", setup)
			}))
			t.Cleanup(srv.Close)
			ctx := context.Background()
			client := testClient()
			u := mustParse(t, srv.URL+"/entry")
			var cache pageCache
			first := cache.get(ctx, client, u)
			body, _, err := fetchDocument(withPage(ctx, first), client, u, nil)
			if err != nil || body != "<html><title>setup=true</title></html>" || entries.Load() != 1 {
				t.Fatalf("initial reader: body=%q, error=%v, entries=%d", body, err, entries.Load())
			}
			next := cache.get(ctx, client, u)
			if next.Err != nil || next.Title() != "setup=false" || entries.Load() != 2 {
				t.Fatalf("next reader: title=%q, error=%v, entries=%d", next.Title(), next.Err, entries.Load())
			}
			if cache.get(ctx, client, u) != next || entries.Load() != 2 {
				t.Error("subsequent stable redirect was not cached")
			}
		})
	}
}

func TestPageCacheRedirectWaiterKeepsItsCookieSession(t *testing.T) {
	var reads atomic.Int32
	entered, hold := make(chan struct{}), make(chan struct{})
	release := sync.OnceFunc(func() { close(hold) })
	defer release()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/entry" {
			http.Redirect(w, r, "/target/feature", http.StatusFound)
			return
		}
		cookie, err := r.Cookie("session")
		if err != nil {
			t.Error(err)
			return
		}
		if reads.Add(1) == 1 {
			close(entered)
			select {
			case <-hold:
			case <-r.Context().Done():
				return
			}
		}
		w.Header().Set(httpx.HeaderContentType, "text/html")
		fmt.Fprintf(w, "<html><title>%s</title></html>", cookie.Value)
	}))
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	u := mustParse(t, srv.URL+"/entry")
	session := func(value string) context.Context {
		jar, _ := cookiejar.New(nil)
		jar.SetCookies(u, []*http.Cookie{{Name: "session", Value: value, Path: "/target"}})
		return httpx.WithCookieJar(ctx, jar)
	}
	client := testClient()
	var cache pageCache
	owner := make(chan *Page, 1)
	go func() { owner <- cache.get(session("first"), client, u) }()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("owner did not reach redirect destination")
	}
	waitCtx := &pageCookieWaitContext{Context: session("second"), waiting: make(chan struct{})}
	waiter := make(chan *Page, 1)
	go func() { waiter <- cache.get(waitCtx, client, u) }()
	select {
	case <-waitCtx.waiting:
	case <-ctx.Done():
		t.Fatal("second caller did not wait for the probe")
	}
	release()
	first, second := <-owner, <-waiter
	if first.Err != nil || first.Title() != "first" || second.Err != nil || second.Title() != "second" {
		t.Fatalf("owner=%q/%v, waiter=%q/%v; each must get its own session", first.Title(), first.Err, second.Title(), second.Err)
	}
	if cache.get(waitCtx, client, u) != second || reads.Load() != 2 {
		t.Error("waiter's own response was not cached")
	}
}

// File-shaped inputs use HEAD so a probe never consumes the media body.
// Their redirect destinations still depend on the session at each hop.
func TestPageCacheHEADValidatesRedirectCookies(t *testing.T) {
	var probes atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodHead {
			t.Error("media probe must not fetch a body")
		}
		switch r.URL.Path {
		case "/entry.mp4":
			probes.Add(1)
			http.Redirect(w, r, "/target/file", http.StatusFound)
		case "/target/file":
			cookie, err := r.Cookie("session")
			if err != nil {
				t.Error(err)
				return
			}
			http.Redirect(w, r, "/media/"+cookie.Value+".mp4", http.StatusFound)
		case "/media/first.mp4", "/media/second.mp4":
			w.Header().Set(httpx.HeaderContentType, "video/mp4")
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	u := mustParse(t, srv.URL+"/entry.mp4")
	jar, _ := cookiejar.New(nil)
	ctx := httpx.WithCookieJar(context.Background(), jar)
	client := testClient()
	var cache pageCache
	for i, value := range []string{"first", "second"} {
		jar.SetCookies(u, []*http.Cookie{{Name: "session", Value: value, Path: "/target"}})
		p := cache.get(ctx, client, u)
		if p.Err != nil || p.Final.Path != "/media/"+value+".mp4" {
			t.Fatalf("HEAD destination=%s, error=%v; want %s", p.Final.Path, p.Err, value)
		}
		if cache.get(ctx, client, u) != p || probes.Load() != int32(i+1) {
			t.Fatal("unchanged HEAD redirect was not reused")
		}
	}
}
