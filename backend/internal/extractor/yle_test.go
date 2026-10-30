// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
)

func TestYleLink(t *testing.T) {
	for raw, want := range map[string][2]string{
		"https://areena.yle.fi/1-1001":                {"fin", "1-1001"},
		"https://arenan.yle.fi/1-1001":                {"swe", "1-1001"},
		"https://areena.yle.fi/podcastit/1-2002":      {"fin", "1-2002"},
		"https://areena.yle.fi/tv/an-invented/1-3003": {"fin", "1-3003"},
		"https://areena.yle.fi/tv/opas":               {"fin", ""},
	} {
		u, _ := url.Parse(raw)
		if lang, id := yleLink(u); lang != want[0] || id != want[1] {
			t.Errorf("%s: %s %q, want %s %q", raw, lang, id, want[0], want[1])
		}
	}
}

// yleFakeSite stands in for Areena in both its languages, the Swedish site
// under /sv: item pages saying what they are, the player's preview, the UI
// API's lists, which serve two cards a page whatever is asked for, and the
// streams, those shown in Finland alone refused.
func yleFakeSite(t *testing.T) *httptest.Server {
	t.Helper()
	var srv *httptest.Server
	page := func(w http.ResponseWriter, kind, title, view string) {
		_, _ = fmt.Fprintf(w, `<html><head><script id="__NEXT_DATA__" type="application/json">`+
			`{"props": {"pageProps": {"meta": {"item": {"type": %q}, "title": %q}, "view": %s}}}</script>`+
			`<script>self.config={"appIdFrontend":"an-invented-app","appKeyFrontend":"an-invented-key"}</script></head></html>`,
			kind, title, view)
	}
	// seasons is a series' page: its episodes in a list whose filter offers
	// each season, the newest first as the site has them, beside a list of
	// recommendations and a tab of clips.
	seasons := func(name, season, current string) string {
		option := func(n int, id string) string {
			return fmt.Sprintf(`{"title": "%s %d", "parameters": {"path.season": %q, "filter.order": "natural:asc"}, "default": %v}`,
				season, n, id, id == current)
		}
		return fmt.Sprintf(`{"title": %q, "tabs": [{"content": [
			{"type": "list", "source": {"uri": "%s/v1/ui/content/list?token=an.invented.series"},
			 "filters": [{"options": [%s, %s]}]},
			{"type": "list", "title": "Katso myös", "source": {"uri": "%s/v1/ui/content/list?token=elsewhere"}}]},
			{"content": [{"type": "list", "source": {"uri": "%s/v1/ui/content/list?token=clips"}}]}]}`,
			name, srv.URL, option(2, "1-2200"), option(1, "1-2100"), srv.URL, srv.URL)
	}
	ondemand := func(fin, swe, region, kind string) string {
		link := fmt.Sprintf(`"manifest_url": "%s/vod/%s/master.m3u8?hdnts=exp=1~hmac=x"`, srv.URL, strings.ToLower(region))
		if kind == "audio" {
			link = fmt.Sprintf(`"media_url": "%s/aod/%s/an-invented-episode.mp3?hdnts=exp=1~hmac=x"`, srv.URL, strings.ToLower(region))
		}
		// Every programme is marked protected, as most of Areena is, plain
		// streams included.
		return fmt.Sprintf(`{"data": {"ongoing_ondemand": {"title": {"fin": %q, "swe": %q},
			"series": {"title": {"fin": "Keksitty sarja", "swe": "En påhittad serie"}},
			"region": %q, "is_content_protected": true, %s}}}`, fin, swe, region, link)
	}
	state := func(name string) string {
		return fmt.Sprintf(`{"data": {%q: {"title": {"fin": "Keksitty ohjelma"}}}}`, name)
	}
	preview := func(w http.ResponseWriter, r *http.Request, id string) {
		q := r.URL.Query()
		if q.Get("app_id") != ylePlayerAppID || q.Get("app_key") != ylePlayerAppKey || q.Get("host") != r.Host || q.Has("countryCode") {
			http.Error(w, "refused", http.StatusForbidden)
			return
		}
		switch {
		case id == "1-1001":
			_, _ = io.WriteString(w, ondemand("Ensimmäinen jakso", "Första avsnittet", "World", "video"))
		case id == "1-1002":
			_, _ = io.WriteString(w, ondemand("Podcastin jakso", "Poddens avsnitt", "World", "audio"))
		case id == "1-1003", id == "1-3005":
			_, _ = io.WriteString(w, state("gone"))
		case id == "1-1004":
			_, _ = io.WriteString(w, state("pending_ondemand"))
		case id == "1-1005":
			_, _ = io.WriteString(w, state("ongoing_event"))
		case id == "1-1006":
			_, _ = io.WriteString(w, ondemand("Kotimainen jakso", "Inhemskt avsnitt", "Finland", "video"))
		case id == "1-1007", id == "1-3004":
			_, _ = io.WriteString(w, ondemand("Kotimainen podcast", "Inhemsk podd", "Finland", "audio"))
		case strings.HasPrefix(id, "1-21"), strings.HasPrefix(id, "1-22"):
			_, _ = io.WriteString(w, ondemand("Jakso "+id, "Avsnitt "+id, "World", "video"))
		case strings.HasPrefix(id, "1-300"):
			_, _ = io.WriteString(w, ondemand("Jakso "+id, "Avsnitt "+id, "World", "audio"))
		default:
			http.NotFound(w, r)
		}
	}
	list := func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("app_id") != "an-invented-app" || q.Get("app_key") != "an-invented-key" || q.Get("v") != yleUIVersion {
			http.Error(w, `{"notifications": [{"developerMessage": "Missing or invalid version parameter"}]}`, http.StatusBadRequest)
			return
		}
		word, ok := map[string]string{"fi": "Jakso", "sv": "Avsnitt"}[q.Get("language")]
		if !ok {
			http.Error(w, `{"notifications": [{"developerMessage": "Missing or invalid language parameter"}]}`, http.StatusBadRequest)
			return
		}
		var ids []string
		switch q.Get("token") {
		case "an.invented.series":
			ids = map[string][]string{"1-2100": {"1-2101", "1-2102", "1-2103"}, "1-2200": {"1-2201"}}[q.Get("path.season")]
		case "an.invented.podcast":
			ids = []string{"1-3001", "1-3002", "1-3003", "1-3004", "1-3005"}
		default:
			http.Error(w, "not a list of episodes", http.StatusBadRequest)
			return
		}
		offset, _ := strconv.Atoi(q.Get("offset"))
		limit, _ := strconv.Atoi(q.Get("limit"))
		var cards []string
		for i := offset; i < min(len(ids), offset+min(limit, 2)); i++ {
			cards = append(cards, fmt.Sprintf(`{"type": "card", "title": "%d. %s %s", "pointer": {"type": "program", "uri": "yleareena://items/%s"}}`,
				i+1, word, ids[i], ids[i]))
		}
		_, _ = fmt.Fprintf(w, `{"meta": {"offset": %d, "count": %d}, "data": [%s]}`, offset, len(ids), strings.Join(cards, ","))
	}
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		swedish := strings.HasPrefix(r.URL.Path, "/sv/")
		path := strings.TrimPrefix(r.URL.Path, "/sv")
		name, season := "Keksitty sarja", "Kausi"
		if swedish {
			name, season = "En påhittad serie", "Säsong"
		}
		switch {
		case strings.HasPrefix(path, "/v1/preview/"):
			preview(w, r, strings.TrimSuffix(strings.TrimPrefix(path, "/v1/preview/"), ".json"))
			return
		case path == "/v1/ui/content/list":
			list(w, r)
			return
		}
		switch path {
		case "/1-1001", "/1-1002", "/1-1003", "/1-1004", "/1-1005", "/1-1006", "/1-1007":
			page(w, "TVProgram", "Keksitty ohjelma", `{"title": "K1, J1: Keksitty ohjelma"}`)
		case "/1-2000":
			page(w, "TVSeries", name, seasons(name, season, "1-2200"))
		case "/1-2100":
			page(w, "TVSeason", name+" | "+season+" 1", seasons(name, season, "1-2100"))
		case "/1-3000":
			page(w, "RadioSeries", "Keksitty podcast", fmt.Sprintf(`{"title": "Keksitty podcast", "tabs": [{"content": [
				{"type": "list", "source": {"uri": "%s/v1/ui/content/list?token=an.invented.podcast"}}]}]}`, srv.URL))
		case "/vod/world/master.m3u8":
			_, _ = io.WriteString(w, "#EXTM3U\n"+
				`#EXT-X-MEDIA:TYPE=SUBTITLES,GROUP-ID="subtitles",NAME="Ohjelmatekstitys",DEFAULT=YES,LANGUAGE="fin",URI="subs.m3u8"`+"\n"+
				`#EXT-X-STREAM-INF:BANDWIDTH=5770160,RESOLUTION=1920x1080,CODECS="avc1.640028,mp4a.40.2",SUBTITLES="subtitles"`+"\nvideo.m3u8\n")
		case "/vod/world/video.m3u8":
			_, _ = io.WriteString(w, "#EXTM3U\n#EXT-X-TARGETDURATION:6\n#EXTINF:6,\nindex_1_0.ts\n#EXTINF:6,\nindex_1_1.ts\n#EXT-X-ENDLIST\n")
		case "/vod/world/subs.m3u8":
			_, _ = io.WriteString(w, "#EXTM3U\n#EXT-X-TARGETDURATION:6\n#EXTINF:6,\nsub_0.vtt\n#EXTINF:6,\nsub_1.vtt\n#EXT-X-ENDLIST\n")
		case "/aod/world/an-invented-episode.mp3":
			w.Header().Set("Content-Range", "bytes 0-0/1000")
			w.WriteHeader(http.StatusPartialContent)
			_, _ = io.WriteString(w, "I")
		case "/vod/finland/master.m3u8", "/aod/finland/an-invented-episode.mp3":
			http.Error(w, "<HTML><HEAD><TITLE>Access Denied</TITLE></HEAD></HTML>", http.StatusForbidden)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func yleWith(srv *httptest.Server) *Yle {
	y := NewYle(testClient())
	y.player = srv.URL + "/v1/preview/"
	y.site = map[string]string{"fin": srv.URL, "swe": srv.URL + "/sv"}
	return y
}

// A programme is read off the player's preview, named for its series and
// itself, its subtitles a track of their own, and minted again when the
// transfer starts. Being marked protected, as most of Areena is, keeps
// nothing out: the stream says whether it is encrypted.
func TestYleReadsAProgrammeFromThePlayersPreview(t *testing.T) {
	srv := yleFakeSite(t)
	res := extractOK(t, yleWith(srv), "https://areena.yle.fi/1-1001", Options{})
	f := res.Files[0]
	if res.Title != "Keksitty sarja - Ensimmäinen jakso" || f.Name != "Keksitty sarja - Ensimmäinen jakso.ts" {
		t.Errorf("title %q, name %q", res.Title, f.Name)
	}
	if len(f.Segments) != 2 || len(f.Subtitles) != 1 || len(f.Subtitles[0].Segments) != 2 || f.Resolve == nil {
		t.Fatalf("segments %v, subtitles %+v", f.Segments, f.Subtitles)
	}
	if target, err := f.Resolve(context.Background()); err != nil || len(target.Segments) != 2 {
		t.Errorf("target %+v, %v", target, err)
	}
}

// An episode of a podcast is one MP3.
func TestYleReadsAnAudioProgrammeAsItsFile(t *testing.T) {
	srv := yleFakeSite(t)
	res := extractOK(t, yleWith(srv), "https://areena.yle.fi/podcastit/1-1002", Options{})
	f := res.Files[0]
	if f.Name != "Keksitty sarja - Podcastin jakso.mp3" || f.URL != srv.URL+"/aod/world/an-invented-episode.mp3?hdnts=exp=1~hmac=x" || len(f.Segments) != 0 {
		t.Errorf("name %q, url %q, segments %v", f.Name, f.URL, f.Segments)
	}
}

// A series is what its page lists: a request for each season, in the site's
// order, a page at a time until the list's own count is reached, each
// episode in its season's folder. An episode keeps the name it is listed
// under, its number with it, when its transfer starts.
func TestYleListsASeriesBySeason(t *testing.T) {
	srv := yleFakeSite(t)
	res := extractOK(t, yleWith(srv), "https://areena.yle.fi/1-2000", Options{})
	want := strings.Join([]string{
		"Kausi 2/Keksitty sarja - 1. Jakso 1-2201.ts",
		"Kausi 1/Keksitty sarja - 1. Jakso 1-2101.ts",
		"Kausi 1/Keksitty sarja - 2. Jakso 1-2102.ts",
		"Kausi 1/Keksitty sarja - 3. Jakso 1-2103.ts",
	}, "\n")
	if got := strings.Join(filePaths(res.Files), "\n"); res.Title != "Keksitty sarja" || got != want || res.Note != "" {
		t.Fatalf("%q (note %q):\n%s\nwant:\n%s", res.Title, res.Note, got, want)
	}
	target, err := res.Files[3].Resolve(context.Background())
	if err != nil || target.Name != "Keksitty sarja - 3. Jakso 1-2103.ts" || len(target.Subtitles) != 1 {
		t.Errorf("target %+v, %v", target, err)
	}
}

// A season's own page offers every season of its series, and lists itself.
func TestYleListsASeasonAlone(t *testing.T) {
	srv := yleFakeSite(t)
	res := extractOK(t, yleWith(srv), "https://areena.yle.fi/1-2100", Options{})
	want := strings.Join([]string{
		"Keksitty sarja - 1. Jakso 1-2101.ts",
		"Keksitty sarja - 2. Jakso 1-2102.ts",
		"Keksitty sarja - 3. Jakso 1-2103.ts",
	}, "\n")
	if got := strings.Join(filePaths(res.Files), "\n"); res.Title != "Keksitty sarja - Kausi 1" || got != want {
		t.Errorf("%q:\n%s\nwant:\n%s", res.Title, got, want)
	}
}

// The Swedish site is read in Swedish: its pages, its lists and the
// preview's titles.
func TestYleReadsTheSwedishSiteInSwedish(t *testing.T) {
	srv := yleFakeSite(t)
	res := extractOK(t, yleWith(srv), "https://arenan.yle.fi/1-2100", Options{})
	if res.Title != "En påhittad serie - Säsong 1" || res.Files[0].Name != "En påhittad serie - 1. Avsnitt 1-2101.ts" {
		t.Errorf("%q: %s", res.Title, strings.Join(filePaths(res.Files), "\n"))
	}
	res = extractOK(t, yleWith(srv), "https://arenan.yle.fi/1-1001", Options{})
	if res.Title != "En påhittad serie - Första avsnittet" {
		t.Errorf("title %q", res.Title)
	}
}

// A podcast's episodes are MP3s, read a page at a time. What Areena no
// longer offers is left out, and so, once one has been refused here, is
// what Yle shows in Finland alone, the note saying so.
func TestYleLeavesOutWhatASeriesCannotHave(t *testing.T) {
	srv := yleFakeSite(t)
	res := extractOK(t, yleWith(srv), "https://areena.yle.fi/1-3000", Options{})
	want := strings.Join([]string{
		"Keksitty podcast - 1. Jakso 1-3001.mp3",
		"Keksitty podcast - 2. Jakso 1-3002.mp3",
		"Keksitty podcast - 3. Jakso 1-3003.mp3",
	}, "\n")
	if got := strings.Join(filePaths(res.Files), "\n"); got != want {
		t.Errorf("%s\nwant:\n%s", got, want)
	}
	if note := "1 left out that Yle shows in Finland only; 1 left out: it is no longer on Areena"; res.Note != note {
		t.Errorf("note %q, want %q", res.Note, note)
	}
}

// A cap keeps the first episodes, and the note counts the rest by what the
// list states rather than by reading it.
func TestYleCountsWhatACapLeavesOut(t *testing.T) {
	srv := yleFakeSite(t)
	res := extractOK(t, yleWith(srv), "https://areena.yle.fi/1-3000", Options{limits: Limits{Files: 2}})
	if len(res.Files) != 2 || res.Note != "2 of 5 episodes" {
		t.Errorf("%d files, note %q", len(res.Files), res.Note)
	}
}

// What cannot be had says why.
func TestYleSaysWhyAProgrammeCannotBeHad(t *testing.T) {
	srv := yleFakeSite(t)
	for id, want := range map[string]string{
		"1-1003": "it is no longer on Areena",
		"1-1004": "it is not published yet",
		"1-1005": "it is a live broadcast",
		"1-1006": errYleFinlandOnly.Error(),
		"1-1007": errYleFinlandOnly.Error(),
		"1-9999": "does not exist",
	} {
		_, err := extract(t, yleWith(srv), "https://areena.yle.fi/"+id, Options{})
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v; want it to say %q", id, err, want)
		}
	}
}
