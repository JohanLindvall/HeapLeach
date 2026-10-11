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

func TestKnuthingRefreshesPlayerPlaylistAndKey(t *testing.T) {
	var visits, players atomic.Int32
	var origin string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/watch/start.html":
			http.Redirect(w, r, "/watch/first.html?keep=yes", http.StatusFound)
		case "/watch/first.html":
			if r.URL.Query().Get("keep") != "yes" {
				t.Error("lost watch query")
			}
			if r.URL.Query().Get("limitprompt") != "true" {
				fmt.Fprint(w, `<a href="/watch/other.html?limitprompt=true">Another video</a>
				<a href="/watch/first.html?keep=yes&amp;limitprompt=true">Watch for free</a>`)
				return
			}
			n := visits.Add(1)
			fmt.Fprintf(w, `<title>First Clip - Part 2 &amp; Friends</title>
			<iframe src="/advert"></iframe>
			<iframe class="responsive bunnyframe" src="/player?token=%d&amp;expires=123"></iframe>
			<iframe class="bunnyframe" src="/wrong-player"></iframe>`, n)
		case "/player":
			players.Add(1)
			if r.Referer() != origin+"/watch/first.html?keep=yes&limitprompt=true" || r.URL.Query().Get("expires") != "123" {
				t.Errorf("player lost final watch referer or signature: %s, %s", r.Referer(), r.URL)
			}
			http.Redirect(w, r, "/current/player?token="+r.URL.Query().Get("token"), http.StatusFound)
		case "/current/player":
			fmt.Fprintf(w, `<video src="/advert.mp4"></video>
			<bunny-stream-video drm-version="2" content-src="/master.m3u8?token=%s">
			<track src="/thumbnails.vtt"></bunny-stream-video>`, r.URL.Query().Get("token"))
		case "/master.m3u8", "/1080p/video.m3u8":
			token := r.URL.Query().Get("token")
			if r.Referer() != origin+"/current/player?token="+token {
				t.Errorf("playlist lost final player referer: %s", r.Referer())
			}
			if r.URL.Path == "/master.m3u8" {
				fmt.Fprintf(w, "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1000,RESOLUTION=640x360,CODECS=\"avc1.4d401e,mp4a.40.2\"\n/low.m3u8\n#EXT-X-STREAM-INF:BANDWIDTH=8000,RESOLUTION=1920x1080,CODECS=\"avc1.64002a,mp4a.40.2\"\n1080p/video.m3u8?token=%s\n", token)
			} else {
				fmt.Fprintf(w, "#EXTM3U\n#EXT-X-MEDIA-SEQUENCE:8\n#EXT-X-KEY:METHOD=AES-128,URI=\"/key-%s\"\n#EXTINF:4,\nvideo0.dts?token=%s\n#EXT-X-ENDLIST\n", token, token)
			}
		default:
			t.Errorf("unexpected request: %s", r.URL)
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	origin = srv.URL
	client := httpx.New("test", "en", 0, time.Second)
	defer client.CloseIdleConnections()
	k := NewKnuthing(client)
	res, err := k.Extract(context.Background(), mustParse(t, origin+"/watch/start.html"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Title != "First Clip - Part 2 & Friends" || len(res.Files) != 1 {
		t.Fatalf("metadata: %+v", res)
	}
	f := res.Files[0]
	if f.Name != res.Title+".ts" || f.URL != "" || f.Size != -1 || f.Resolve == nil || f.Refresh == nil || len(f.Segments) != 0 || players.Load() != 0 {
		t.Fatalf("discovery spent the player signature or lost its resolver: %+v", f)
	}
	for _, token := range []string{"2", "3"} {
		target, err := f.Resolve(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if target.Name != f.Name || target.Size != -1 || len(target.Segments) != 1 ||
			target.Segments[0] != origin+"/1080p/video0.dts?token="+token || target.SegmentKey == nil ||
			target.SegmentKey.URI != origin+"/key-"+token || target.SegmentKey.Sequence != 8 ||
			target.Headers[httpx.HeaderReferer] != origin+"/current/player?token="+token {
			t.Fatalf("stale rendition, key, title or referer: %+v", target)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := f.Resolve(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation: %v", err)
	}
}

func TestKnuthingUnavailablePlayersFail(t *testing.T) {
	for _, tc := range []struct {
		name, watch, player, playlist, want string
	}{
		{name: "restricted", watch: `<a href="/login">Sign in</a><iframe src="/preview"></iframe>`, want: "no accessible video player"},
		{name: "login-required", watch: `<div class="player-video"><div class="video-processing"><h5><a href="/login">Please login to watch this video</a></h5></div></div>`, want: "Please login to watch this video"},
		{name: "foreign-confirmation", watch: `<a href="https://other.example.test/watch/test.html?limitprompt=true">Watch</a>`, want: "no accessible video player"},
		{name: "bad-frame", watch: `<iframe class="bunnyframe" src="javascript:play()"></iframe>`, want: "no usable embed"},
		{name: "missing-player", player: `<meta property="og:video" content="/preview.mp4"><track src="/thumbnails.vtt">`, want: "no Bunny Stream video"},
		{name: "bad-source", player: `<bunny-stream-video content-src="blob:local" cast-src="/preview.mp4"></bunny-stream-video>`, want: "no HLS source"},
		{name: "html-playlist", playlist: `<html>Not available</html>`, want: "not an HLS playlist"},
		{name: "unfinished-stream", playlist: "#EXTM3U\n#EXTINF:4,\npart.ts\n", want: "still being written"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/watch/test.html":
					if tc.watch != "" {
						fmt.Fprint(w, tc.watch)
					} else {
						fmt.Fprint(w, `<title>Synthetic Clip</title><iframe class="bunnyframe" src="/player"></iframe>`)
					}
				case "/player":
					if tc.player != "" {
						fmt.Fprint(w, tc.player)
					} else {
						fmt.Fprint(w, `<bunny-stream-video content-src="/playlist.m3u8"></bunny-stream-video>`)
					}
				case "/playlist.m3u8":
					fmt.Fprint(w, tc.playlist)
				default:
					t.Errorf("unexpected request: %s", r.URL)
					http.NotFound(w, r)
				}
			}))
			defer srv.Close()
			k := NewKnuthing(httpx.New("test", "en", 0, time.Second))
			res, err := k.Extract(context.Background(), mustParse(t, srv.URL+"/watch/test.html"), Options{})
			if err == nil {
				_, err = res.Files[0].Resolve(context.Background())
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want %s", err, tc.want)
			}
		})
	}
}

func TestKnuthingPreservesChallenges(t *testing.T) {
	for _, stage := range []string{"page", "player"} {
		t.Run(stage, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if stage == "player" && r.URL.Path == "/watch/test.html" {
					fmt.Fprint(w, `<iframe class="bunnyframe" src="/player"></iframe>`)
					return
				}
				w.Header().Set("cf-mitigated", "challenge")
				w.WriteHeader(http.StatusForbidden)
			}))
			defer srv.Close()
			k := NewKnuthing(httpx.New("test", "en", 0, time.Second))
			_, err := k.resolve(context.Background(), srv.URL+"/watch/test.html")
			if _, ok := errors.AsType[*httpx.ChallengeError](err); !ok {
				t.Fatalf("challenge lost: %v", err)
			}
		})
	}
}
