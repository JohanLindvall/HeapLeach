// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
)

func TestVimeoVideoID(t *testing.T) {
	tests := []struct {
		raw      string
		wantID   string
		wantHash string
	}{
		{raw: "https://vimeo.com/123456789", wantID: "123456789"},
		{raw: "https://vimeo.com/123456789?foo=bar", wantID: "123456789"},
		// The second segment of an unlisted link is its privacy hash.
		{raw: "https://vimeo.com/123456789/abcdef1234", wantID: "123456789", wantHash: "abcdef1234"},
		{raw: "https://player.vimeo.com/video/123456789", wantID: "123456789"},
		{raw: "https://player.vimeo.com/video/123456789?h=abcdef1234", wantID: "123456789", wantHash: "abcdef1234"},
		{raw: "https://vimeo.com/channels/staffpicks/123456789", wantID: "123456789"},
		{raw: "https://vimeo.com/groups/agroup/videos/123456789", wantID: "123456789"},
		{raw: "https://vimeo.com/album/987654/video/123456789", wantID: "123456789"},
		// Nothing numeric to take: a channel or a profile, not a video.
		{raw: "https://vimeo.com/channels/staffpicks"},
		{raw: "https://vimeo.com/someuser"},
		{raw: "https://vimeo.com/"},
	}
	for _, tc := range tests {
		u, err := ParseURL(tc.raw)
		if err != nil {
			t.Fatal(err)
		}
		id, hash := vimeoVideoID(u)
		if id != tc.wantID || hash != tc.wantHash {
			t.Errorf("vimeoVideoID(%s) = %q, %q; want %q, %q", tc.raw, id, hash, tc.wantID, tc.wantHash)
		}
	}
}

// TestVimeoPlayerURL pins the one thing that makes this host work at all:
// the request goes to the embed player, not to the watch page.
func TestVimeoPlayerURL(t *testing.T) {
	if got, want := vimeoPlayerURL("123456789", ""), "https://player.vimeo.com/video/123456789"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if got, want := vimeoPlayerURL("123456789", "abcdef1234"),
		"https://player.vimeo.com/video/123456789?h=abcdef1234"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// vimeoServer stands in for the player pages and the CDN behind them. Each
// page is a configuration, with {{base}} standing for the server's own URL;
// the playlists are one master naming a video, an audio group and a
// subtitle group, and their media playlists, the video's encrypted as the
// sample-aes page asks.
func vimeoServer(t *testing.T, pages map[string]string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var reads atomic.Int32
	var srv *httptest.Server
	media := func(w http.ResponseWriter, prefix, key string) {
		_, _ = io.WriteString(w, "#EXTM3U\n#EXT-X-TARGETDURATION:6\n"+key+`#EXT-X-MAP:URI="`+prefix+`-init.mp4"`+
			"\n#EXTINF:6,\n"+prefix+"-1.m4s\n#EXTINF:6,\n"+prefix+"-2.m4s\n#EXT-X-ENDLIST\n")
	}
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch path := r.URL.Path; {
		case strings.HasPrefix(path, "/video/"):
			page, ok := pages[strings.TrimPrefix(path, "/video/")]
			if !ok {
				http.Error(w, "Sorry", http.StatusForbidden)
				return
			}
			reads.Add(1)
			_, _ = io.WriteString(w, "<!doctype html><html><body><script>window.playerConfig = "+
				strings.ReplaceAll(page, "{{base}}", srv.URL)+"; var started = true;</script></body></html>")
		case path == "/hls/master.m3u8", path == "/drm/master.m3u8":
			_, _ = io.WriteString(w, "#EXTM3U\n"+
				`#EXT-X-MEDIA:TYPE=SUBTITLES,GROUP-ID="subs",NAME="English",LANGUAGE="en",URI="subs.m3u8"`+"\n"+
				`#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="audio-high",NAME="English",DEFAULT=YES,LANGUAGE="en",URI="audio.m3u8"`+"\n"+
				`#EXT-X-STREAM-INF:SUBTITLES="subs",BANDWIDTH=3063040,RESOLUTION=1280x720,CODECS="avc1.64001F,mp4a.40.2",AUDIO="audio-high"`+
				"\nvideo.m3u8\n")
		case path == "/hls/video.m3u8":
			media(w, "video", "")
		case path == "/drm/video.m3u8":
			media(w, "video", `#EXT-X-KEY:METHOD=SAMPLE-AES,URI="skd://drm",KEYFORMAT="com.apple.streamingkeydelivery"`+"\n")
		case strings.HasSuffix(path, "/audio.m3u8"):
			media(w, "audio", "")
		case strings.HasSuffix(path, "/subs.m3u8"):
			_, _ = io.WriteString(w, "#EXTM3U\n#EXT-X-TARGETDURATION:62\n#EXTINF:61.8,\nen.vtt\n#EXT-X-ENDLIST\n")
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &reads
}

// vimeoPage is a player configuration with the given request part.
func vimeoPage(view int, title, request string) string {
	return fmt.Sprintf(`{"view": %d, "video": {"id": 111, "title": %q, "live_event": null}, "request": %s}`,
		view, title, request)
}

func vimeoWith(srv *httptest.Server) *Vimeo {
	v := NewVimeo(testClient())
	v.player = srv.URL + "/video/"
	return v
}

// A video is read off its player page: the HLS master the configuration
// names, AVC first and on its default CDN, with the audio and subtitles the
// master keeps apart. The page is read again for fresh links when the
// transfer starts.
func TestVimeoReadsThePlayersConfiguration(t *testing.T) {
	srv, reads := vimeoServer(t, map[string]string{
		"111": vimeoPage(1, "An Invented Film", `{"drm": null, "files": {
			"hls": {"default_cdn": "second", "cdns": {
				"first": {"url": "{{base}}/elsewhere/master.m3u8"},
				"second": {"url": "{{base}}/elsewhere/master.m3u8", "avc_url": "{{base}}/hls/master.m3u8"}}},
			"progressive": [{"url": "{{base}}/file-360.mp4", "height": 360}]}}`),
	})
	u, _ := url.Parse("https://vimeo.com/111")
	res, err := vimeoWith(srv).Extract(context.Background(), u, Options{})
	if err != nil {
		t.Fatal(err)
	}
	f := res.Files[0]
	if res.Title != "An Invented Film" || f.Name != "An Invented Film.mp4" || f.URL != srv.URL+"/hls/master.m3u8" {
		t.Fatalf("result %q, file %+v", res.Title, f)
	}
	if len(f.Segments) != 3 || len(f.Audio) != 1 || f.Audio[0].Name != "English" || len(f.Subtitles) != 1 ||
		f.Subtitles[0].Segments[0] != srv.URL+"/hls/en.vtt" {
		t.Fatalf("segments %d, audio %+v, subtitles %+v", len(f.Segments), f.Audio, f.Subtitles)
	}

	target, err := f.Resolve(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if reads.Load() != 2 || len(target.Segments) != 3 || len(target.Audio) != 1 || len(target.Subtitles) != 1 {
		t.Errorf("after %d reads, target %+v; want the page read again, every track with it", reads.Load(), target)
	}
}

// Where the HLS cannot be read, the tallest progressive MP4 is taken, and
// a transfer re-reading the page for fresh links takes it again.
func TestVimeoFallsBackToAProgressiveFile(t *testing.T) {
	srv, _ := vimeoServer(t, map[string]string{
		"222": vimeoPage(1, "Another Invented Film", `{"drm": null, "files": {
			"hls": {"default_cdn": "first", "cdns": {"first": {"url": "{{base}}/gone/master.m3u8"}}},
			"progressive": [{"url": "{{base}}/file-360.mp4", "height": 360}, {"url": "{{base}}/file-720.mp4", "height": 720}]}}`),
	})
	u, _ := url.Parse("https://player.vimeo.com/video/222")
	res, err := vimeoWith(srv).Extract(context.Background(), u, Options{})
	if err != nil {
		t.Fatal(err)
	}
	f := res.Files[0]
	if f.URL != srv.URL+"/file-720.mp4" || len(f.Segments) != 0 || f.Name != "Another Invented Film.mp4" {
		t.Fatalf("file = %+v; want the 720p progressive file", f)
	}
	target, err := f.Resolve(context.Background())
	if err != nil || target.URL != srv.URL+"/file-720.mp4" {
		t.Errorf("target %+v, %v", target, err)
	}
}

// A video DRM-protected in every form is refused in so many words, as is one
// only its owner can play.
func TestVimeoRefusesWhatCannotBeFetched(t *testing.T) {
	srv, _ := vimeoServer(t, map[string]string{
		"333": vimeoPage(1, "A Protected Film", `{"drm": {"asset": "an-invented-asset"}, "files": {
			"hls": {"default_cdn": "first", "cdns": {"first": {"avc_url": "{{base}}/drm/master.m3u8"}}},
			"progressive": []}}`),
		"444": vimeoPage(2, "", `{"files": {}}`),
	})
	for id, want := range map[string]string{
		"333": "DRM-protected in every form",
		"444": errVimeoNotPublic.Error(),
		"555": errVimeoNotPublic.Error(), // answered 403
	} {
		u, _ := url.Parse("https://vimeo.com/" + id)
		_, err := vimeoWith(srv).Extract(context.Background(), u, Options{})
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v; want it to say %q", id, err, want)
		}
		if id != "333" && !errors.Is(err, errVimeoNotPublic) {
			t.Errorf("%s: %v is not errVimeoNotPublic", id, err)
		}
	}
}
