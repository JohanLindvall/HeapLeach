// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"fmt"
	"html"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

// Hand-written media names and reserved URLs only; the saved browser page
// stays outside the repository. Escape the JSON just as an HTML attribute is
// escaped, including JSON's optional slash escaping inside the attribute.
const darkGramTestAlbum = `{"id":"album-one","title":"Internal album label","items":[
	{"id":"video-one","kind":"video","thumb_url":"/thumb/video.jpg","hls_url":"\/media\/video-one\/hls\/master.m3u8"},
	{"id":"photo-one","kind":"photo","thumb_url":"/thumb/photo.jpg","photo_url":"media/photo-one/photo"},
	{"id":"photo-two","kind":"photo","photo_url":"/media/photo-two.PNG?token=one&size=full"}
]}`

func darkGramTestPage(album string) string {
	return `<html><head><meta property="og:title" content="Morning Walk &amp; Field Notes | Example Site"></head><body>
	<div class="entry-content"><div class="darkgram-album" data-album="` + html.EscapeString(album) + `"></div>
	<div class="crp_related"><a href="/another-post/"><img src="/related.jpg"></a></div></div>
	<video src="/advert.mp4"></video></body></html>`
}

func TestDarkGramPageQueuesAlbumWithoutFetchingMedia(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/post.html", "/post/":
			http.Redirect(w, r, "/stories/post/", http.StatusFound)
		case "/stories/post/":
			w.Header().Set(httpx.HeaderContentType, "text/html; charset=utf-8")
			fmt.Fprint(w, darkGramTestPage(darkGramTestAlbum))
		default:
			t.Errorf("album discovery fetched media or navigation: %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	client := httpx.New("test-agent", "en-US", 0, time.Second)
	reg := NewRegistry(&config.Config{}, client)
	for _, page := range []string{"/post.html", "/post/"} {
		for _, direct := range []*Direct{reg.fallback.(*Direct), NewDirect(client)} {
			res, err := direct.Extract(context.Background(), mustParse(t, srv.URL+page), Options{})
			if err != nil {
				t.Fatal(err)
			}
			if res.Title != "Morning Walk & Field Notes" || len(res.Files) != 3 || res.Note != "" {
				t.Fatalf("did not resolve the embedded album: %+v", res)
			}
			for i, want := range []struct{ link, ext string }{
				{"/media/video-one/hls/master.m3u8", ".ts"},
				{"/stories/post/media/photo-one/photo", ".jpg"},
				{"/media/photo-two.PNG?token=one&size=full", ".png"},
			} {
				f := res.Files[i]
				if f.Name != fmt.Sprintf("Morning Walk & Field Notes - %03d%s", i+1, want.ext) || f.URL != srv.URL+want.link {
					t.Errorf("file %d: %+v", i, f)
				}
				if (f.Resolve != nil) != (i == 0) || len(f.Segments) != 0 || f.Size != -1 || f.External != "" {
					t.Errorf("file %d has the wrong transfer mode: %+v", i, f)
				}
				if f.Headers[httpx.HeaderReferer] != srv.URL+"/stories/post/" {
					t.Errorf("file %d lost the final page's referer: %v", i, f.Headers)
				}
			}
		}
	}
}

func TestDarkGramAlbumLimitsAndUnavailableItems(t *testing.T) {
	base := mustParse(t, "https://example.test/post/")
	for _, tc := range []struct {
		name, album, note string
		limit             int
		positions         []int
	}{
		{"all", darkGramTestAlbum, "", 0, []int{1, 2, 3}},
		{"cap", darkGramTestAlbum, "partial — file limit", 2, []int{1, 2}},
		{"exact cap", darkGramTestAlbum, "", 3, []int{1, 2, 3}},
		{"unavailable", `{"items":[
			{"kind":"video","thumb_url":"/thumb.jpg"},
			{"kind":"photo","photo_url":"javascript:alert(1)"},
			{"kind":"photo","photo_url":"data:image/jpeg;base64,example"},
			{"kind":"video","hls_url":"https:///missing-host"},
			{"kind":"photo","photo_url":"/original.jpg"}
		]}`, "4 album items unavailable", 0, []int{5}},
		{"duplicates", `{"items":[
			{"id":"one","kind":"video","hls_url":"/master.m3u8?token=one"},
			{"id":"one","kind":"video","hls_url":"/master.m3u8?token=two"},
			{"kind":"photo","photo_url":"/original.jpg"},
			{"kind":"photo","photo_url":"/original.jpg"}
		]}`, "", 2, []int{1, 3}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// A second player elsewhere on the page is not part of this album.
			doc := darkGramTestPage(tc.album) + darkGramTestPage(`{"items":[{"kind":"photo","photo_url":"/recommendation.jpg"}]}`)
			res, err := darkGramResult(nil, base, mustParseDoc(t, doc), Options{limits: Limits{Files: tc.limit}})
			if err != nil {
				t.Fatal(err)
			}
			if len(res.Files) != len(tc.positions) || res.Note != tc.note || res.Title != "Morning Walk & Field Notes" {
				t.Fatalf("result = %+v", res)
			}
			for i, position := range tc.positions {
				if !strings.HasPrefix(res.Files[i].Name, fmt.Sprintf("Morning Walk & Field Notes - %03d.", position)) {
					t.Errorf("unstable file position: %q", res.Files[i].Name)
				}
			}
		})
	}
}

func TestDarkGramRecognitionAndErrors(t *testing.T) {
	for _, tc := range []struct {
		name, doc, wantError string
	}{
		{"unrelated attribute", `<div data-album='{"items":[]}'></div>`, ""},
		{"class substring", `<div class="darkgram-album-preview" data-album='{}'></div>`, ""},
		{"malformed JSON", darkGramTestPage(`{"items":`), "invalid album data"},
		{"missing attribute", `<div class="darkgram-album"></div>`, "invalid album data"},
		{"empty album", darkGramTestPage(`{"items":[]}`), "no downloadable media"},
		{"thumbnail only", darkGramTestPage(`{"items":[{"kind":"photo","thumb_url":"/thumb.jpg"}]}`), "no downloadable media"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, err := darkGramResult(nil, mustParse(t, "https://example.test/post/"), mustParseDoc(t, tc.doc), Options{})
			if res != nil || (tc.wantError == "" && err != nil) || (tc.wantError != "" && (err == nil || !strings.Contains(err.Error(), tc.wantError))) {
				t.Fatalf("result = %+v, error = %v; want %q", res, err, tc.wantError)
			}
		})
	}

	// Once the player is recognised, a bad album must not fall through to
	// either a preview clip or an apparently successful HTML download.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set(httpx.HeaderContentType, "text/html")
		fmt.Fprint(w, darkGramTestPage(`{broken`))
	}))
	defer srv.Close()
	direct := NewDirect(httpx.New("test-agent", "en-US", 0, time.Second))
	if res, err := direct.Extract(context.Background(), mustParse(t, srv.URL+"/post.html"), Options{}); res != nil || err == nil {
		t.Fatalf("broken player fell through: %+v, %v", res, err)
	}
}

func TestDarkGramVideoRefreshesTheBestPlaylistAtEachAttempt(t *testing.T) {
	var calls atomic.Int32
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(httpx.HeaderReferer) != srv.URL+"/post/" {
			t.Errorf("playlist lost the page referer: %v", r.Header)
		}
		switch r.URL.Path {
		case "/media/video-one/hls/master.m3u8":
			n := calls.Add(1)
			fmt.Fprintf(w, "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=500000,RESOLUTION=640x360\nlow.m3u8\n"+
				"#EXT-X-STREAM-INF:BANDWIDTH=3000000,RESOLUTION=1920x1080\nhigh.m3u8?token=%d\n", n)
		case "/media/video-one/hls/high.m3u8":
			token := r.URL.Query().Get("token")
			fmt.Fprintf(w, "#EXTM3U\n#EXT-X-KEY:METHOD=AES-128,URI=\"key?token=%s\"\n"+
				"#EXTINF:4,\none.ts?token=%s\n#EXTINF:4,\ntwo.ts?token=%s\n#EXT-X-ENDLIST\n", token, token, token)
		default:
			t.Errorf("resolver fetched a lower rendition, segment, key or photo: %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	client := httpx.New("test-agent", "en-US", 0, time.Second)
	res, err := darkGramResult(client, mustParse(t, srv.URL+"/post/"), mustParseDoc(t, darkGramTestPage(darkGramTestAlbum)), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 0 {
		t.Fatal("playlist was read before the file's turn")
	}
	for attempt := 1; attempt <= 2; attempt++ {
		target, err := res.Files[0].Resolve(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if target.Name != res.Files[0].Name || target.Size != -1 || len(target.Segments) != 2 || calls.Load() != int32(attempt) {
			t.Fatalf("target = %+v, playlist reads = %d", target, calls.Load())
		}
		for i, part := range []string{"one.ts", "two.ts"} {
			if target.Segments[i] != fmt.Sprintf("%s/media/video-one/hls/%s?token=%d", srv.URL, part, attempt) {
				t.Errorf("stale or wrong segment: %s", target.Segments[i])
			}
		}
		if target.SegmentKey == nil || target.SegmentKey.URI != fmt.Sprintf("%s/media/video-one/hls/key?token=%d", srv.URL, attempt) {
			t.Errorf("stale or missing key: %+v", target.SegmentKey)
		}
		if target.Headers[httpx.HeaderReferer] != srv.URL+"/post/" {
			t.Errorf("target lost referer: %v", target.Headers)
		}
	}
}

func TestDarkGramVideoContainerAndPlaylistFailures(t *testing.T) {
	for _, tc := range []struct {
		name, playlist, wantError string
		status                    int
	}{
		{"fragmented MP4", "#EXTM3U\n#EXT-X-MAP:URI=\"init.mp4\"\n#EXTINF:4,\npart.m4s\n#EXT-X-ENDLIST\n", "", 200},
		{"live", "#EXTM3U\n#EXTINF:4,\npart.ts\n", "still being written", 200},
		{"HTML", `<html>Unavailable</html>`, "not an HLS playlist", 200},
		{"removed", "", "404", 404},
		{"separate audio", "#EXTM3U\n#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=\"audio\",URI=\"audio.m3u8\"\n" +
			"#EXT-X-STREAM-INF:BANDWIDTH=3000000,AUDIO=\"audio\"\nvideo.m3u8\n", "no rendition that carries its own audio", 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/master.m3u8" {
					t.Errorf("unexpected fetch: %s", r.URL.Path)
				}
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.playlist)
			}))
			defer srv.Close()
			client := httpx.New("test-agent", "en-US", 0, time.Second)
			res, err := darkGramResult(client, mustParse(t, srv.URL+"/post/"), mustParseDoc(t,
				darkGramTestPage(`{"items":[{"kind":"video","hls_url":"/master.m3u8"}]}`)), Options{})
			if err != nil {
				t.Fatal(err)
			}
			target, err := res.Files[0].Resolve(context.Background())
			if tc.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantError) || target != nil {
					t.Fatalf("target = %+v, error = %v; want %q", target, err, tc.wantError)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if target.Name != "Morning Walk & Field Notes - 001.mp4" || len(target.Segments) != 2 || target.Segments[0] != srv.URL+"/init.mp4" {
				t.Fatalf("fragmented MP4 was not kept intact: %+v", target)
			}
		})
	}
}
