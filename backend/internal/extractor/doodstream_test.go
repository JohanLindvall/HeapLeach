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

	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

const doodGateFixture = `<html><div id="video_player">
	<button class="vjs-big-play-button captcha_l">Play</button>
	<div class="captcha-player"><div class="g-recaptcha" id="turnstile-container"></div></div>
</div><script src="//challenges.cloudflare.com/turnstile/v0/api.js"></script></html>`

func TestDoodTurnstileChallengeAtPageOrToken(t *testing.T) {
	for _, stage := range []string{"page", "token"} {
		t.Run(stage, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set(httpx.HeaderContentType, "text/html; charset=UTF-8")
				if stage == "token" && r.URL.Path == "/e/synthetic" {
					fmt.Fprint(w, `<title>Test Clip</title><script>fetch('/pass_md5/session/synthetic')</script>`)
					return
				}
				fmt.Fprint(w, doodGateFixture)
			}))
			defer srv.Close()
			alias := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, srv.URL+r.URL.Path, http.StatusFound)
			}))
			defer alias.Close()
			client := httpx.New("test", "en", 0, time.Second)
			defer client.CloseIdleConnections()
			_, err := NewDoodStream(client).Extract(context.Background(), mustParse(t, alias.URL+"/e/synthetic"), Options{})
			challenge, ok := errors.AsType[*httpx.ChallengeError](err)
			path := "/e/synthetic"
			if stage == "token" {
				path = "/pass_md5/session/synthetic"
			}
			if !ok || challenge.Err.Code != http.StatusOK || challenge.Err.URL != srv.URL+path {
				t.Fatalf("challenge=%+v err=%v", challenge, err)
			}
			if strings.Contains(err.Error(), "removed") {
				t.Fatalf("challenge was reported as a removed video: %v", err)
			}
		})
	}
}

func TestDoodChallengeNeedsABlockedPlayer(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		challenge  bool
	}{
		{"gate", doodGateFixture, true},
		{"removed", `<html>Video not found</html>`, false},
		{"background", `<script src="/cdn-cgi/challenge-platform/scripts/jsd/main.js"></script>`, false},
		{"unrelated-widget", `<div id="turnstile-container"></div>`, false},
		{"no-widget", `<div class="captcha-player"></div>`, false},
		{"playable", doodGateFixture + `<script>fetch('/pass_md5/session/synthetic')</script>`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := doodChallengePage(tc.body); got != tc.challenge {
				t.Fatalf("challenge=%t, want %t", got, tc.challenge)
			}
		})
	}
}

func TestDoodRedirectKeepsTokensCookiesAndMediaOnTheCurrentHost(t *testing.T) {
	client := httpx.New("test", "en", 0, time.Second)
	defer client.CloseIdleConnections()
	var current atomic.Int32
	var tokenRequests atomic.Int32
	var pages []string
	const payload = "synthetic video bytes"
	for i := range 2 {
		var origin string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/e/synthetic" {
				http.SetCookie(w, &http.Cookie{Name: "visit", Value: fmt.Sprint(i), Path: "/"})
				fmt.Fprintf(w, `<title>Test Clip</title><script>fetch('/pass_md5/session/token%d')</script>`, i)
				return
			}
			if got := r.Header.Get(httpx.HeaderReferer); got != origin+"/e/synthetic" {
				t.Errorf("referer=%q, want the current player page", got)
			}
			cookie, err := r.Cookie("visit")
			if err != nil || cookie.Value != fmt.Sprint(i) {
				t.Errorf("request lost its visit cookie: %v, %v", cookie, err)
			}
			if r.URL.Path == fmt.Sprintf("/pass_md5/session/token%d", i) {
				tokenRequests.Add(1)
				fmt.Fprint(w, origin+"/media/")
				return
			}
			if strings.HasPrefix(r.URL.Path, "/media/") {
				if r.URL.Query().Get("token") != fmt.Sprintf("token%d", i) || r.URL.Query().Get("expiry") == "" {
					t.Error("media request lost its current token or timestamp")
				}
				w.Header().Set(httpx.HeaderContentType, "video/mp4")
				http.ServeContent(w, r, "clip.mp4", time.Time{}, strings.NewReader(payload))
				return
			}
			http.NotFound(w, r)
		}))
		t.Cleanup(srv.Close)
		origin = srv.URL
		pages = append(pages, origin+"/e/synthetic")
	}
	alias := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/e/synthetic" {
			t.Errorf("asked the old host for %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		http.Redirect(w, r, pages[current.Load()], http.StatusFound)
	}))
	defer alias.Close()
	res, err := NewDoodStream(client).Extract(context.Background(), mustParse(t, alias.URL+"/d/synthetic"), Options{})
	if err != nil || len(res.Files) != 1 || res.Files[0].Resolve == nil {
		t.Fatalf("result=%+v err=%v", res, err)
	}
	f := res.Files[0]
	if res.Title != "Test Clip" || f.Name != "Test Clip.mp4" || f.Headers[httpx.HeaderReferer] != pages[0] {
		t.Fatalf("result=%+v file=%+v", res, f)
	}
	for i := range 2 {
		target := &Target{URL: f.URL, Headers: f.Headers}
		if i == 1 {
			current.Store(1)
			target, err = f.Resolve(context.Background())
			if err != nil {
				t.Fatal(err)
			}
		}
		if target.Headers[httpx.HeaderReferer] != pages[i] || f.Headers[httpx.HeaderReferer] != pages[0] {
			t.Fatal("refresh reused the old referer or mutated the original headers")
		}
		req, err := client.NewRequest(context.Background(), http.MethodGet, target.URL, nil)
		if err != nil {
			t.Fatal(err)
		}
		for key, value := range target.Headers {
			req.Header.Set(key, value)
		}
		req.Header.Set(httpx.HeaderRange, "bytes=0-7")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusPartialContent || resp.Header.Get(httpx.HeaderContentType) != "video/mp4" {
			t.Fatalf("media status=%d type=%s", resp.StatusCode, resp.Header.Get(httpx.HeaderContentType))
		}
	}
	if tokenRequests.Load() != 2 {
		t.Fatalf("token requests=%d, want one per visit", tokenRequests.Load())
	}
}
