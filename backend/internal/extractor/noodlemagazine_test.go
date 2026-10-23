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

func noodleTestClient(t *testing.T) *httpx.Client {
	t.Helper()
	client := httpx.New("test", "en", 0, time.Second)
	t.Cleanup(client.CloseIdleConnections)
	return client
}

func TestNoodleMagazineRouting(t *testing.T) {
	reg := testRegistry(t, nil)
	for _, host := range []string{"noodlemagazine.com", "www.noodlemagazine.com", "adult.noodlemagazine.com"} {
		if got := reg.Find(mustParse(t, "https://"+host+"/")).Name(); got != "noodlemagazine" {
			t.Errorf("%s routes to %s", host, got)
		}
	}
	n := NewNoodleMagazine(nil)
	if n.Match(mustParse(t, "https://noodlemagazine.com.example.test/")) {
		t.Fatal("matched a different host")
	}
	for _, path := range []string{"/", "/video/", "/watch/", "/watch/not-an-id", "/other/search"} {
		if _, err := n.Extract(context.Background(), mustParse(t, "https://example.test"+path), Options{}); err == nil {
			t.Errorf("accepted unsupported path %s", path)
		}
	}
}

func TestNoodleMagazineRefreshesBestSourceAndFinalReferer(t *testing.T) {
	var visits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/watch/-1_1":
			token := visits.Add(1)
			http.Redirect(w, r, fmt.Sprintf("/watch/-1_2?visit=%d", token), http.StatusFound)
		case "/watch/-1_2":
			fmt.Fprintf(w, `<meta property="og:title" content="Harbour Walk - Part 2 &amp; Friends">
			<video src="/advert.mp4"></video><div data-trailer_url="/preview.mp4"></div>
			<script>window.playlist = {"sources":[
			{"file":"/high.m3u8","label":"1080","type":"hls"},
			{"file":"../media/harbour.mp4?token=%s\u0026expires=2000","label":1080,"type":"mp4"},
			{"file":"/default.mp4","label":"480","type":"mp4","default":true},
			{"file":"javascript:play()","label":"2160","type":"mp4"}]}; window.ads=["/advert.mp4"];</script>`, r.URL.Query().Get("visit"))
		default:
			t.Errorf("discovery fetched something besides the watch page: %s", r.URL)
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	n := NewNoodleMagazine(noodleTestClient(t))
	res, err := n.Extract(context.Background(), mustParse(t, srv.URL+"/watch/-1_1"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Title != "Harbour Walk - Part 2 & Friends" || len(res.Files) != 1 {
		t.Fatalf("metadata: %+v", res)
	}
	f := res.Files[0]
	if f.Name != res.Title+".mp4" || f.URL != "" || f.Resolve == nil || f.Refresh == nil || visits.Load() != 1 {
		t.Fatalf("signed media was not deferred: %+v, visits=%d", f, visits.Load())
	}
	for index, resolve := range []func(context.Context) (*Target, error){f.Resolve, f.Refresh} {
		target, err := resolve(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		token := index + 2
		if target.URL != fmt.Sprintf("%s/media/harbour.mp4?token=%d&expires=2000", srv.URL, token) ||
			target.Headers[httpx.HeaderReferer] != fmt.Sprintf("%s/watch/-1_2?visit=%d", srv.URL, token) ||
			target.Name != f.Name || target.Size != -1 || len(target.Segments) != 0 {
			t.Fatalf("wrong rendition, stale signature or referer: %+v", target)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := f.Resolve(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("lost cancellation: %v", err)
	}
}

func TestNoodleMagazineRefreshesHLSWithKeysAndTracks(t *testing.T) {
	var visits, playlists atomic.Int32
	var origin string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/watch/1_2" {
			token := visits.Add(1)
			fmt.Fprintf(w, `<h1>Harbour Lights</h1><script>window.playlist={"sources":[
			{"file":"/low.mp4","label":"720p","type":"mp4"},
			{"file":"/master.m3u8?token=%d","label":"1080p","type":"hls"}]};</script>`, token)
			return
		}
		playlists.Add(1)
		if r.Referer() != origin+"/watch/1_2" {
			t.Errorf("playlist lost referer: %s", r.Referer())
		}
		token := r.URL.Query().Get("token")
		switch r.URL.Path {
		case "/master.m3u8":
			fmt.Fprintf(w, "#EXTM3U\n#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=\"audio\",NAME=\"English\",LANGUAGE=\"en\",DEFAULT=YES,URI=\"/audio.m3u8?token=%[1]s\"\n#EXT-X-MEDIA:TYPE=SUBTITLES,GROUP-ID=\"subs\",NAME=\"English\",LANGUAGE=\"en\",URI=\"/subs.m3u8?token=%[1]s\"\n#EXT-X-STREAM-INF:BANDWIDTH=8000,RESOLUTION=1920x1080,AUDIO=\"audio\",SUBTITLES=\"subs\"\n/video.m3u8?token=%[1]s\n", token)
		case "/video.m3u8":
			fmt.Fprintf(w, "#EXTM3U\n#EXT-X-MEDIA-SEQUENCE:3\n#EXT-X-KEY:METHOD=AES-128,URI=\"/key?token=%[1]s\"\n#EXTINF:4,\n/part.ts?token=%[1]s\n#EXT-X-ENDLIST\n", token)
		case "/audio.m3u8":
			fmt.Fprintf(w, "#EXTM3U\n#EXTINF:4,\n/audio.aac?token=%s\n#EXT-X-ENDLIST\n", token)
		case "/subs.m3u8":
			fmt.Fprintf(w, "#EXTM3U\n#EXTINF:4,\n/subs.vtt?token=%s\n#EXT-X-ENDLIST\n", token)
		default:
			t.Errorf("unexpected request: %s", r.URL)
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	origin = srv.URL
	n := NewNoodleMagazine(noodleTestClient(t))
	res, err := n.Extract(context.Background(), mustParse(t, origin+"/watch/1_2"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if playlists.Load() != 0 {
		t.Fatal("discovery spent a playlist signature")
	}
	for _, token := range []string{"2", "3"} {
		target, err := res.Files[0].Resolve(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if target.Name != "Harbour Lights.mp4" || len(target.Segments) != 1 ||
			target.Segments[0] != origin+"/part.ts?token="+token || target.SegmentKey == nil ||
			target.SegmentKey.URI != origin+"/key?token="+token || target.SegmentKey.Sequence != 3 ||
			len(target.Audio) != 1 || len(target.Subtitles) != 1 ||
			target.Audio[0].Segments[0] != origin+"/audio.aac?token="+token ||
			target.Subtitles[0].Segments[0] != origin+"/subs.vtt?token="+token {
			t.Fatalf("stale or missing HLS metadata: %+v", target)
		}
	}
}

func TestNoodleMagazineRefusesUnavailablePlayers(t *testing.T) {
	for _, tc := range []struct{ name, doc, want string }{
		{"missing", `<video src="/preview.mp4"></video>`, "no player"},
		{"malformed", `<script>window.playlist={broken};</script>`, "read player sources"},
		{"empty", `<script>window.playlist={"sources":[]};</script>`, "no usable"},
		{"blank", `<script>window.playlist={"sources":[{"file":" ","type":"mp4"}]};</script>`, "no usable"},
		{"local", `<script>window.playlist={"sources":[{"file":"file:///video.mp4","type":"mp4"}]};</script>`, "no usable"},
		{"dash", `<script>window.playlist={"sources":[{"file":"/video.mpd","type":"dash"}]};</script>`, "no usable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, tc.doc) }))
			defer srv.Close()
			_, err := NewNoodleMagazine(noodleTestClient(t)).Extract(context.Background(), mustParse(t, srv.URL+"/watch/1_2"), Options{})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %s", err, tc.want)
			}
		})
	}
}
