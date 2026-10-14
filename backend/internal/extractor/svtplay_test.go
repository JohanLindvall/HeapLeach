// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

// TestSVTBestManifest pins the rendition choice: plain MPEG-TS HLS first,
// because its segments carry audio and video together and join into a
// playable file; the CMAF flavours next; and any HLS at all before nothing.
func TestSVTBestManifest(t *testing.T) {
	ref := func(format, url string) struct {
		Format string `json:"format"`
		URL    string `json:"url"`
	} {
		return struct {
			Format string `json:"format"`
			URL    string `json:"url"`
		}{Format: format, URL: url}
	}

	video := svtVideo{}
	video.VideoReferences = append(video.VideoReferences,
		ref("dash", "https://cdn.example.test/dash.mpd"),
		ref("hls-cmaf-full", "https://cdn.example.test/cmaf.m3u8"),
		ref("hls-ts-full", "https://cdn.example.test/ts.m3u8"),
	)
	if got := video.bestManifest(); got != "https://cdn.example.test/ts.m3u8" {
		t.Errorf("best = %q, want the plain MPEG-TS flavour", got)
	}

	// Without a TS flavour the CMAF one is the best on offer.
	video.VideoReferences = video.VideoReferences[:2]
	if got := video.bestManifest(); got != "https://cdn.example.test/cmaf.m3u8" {
		t.Errorf("best = %q, want the CMAF flavour", got)
	}

	// A flavour nobody here has named still beats returning nothing.
	video.VideoReferences = video.VideoReferences[:0]
	video.VideoReferences = append(video.VideoReferences,
		ref("hls-something-new", "https://cdn.example.test/new.m3u8"))
	if got := video.bestManifest(); got != "https://cdn.example.test/new.m3u8" {
		t.Errorf("best = %q, want the unnamed HLS flavour taken as a fallback", got)
	}

	// DASH alone is nothing this can join.
	video.VideoReferences = video.VideoReferences[:0]
	video.VideoReferences = append(video.VideoReferences,
		ref("dash", "https://cdn.example.test/dash.mpd"))
	if got := video.bestManifest(); got != "" {
		t.Errorf("best = %q, want nothing for DASH alone", got)
	}
}

func TestSVTVideoLinkPattern(t *testing.T) {
	doc := `<a href="/video/abc123DEF/a-slug">one</a>` +
		`<a href="/video/xy/short">too short</a>` +
		`"canonical":"/video/qrs789tuv"`
	var ids []string
	for _, m := range svtVideoLink.FindAllStringSubmatch(doc, -1) {
		ids = append(ids, m[1])
	}
	if len(ids) != 2 || ids[0] != "abc123DEF" || ids[1] != "qrs789tuv" {
		t.Errorf("ids = %v, want the two real ids and not the two-character segment", ids)
	}
}

// A video whose every rendition keeps its audio apart, as SVT's newer CMAF
// ones do, comes with each audio rendition as a track of its own, the
// default first, for the downloader to join to the video. It used to be
// refused, saying to use an external downloader nothing let anyone choose. A
// rendition that carries its own audio is still joined as it is.
func TestSVTCarriesAudioKeptApartAsTracksOfItsOwn(t *testing.T) {
	var srv *httptest.Server
	serve := func(body func() string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body()) }
	}
	media := func(prefix string) string {
		return "#EXTM3U\n#EXT-X-TARGETDURATION:4\n#EXT-X-MAP:URI=\"" + prefix + "-init.mp4\"\n" +
			"#EXTINF:3.84,\n" + prefix + "-1.m4s\n#EXTINF:3.84,\n" + prefix + "-2.m4s\n#EXT-X-ENDLIST\n"
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/separate", serve(func() string {
		return fmt.Sprintf(`{"programTitle":"A Programme","episodeTitle":"First Episode",`+
			`"videoReferences":[{"format":"hls-cmaf-full","url":"%s/separate/master.m3u8"}]}`, srv.URL)
	}))
	mux.HandleFunc("/separate/master.m3u8", serve(func() string {
		return "#EXTM3U\n" +
			`#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="aac",LANGUAGE="sv-x-tydligaretal",NAME="Tydligare tal",DEFAULT=NO,URI="clear.m3u8"` + "\n" +
			`#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="aac",LANGUAGE="sv",NAME="Svenska",DEFAULT=YES,URI="swedish.m3u8"` + "\n" +
			`#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="ac3",LANGUAGE="sv",NAME="Svenska",DEFAULT=YES,URI="surround.m3u8"` + "\n" +
			`#EXT-X-STREAM-INF:BANDWIDTH=2000000,RESOLUTION=1280x720,CODECS="avc1.640028,mp4a.40.2",AUDIO="aac"` + "\nvideo.m3u8\n"
	}))
	mux.HandleFunc("/separate/video.m3u8", serve(func() string { return media("v") }))
	mux.HandleFunc("/separate/swedish.m3u8", serve(func() string { return media("sv") }))
	mux.HandleFunc("/separate/clear.m3u8", serve(func() string { return media("clear") }))
	mux.HandleFunc("/api/joined", serve(func() string {
		return fmt.Sprintf(`{"programTitle":"A Programme","episodeTitle":"Second Episode",`+
			`"videoReferences":[{"format":"hls-ts-full","url":"%s/joined/master.m3u8"}]}`, srv.URL)
	}))
	mux.HandleFunc("/joined/master.m3u8", serve(func() string {
		return "#EXTM3U\n" + `#EXT-X-STREAM-INF:BANDWIDTH=2000000,CODECS="avc1.640028,mp4a.40.2"` + "\nvariant.m3u8\n"
	}))
	mux.HandleFunc("/joined/variant.m3u8", serve(func() string {
		return "#EXTM3U\n#EXT-X-TARGETDURATION:4\n#EXTINF:4,\nseg0.ts\n#EXTINF:4,\nseg1.ts\n#EXT-X-ENDLIST\n"
	}))
	srv = httptest.NewServer(mux)
	defer srv.Close()

	s := NewSVTPlay(httpx.New("test-agent", "en-US", 0, 5*time.Second))
	s.api = srv.URL + "/api/"
	page, _ := url.Parse("https://www.svtplay.se/video/separate/an-invented-title/")
	res, err := s.Extract(context.Background(), page, Options{})
	if err != nil {
		t.Fatal(err)
	}
	f := res.Files[0]
	if f.Name != "First Episode.mp4" || f.External != "" || len(f.Segments) != 3 || !strings.HasSuffix(f.Segments[0], "/v-init.mp4") {
		t.Fatalf("file = %+v; want the video's own segments, its initialisation segment first", f)
	}
	if len(f.Audio) != 2 {
		t.Fatalf("audio = %+v; want the two renditions of the group the variant names", f.Audio)
	}
	if a := f.Audio[0]; a.Language != "sv" || a.Name != "Svenska" || len(a.Segments) != 3 || !strings.HasSuffix(a.Segments[0], "/sv-init.mp4") {
		t.Errorf("first track = %+v; want the default rendition, init first", a)
	}
	if a := f.Audio[1]; a.Language != "sv-x-tydligaretal" || a.Name != "Tydligare tal" {
		t.Errorf("second track = %+v", a)
	}

	file, _, _, err := s.episode(context.Background(), "joined")
	if err != nil || len(file.Audio) != 0 || len(file.Segments) != 2 || file.Name != ".ts" {
		t.Fatalf("file = %+v, err %v; want the rendition that carries its own audio joined as it is", file, err)
	}
}
