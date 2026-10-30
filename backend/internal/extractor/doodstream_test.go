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

const doodInterstitialFixture = `<html><title>Just a moment...</title><script>
window._cf_chl_opt = {cType: 'managed', cUPMDTk: '/e/synthetic?__cf_chl_tk=example'};
var script = document.createElement('script');
script.src = '/cdn-cgi/challenge-platform/h/g/orchestrate/chl_page/v1';
document.head.appendChild(script);
</script></html>`

func TestDoodChallengeAtPageOrToken(t *testing.T) {
	for _, gate := range []struct{ name, body string }{
		{"turnstile", doodGateFixture},
		{"interstitial", doodInterstitialFixture},
	} {
		for _, stage := range []string{"page", "token"} {
			t.Run(gate.name+"/"+stage, func(t *testing.T) {
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set(httpx.HeaderContentType, "text/html; charset=UTF-8")
					if stage == "token" && r.URL.Path == "/e/synthetic" {
						fmt.Fprint(w, `<title>Test Clip</title><script>fetch('/pass_md5/session/synthetic')</script>`)
						return
					}
					body := gate.body
					if stage == "token" {
						body = strings.ReplaceAll(body, "/e/synthetic", "/pass_md5/session/synthetic")
					}
					fmt.Fprint(w, body)
				}))
				defer srv.Close()
				alias := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					http.Redirect(w, r, srv.URL+r.URL.Path, http.StatusFound)
				}))
				defer alias.Close()
				client := testClient()
				defer client.CloseIdleConnections()
				_, err := extract(t, NewDoodStream(client), alias.URL+"/e/synthetic", Options{})
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
}

func TestDoodChallengeNeedsABlockedPlayer(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		challenge  bool
	}{
		{"gate", doodGateFixture, true},
		{"interstitial", doodInterstitialFixture, true},
		{"removed", `<html>Video not found</html>`, false},
		{"background", `<script src="/cdn-cgi/challenge-platform/scripts/jsd/main.js"></script>`, false},
		{"background-with-options", `<script>window._cf_chl_opt = {}; load('/cdn-cgi/challenge-platform/scripts/jsd/main.js');</script>`, false},
		{"challenge-title-only", `<title>Just a moment...</title>`, false},
		{"challenge-text-only", `<pre>window._cf_chl_opt /cdn-cgi/challenge-platform/h/g/orchestrate/chl_page/v1</pre>`, false},
		{"unrelated-widget", `<div id="turnstile-container"></div>`, false},
		{"invisible-widget", `<script>turnstile.render(document.body, {size: 'invisible'});</script>`, false},
		{"no-widget", `<div class="captcha-player"></div>`, false},
		{"playable", doodGateFixture + `<script>fetch('/pass_md5/session/synthetic')</script>`, false},
		{"interstitial-echoes-token-path", strings.ReplaceAll(doodInterstitialFixture, "/e/synthetic", "/pass_md5/session/synthetic"), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := doodChallengePage(tc.body); got != tc.challenge {
				t.Fatalf("challenge=%t, want %t", got, tc.challenge)
			}
		})
	}
}

func TestDoodRedirectKeepsTokensCookiesAndMediaOnTheCurrentHost(t *testing.T) {
	client := testClient()
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
	res, err := extract(t, NewDoodStream(client), alias.URL+"/d/synthetic", Options{})
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
		target.Headers.Apply(req)
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

// Some addresses are served one bare sentence where the player should be, for
// a real video and a missing one alike, while every other address is shown
// the player. That is the address refused, for the queue to try another, not
// the video removed; the host's own not-found page still fails the file.
func TestDoodRefusalIsTheAddressesNotTheVideos(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		refused    bool
	}{
		{"bare-sentence", "The resource you are looking for has been removed or is temporarily unavailable.", true},
		{"not-found-page", `<html><head><title>Video not found | DoodStream</title></head>` +
			`<body><div class="container not_found"><h1>Not Found</h1></div></body></html>`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set(httpx.HeaderContentType, "text/html; charset=UTF-8")
				fmt.Fprint(w, tc.body)
			}))
			defer srv.Close()
			client := testClient()
			defer client.CloseIdleConnections()
			_, err := extract(t, NewDoodStream(client), srv.URL+"/e/synthetic", Options{})
			_, refused := errors.AsType[*RefusedError](err)
			if err == nil || refused != tc.refused {
				t.Fatalf("refused=%v, want %v: %v", refused, tc.refused, err)
			}
		})
	}
}

// A video the host has removed is shown its own "Video not found" page,
// which says so: that is the video's absence, not a layout the parser no
// longer knows, nor an address refused.
func TestDoodSaysWhenTheHostHasRemovedTheVideo(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(httpx.HeaderContentType, "text/html")
		fmt.Fprint(w, `<!doctype html><html><head><title>Video not found</title></head><body><div class="error">Not found</div></body></html>`)
	}))
	defer srv.Close()
	client := testClient()
	_, err := extract(t, NewDoodStream(client), srv.URL+"/e/synthetic", Options{})
	if err == nil || !strings.Contains(err.Error(), "video not found") || strings.Contains(err.Error(), "layout") {
		t.Fatalf("error = %v; want it to say the host removed the video", err)
	}
	if _, refused := errors.AsType[*RefusedError](err); refused {
		t.Fatal("a removed video was read as a refused address")
	}
}

func TestRandomTokenLength(t *testing.T) {
	seen := make(map[string]bool)
	for range 100 {
		token := randomToken(doodTailLength)
		if len(token) != doodTailLength {
			t.Fatalf("token %q is %d characters, want %d", token, len(token), doodTailLength)
		}
		seen[token] = true
	}
	if len(seen) < 90 {
		t.Errorf("only %d of 100 tokens were distinct", len(seen))
	}
}
