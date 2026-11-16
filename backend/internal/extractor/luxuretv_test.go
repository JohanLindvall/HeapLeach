// SPDX-License-Identifier: MIT

package extractor

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

const luxuretvFixture = `<title>A sample clip</title>
<script type="application/ld+json">{"@type":"VideoObject","contentUrl":"https://cdn.example.test/cf-stream/123/a/clip.mp4"}</script>
<div id="player"><video id="thisPlayer" class="video-js">
<source src="/cf-stream/123/a/clip.mp4?md5=first%2Btoken&amp;expires=1" type="video/mp4">
</video></div>
<video id="ltvModalVideo"><source src="https://cdn.example.test/promotion.mp4"></video>`

func TestLuxureTVRecognition(t *testing.T) {
	p := NewPlayer(nil, &luxuretvPlayer)
	for _, tc := range []struct {
		name, body string
		want       bool
	}{
		{"player", luxuretvFixture, true},
		{"name only", `<title>LuxureTV</title>`, false},
		{"wrong video", strings.ReplaceAll(luxuretvFixture, "/cf-stream/123/", "/cf-stream/1234/"), false},
		{"wrong container", strings.ReplaceAll(luxuretvFixture, `id="player"`, `id="promotion"`), false},
		{"wrong player", strings.ReplaceAll(luxuretvFixture, `id="thisPlayer"`, `id="preview"`), false},
		{"no source", strings.ReplaceAll(luxuretvFixture, `<source src="/cf-stream/`, `<source data-src="/cf-stream/`), false},
		{"image", strings.ReplaceAll(luxuretvFixture, "clip.mp4", "clip.jpg"), false},
		{"non-HTTP source", strings.ReplaceAll(luxuretvFixture, `src="/cf-stream/`, `src="file:///cf-stream/`), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			expectRecognised(t, p, htmlPage(t, "https://example.test/videos/sample-123.html", tc.body), tc.want)
		})
	}
	expectShape(t, p, "https://example.test/videos/sample-123.html", true)
	expectShape(t, p, "https://example.test/videos/sample.html", false)
	expectRecognised(t, p, htmlPage(t, "https://example.test/categories/sample.html", luxuretvFixture), false)
	expectRecognised(t, p, fixturePage(t, "https://example.test/videos/sample-123.html", http.StatusOK, "video/mp4", ""), false)
}

func TestLuxureTVSignedPlayer(t *testing.T) {
	for _, redirected := range []bool{false, true} {
		t.Run(fmt.Sprintf("redirect=%t", redirected), func(t *testing.T) {
			var pages atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/videos/old-999.html" {
					http.Redirect(w, r, "/videos/sample-123.html", http.StatusFound)
					return
				}
				if r.URL.Path != "/videos/sample-123.html" {
					t.Errorf("unexpected request: %s", r.URL.Path)
					http.NotFound(w, r)
					return
				}
				w.Header().Set(httpx.HeaderContentType, "text/html")
				fmt.Fprint(w, strings.ReplaceAll(luxuretvFixture, "first%2Btoken", fmt.Sprintf("token%%2B%d", pages.Add(1))))
			}))
			t.Cleanup(srv.Close)
			reg := NewRegistry(&config.Config{}, testClient())
			page := srv.URL + "/videos/sample-123.html"
			source := page
			if redirected {
				source = srv.URL + "/videos/old-999.html"
			}
			result, ex, err := reg.Extract(t.Context(), source, Options{})
			if err != nil || ex == nil || ex.Name() != "luxuretv" {
				t.Fatalf("extractor=%v err=%v", ex, err)
			}
			if len(result.Files) != 1 || result.Title != "A sample clip" {
				t.Fatalf("result = %+v", result)
			}
			f := result.Files[0]
			want := srv.URL + "/cf-stream/123/a/clip.mp4?md5=token%2B1&expires=1"
			if f.URL != want || f.Name != "clip.mp4" || f.Headers[httpx.HeaderReferer] != page || f.Resolve == nil || f.Refresh == nil || pages.Load() != 1 {
				t.Fatalf("file=%+v pages=%d", f, pages.Load())
			}
			target, err := f.Resolve(t.Context())
			if err != nil || target.URL != strings.ReplaceAll(want, "token%2B1", "token%2B2") || target.Headers[httpx.HeaderReferer] != page || pages.Load() != 2 {
				t.Fatalf("resolved=%+v pages=%d err=%v", target, pages.Load(), err)
			}
		})
	}
}

func TestLuxureTVMissingPlayer(t *testing.T) {
	srv, _ := mediaPageServer(t, map[string]sitePage{
		"/videos/sample-123.html": {kind: "text/html", body: strings.ReplaceAll(luxuretvFixture, `id="thisPlayer"`, `id="removed"`)},
	})
	reg := claimedRegistry(NewPlayer(testClient(), &luxuretvPlayer))
	_, ex, err := reg.Extract(t.Context(), srv.URL+"/videos/sample-123.html", Options{})
	if ex == nil || ex.Name() != "luxuretv" || err == nil || !strings.Contains(err.Error(), "no supported video player") {
		t.Fatalf("extractor=%v err=%v", ex, err)
	}
}
