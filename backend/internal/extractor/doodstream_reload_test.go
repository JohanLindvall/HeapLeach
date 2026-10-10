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
	"sync/atomic"
	"testing"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

func TestDoodReloadRefreshesThePageSessionAndToken(t *testing.T) {
	for _, routed := range []bool{false, true} {
		t.Run(fmt.Sprintf("proxy=%t", routed), func(t *testing.T) {
			var aliases, pages, tokens atomic.Int32
			var origin string
			checkCookie := func(r *http.Request, name string, n int32) {
				t.Helper()
				cookie, err := r.Cookie(name)
				if err != nil || cookie.Value != fmt.Sprint(n) {
					t.Errorf("%s lost %s cookie from visit %d: %v, %v", r.URL.Path, name, n, cookie, err)
				}
			}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.URL.Path == "/e/synthetic":
					aliases.Add(1)
					http.Redirect(w, r, origin+"/e/current", http.StatusFound)
				case r.URL.Path == "/e/current":
					if previous := pages.Load(); previous > 0 {
						checkCookie(r, "token-visit", previous)
					}
					n := pages.Add(1)
					http.SetCookie(w, &http.Cookie{Name: "page-visit", Value: fmt.Sprint(n), Path: "/"})
					fmt.Fprintf(w, `<title>Test Clip</title><script>fetch('/pass_md5/session/token%d')</script>`, n)
				case strings.HasPrefix(r.URL.Path, "/pass_md5/"):
					n := tokens.Add(1)
					if r.URL.Path != fmt.Sprintf("/pass_md5/session/token%d", n) || pages.Load() != n {
						t.Errorf("reused an old token: path=%s, pages=%d, tokens=%d", r.URL.Path, pages.Load(), n)
					}
					if r.Referer() != origin+"/e/current" {
						t.Errorf("token referer=%q", r.Referer())
					}
					checkCookie(r, "page-visit", n)
					http.SetCookie(w, &http.Cookie{Name: "token-visit", Value: fmt.Sprint(n), Path: "/"})
					if n%2 == 1 {
						fmt.Fprint(w, " \nRELOAD\t")
						return
					}
					fmt.Fprint(w, origin+"/media/")
				case strings.HasPrefix(r.URL.Path, "/media/"):
					if r.Referer() != origin+"/e/current" || r.URL.Query().Get("token") != fmt.Sprintf("token%d", tokens.Load()) {
						t.Errorf("media lost refreshed referer or token: referer=%q, token=%q", r.Referer(), r.URL.Query().Get("token"))
					}
					checkCookie(r, "token-visit", tokens.Load())
					w.Header().Set(httpx.HeaderContentType, "video/mp4")
					http.ServeContent(w, r, "clip.mp4", time.Time{}, strings.NewReader("synthetic video bytes"))
				default:
					t.Errorf("unexpected path: %s", r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer srv.Close()
			origin = srv.URL
			client := httpx.New("test", "en", 0, time.Second)
			defer client.CloseIdleConnections()
			ctx := context.Background()
			if routed {
				proxyClient, err := client.ThroughProxy(srv.URL)
				if err != nil {
					t.Fatal(err)
				}
				defer proxyClient.CloseIdleConnections()
				ctx = httpx.WithRoute(ctx, "test-route", proxyClient, nil)
				origin = "http://player.example.test"
			}
			res, err := NewDoodStream(client).Extract(ctx, mustParse(t, origin+"/d/synthetic"), Options{})
			if err != nil || res == nil || len(res.Files) != 1 || res.Files[0].Resolve == nil {
				t.Fatalf("result=%+v err=%v", res, err)
			}
			f := res.Files[0]
			target := &Target{URL: f.URL, Headers: f.Headers}
			for i := range 2 {
				if i > 0 {
					target, err = f.Resolve(ctx)
					if err != nil {
						t.Fatal(err)
					}
				}
				req, err := client.NewRequest(ctx, http.MethodGet, target.URL, nil)
				if err != nil {
					t.Fatal(err)
				}
				for name, value := range target.Headers {
					req.Header.Set(name, value)
				}
				req.Header.Set(httpx.HeaderRange, "bytes=0-8")
				resp, err := client.DoOnce(req)
				if err != nil {
					t.Fatal(err)
				}
				body, err := io.ReadAll(resp.Body)
				resp.Body.Close()
				if err != nil || resp.StatusCode != http.StatusPartialContent || string(body) != "synthetic" {
					t.Fatalf("media status=%d body=%q err=%v", resp.StatusCode, body, err)
				}
			}
			if aliases.Load() != 2 || pages.Load() != 4 || tokens.Load() != 4 {
				t.Fatalf("aliases=%d pages=%d tokens=%d; want 2, 4, 4", aliases.Load(), pages.Load(), tokens.Load())
			}
		})
	}
}

func TestDoodReloadStopsAndPreservesOtherErrors(t *testing.T) {
	for _, mode := range []string{"persistent", "cancelled", "challenge", "unknown-response", "missing-page"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			ctx, captured := httpx.CaptureChallenges(ctx)
			var pages, tokens atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/e/synthetic" {
					n := pages.Add(1)
					if mode == "missing-page" {
						http.NotFound(w, r)
						return
					}
					if mode == "challenge" && n > 1 {
						w.Header().Set(httpx.HeaderContentType, "text/html")
						fmt.Fprint(w, doodGateFixture)
						return
					}
					fmt.Fprint(w, `<script>fetch('/pass_md5/session/synthetic')</script>`)
					return
				}
				tokens.Add(1)
				if mode == "unknown-response" {
					fmt.Fprint(w, "DENIED")
					return
				}
				fmt.Fprint(w, "RELOAD")
				if mode == "cancelled" {
					// Cancel during the backoff, after the response has arrived.
					timer := time.AfterFunc(config.RequestRetryBase/4, cancel)
					t.Cleanup(func() { timer.Stop() })
				}
			}))
			defer srv.Close()
			client := httpx.New("test", "en", 0, time.Second)
			defer client.CloseIdleConnections()
			res, err := NewDoodStream(client).Extract(ctx, mustParse(t, srv.URL+"/e/synthetic"), Options{})
			if res != nil || err == nil {
				t.Fatalf("result=%+v err=%v", res, err)
			}
			_, transient := errors.AsType[*TransientError](err)
			if transient != (mode == "persistent") {
				t.Fatalf("transient=%t err=%v", transient, err)
			}
			wantPages, wantTokens := int32(1), int32(1)
			switch mode {
			case "persistent":
				wantPages, wantTokens = config.ExtractRetries, config.ExtractRetries
			case "cancelled":
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("lost cancellation: %v", err)
				}
			case "challenge":
				wantPages = 2
				if _, ok := errors.AsType[*httpx.ChallengeError](err); !ok || captured() == nil {
					t.Fatalf("lost challenge: %v, captured=%v", err, captured())
				}
			case "unknown-response":
				if !strings.Contains(err.Error(), "DENIED") {
					t.Fatalf("lost token response: %v", err)
				}
			case "missing-page":
				wantTokens = 0
				if !httpx.HasStatus(err, http.StatusNotFound) {
					t.Fatalf("lost 404: %v", err)
				}
			}
			if pages.Load() != wantPages || tokens.Load() != wantTokens {
				t.Fatalf("pages=%d tokens=%d; want %d, %d", pages.Load(), tokens.Load(), wantPages, wantTokens)
			}
		})
	}
}
