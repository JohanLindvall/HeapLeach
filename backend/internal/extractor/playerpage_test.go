// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

// Rows whose page is all they read go through the engine's one page
// acquisition: through the registry, the probe's page and tree are read,
// so a video costs the one request recognition made, and the result is
// what the row's parser reads off the page — title, name, link, referer.
func TestPlayerPageRowsReadTheProbedPage(t *testing.T) {
	const xhamsterPage = `<html><head><title>A Synthetic Clip | xHamster</title>
<meta property="og:site_name" content="xHamster"></head><body>
<script>window.initials = {}; var xplayerSettings = {};</script>
<a href="https://video.xhcdn.com/key=AAA/720p.h264.mp4">720p</a>
<a href="https://video.xhcdn.com/key=AAA/480p.h264.mp4">480p</a>
</body></html>`
	const tnaflixPage = `<html><head><title>Another Clip - TNAFlix</title></head><body>
<video><source src="https://cdn.tnaflix.com/a/1080p.mp4" type="video/mp4"></video>
</body></html>`
	pornonePage := strings.Replace(pornonePlayerPage, "<html>", `<html><head><title>PornOne - x</title></head>`, 1)
	for _, tc := range []struct {
		name, path, page string
		row              *playerSite
		title, url, ref  string
	}{
		{"xhamster", "/videos/a-synthetic-clip-AbC12", xhamsterPage, &xhamsterPlayer,
			"A Synthetic Clip", "https://video.xhcdn.com/key=AAA/720p.h264.mp4", xhamsterRoot + "/"},
		{"tnaflix", "/category/a-clip/video12345", tnaflixPage, &tnaflixPlayer,
			"Another Clip", "https://cdn.tnaflix.com/a/1080p.mp4", tnaflixRoot + "/"},
		{"pornone", "/funny/a-clip/12345678/", pornonePage, &pornonePlayer,
			"a synthetic clip", "https://s1.example.test/vid2/TOKEN/1787378460/21/1/1_1920x1080_4000k.mp4?lang=en", pornoneRoot + "/"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var hits atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				hits.Add(1)
				w.Header().Set(httpx.HeaderContentType, "text/html")
				_, _ = io.WriteString(w, tc.page)
			}))
			t.Cleanup(srv.Close)
			client := testClient()
			player := NewPlayer(client, tc.row)
			reg := &Registry{client: client, extractors: []Extractor{player}, fallback: NewDirect(client)}
			res, ex, err := reg.Extract(context.Background(), srv.URL+tc.path, Options{})
			if err != nil || ex != player || len(res.Files) != 1 {
				t.Fatalf("result=%+v extractor=%v err=%v", res, ex, err)
			}
			f := res.Files[0]
			if res.Title != tc.title || f.Name != tc.title+".mp4" || f.URL != tc.url || f.Headers[httpx.HeaderReferer] != tc.ref {
				t.Errorf("title %q, file %q at %q with referer %q", res.Title, f.Name, f.URL, f.Headers[httpx.HeaderReferer])
			}
			if f.Resolve == nil {
				t.Error("a signed row's file lost its resolver")
			}
			if n := hits.Load(); n != 1 {
				t.Errorf("%d requests, want the probe alone", n)
			}
		})
	}
}
