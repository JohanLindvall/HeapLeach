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
)

func TestDRLink(t *testing.T) {
	for raw, want := range map[string]string{
		"https://www.dr.dk/drtv/episode/an-invented_-title_1001": "1001",
		"https://www.dr.dk/drtv/program/an-invented-film_1002":   "1002",
		"https://www.dr.dk/drtv/se/an-invented_-title_1001":      "1001",
		"https://www.dr.dk/drtv/serie/an-invented-series_2000":   "2000",
		"https://www.dr.dk/drtv/saeson/an-invented-series_2100":  "2100",
		"https://www.dr.dk/drtv/tvlive/1006":                     "1006",
		"https://www.dr.dk/nyheder/indland/an-invented-article":  "",
		"https://www.dr.dk/drtv":                                 "",
	} {
		u, _ := url.Parse(raw)
		if id := drLink(u); id != want {
			t.Errorf("%s: %q, want %q", raw, id, want)
		}
	}
}

// drFakeSite stands in for DR TV's API, asked as the anonymous subscriber,
// and its CDN: a master holding one audio track at two bitrates, both
// tagged English, and two subtitle sets, every part a byte range of one
// file; and what DR shows in Denmark alone refused.
func drFakeSite(t *testing.T) *httptest.Server {
	t.Helper()
	var srv *httptest.Server
	item := func(id, kind, title, contextual string, geo bool, extra string) string {
		restricted := "False"
		if geo {
			restricted = "True"
		}
		return fmt.Sprintf(`{"id": %q, "type": %q, "title": %q, "contextualTitle": %q, "customFields": {"IsGeoRestricted": %q}%s}`,
			id, kind, title, contextual, restricted, extra)
	}
	inSeason := `, "season": {"title": "Opfundet serie", "seasonNumber": 1, "show": {"title": "Opfundet serie"}}`
	stream := func(service, drm, language, path string) string {
		return fmt.Sprintf(`{"accessService": %q, "drm": %q, "format": "video/hls", "containerFormat": "FMP4", "language": %q, "url": "%s%s"}`,
			service, drm, language, srv.URL, path)
	}
	episode := func(id string, n int, geo bool) string {
		return item(id, "episode", "Opfundet serie: Afsnit "+id, fmt.Sprintf("%d. Afsnit %s", n, id), geo, inSeason)
	}
	page := func(items []string, pages int) string {
		return fmt.Sprintf(`{"items": [%s], "paging": {"total": %d, "size": 200}}`, strings.Join(items, ","), pages)
	}
	api := func(w http.ResponseWriter, r *http.Request, id, what string) {
		q := r.URL.Query()
		if q.Get("sub") != "Anonymous" {
			http.Error(w, `{"code": 8009, "message": "Ingen filer"}`, http.StatusNotFound)
			return
		}
		switch what {
		case "":
			switch id {
			case "1001":
				_, _ = io.WriteString(w, item(id, "episode", "Opfundet serie: Første afsnit", "1. Første afsnit", false, inSeason))
			case "1002":
				_, _ = io.WriteString(w, item(id, "program", "En opfundet film", "", true, ""))
			case "1003", "1005", "1007":
				_, _ = io.WriteString(w, item(id, "episode", "Opfundet serie: Afsnit "+id, "Afsnit "+id, false, inSeason))
			case "1006":
				_, _ = io.WriteString(w, item(id, "channel", "En opfundet kanal", "", false, ""))
			case "2000":
				_, _ = io.WriteString(w, item(id, "show", "Opfundet serie", "", false, ""))
			case "2100":
				_, _ = io.WriteString(w, item(id, "season", "Opfundet serie", "", false, `, "seasonNumber": 1, "show": {"title": "Opfundet serie"}`))
			case "2101", "2102", "2201":
				_, _ = io.WriteString(w, episode(id, int(id[3]-'0'), false))
			case "2202":
				_, _ = io.WriteString(w, episode(id, 2, true))
			default:
				http.Error(w, fmt.Sprintf(`{"message": "Item '%s' not found"}`, id), http.StatusNotFound)
			}
		case "videos":
			if q.Get("delivery") == "" || q.Get("resolution") == "" {
				http.Error(w, `{"code": 0, "message": "delivery and resolution are required"}`, http.StatusBadRequest)
				return
			}
			switch id {
			case "1001", "2101", "2102", "2201":
				_, _ = fmt.Fprintf(w, "[%s, %s]", stream("StandardVideo", "None", "da", "/all/clear/1001/master.m3u8"),
					stream("SpokenSubtitles", "None", "da", "/all/clear/1001/spoken.m3u8"))
			case "1002", "2202":
				_, _ = fmt.Fprintf(w, "[%s]", stream("StandardVideo", "None", "da", "/dk/clear/"+id+"/master.m3u8"))
			case "1003":
				_, _ = fmt.Fprintf(w, "[%s]", stream("StandardVideo", "PlayReady", "da", "/dk/encrypted/1003/master.m3u8"))
			case "1005":
				http.Error(w, `{"code": 8009, "message": "Ingen filer"}`, http.StatusNotFound)
			case "1007":
				_, _ = fmt.Fprintf(w, "[%s]", stream("StandardVideo", "None", "foreign", "/all/clear/1001/master.m3u8"))
			default:
				http.Error(w, `{"code": 0, "message": "Item was not found"}`, http.StatusBadRequest)
			}
		case "children":
			switch id + "/" + q.Get("page") {
			case "2000/1":
				_, _ = io.WriteString(w, page([]string{
					item("2100", "season", "Opfundet serie", "", false, `, "seasonNumber": 1`),
					item("2200", "season", "Opfundet serie: 2026", "", false, `, "seasonNumber": 2`),
				}, 1))
			case "2100/1":
				_, _ = io.WriteString(w, page([]string{episode("2101", 1, false)}, 2))
			case "2100/2":
				_, _ = io.WriteString(w, page([]string{episode("2102", 2, false)}, 2))
			case "2200/1":
				_, _ = io.WriteString(w, page([]string{episode("2201", 1, false), episode("2202", 2, true)}, 1))
			default:
				_, _ = io.WriteString(w, page(nil, 0))
			}
		}
	}
	playlist := func(file string, offsets ...int) string {
		var b strings.Builder
		fmt.Fprintf(&b, "#EXTM3U\n#EXT-X-VERSION:6\n#EXT-X-TARGETDURATION:10\n#EXT-X-MAP:URI=%q,BYTERANGE=\"803@0\"\n", file)
		for _, offset := range offsets {
			fmt.Fprintf(&b, "#EXTINF:10.0,\n#EXT-X-BYTERANGE:1000@%d\n%s\n", offset, file)
		}
		return b.String() + "#EXT-X-ENDLIST\n"
	}
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if rest, ok := strings.CutPrefix(r.URL.Path, "/api/items/"); ok {
			id, what, _ := strings.Cut(rest, "/")
			api(w, r, id, what)
			return
		}
		switch r.URL.Path {
		case "/all/clear/1001/master.m3u8":
			_, _ = io.WriteString(w, "#EXTM3U\n"+
				`#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="AUDIO",NAME="English",LANGUAGE="en",AUTOSELECT=YES,DEFAULT=YES,CHANNELS="2",URI="audio_192.m3u8"`+"\n"+
				`#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="AUDIO",NAME="English_2",LANGUAGE="en",AUTOSELECT=YES,DEFAULT=NO,CHANNELS="2",URI="audio_128.m3u8"`+"\n"+
				`#EXT-X-MEDIA:TYPE=SUBTITLES,GROUP-ID="subs",NAME="Fremmedsprogstekster",DEFAULT=YES,AUTOSELECT=YES,FORCED=NO,LANGUAGE="da",URI="subs_foreign.m3u8"`+"\n"+
				`#EXT-X-MEDIA:TYPE=SUBTITLES,GROUP-ID="subs",NAME="Dansk",DEFAULT=NO,AUTOSELECT=NO,FORCED=NO,LANGUAGE="da",URI="subs_danish.m3u8"`+"\n"+
				`#EXT-X-STREAM-INF:BANDWIDTH=765471,CODECS="avc1.42C01E,mp4a.40.2",RESOLUTION=640x360,AUDIO="AUDIO",SUBTITLES="subs"`+"\nvideo_500.m3u8\n"+
				`#EXT-X-STREAM-INF:BANDWIDTH=9170000,CODECS="avc1.64002A,mp4a.40.2",RESOLUTION=1920x1080,AUDIO="AUDIO",SUBTITLES="subs"`+"\nvideo_8000.m3u8\n")
		case "/all/clear/1001/video_8000.m3u8":
			_, _ = io.WriteString(w, playlist("video_8000.mp4", 2859, 3859))
		case "/all/clear/1001/audio_192.m3u8":
			_, _ = io.WriteString(w, playlist("audio_192.mp4", 1200, 2200))
		case "/all/clear/1001/audio_128.m3u8":
			_, _ = io.WriteString(w, playlist("audio_128.mp4", 1200, 2200))
		case "/all/clear/1001/subs_foreign.m3u8", "/all/clear/1001/subs_danish.m3u8":
			_, _ = io.WriteString(w, "#EXTM3U\n#EXT-X-TARGETDURATION:60\n#EXTINF:60,\nsegment_0.vtt\n#EXT-X-ENDLIST\n")
		case "/dk/clear/1002/master.m3u8", "/dk/clear/2202/master.m3u8":
			w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, "<HTML><HEAD><TITLE>Access Denied</TITLE></HEAD></HTML>")
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func drWith(srv *httptest.Server) *DRTV {
	d := NewDRTV(testClient())
	d.api = srv.URL + "/api"
	return d
}

// An episode is its standard version: the video's byte ranges, one audio
// track however many bitrates the master offers it at, in the language the
// API gives it rather than the English every rendition claims, and the
// subtitles as DR names them.
func TestDRReadsAnEpisodesStandardVersion(t *testing.T) {
	srv := drFakeSite(t)
	res := extractOK(t, drWith(srv), "https://www.dr.dk/drtv/episode/opfundet-serie_-foerste-afsnit_1001", Options{})
	f := res.Files[0]
	if res.Title != "Opfundet serie - 1. Første afsnit" || f.Name != "Opfundet serie - 1. Første afsnit.mp4" {
		t.Errorf("title %q, name %q", res.Title, f.Name)
	}
	if len(f.Segments) != 3 || !strings.HasSuffix(f.Segments[1], "/video_8000.mp4#range=2859-3858") {
		t.Errorf("segments %v; want the map and two byte ranges of the 1080p rendition", f.Segments)
	}
	if len(f.Audio) != 1 || !strings.Contains(f.Audio[0].Segments[1], "audio_192.mp4") ||
		f.Audio[0].Language != "da" || f.Audio[0].Name != "Dansk" || !f.Audio[0].Default {
		t.Errorf("audio %+v; want the default rendition alone, in Danish", f.Audio)
	}
	if len(f.Subtitles) != 2 || f.Subtitles[0].Name != "Fremmedsprogstekster" || f.Subtitles[1].Name != "Dansk" {
		t.Errorf("subtitles %+v", f.Subtitles)
	}
}

// A programme the API calls foreign names no language at all, which beats
// the English its renditions claim.
func TestDRLeavesAForeignProgrammesLanguageUnnamed(t *testing.T) {
	srv := drFakeSite(t)
	res := extractOK(t, drWith(srv), "https://www.dr.dk/drtv/episode/opfundet-serie_-afsnit_1007", Options{})
	if audio := res.Files[0].Audio; len(audio) != 1 || audio[0].Language != "" || audio[0].Name != "" {
		t.Errorf("audio %+v", audio)
	}
}

// A show is its seasons, each in a folder named for what sets it apart from
// the show, a page at a time. What DR shows in Denmark alone is left out
// once one of it has been refused here, and an episode keeps its listed
// name when its transfer starts.
func TestDRListsAShowBySeason(t *testing.T) {
	srv := drFakeSite(t)
	res := extractOK(t, drWith(srv), "https://www.dr.dk/drtv/serie/opfundet-serie_2000", Options{})
	want := strings.Join([]string{
		"Sæson 1/Opfundet serie - 1. Afsnit 2101.mp4",
		"Sæson 1/Opfundet serie - 2. Afsnit 2102.mp4",
		"2026/Opfundet serie - 1. Afsnit 2201.mp4",
	}, "\n")
	if got := strings.Join(filePaths(res.Files), "\n"); res.Title != "Opfundet serie" || got != want {
		t.Fatalf("%q:\n%s\nwant:\n%s", res.Title, got, want)
	}
	if note := "1 left out that DR shows in Denmark only"; res.Note != note {
		t.Errorf("note %q, want %q", res.Note, note)
	}
	target, err := res.Files[1].Resolve(context.Background())
	if err != nil || target.Name != "Opfundet serie - 2. Afsnit 2102.mp4" || len(target.Audio) != 1 {
		t.Errorf("target %+v, %v", target, err)
	}
}

// A season pasted on its own is that season, under the show's name and its
// own.
func TestDRListsASeasonAlone(t *testing.T) {
	srv := drFakeSite(t)
	res := extractOK(t, drWith(srv), "https://www.dr.dk/drtv/saeson/opfundet-serie_2100", Options{})
	want := "Opfundet serie - 1. Afsnit 2101.mp4\nOpfundet serie - 2. Afsnit 2102.mp4"
	if got := strings.Join(filePaths(res.Files), "\n"); res.Title != "Opfundet serie - Sæson 1" || got != want {
		t.Errorf("%q:\n%s", res.Title, got)
	}
	res = extractOK(t, drWith(srv), "https://www.dr.dk/drtv/saeson/opfundet-serie_2100", Options{limits: Limits{Files: 1}})
	if len(res.Files) != 1 || res.Note != "1 of 2 episodes" {
		t.Errorf("%d files, note %q", len(res.Files), res.Note)
	}
}

// What cannot be had says why.
func TestDRSaysWhyAProgrammeCannotBeHad(t *testing.T) {
	srv := drFakeSite(t)
	for id, want := range map[string]string{
		"1002": errDRDenmarkOnly.Error(),
		"1003": "it is DRM-protected",
		"1004": "it is not on DR TV",
		"1005": "without signing in",
		"1006": "it is a live channel",
	} {
		_, err := extract(t, drWith(srv), "https://www.dr.dk/drtv/program/an-invented-title_"+id, Options{})
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v; want it to say %q", id, err, want)
		}
	}
}
