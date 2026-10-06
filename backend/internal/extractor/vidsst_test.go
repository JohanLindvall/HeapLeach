// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

// vidsSite serves video pages shaped like the site's: the player
// configuration as one object in a script, followed by the rest of the
// script, which the reader must stop short of.
func vidsSite(t *testing.T, configs map[string]string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	for id, cfg := range configs {
		mux.HandleFunc("/v/"+id, func(w http.ResponseWriter, _ *http.Request) {
			_, _ = fmt.Fprintf(w, `<html><head><title>x - VIDS.ST</title></head><body><div id="player"></div>
<script>
const playerConfig = %s;
(function(){ if (playerConfig.videoUrl) { new Artplayer({url: playerConfig.videoUrl}); } })();
</script></body></html>`, cfg)
		})
	}
	mux.HandleFunc("/hls/index.m3u8", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "#EXTM3U\n#EXT-X-TARGETDURATION:6\n"+`#EXT-X-KEY:METHOD=AES-128,URI="key.bin"`+
			"\n#EXTINF:6,\nseg0.ts\n#EXTINF:6,\nseg1.ts\n#EXT-X-ENDLIST\n")
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func vidsExtract(t *testing.T, raw string) (*Result, error) {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return NewVidsSt(httpx.New("test-agent", "en-US", 0, 5*time.Second)).Extract(context.Background(), u, Options{})
}

func TestVidsStTakesTheUploadedFileAsItIs(t *testing.T) {
	srv := vidsSite(t, map[string]string{
		"101": `{"videoUrl":"/storage/uploads/video101/first-clip.mp4","videoName":"First Clip.mp4","thumbnail":"/t.jpg","videoId":101,"subtitleTracks":[],"audioTracks":[]}`,
	})
	res, err := vidsExtract(t, srv.URL+"/e/101")
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	f := res.Files[0]
	if res.Title != "First Clip" || f.Name != "First Clip.mp4" {
		t.Errorf("title %q, name %q", res.Title, f.Name)
	}
	if f.URL != srv.URL+"/storage/uploads/video101/first-clip.mp4" {
		t.Errorf("url = %q, want the uploaded file, resolved against the page", f.URL)
	}
	if len(f.Segments) != 0 {
		t.Error("a plain file was treated as a playlist")
	}
}

func TestVidsStFollowsAPlaylistNatively(t *testing.T) {
	srv := vidsSite(t, map[string]string{
		"202": `{"videoUrl":"/hls/index.m3u8","videoName":"Second Clip.mp4"}`,
	})
	res, err := vidsExtract(t, srv.URL+"/v/202")
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	f := res.Files[0]
	if f.Name != "Second Clip.ts" || len(f.Segments) != 2 || f.SegmentKey == nil {
		t.Errorf("name %q, %d segments, key %v; want the playlist joined and decrypted as TS", f.Name, len(f.Segments), f.SegmentKey)
	}
}

func TestVidsStSaysWhenAVideoHasBeenRemoved(t *testing.T) {
	srv := vidsSite(t, nil)
	_, err := vidsExtract(t, srv.URL+"/v/303")
	if err == nil || !strings.Contains(err.Error(), "removed") {
		t.Errorf("err = %v, want it to say the video is gone", err)
	}
}

func TestVidsID(t *testing.T) {
	for raw, want := range map[string]string{
		"https://vids.st/v/123": "123",
		"https://vids.st/e/123": "123",
		"https://vids.st/":      "",
		"https://vids.st/faq":   "",
	} {
		u, _ := url.Parse(raw)
		if got := vidsID(u); got != want {
			t.Errorf("vidsID(%s) = %q, want %q", raw, got, want)
		}
	}
}
