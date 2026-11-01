// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

// The effective jar may belong to the client, its route, or the source.
// A later cookie update invalidates an attached probe in each case, while
// a cookie scoped to an unrelated path does not change the page's request.
func TestAttachedProbeFollowsCookieChanges(t *testing.T) {
	for _, owner := range []string{"client", "route", "source"} {
		t.Run(owner, func(t *testing.T) {
			var reads atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set(httpx.HeaderContentType, "text/html")
				if r.URL.Path == "/session" {
					cookie := &http.Cookie{Name: "session", Value: r.URL.Query().Get("value"), Path: "/"}
					if r.URL.Query().Has("unrelated") {
						cookie.Path = "/elsewhere"
					}
					if cookie.Value == "" {
						cookie.MaxAge = -1
					}
					http.SetCookie(w, cookie)
					return
				}
				reads.Add(1)
				who := "anonymous"
				if cookie, err := r.Cookie("session"); err == nil {
					who = cookie.Value
				}
				fmt.Fprintf(w, "<html><title>%s</title></html>", who)
			}))
			t.Cleanup(srv.Close)
			client := testClient()
			ctx := context.Background()
			if owner != "client" {
				ctx = httpx.WithRoute(ctx, "route", testClient(), nil)
			}
			if owner == "source" {
				jar, err := cookiejar.New(nil)
				if err != nil {
					t.Fatal(err)
				}
				ctx = httpx.WithCookieJar(ctx, jar)
			}
			set := func(query string) {
				t.Helper()
				if _, _, err := client.Fetch(ctx, srv.URL+"/session?"+query, nil); err != nil {
					t.Fatal(err)
				}
			}
			set("value=first")
			u := mustParse(t, srv.URL+"/feature")
			p := probePage(ctx, client, u)
			if p.Err != nil || p.Title() != "first" {
				t.Fatalf("probe: title %q, error %v", p.Title(), p.Err)
			}
			attached := withPage(ctx, p)
			set("value=other&unrelated=1")
			if probedFor(attached, client, u, nil) != p || reads.Load() != 1 {
				t.Fatal("an unrelated cookie invalidated the probe")
			}
			for _, value := range []string{"second", ""} {
				set("value=" + value)
				if probed(attached, u) != nil || probedFor(attached, client, u, nil) != nil {
					t.Error("attached probe survived a cookie change")
				}
				want := value
				if want == "" {
					want = "anonymous"
				}
				doc, _, err := fetchDocument(attached, client, u, nil)
				if err != nil || !strings.Contains(doc, "<title>"+want+"</title>") {
					t.Fatalf("updated document = %q, error %v; want %s", doc, err, want)
				}
			}
			if got := reads.Load(); got != 3 {
				t.Errorf("%d page reads, want one per cookie state", got)
			}
		})
	}
}

// Changing cookies while an older request is in flight must neither wait
// for that request nor let its late answer replace the new session's page.
func TestPageCacheSeparatesInflightCookieStates(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var reads atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := reads.Add(1)
		cookie, err := r.Cookie("session")
		if err != nil {
			t.Error(err)
			return
		}
		if n == 1 {
			close(started)
			<-release
		}
		w.Header().Set(httpx.HeaderContentType, "text/html")
		fmt.Fprintf(w, "<html><title>%s</title></html>", cookie.Value)
	}))
	t.Cleanup(srv.Close)
	// Register after Close so an early failure releases the blocked handler.
	t.Cleanup(func() { close(release) })
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	u := mustParse(t, srv.URL+"/feature")
	jar.SetCookies(u, []*http.Cookie{{Name: "session", Value: "first"}})
	ctx, cancel := context.WithTimeout(httpx.WithCookieJar(context.Background(), jar), 5*time.Second)
	defer cancel()
	client := testClient()
	var cache pageCache
	first := make(chan *Page, 1)
	go func() { first <- cache.get(ctx, client, u) }()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("first request did not start")
	}
	jar.SetCookies(u, []*http.Cookie{{Name: "session", Value: "second"}})
	second := cache.get(ctx, client, u)
	if second.Err != nil || second.Title() != "second" {
		t.Fatalf("new session joined the old request: title %q, error %v", second.Title(), second.Err)
	}
	// Release the handler once without closing the cleanup-owned channel.
	release <- struct{}{}
	select {
	case p := <-first:
		if p.Err != nil || p.Title() != "first" {
			t.Fatalf("original response: title %q, error %v", p.Title(), p.Err)
		}
	case <-ctx.Done():
		t.Fatal("first request did not finish")
	}
	if later := cache.get(ctx, client, u); later != second || reads.Load() != 2 {
		t.Fatalf("late response replaced the new session's cached page: %q after %d reads", later.Title(), reads.Load())
	}
}
