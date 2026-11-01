// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"fmt"
	"io"
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

// Separate source sessions can read the same URL on the same route. Each
// must receive its own media, while an unchanged session still uses the cache.
func TestRegistryPageCacheSeparatesCookieSessions(t *testing.T) {
	var reads atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/feature" {
			http.NotFound(w, r)
			return
		}
		reads.Add(1)
		w.Header().Set(httpx.HeaderContentType, "text/html")
		cookie, err := r.Cookie("session")
		if err != nil {
			t.Error("page request omitted its session cookie")
			return
		}
		fmt.Fprintf(w, `<html><video src="https://cdn.example.test/%s.mp4"></video></html>`, cookie.Value)
	}))
	t.Cleanup(srv.Close)
	reg := NewRegistry(&config.Config{}, testClient())
	u := mustParse(t, srv.URL+"/feature")
	for _, session := range []string{"first", "second"} {
		jar, err := cookiejar.New(nil)
		if err != nil {
			t.Fatal(err)
		}
		jar.SetCookies(u, []*http.Cookie{{Name: "session", Value: session}})
		ctx := httpx.WithCookieJar(context.Background(), jar)
		for range 2 {
			registryCookieMedia(t, reg, ctx, u.String(), Options{}, "https://cdn.example.test/"+session+".mp4")
		}
	}
	if got := reads.Load(); got != 2 {
		t.Errorf("page fetched %d times, want one per session", got)
	}
}

// A Set-Cookie response can change or remove the cookies in the ordinary
// client's jar without replacing the jar itself.
func TestRegistryPageCacheFollowsResponseCookieChanges(t *testing.T) {
	var reads atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/session":
			cookie := &http.Cookie{Name: "session", Value: r.URL.Query().Get("value"), Path: "/"}
			if cookie.Value == "" {
				cookie.MaxAge = -1
			}
			http.SetCookie(w, cookie)
		case "/feature":
			reads.Add(1)
			who := "anonymous"
			if cookie, err := r.Cookie("session"); err == nil {
				who = cookie.Value
			}
			w.Header().Set(httpx.HeaderContentType, "text/html")
			fmt.Fprintf(w, `<html><video src="https://cdn.example.test/%s.mp4"></video></html>`, who)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	client := testClient()
	reg := NewRegistry(&config.Config{}, client)
	ctx := context.Background()
	for _, session := range []string{"first", "second", ""} {
		if _, _, err := client.Fetch(ctx, srv.URL+"/session?value="+session, nil); err != nil {
			t.Fatal(err)
		}
		who := session
		if who == "" {
			who = "anonymous"
		}
		for range 2 {
			registryCookieMedia(t, reg, ctx, srv.URL+"/feature", Options{}, "https://cdn.example.test/"+who+".mp4")
		}
	}
	if got := reads.Load(); got != 3 {
		t.Errorf("page fetched %d times, want one per cookie state", got)
	}
}

// A successful password POST consumes the gate's token and unlocks later
// GETs through the shared jar. Reusing the old gate would post that spent
// token again and reject a password that already succeeded.
func TestRegistryPageCacheDropsUnlockedPasswordGate(t *testing.T) {
	const gate = `<html><meta name="generator" content="Chevereto"><form><input name="content-password"><input name="auth_token" value="first-token"></form></html>`
	const item = `<html><meta name="generator" content="Chevereto"><meta property="og:image" content="https://cdn.example.test/photo.jpg"><div id="image-viewer"><img src="https://cdn.example.test/photo.jpg"></div></html>`
	var posts, reads atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/image/example" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set(httpx.HeaderContentType, "text/html")
		if r.Method == http.MethodPost {
			posts.Add(1)
			if err := r.ParseForm(); err != nil {
				t.Error(err)
				return
			}
			_, cookieErr := r.Cookie("unlocked")
			if cookieErr != nil && r.Form.Get("content-password") == "secret" && r.Form.Get("auth_token") == "first-token" {
				http.SetCookie(w, &http.Cookie{Name: "unlocked", Value: "yes", Path: "/"})
				_, _ = io.WriteString(w, item)
				return
			}
			_, _ = io.WriteString(w, gate)
			return
		}
		reads.Add(1)
		if _, err := r.Cookie("unlocked"); err == nil {
			_, _ = io.WriteString(w, item)
			return
		}
		_, _ = io.WriteString(w, gate)
	}))
	t.Cleanup(srv.Close)
	u := mustParse(t, srv.URL+"/image/example")
	cfg := &config.Config{ExtraHosts: map[string][]string{config.FamilyChevereto: {u.Hostname()}}}
	reg := NewRegistry(cfg, testClient())
	for range 3 {
		registryCookieMedia(t, reg, context.Background(), u.String(), Options{Password: "secret"}, "https://cdn.example.test/photo.jpg")
	}
	if got := posts.Load(); got != 1 {
		t.Errorf("password posted %d times, want 1", got)
	}
	if got := reads.Load(); got != 5 {
		t.Errorf("page fetched %d times, want gate and unlocked probes plus three Referer-bearing reads", got)
	}
}

// A probe can establish a session while returning a document that must be
// read exactly once. Its originating extraction owns that answer, but it is
// not the answer to another GET with either the old or the new cookies.
func TestPageCacheCookieSettingProbeIsOnlyItsCallersAnswer(t *testing.T) {
	var reads atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reads.Add(1)
		w.Header().Set(httpx.HeaderContentType, "text/html")
		if _, err := r.Cookie("session"); err != nil {
			http.SetCookie(w, &http.Cookie{Name: "session", Value: "established", Path: "/"})
			_, _ = io.WriteString(w, "<html>initial response</html>")
			return
		}
		_, _ = io.WriteString(w, "<html>session response</html>")
	}))
	t.Cleanup(srv.Close)
	client := testClient()
	u := mustParse(t, srv.URL+"/feature")
	ctx := context.Background()
	var cache pageCache
	p := cache.get(ctx, client, u)
	if p.Err != nil {
		t.Fatal(p.Err)
	}
	body, _, err := fetchDocument(withPage(ctx, p), client, u, nil)
	if err != nil || body != "<html>initial response</html>" || reads.Load() != 1 {
		t.Fatalf("originating read: body=%q, err=%v, requests=%d; want its initial response without another GET", body, err, reads.Load())
	}
	next := cache.get(ctx, client, u)
	if next.Err != nil || next.Body != "<html>session response</html>" || reads.Load() != 2 {
		t.Fatalf("next read: body=%q, err=%v, requests=%d; want a GET with the new session", next.Body, next.Err, reads.Load())
	}
	if stable := cache.get(ctx, client, u); stable != next || reads.Load() != 2 {
		t.Error("the subsequent response with unchanged cookies was not cached")
	}
	old := cache.get(ctx, testClient(), u)
	if old.Err != nil || old.Body != "<html>initial response</html>" || reads.Load() != 3 {
		t.Fatalf("new session: body=%q, err=%v, requests=%d; want a GET with the original empty cookies", old.Body, old.Err, reads.Load())
	}
}

// A caller already waiting for a probe that sets cookies must ask again
// with the updated jar; it cannot inherit the first caller's session setup.
func TestPageCacheCookieSettingProbeIsNotSharedWithWaiters(t *testing.T) {
	var reads atomic.Int32
	entered, hold := make(chan struct{}), make(chan struct{})
	release := sync.OnceFunc(func() { close(hold) })
	defer release()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(httpx.HeaderContentType, "text/html")
		if reads.Add(1) == 1 {
			close(entered)
			select {
			case <-hold:
			case <-r.Context().Done():
				return
			}
			http.SetCookie(w, &http.Cookie{Name: "session", Value: "established", Path: "/"})
			_, _ = io.WriteString(w, "<html>initial response</html>")
			return
		}
		if cookie, err := r.Cookie("session"); err != nil || cookie.Value != "established" {
			t.Error("waiting caller retried without the new session cookie")
		}
		_, _ = io.WriteString(w, "<html>session response</html>")
	}))
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	client := testClient()
	u := mustParse(t, srv.URL+"/feature")
	var cache pageCache
	owner := make(chan *Page, 1)
	go func() { owner <- cache.get(ctx, client, u) }()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("probe did not reach the server")
	}
	waitCtx := &pageCookieWaitContext{Context: ctx, waiting: make(chan struct{})}
	waiter := make(chan *Page, 1)
	go func() { waiter <- cache.get(waitCtx, client, u) }()
	select {
	case <-waitCtx.waiting:
	case <-ctx.Done():
		t.Fatal("second caller did not wait for the probe")
	}
	release()
	first, next := <-owner, <-waiter
	if first.Err != nil || first.Body != "<html>initial response</html>" {
		t.Fatalf("initial probe: body=%q, err=%v", first.Body, first.Err)
	}
	if next.Err != nil || next.Body != "<html>session response</html>" || reads.Load() != 2 {
		t.Fatalf("waiting read: body=%q, err=%v, requests=%d; want a GET with the new session", next.Body, next.Err, reads.Load())
	}
	if stable := cache.get(ctx, client, u); stable != next || reads.Load() != 2 {
		t.Error("the waiting caller's unchanged-cookie response was not cached under its new request identity")
	}
}

// pageCache.get consults Done when it waits on another caller. Observing
// that call lets the test release the server without a scheduling delay.
type pageCookieWaitContext struct {
	context.Context
	waiting chan struct{}
	once    sync.Once
}

func (c *pageCookieWaitContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.waiting) })
	return c.Context.Done()
}

func registryCookieMedia(t *testing.T, reg *Registry, ctx context.Context, rawURL string, opts Options, want string) {
	t.Helper()
	res, _, err := reg.Extract(ctx, rawURL, opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Files) != 1 || res.Files[0].URL != want {
		t.Fatalf("files = %#v, want %s", res.Files, want)
	}
}
