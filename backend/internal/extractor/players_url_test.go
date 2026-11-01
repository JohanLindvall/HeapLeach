// SPDX-License-Identifier: MIT

package extractor

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

func TestPlaylistLinkUsesDecodedPathSuffix(t *testing.T) {
	for _, tc := range []struct {
		name, link string
		want       bool
	}{
		{"plain playlist", "https://media.example.test/master.m3u8", true},
		{"uppercase suffix", "https://media.example.test/MASTER.M3U8?sig=a%2Bb#player", true},
		{"encoded dot", "https://media.example.test/master%2Em3u8", true},
		{"encoded extension", "https://media.example.test/master.%6d%33%75%38", true},
		{"relative link", "../hls/master%2em3u8", true},
		{"protocol relative link", "//media.example.test/master.m3u8", true},
		{"playlist with file query", "https://media.example.test/master.m3u8?file=clip.mp4", true},
		{"progressive filename", "https://media.example.test/Synthetic.m3u8.mp4", false},
		{"directory name", "https://media.example.test/hls.m3u8/clip.mp4", false},
		{"query value", "https://media.example.test/clip.mp4?playlist=master.m3u8", false},
		{"fragment value", "https://media.example.test/clip.mp4#master.m3u8", false},
		{"host label", "https://edge.m3u8.example.test/clip.mp4", false},
		{"trailing slash", "https://media.example.test/master.m3u8/", false},
		{"decoded only once", "https://media.example.test/master%252Em3u8", false},
		{"malformed escape", "https://media.example.test/bad%zz/master.m3u8", false},
		{"malformed port", "https://media.example.test:bad/master.m3u8", false},
		{"empty", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := isPlaylistLink(tc.link); got != tc.want {
				t.Errorf("isPlaylistLink(%q) = %v, want %v", tc.link, got, tc.want)
			}
		})
	}
}

func TestVidsClassifiesMediaByDecodedPath(t *testing.T) {
	for _, tc := range []struct {
		name, link, filename string
		hls                  bool
	}{
		{"progressive filename", "/media/Synthetic.m3u8.mp4", "Synthetic.m3u8.mp4", false},
		{"extensionless original", "/media/opaque-token", "Synthetic recording", false},
		{"dot-prefixed original", "/media/opaque-token", ".Synthetic", false},
		{"progressive directory", "/media/hls.m3u8/Synthetic.mp4", "Synthetic.mp4", false},
		{"progressive query", "/media/Synthetic.mp4?file=master.m3u8&sig=a%2Bb", "Synthetic.mp4", false},
		{"progressive fragment", "/media/Synthetic.mp4#master.m3u8", "Synthetic.mp4", false},
		{"encoded progressive path", "/media/Synthetic%2Em3u8%2Emp4", "Synthetic.mp4", false},
		{"encoded playlist dot", "/media/master%2Em3u8?sig=a%2Bb&part=2&part=1", "Synthetic.mp4", true},
		{"extensionless playlist title", "/media/master.m3u8", "Synthetic", true},
		{"encoded playlist extension", "/media/master.%6d%33%75%38", "Synthetic.mp4", true},
		{"uppercase playlist", "/media/MASTER.M3U8?file=clip.mp4#player", "Synthetic.mp4", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var pages, playlists atomic.Int32
			requestURI, _, _ := strings.Cut(tc.link, "#")
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet && r.URL.Path == "/v/synthetic" {
					pages.Add(1)
					w.Header().Set(httpx.HeaderContentType, "text/html")
					fmt.Fprintf(w, `<script>var playerConfig = {"videoUrl":%q,"videoName":%q};</script>`, tc.link, tc.filename)
					return
				}
				// Only an advertised playlist can be fetched. Progressive
				// files and playlist segments must wait for the downloader.
				if !tc.hls || r.Method != http.MethodGet || r.RequestURI != requestURI {
					t.Errorf("unexpected media request: %s %s", r.Method, r.RequestURI)
					http.NotFound(w, r)
					return
				}
				playlists.Add(1)
				fmt.Fprint(w, "#EXTM3U\n#EXT-X-TARGETDURATION:4\n#EXTINF:4,\npart.ts?sig=segment%2Btoken\n#EXT-X-ENDLIST\n")
			}))
			defer srv.Close()
			page := srv.URL + "/v/synthetic"
			res, err := NewPlayer(testClient(), &vidsPlayer).Extract(t.Context(), mustParse(t, page), Options{})
			if err != nil {
				t.Fatal(err)
			}
			if len(res.Files) != 1 {
				t.Fatalf("got %d files, want one", len(res.Files))
			}
			file := res.Files[0]
			name, segments := tc.filename, 0
			if tc.hls {
				name, segments = "Synthetic.ts", 1
			}
			if file.Name != name || file.URL != srv.URL+tc.link || len(file.Segments) != segments {
				t.Errorf("name %q, URL %q, segments %d; want %q, unchanged URL %q, %d segments", file.Name, file.URL, len(file.Segments), name, srv.URL+tc.link, segments)
			}
			if tc.hls && len(file.Segments) == 1 && file.Segments[0] != srv.URL+"/media/part.ts?sig=segment%2Btoken" {
				t.Errorf("segment URL = %q", file.Segments[0])
			}
			if got := file.Headers[httpx.HeaderReferer]; got != page {
				t.Errorf("Referer = %q, want %q", got, page)
			}
			if pages.Load() != 1 || int(playlists.Load()) != segments {
				t.Errorf("page requests %d, playlist requests %d; want 1 and %d", pages.Load(), playlists.Load(), segments)
			}
		})
	}
}
