// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

// luluSite serves the shape a lulustream page has: a heading naming the
// video, a packed player setup naming a signed master playlist, one muxed
// rendition, and AES-128 segments under one key. The packer is given no
// words, so the payload — the test server's own address — passes through it
// untouched.
func luluSite(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var pages atomic.Int32
	mux := http.NewServeMux()
	var srv *httptest.Server
	mux.HandleFunc("/abcd1234wxyz", func(w http.ResponseWriter, _ *http.Request) {
		pages.Add(1)
		setup := fmt.Sprintf(`jwplayer("vplayer").setup({sources:[{file:"%s/hls/master.m3u8?t=signed&e=28800"}],image:"%s/thumb.jpg"});`,
			srv.URL, srv.URL)
		_, _ = fmt.Fprintf(w, `<html><head><title>First Clip - Lulustream.mp4</title></head><body>
<h1 class="h5">First Clip</h1>
<script>eval(function(p,a,c,k,e,d){while(c--)if(k[c])p=p.replace(new RegExp('\\b'+c.toString(a)+'\\b','g'),k[c]);return p}('%s',36,0,''.split('|')))</script>
</body></html>`, setup)
	})
	mux.HandleFunc("/hls/master.m3u8", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=3000000,RESOLUTION=720x1280,CODECS=\"avc1.64001f,mp4a.40.2\"\n"+
			"index-v1-a1.m3u8?t=signed\n")
	})
	mux.HandleFunc("/hls/index-v1-a1.m3u8", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "#EXTM3U\n#EXT-X-TARGETDURATION:12\n#EXT-X-PLAYLIST-TYPE:VOD\n"+
			`#EXT-X-KEY:METHOD=AES-128,URI="encryption.key?t=signed"`+"\n"+
			"#EXTINF:10,\nseg-1-v1-a1.ts?t=signed\n#EXTINF:8,\nseg-2-v1-a1.ts?t=signed\n#EXT-X-ENDLIST\n")
	})
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &pages
}

func TestLuluStreamFollowsThePackedPlayerToEncryptedSegments(t *testing.T) {
	srv, pages := luluSite(t)
	l := NewLuluStream(httpx.New("test-agent", "en-US", 0, 5*time.Second))

	u, _ := url.Parse(srv.URL + "/e/abcd1234wxyz")
	res, err := l.Extract(context.Background(), u, Options{})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if res.Title != "First Clip" {
		t.Errorf("title = %q, want the page's heading", res.Title)
	}
	f := res.Files[0]
	if f.Name != "First Clip.ts" {
		t.Errorf("name = %q, want the title and the segments' container", f.Name)
	}
	if len(f.Segments) != 2 || f.Segments[0] != srv.URL+"/hls/seg-1-v1-a1.ts?t=signed" {
		t.Errorf("segments = %v", f.Segments)
	}
	if f.SegmentKey == nil || f.SegmentKey.URI != srv.URL+"/hls/encryption.key?t=signed" {
		t.Errorf("key = %+v, want the playlist's own", f.SegmentKey)
	}

	// The page is read again at transfer time, segments and key with it.
	tg, err := f.Resolve(context.Background())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(tg.Segments) != 2 || tg.SegmentKey == nil || pages.Load() != 2 {
		t.Errorf("resolve gave %d segments, key %v, after %d page reads", len(tg.Segments), tg.SegmentKey, pages.Load())
	}
}

func TestLuluID(t *testing.T) {
	for raw, want := range map[string]string{
		"https://luluvido.com/abcd1234wxyz":     "abcd1234wxyz",
		"https://luluvdo.com/e/abcd1234wxyz":    "abcd1234wxyz",
		"https://lulustream.com/d/abcd1234wxyz": "abcd1234wxyz",
		"https://lulustream.com/":               "",
		"https://lulustream.com/a/b/c":          "",
	} {
		u, _ := url.Parse(raw)
		if got := luluID(u); got != want {
			t.Errorf("luluID(%s) = %q, want %q", raw, got, want)
		}
	}
}
