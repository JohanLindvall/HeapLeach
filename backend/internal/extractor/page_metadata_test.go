// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

func TestRepeatedExtractionRefreshesAPIMetadata(t *testing.T) {
	for _, mode := range []string{"no-store", "no-cache", "max-age=0", "api-cookie"} {
		t.Run(mode, func(t *testing.T) {
			var current atomic.Int32
			current.Store(1)
			var apiCalls, pageCalls atomic.Int32
			var srv *httptest.Server
			srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/posts/42":
					pageCalls.Add(1)
					w.Header().Set(httpx.HeaderContentType, "text/html")
					w.Header().Set(httpx.HeaderCacheControl, "max-age=60")
					fmt.Fprint(w, `<html><head><meta name="generator" content="Danbooru"></head><body></body></html>`)
				case "/posts.json":
					apiCalls.Add(1)
					w.Header().Set(httpx.HeaderContentType, "application/json")
					n := current.Load()
					if mode == "api-cookie" {
						w.Header().Set(httpx.HeaderCacheControl, "max-age=60")
						w.Header().Set(httpx.HeaderVary, "Cookie")
						n = 1
						if c, err := r.Cookie("session"); err == nil && c.Value == "second" {
							n = 2
						}
					} else {
						w.Header().Set(httpx.HeaderCacheControl, mode)
					}
					fmt.Fprintf(w, `[{"id":42,"file_url":%q,"file_size":12}]`, srv.URL+fmt.Sprintf("/media/version-%d.jpg", n))
				default:
					http.NotFound(w, r)
				}
			}))
			defer srv.Close()
			client := httpx.New("audit-agent", "en-US", 0, 5*time.Second)
			defer client.CloseIdleConnections()
			reg := NewRegistry(&config.Config{}, client)
			jar, _ := cookiejar.New(nil)
			origin, _ := url.Parse(srv.URL)
			ctx := httpx.WithCookieJar(context.Background(), jar)
			link := srv.URL + "/posts/42"
			first, _, err := reg.Extract(ctx, link, Options{})
			if err != nil || first == nil || len(first.Files) != 1 || !strings.HasSuffix(first.Files[0].URL, "version-1.jpg") {
				t.Fatalf("first=%+v err=%v", first, err)
			}
			current.Store(2)
			if mode == "api-cookie" {
				jar.SetCookies(origin, []*http.Cookie{{Name: "session", Value: "second", Path: "/posts.json"}})
			}
			second, _, err := reg.Extract(ctx, link, Options{})
			if err != nil || second == nil || len(second.Files) != 1 {
				t.Fatalf("second=%+v err=%v", second, err)
			}
			if apiCalls.Load() != 2 || pageCalls.Load() != 1 {
				t.Errorf("API calls=%d page calls=%d; want one API answer per extraction and one cached HTML response", apiCalls.Load(), pageCalls.Load())
			}
			if !strings.HasSuffix(second.Files[0].URL, "version-2.jpg") {
				t.Fatalf("second extraction reused obsolete API response: %s", second.Files[0].URL)
			}
		})
	}
}

// API-scoped cookies do not change the source page's cache key. Concurrent
// jobs must share the HTML without sharing either session's API answer.
func TestConcurrentExtractionsKeepAPISessionsSeparate(t *testing.T) {
	var pageCalls atomic.Int32
	arrived := make(chan struct{}, 2)
	release := make(chan struct{})
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/posts/42":
			pageCalls.Add(1)
			w.Header().Set(httpx.HeaderContentType, "text/html")
			w.Header().Set(httpx.HeaderCacheControl, "max-age=60")
			fmt.Fprint(w, `<html><meta name="generator" content="Danbooru"></html>`)
		case "/posts.json":
			cookie, err := r.Cookie("session")
			if err != nil {
				http.Error(w, "missing fixture session", 400)
				return
			}
			arrived <- struct{}{}
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
			w.Header().Set(httpx.HeaderContentType, httpx.ContentTypeJSON)
			w.Header().Set(httpx.HeaderVary, httpx.HeaderCookie)
			w.Header().Set(httpx.HeaderCacheControl, "no-store")
			fmt.Fprintf(w, `[{"id":42,"file_url":%q,"file_size":12}]`, srv.URL+"/media/"+cookie.Value+".jpg")
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	client := httpx.New("test", "en-US", 0, 5*time.Second)
	defer client.CloseIdleConnections()
	reg := NewRegistry(&config.Config{}, client)
	origin, _ := url.Parse(srv.URL)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	type outcome struct {
		session string
		result  *Result
		err     error
	}
	results := make(chan outcome, 2)
	for _, session := range []string{"first", "second"} {
		jar, _ := cookiejar.New(nil)
		jar.SetCookies(origin, []*http.Cookie{{Name: "session", Value: session, Path: "/posts.json"}})
		go func() {
			result, _, err := reg.Extract(httpx.WithCookieJar(ctx, jar), srv.URL+"/posts/42", Options{})
			results <- outcome{session, result, err}
		}()
	}
	for range 2 {
		select {
		case <-arrived:
		case <-ctx.Done():
			close(release)
			t.Fatal("concurrent acquisitions shared or failed to issue their separate API requests")
		}
	}
	close(release)
	for range 2 {
		got := <-results
		if got.err != nil || got.result == nil || len(got.result.Files) != 1 {
			t.Fatalf("session=%s result=%+v error=%v", got.session, got.result, got.err)
		}
		want := srv.URL + "/media/" + got.session + ".jpg"
		if got.result.Files[0].URL != want {
			t.Errorf("session=%s URL=%q want=%q", got.session, got.result.Files[0].URL, want)
		}
	}
	if pageCalls.Load() != 1 {
		t.Errorf("HTML calls=%d, want one shared source response", pageCalls.Load())
	}
}

func TestCachedPageSharesDocumentWithoutRecognitionMemos(t *testing.T) {
	var pages atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/generic/article" {
			http.NotFound(w, r)
			return
		}
		pages.Add(1)
		w.Header().Set(httpx.HeaderContentType, "text/html")
		w.Header().Set(httpx.HeaderCacheControl, "max-age=60")
		fmt.Fprint(w, `<html><head><title>Example Article</title></head><body>Example text</body></html>`)
	}))
	defer srv.Close()
	client := testClient()
	defer client.CloseIdleConnections()
	reg := NewRegistry(&config.Config{}, client)
	u := mustParse(t, srv.URL+"/generic/article")
	_, first := reg.identify(t.Context(), u)
	firstTree := first.HTML()
	memo(first, "recognition", func() string { return "first acquisition" })
	_, second := reg.identify(t.Context(), u)
	if firstTree == nil || firstTree != second.HTML() || pages.Load() != 1 {
		t.Fatal("fresh acquisition discarded the reusable source document or parsed tree")
	}
	if got := memo(second, "recognition", func() string { return "second acquisition" }); got != "second acquisition" {
		t.Fatalf("recognition result leaked across acquisitions: %q", got)
	}
}
