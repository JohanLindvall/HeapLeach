// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

func TestORFLink(t *testing.T) {
	for raw, want := range map[string]struct {
		kind int
		id   string
	}{
		"https://on.orf.at/video/1001/a-title":                         {orfEpisode, "1001"},
		"https://on.orf.at/video/1001/2002/a-segment":                  {orfEpisode, "1001"},
		"https://on.orf.at/sendereihe/3003/a-series":                   {orfSeries, "3003"},
		"https://tvthek.orf.at/profile/A-Series/3003/An-Episode/1001":  {orfEpisode, "1001"},
		"https://tvthek.orf.at/profile/A-Series/3003":                  {orfSeries, "3003"},
		"https://orf.at/stories/1234567/":                              {0, ""},
		"https://on.orf.at/sammlung/4004/an-invented-collection":       {0, ""},
		"https://sound.orf.at/radio/an-invented-station/an-invented-1": {0, ""},
	} {
		u, _ := url.Parse(raw)
		if kind, id := orfLink(u); kind != want.kind || id != want.id {
			t.Errorf("%s: %d %q, want %d %q", raw, kind, id, want.kind, want.id)
		}
	}
}

// orfFakeSite stands in for ORF ON: a page whose runtime configuration names the
// API and its credentials, an API that wants them, and the streams. The
// credentials rotate once the first have been read, as a new build does.
func orfFakeSite(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var reads atomic.Int32
	var srv *httptest.Server
	basic := func(user string) string {
		return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":an-invented-password"))
	}
	episode := func(id int, title, right string, drm bool, master string) string {
		return fmt.Sprintf(`{"id": %d, "title": %q, "profile_title": "An Invented Series", "right": %q,
			"is_drm_protected": %v, "sources": {"hls": [
				{"src": "%s/stream/low.m3u8", "quality_key": "Q4A", "is_adaptive_stream": false},
				{"src": "%s", "quality_key": "QXA", "is_adaptive_stream": true}]},
			"_embedded": {"subtitle": {"vtt_url": "%s/subtitles/%d.vtt"}}}`,
			id, title, right, drm, srv.URL, master, srv.URL, id)
	}
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		switch {
		case path == "/":
			user := "an-invented-user"
			if reads.Add(1) > 1 {
				user = "a-rotated-user"
			}
			_, _ = fmt.Fprintf(w, `<html><script>window.__NUXT__={};window.__NUXT__.config={public:{api:{baseUrl:"%s/api/v4.3/",`+
				`user:%q,password:"an-invented-password",timeout:7000},login:{api:"https://login.example.test/"}}}</script></html>`, srv.URL, user)
			return
		case strings.HasPrefix(path, "/api/"):
			if r.Header.Get("Authorization") != basic("a-rotated-user") {
				http.Error(w, "unauthorised", http.StatusUnauthorized)
				return
			}
		}
		switch path {
		case "/api/v4.3/episode/1001":
			_, _ = io.WriteString(w, episode(1001, "An Invented Series (1/3)", "worldwide", false, srv.URL+"/stream/ww.m3u8"))
		case "/api/v4.3/episode/1002":
			_, _ = io.WriteString(w, episode(1002, "An Invented Series (2/3)", "austria", false, srv.URL+"/stream/at.m3u8"))
		case "/api/v4.3/episode/1003":
			_, _ = io.WriteString(w, episode(1003, "An Invented Series (3/3)", "worldwide", true, srv.URL+"/stream/drm.m3u8"))
		case "/api/v4.3/profile/3003/episodes":
			page := r.URL.Query().Get("page")
			if page == "1" {
				_, _ = fmt.Fprintf(w, `{"page": 1, "pages": 2, "total": 3, "_embedded": {"items": [%s, %s]}}`,
					episode(1003, "An Invented Series (3/3)", "worldwide", true, srv.URL+"/stream/drm.m3u8"),
					episode(1002, "An Invented Series (2/3)", "austria", false, srv.URL+"/stream/at.m3u8"))
				return
			}
			_, _ = fmt.Fprintf(w, `{"page": 2, "pages": 2, "total": 3, "_embedded": {"items": [%s]}}`,
				episode(1001, "An Invented Series (1/3)", "worldwide", false, srv.URL+"/stream/ww.m3u8"))
		case "/stream/ww.m3u8":
			_, _ = io.WriteString(w, "#EXTM3U\n"+
				`#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="audio",LANGUAGE="de",NAME="Deutsch",DEFAULT=YES,URI="audio.m3u8"`+"\n"+
				`#EXT-X-STREAM-INF:BANDWIDTH=3192000,RESOLUTION=1280x720,AUDIO="audio"`+"\nvideo.m3u8\n")
		case "/stream/at.m3u8":
			// Outside Austria, the stream is a notice saying so.
			http.Redirect(w, r, "/gp/insert_geoprotection_hr.smil/manifest.m3u8", http.StatusMovedPermanently)
		case "/stream/video.m3u8", "/stream/audio.m3u8", "/gp/insert_geoprotection_hr.smil/chunklist.m3u8":
			_, _ = io.WriteString(w, "#EXTM3U\n#EXT-X-TARGETDURATION:6\n#EXTINF:6,\npart-1.ts\n#EXT-X-ENDLIST\n")
		case "/gp/insert_geoprotection_hr.smil/manifest.m3u8":
			_, _ = io.WriteString(w, "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1800000\nchunklist.m3u8\n")
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &reads
}

func orfWith(srv *httptest.Server) *ORF {
	o := NewORF(httpx.New("test-agent", "en-US", 0, 5*time.Second))
	o.site = srv.URL + "/"
	return o
}

// An episode is read with the credentials the site publishes, read again
// when the API stops taking them, and comes with its audio and subtitles.
func TestORFReadsAnEpisodeWithTheSitesCredentials(t *testing.T) {
	srv, reads := orfFakeSite(t)
	u, _ := url.Parse("https://on.orf.at/video/1001/an-invented-title")
	res, err := orfWith(srv).Extract(context.Background(), u, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if reads.Load() != 2 {
		t.Errorf("the site was read %d times; want the rotated credentials read once more", reads.Load())
	}
	f := res.Files[0]
	if res.Title != "An Invented Series (1/3)" || f.Name != "An Invented Series (1/3).mp4" {
		t.Errorf("title %q, name %q", res.Title, f.Name)
	}
	if len(f.Segments) != 1 || len(f.Audio) != 1 || f.Audio[0].Name != "Deutsch" {
		t.Errorf("segments %v, audio %+v", f.Segments, f.Audio)
	}
	if len(f.Subtitles) != 1 || f.Subtitles[0].Segments[0] != srv.URL+"/subtitles/1001.vtt" || f.Subtitles[0].Language != "de" {
		t.Errorf("subtitles %+v; want the episode's WebVTT", f.Subtitles)
	}
}

// What ORF shows in Austria alone is a notice on screen from anywhere else,
// and is refused rather than downloaded; DRM is refused too.
func TestORFRefusesWhatItCannotFetch(t *testing.T) {
	srv, _ := orfFakeSite(t)
	for id, want := range map[string]string{
		"1002": errORFAustriaOnly.Error(),
		"1003": "is DRM-protected",
		"9999": "does not exist",
	} {
		u, _ := url.Parse("https://on.orf.at/video/" + id)
		_, err := orfWith(srv).Extract(context.Background(), u, Options{})
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v; want it to say %q", id, err, want)
		}
	}
}

// A series is read a page at a time, oldest first, each episode resolved
// when its transfer starts. What is DRM-protected is left out, and so,
// once one has shown it refused here, is what ORF shows in Austria alone.
func TestORFListsASeries(t *testing.T) {
	srv, _ := orfFakeSite(t)
	u, _ := url.Parse("https://on.orf.at/sendereihe/3003/an-invented-series")
	res, err := orfWith(srv).Extract(context.Background(), u, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Title != "An Invented Series" || len(res.Files) != 1 || res.Files[0].Name != "An Invented Series (1/3).mp4" {
		t.Fatalf("%q: %+v", res.Title, res.Files)
	}
	if want := "1 left out that ORF shows in Austria only; 1 left out that are DRM-protected"; res.Note != want {
		t.Errorf("note %q, want %q", res.Note, want)
	}
	target, err := res.Files[0].Resolve(context.Background())
	if err != nil || len(target.Audio) != 1 || len(target.Subtitles) != 1 {
		t.Errorf("target %+v, %v", target, err)
	}
}
