// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
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
	mux.HandleFunc("/graphql", serve(func() string { return `{"data":{"listablesBySlug":[]}}` }))
	srv = httptest.NewServer(mux)
	defer srv.Close()

	s := NewSVTPlay(httpx.New("test-agent", "en-US", 0, 5*time.Second))
	s.api, s.graphql = srv.URL+"/api/", srv.URL+"/graphql"
	res := extractOK(t, s, "https://www.svtplay.se/video/separate/an-invented-title/", Options{})
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

// A video the player API offers no stream at all is explained from what SVT
// Play's page knows of it: when it could be watched, and where. The API
// answers such a video with an empty list and no reason, which read "offers
// no usable stream" of a programme whose window had closed two years before.
func TestSVTSaysWhyAVideoOffersNoStream(t *testing.T) {
	items := map[string]string{
		"/video/closed1": `{"__typename":"Single","name":"A Closed Film","validFrom":"2001-06-01T06:00:00+02:00","validTo":"2001-09-30T23:59:00+02:00","restrictions":{"onlyAvailableInSweden":true}}`,
		"/video/later22": `{"__typename":"Episode","name":"A Later Episode","validFrom":"2999-01-01T06:00:00+01:00","validTo":"2999-12-31T23:59:00+01:00","restrictions":{"onlyAvailableInSweden":false}}`,
		"/video/sweden3": `{"__typename":"Single","name":"A Swedish Film","validFrom":"2001-01-01T00:00:00+01:00","validTo":"2999-12-31T23:59:00+01:00","restrictions":{"onlyAvailableInSweden":true}}`,
		"/video/open444": `{"__typename":"Single","name":"An Open Film","validFrom":"2001-01-01T00:00:00+01:00","validTo":"2999-12-31T23:59:00+01:00","restrictions":{"onlyAvailableInSweden":false}}`,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/dashonly") {
			fmt.Fprint(w, `{"programTitle":"A Programme","videoReferences":[{"format":"dash","url":"https://cdn.example.test/a.mpd"}]}`)
			return
		}
		fmt.Fprint(w, `{"svtId":"x","hasVodReferences":false,"rights":null,"videoReferences":[]}`)
	})
	mux.HandleFunc("/graphql", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Query     string            `json:"query"`
			Variables map[string]string `json:"variables"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || !strings.Contains(req.Query, "detailsPageByPath") {
			http.Error(w, "unexpected query", http.StatusBadRequest)
			return
		}
		item, ok := items[req.Variables["path"]]
		if !ok {
			fmt.Fprint(w, `{"data":{"detailsPageByPath":null}}`)
			return
		}
		fmt.Fprintf(w, `{"data":{"detailsPageByPath":{"item":%s}}}`, item)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	s := NewSVTPlay(httpx.New("test-agent", "en-US", 0, 5*time.Second))
	s.api, s.graphql = srv.URL+"/api/", srv.URL+"/graphql"

	for id, want := range map[string]string{
		"closed1":  "A Closed Film could be watched until 2001-09-30 and is no longer available",
		"later22":  "A Later Episode cannot be watched before 2999-01-01",
		"sweden3":  "SVT offers no stream for A Swedish Film here: it is shown in Sweden only",
		"open444":  "SVT offers no stream for An Open Film, and gives no reason",
		"unknown5": "SVT offers no stream for unknown5 (it may no longer be available, or not be yet)",
		"dashonly": "dashonly offers no usable stream (only dash)",
	} {
		_, err := extract(t, s, "https://www.svtplay.se/video/"+id+"/an-invented-title", Options{})
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v, want %q", id, err, want)
		}
	}
}

// SVT links a programme's card as /video/<id>/<programme>, the episode it
// would start playing named by the programme alone, and an episode as
// /video/<id>/<programme>/<episode>. The first means the programme and
// expands to every episode; the second is the one episode, and a film's card
// stays the film.
func TestSVTProgrammeCardExpandsToItsEpisodes(t *testing.T) {
	var srv *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, "/api/")
		fmt.Fprintf(w, `{"programTitle":"A Programme","episodeTitle":"Episode %s",`+
			`"videoReferences":[{"format":"hls-ts-full","url":"%s/media.m3u8"}]}`, id, srv.URL)
	})
	mux.HandleFunc("/media.m3u8", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "#EXTM3U\n#EXT-X-TARGETDURATION:4\n#EXTINF:4,\nseg0.ts\n#EXT-X-ENDLIST\n")
	})
	mux.HandleFunc("/graphql", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Variables map[string]string `json:"variables"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		switch req.Variables["slug"] {
		case "a-series":
			fmt.Fprint(w, `{"data":{"listablesBySlug":[{"__typename":"TvSeries","name":"A Series","associatedContent":[`+
				`{"name":"Season 1","type":"Season","items":[`+
				`{"item":{"__typename":"Episode","name":"1. First","videoSvtId":"first1"}},`+
				`{"item":{"__typename":"Episode","name":"2. Second","videoSvtId":"second2"}},`+
				`{"item":{"__typename":"Episode","name":"3. Third","videoSvtId":"third3"}}]},`+
				`{"name":"Trailers","type":"Clips","items":[{"item":{"__typename":"Clip","name":"A Trailer"}}]}]}]}}`)
		case "a-film":
			fmt.Fprint(w, `{"data":{"listablesBySlug":[{"__typename":"Single","name":"A Film","associatedContent":[]}]}}`)
		default:
			http.Error(w, "unexpected slug", http.StatusBadRequest)
		}
	})
	srv = httptest.NewServer(mux)
	defer srv.Close()
	s := NewSVTPlay(httpx.New("test-agent", "en-US", 0, 5*time.Second))
	s.api, s.graphql = srv.URL+"/api/", srv.URL+"/graphql"

	for path, want := range map[string][]string{
		"/video/card123/a-series":             {"1. First.ts", "2. Second.ts", "3. Third.ts"},
		"/video/card123/a-series/3-the-third": {"Episode card123.ts"},
		"/video/film123/a-film":               {"Episode film123.ts"},
	} {
		res := extractOK(t, s, "https://www.svtplay.se"+path, Options{})
		names := fileNames(res.Files)
		if strings.Join(names, "|") != strings.Join(want, "|") {
			t.Errorf("%s: files = %v, want %v", path, names, want)
		}
		if len(want) > 1 && res.Title != "A Series" {
			t.Errorf("%s: title = %q, want the series' name", path, res.Title)
		}
	}
}
