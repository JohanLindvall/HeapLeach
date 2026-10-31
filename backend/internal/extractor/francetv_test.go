// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The video ids a fake france.tv knows.
const (
	ftvWorld  = "00000000-0000-4000-8000-000000000001"
	ftvFrance = "00000000-0000-4000-8000-000000000002"
	ftvDRM    = "00000000-0000-4000-8000-000000000003"
	ftvGone   = "00000000-0000-4000-8000-000000000004"
	ftvLive   = "00000000-0000-4000-8000-000000000005"
	ftvClip   = "00000000-0000-4000-8000-000000000006"
	ftvSecond = "00000000-0000-4000-8000-000000000007"
)

// francetvFakeSite stands in for france.tv: pages whose flight data is
// JSON written into script as a string, the player API answering as it
// does Safari, the token service, and the CDN, which refuses what is shown
// in France alone.
func francetvFakeSite(t *testing.T) *httptest.Server {
	t.Helper()
	var srv *httptest.Server
	flight := func(json string) string {
		return `<script>self.__next_f.push([1,"` + strings.ReplaceAll(json, `"`, `\"`) + `"])</script>`
	}
	page := func(w http.ResponseWriter, title, og, body string) {
		_, _ = fmt.Fprintf(w, `<html><head><title>%s</title><meta property="og:title" content="%s"></head><body>%s</body></html>`, title, og, body)
	}
	card := func(id int, title, format, video string) string {
		return fmt.Sprintf(`{"content":{"id":%d,"title":%q,"url":"/france-5/une-emission-inventee/%d-x.html"},"tracking":{"content_id":%d,"content_format":%q,"video_factory_id":%q}}`,
			id, title, id, id, format, video)
	}
	answer := func(w http.ResponseWriter, asset string, extra string) {
		_, _ = fmt.Fprintf(w, `{"video": {"format": "hls", "url": "%s/ftv/%s_TA.ism/master.m3u8", "token": {"akamai": "%s/esi/TA?format=json"}, "drm": false, "is_live": false%s},
			"meta": {"title": "Une émission inventée", "additional_title": "Émission du lundi", "pre_title": null,
			"subtitles": [{"url": "%s/captions/an-invented-caption.vtt", "lang": "qsm"}]}}`, srv.URL, asset, srv.URL, extra, srv.URL)
	}
	refusal := func(w http.ResponseWriter, status, code int) {
		w.WriteHeader(status)
		_, _ = fmt.Fprintf(w, `{"code": %d, "message": "Une raison inventée.<br /><br />Et une autre ligne.", "can_retry": false}`, code)
	}
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		switch path := r.URL.Path; {
		case path == "/france-5/une-emission-inventee/1001-x.html":
			page(w, "France TV", "Une émission inventée en replay", flight(`{"options":{"id":"`+ftvWorld+`"}}`))
		case path == "/france-5/une-emission-inventee/1002-x.html":
			// No player options: the tracking whose content id the link ends in says it.
			page(w, "France TV", "Une autre en replay", flight(card(9999, "Autre", "integrale", ftvClip)+card(1002, "Celle-ci", "integrale", ftvFrance)))
		case strings.HasSuffix(path, "-drm.html"):
			page(w, "France TV", "Un téléfilm inventé en replay", flight(`{"options":{"id":"`+ftvDRM+`"}}`))
		case strings.HasSuffix(path, "-gone.html"):
			page(w, "France TV", "Une émission partie en replay", flight(`{"options":{"id":"`+ftvGone+`"}}`))
		case strings.HasSuffix(path, "-live.html"):
			page(w, "France TV", "Un direct en replay", flight(`{"options":{"id":"`+ftvLive+`"}}`))
		case path == "/france-5/une-emission-inventee/toutes-les-videos/":
			cards := card(1001, "Émission du lundi", "integrale", ftvWorld) + card(1006, "Un extrait", "extrait", ftvClip)
			if q.Get("page") == "2" {
				cards = card(1007, "Émission du mardi", "integrale", ftvSecond) + card(1001, "Émission du lundi", "integrale", ftvWorld)
			}
			page(w, "Une émission inventée - Toutes les vidéos | France TV", "France TV - Replay et Direct", flight(cards))
		case strings.HasPrefix(path, "/videos/"):
			if q.Get("browser") != "safari" || q.Get("domain") == "" || q.Get("device_type") == "" {
				refusal(w, http.StatusBadRequest, 2028)
				return
			}
			switch strings.TrimPrefix(path, "/videos/") {
			case ftvWorld, ftvSecond:
				answer(w, "1758196440-1791479529_monde", "")
			case ftvFrance:
				answer(w, "1758196440-1791479529_france-domtom", "")
			case ftvLive:
				answer(w, "live", `, "is_live": true`)
			case ftvDRM:
				refusal(w, http.StatusUnprocessableEntity, 2019)
			case ftvGone:
				refusal(w, http.StatusUnprocessableEntity, 2003)
			default:
				refusal(w, http.StatusNotFound, 2012)
			}
		case path == "/esi/TA":
			_, _ = fmt.Fprintf(w, `{"url": "%s?hdnea=exp=1~acl=%%2f*~hmac=x"}`, q.Get("url"))
		case strings.HasSuffix(path, "_monde_TA.ism/master.m3u8"):
			if q.Get("hdnea") == "" {
				http.Error(w, "stoken", http.StatusForbidden)
				return
			}
			_, _ = io.WriteString(w, "#EXTM3U\n#EXT-X-SESSION-KEY:METHOD=AES-128,URI=\"https://example.test/keys/a.key\"\n"+
				`#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="audio-aacl-96",LANGUAGE="fr",NAME="Audio Français",DEFAULT=YES,AUTOSELECT=YES,URI="audio.m3u8"`+"\n"+
				`#EXT-X-MEDIA:TYPE=SUBTITLES,GROUP-ID="textstream",LANGUAGE="qsm",NAME="Sous-Titres Malentendant",DEFAULT=YES,URI="text.m3u8"`+"\n"+
				`#EXT-X-STREAM-INF:BANDWIDTH=3509000,CODECS="mp4a.40.2,avc1.640029",RESOLUTION=1920x1080,AUDIO="audio-aacl-96",SUBTITLES="textstream"`+"\nvideo.m3u8\n")
		case strings.HasSuffix(path, "/video.m3u8"), strings.HasSuffix(path, "/audio.m3u8"), strings.HasSuffix(path, "/text.m3u8"):
			ext := map[bool]string{true: ".ts", false: ".aac"}[strings.HasSuffix(path, "/video.m3u8")]
			_, _ = io.WriteString(w, "#EXTM3U\n#EXT-X-TARGETDURATION:8\n#EXT-X-MEDIA-SEQUENCE:1\n"+
				"#EXT-X-KEY:METHOD=AES-128,URI=\"https://example.test/keys/a.key\"\n#EXTINF:8, no desc\npart-1"+ext+"\n#EXT-X-ENDLIST\n")
		case strings.HasSuffix(path, "_france-domtom_TA.ism/master.m3u8"):
			w.Header().Set("X-Errortype", "geo")
			http.Error(w, "<HTML><HEAD><TITLE>Access Denied</TITLE></HEAD></HTML>", http.StatusForbidden)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func francetvWith(srv *httptest.Server) *FranceTV {
	f := NewFranceTV(testClient())
	f.api = srv.URL + "/videos/"
	return f
}

func francetvExtract(t *testing.T, srv *httptest.Server, path string) (*Result, error) {
	t.Helper()
	return extract(t, francetvWith(srv), srv.URL+path, Options{})
}

// A video page's player names the video, which the API asked as Safari
// answers with HLS: signed by the token service, its video and its packed
// audio under one AES-128 key, and its subtitles taken whole rather than
// the master's segmented ones.
func TestFranceTVReadsAVideoAsSafariIsServedIt(t *testing.T) {
	srv := francetvFakeSite(t)
	res, err := francetvExtract(t, srv, "/france-5/une-emission-inventee/1001-x.html")
	if err != nil {
		t.Fatal(err)
	}
	f := res.Files[0]
	if res.Title != "Une émission inventée - Émission du lundi" || f.Name != "Une émission inventée - Émission du lundi.mp4" {
		t.Errorf("title %q, name %q", res.Title, f.Name)
	}
	if !strings.Contains(f.URL, "hdnea=") || len(f.Segments) != 1 || f.SegmentKey == nil || f.Resolve == nil {
		t.Errorf("url %q, segments %v, key %+v; want the signed master's", f.URL, f.Segments, f.SegmentKey)
	}
	if len(f.Audio) != 1 || f.Audio[0].Key == nil || !strings.HasSuffix(f.Audio[0].Segments[0], "part-1.aac") {
		t.Errorf("audio %+v; want the packed audio, with its key", f.Audio)
	}
	if len(f.Subtitles) != 1 || f.Subtitles[0].Language != "fr" || !strings.HasSuffix(f.Subtitles[0].Segments[0], "an-invented-caption.vtt") {
		t.Errorf("subtitles %+v; want the whole caption file", f.Subtitles)
	}
}

// A page without the player's options names the video in the tracking
// whose content id its link ends in.
func TestFranceTVFindsAVideoByItsContentID(t *testing.T) {
	srv := francetvFakeSite(t)
	_, err := francetvExtract(t, srv, "/france-5/une-emission-inventee/1002-x.html")
	if err == nil || !strings.Contains(err.Error(), errFranceTVFranceOnly.Error()) {
		t.Errorf("err = %v; want the France-only video the content id names", err)
	}
}

// A programme's every video is read a page at a time, whole programmes
// alone where clips are listed beside them.
func TestFranceTVListsAProgrammesVideos(t *testing.T) {
	srv := francetvFakeSite(t)
	res, err := francetvExtract(t, srv, "/france-5/une-emission-inventee/toutes-les-videos/")
	if err != nil {
		t.Fatal(err)
	}
	names := fileNames(res.Files)
	if res.Title != "Une émission inventée" || strings.Join(names, "|") != "Émission du lundi.mp4|Émission du mardi.mp4" || res.Note != "1 clip left out" {
		t.Errorf("%q: %v, note %q", res.Title, names, res.Note)
	}
	if target, err := res.Files[1].Resolve(context.Background()); err != nil || target.Name != "Émission du mardi.mp4" {
		t.Errorf("target %+v, %v", target, err)
	}
}

// What cannot be had says why, named by the page where the API does not.
func TestFranceTVSaysWhyAVideoCannotBeHad(t *testing.T) {
	srv := francetvFakeSite(t)
	for path, want := range map[string]string{
		"/une-emission-inventee/x-drm.html":  "Un téléfilm inventé: it is DRM-protected",
		"/une-emission-inventee/x-gone.html": "it is no longer on france.tv",
		"/une-emission-inventee/x-live.html": "live broadcast",
	} {
		_, err := francetvExtract(t, srv, path)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v; want it to say %q", path, err, want)
		}
	}
}
