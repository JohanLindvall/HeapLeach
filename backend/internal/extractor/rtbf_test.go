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
	"sync/atomic"
	"testing"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

func TestRTBFLink(t *testing.T) {
	for raw, want := range map[string]struct {
		kind int
		id   string
	}{
		"https://auvio.rtbf.be/media/an-invented-programme-an-invented-episode-1001": {rtbfMedia, "1001"},
		"https://auvio.rtbf.be/embed/generic/content/media/1001":                     {rtbfMedia, "1001"},
		"https://www.rtbf.be/auvio/detail_an-invented-episode?id=1001":               {rtbfMedia, "1001"},
		"https://auvio.rtbf.be/emission/an-invented-programme-2000":                  {rtbfProgramme, "an-invented-programme-2000"},
		"https://auvio.rtbf.be/live/an-invented-channel-3000":                        {rtbfLive, ""},
		"https://auvio.rtbf.be/embed/rtbf/highlight/live/3000":                       {rtbfLive, ""},
		"https://www.rtbf.be/article/an-invented-article-4000":                       {rtbfOther, ""},
	} {
		u, _ := url.Parse(raw)
		if kind, id := rtbfLink(u); kind != want.kind || id != want.id {
			t.Errorf("%s: %d %q, want %d %q", raw, kind, id, want.kind, want.id)
		}
	}
}

// rtbfFake stands in for RTBF's API, Red Bee's, and the CDN. Red Bee hands
// out anonymous sessions and plays only for the latest; sessions counts
// how many it has handed out.
type rtbfFake struct {
	srv      *httptest.Server
	sessions atomic.Int32
	// stale makes the session handed out first stop working after one play.
	stale atomic.Bool
}

func newRTBFFake(t *testing.T) *rtbfFake {
	t.Helper()
	f := &rtbfFake{}
	media := func(id, kind, programme, episode string) string {
		return fmt.Sprintf(`{"data": {"id": %q, "assetId": "%s_6BA97Bb", "type": "VIDEO", "playerType": %q, "title": %q, "subtitle": %q, "program": {"title": %q}}}`,
			id, id, kind, strings.ToUpper(programme), episode, programme)
	}
	refusal := func(w http.ResponseWriter, body string) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, body)
	}
	hls := func(w http.ResponseWriter, master string) {
		_, _ = fmt.Fprintf(w, `{"formats": [{"format": "DASH", "mediaLocator": "%s/cdn/%s.mpd"}, {"format": "HLS", "mediaLocator": "%s/cdn/%s.m3u8"}], "audioOnly": false, "streamInfo": {"live": false}}`,
			f.srv.URL, master, f.srv.URL, master)
	}
	play := func(w http.ResponseWriter, r *http.Request, asset string) {
		want := fmt.Sprintf("Bearer token-%d", f.sessions.Load())
		if r.Header.Get(httpx.HeaderAuthorization) != want || f.stale.CompareAndSwap(true, false) {
			http.Error(w, `{"httpCode": 401, "message": "INVALID_SESSION_TOKEN"}`, http.StatusUnauthorized)
			return
		}
		switch strings.TrimSuffix(asset, "_6BA97Bb") {
		case "1001", "2001", "2003", "2005":
			hls(w, "triple")
		case "1010":
			hls(w, "two")
		case "1002", "2002":
			_, _ = fmt.Fprintf(w, `{"formats": [{"format": "MP3", "mediaLocator": "%s/aod/an-invented-episode.mp3"}], "audioOnly": true}`, f.srv.URL)
		case "1003", "2004":
			refusal(w, `{"httpCode": 403, "message": "NOT_ENTITLED", "actions": [{"type": "LOGIN", "publication": {"availableAt": "2023-09-08T04:00:00Z"}}]}`)
		case "1004", "2006":
			refusal(w, `{"httpCode": 403, "message": "NOT_ENTITLED"}`)
		case "1005":
			refusal(w, `{"httpCode": 403, "message": "NOT_ENTITLED", "actions": [{"type": "BUY_WATCH_NOW", "offerings": [{"offeringId": "sooner_6BA97Bb"}]}]}`)
		case "1006":
			refusal(w, `{"httpCode": 403, "message": "NOT_PUBLISHED"}`)
		case "1007":
			refusal(w, `{"httpCode": 403, "message": "NOT_ENTITLED", "actions": [{"type": "LOGIN", "publication": {"availableAt": "2999-01-02T03:04:05Z"}}]}`)
		default:
			http.Error(w, `{"httpCode": 404, "message": "UNKNOWN_ASSET"}`, http.StatusNotFound)
		}
	}
	tab := func(year string) string {
		return fmt.Sprintf(`{"id": "18800", "type": "MEDIA_LIST", "title": "Année %s", "contentPath": "%s/bff/widgets/18800?context%%5BprogramId%%5D=2000&context%%5Byear%%5D=%s"}`,
			year, f.srv.URL, year)
	}
	entry := func(id, episode string) string {
		return fmt.Sprintf(`{"id": %q, "resourceType": "MEDIA", "type": "AUDIO", "assetId": "%s_6BA97Bb", "title": "Un podcast inventé", "subtitle": %q}`, id, id, episode)
	}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, q := r.URL.Path, r.URL.Query()
		switch {
		case strings.HasPrefix(path, "/bff/embed/media/"):
			switch id := strings.TrimPrefix(path, "/bff/embed/media/"); id {
			case "1001", "1002", "1003", "1004", "1005", "1006", "1007", "1010":
				_, _ = io.WriteString(w, media(id, "MEDIA", "Un programme inventé", "Épisode "+id))
			case "1008":
				_, _ = io.WriteString(w, media(id, "LIVE", "Une chaîne inventée", ""))
			default:
				http.Error(w, `{"status": 404, "code": "NOT_FOUND"}`, http.StatusNotFound)
			}
		case path == "/bff/pages/emission/un-podcast-invente-2000":
			_, _ = fmt.Fprintf(w, `{"data": {"content": {"title": "Un podcast inventé", "type": "AUDIO_SHOW"}, "widgets": [
				{"id": 18801, "type": "TAB_LIST", "title": "Épisodes", "contentPath": "%s/bff/widgets/18801?context%%5BprogramId%%5D=2000"},
				{"id": 19495, "type": "MEDIA_LIST", "title": "Des extraits", "contentPath": "%s/bff/widgets/19495"}]}}`, f.srv.URL, f.srv.URL)
		case path == "/bff/widgets/18801":
			_, _ = fmt.Fprintf(w, `{"data": {"type": "TAB_LIST", "content": [%s, %s]}}`, tab("2026"), tab("2025"))
		case path == "/bff/widgets/18800":
			// Two entries a page whatever is asked for, each page naming the next.
			entries := map[string][]string{
				"2026": {entry("2001", "Le premier"), entry("2002", "Le deuxième"), entry("2003", "Le troisième")},
				"2025": {entry("2004", "Le quatrième"), entry("2005", "Le cinquième"), entry("2006", "Le sixième")},
			}[q.Get("context[year]")]
			pageNo, _ := strconv.Atoi(q.Get("_page"))
			from := (pageNo - 1) * 2
			to := min(len(entries), from+2)
			next := "null"
			if to < len(entries) {
				nq := r.URL.Query()
				nq.Set("_page", strconv.Itoa(pageNo+1))
				next = fmt.Sprintf("%q", f.srv.URL+path+"?"+nq.Encode())
			}
			_, _ = fmt.Fprintf(w, `{"data": {"type": "MEDIA_LIST", "content": [%s]}, "links": {"next": %s}}`, strings.Join(entries[from:to], ","), next)
		case path == "/exposure/v2/customer/RTBF/businessunit/Auvio/auth/anonymous":
			_, _ = fmt.Fprintf(w, `{"sessionToken": "token-%d"}`, f.sessions.Add(1))
		case strings.HasPrefix(path, "/exposure/v2/customer/RTBF/businessunit/Auvio/entitlement/"):
			play(w, r, strings.TrimSuffix(strings.TrimPrefix(path, "/exposure/v2/customer/RTBF/businessunit/Auvio/entitlement/"), "/play"))
		case strings.HasPrefix(path, "/exposure/v1/customer/RTBF/businessunit/Auvio/content/asset/"):
			if strings.Contains(path, "1010") {
				_, _ = io.WriteString(w, `{"audio": [{"language": "fr", "roles": ["main"]}, {"language": "fr", "roles": ["description"]}]}`)
				return
			}
			_, _ = io.WriteString(w, `{"audio": [{"language": "fr", "roles": ["main"]}]}`)
		case path == "/cdn/triple.m3u8", path == "/cdn/two.m3u8":
			// One recording declared three times over, as the packager does.
			_, _ = io.WriteString(w, "#EXTM3U\n"+
				`#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="audio-aacl-128",LANGUAGE="fr",NAME="Français",DEFAULT=YES,AUTOSELECT=YES,URI="audio_fra.m3u8"`+"\n"+
				`#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="audio-aacl-128",LANGUAGE="qaa",NAME="Version Originale",AUTOSELECT=YES,URI="audio_qaa.m3u8"`+"\n"+
				`#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="audio-aacl-128",LANGUAGE="qad",NAME="Français (Audio Description)",AUTOSELECT=YES,URI="audio_qad.m3u8"`+"\n"+
				`#EXT-X-MEDIA:TYPE=SUBTITLES,GROUP-ID="textstream",LANGUAGE="qhh",NAME="Français (Sourds et malentendants)",AUTOSELECT=YES,URI="text_qhh.m3u8"`+"\n"+
				`#EXT-X-STREAM-INF:BANDWIDTH=3850000,CODECS="mp4a.40.2,avc1.64001F",RESOLUTION=1280x720,AUDIO="audio-aacl-128",SUBTITLES="textstream"`+"\nvideo.m3u8\n")
		case path == "/cdn/video.m3u8", strings.HasPrefix(path, "/cdn/audio_"):
			name := strings.TrimSuffix(strings.TrimPrefix(path, "/cdn/"), ".m3u8")
			_, _ = fmt.Fprintf(w, "#EXTM3U\n#EXT-X-TARGETDURATION:6\n#EXT-X-MAP:URI=\"%s.m4s\"\n#EXTINF:5.76, no desc\n%s-1.m4s\n#EXT-X-ENDLIST\n", name, name)
		case path == "/cdn/text_qhh.m3u8":
			_, _ = io.WriteString(w, "#EXTM3U\n#EXT-X-TARGETDURATION:6\n#EXTINF:5.76, no desc\ntext_qhh-1.webvtt\n#EXT-X-ENDLIST\n")
		case path == "/article/un-article-invente-4000":
			_, _ = fmt.Fprintf(w, `<html><head><title>Un article inventé</title></head><body>
				<iframe data-src="https://auvio.rtbf.be/embed/rtbf/highlight/media/1001?autoplay=1&amp;mute=1" data-asset-id="1001"></iframe>
				<script>{"embedUrl":"https:\/\/auvio.rtbf.be\/embed\/rtbf\/highlight\/media\/1002"}</script></body></html>`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *rtbfFake) extractor() *RTBF {
	r := NewRTBF(httpx.New("test-agent", "en-US", 0, 5*time.Second))
	r.bff = f.srv.URL + "/bff"
	r.exposure = f.srv.URL + "/exposure"
	return r
}

func (f *rtbfFake) extract(t *testing.T, raw string, opts Options) *Result {
	t.Helper()
	res := extractOK(t, f.extractor(), raw, opts)
	return res
}

// A video is the HLS its anonymous session is given, named for its
// programme and episode. A recording declared three times over is taken
// once, since the asset itself has one audio track, and its subtitles come
// with it.
func TestRTBFReadsAVideoWithTheAudioItReallyHas(t *testing.T) {
	f := newRTBFFake(t)
	res := f.extract(t, "https://auvio.rtbf.be/media/un-programme-invente-episode-1001", Options{})
	file := res.Files[0]
	if res.Title != "Un programme inventé - Épisode 1001" || file.Name != "Un programme inventé - Épisode 1001.mp4" {
		t.Errorf("title %q, name %q", res.Title, file.Name)
	}
	if len(file.Segments) != 2 || len(file.Audio) != 1 || file.Audio[0].Name != "Français" || len(file.Subtitles) != 1 {
		t.Errorf("segments %v, audio %+v, subtitles %+v", file.Segments, file.Audio, file.Subtitles)
	}

	// Where the asset has several tracks, every one the master names is kept.
	res = f.extract(t, "https://auvio.rtbf.be/media/un-programme-invente-episode-1010", Options{})
	if audio := res.Files[0].Audio; len(audio) != 3 {
		t.Errorf("audio %+v; want every track kept", audio)
	}
}

// A podcast episode is one MP3, and a session Red Bee no longer takes is
// made again.
func TestRTBFReadsAPodcastAndRenewsItsSession(t *testing.T) {
	f := newRTBFFake(t)
	r := f.extractor()
	const link = "https://auvio.rtbf.be/media/un-podcast-invente-episode-1002"
	extractOK(t, r, link, Options{})
	f.stale.Store(true)
	res := extractOK(t, r, link, Options{})
	if file := res.Files[0]; file.Name != "Un programme inventé - Épisode 1002.mp3" || file.URL != f.srv.URL+"/aod/an-invented-episode.mp3" {
		t.Errorf("name %q, url %q", file.Name, file.URL)
	}
	if n := f.sessions.Load(); n != 2 {
		t.Errorf("%d sessions made; want one, and one more when it went stale", n)
	}
}

// A programme is its tabs, each a folder, each list followed page by page
// by its own next link; what cannot be had here is left out, the note
// counting each reason once.
func TestRTBFListsAProgrammeByTab(t *testing.T) {
	f := newRTBFFake(t)
	res := f.extract(t, "https://auvio.rtbf.be/emission/un-podcast-invente-2000", Options{})
	var got []string
	for _, file := range res.Files {
		got = append(got, file.Dir+"/"+file.Name)
	}
	want := []string{
		"Année 2026/Un podcast inventé - Le premier.mp4",
		"Année 2026/Un podcast inventé - Le deuxième.mp3",
		"Année 2026/Un podcast inventé - Le troisième.mp4",
		"Année 2025/Un podcast inventé - Le cinquième.mp4",
	}
	if res.Title != "Un podcast inventé" || strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("%q:\n%s\nwant:\n%s", res.Title, strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if note := "1 left out: RTBF shows it to signed-in viewers only; 1 left out: RTBF shows it in Belgium only, or not yet"; res.Note != note {
		t.Errorf("note %q, want %q", res.Note, note)
	}
	if target, err := res.Files[1].Resolve(context.Background()); err != nil || target.Name != "Un podcast inventé - Le deuxième.mp3" {
		t.Errorf("target %+v, %v", target, err)
	}
}

// A page embedding players, an article's, is read for them, whether named
// in an attribute or in the page's script.
func TestRTBFReadsThePlayersAPageEmbeds(t *testing.T) {
	f := newRTBFFake(t)
	res := f.extract(t, f.srv.URL+"/article/un-article-invente-4000", Options{})
	if res.Title != "Un article inventé" || len(res.Files) != 2 || !strings.HasSuffix(res.Files[1].Name, ".mp3") {
		t.Errorf("%q: %+v", res.Title, res.Files)
	}
}

// What cannot be had says why, in the play answer's own terms.
func TestRTBFSaysWhyAMediaCannotBeHad(t *testing.T) {
	f := newRTBFFake(t)
	for id, want := range map[string]string{
		"1003": "signed-in viewers only",
		"1004": "Belgium only",
		"1005": "paying viewers only",
		"1006": "it is not on Auvio",
		"1007": "not on Auvio before 2999-01-02",
		"1008": "live stream",
		"1009": "it is not on Auvio",
	} {
		_, err := extract(t, f.extractor(), "https://auvio.rtbf.be/media/un-programme-invente-"+id, Options{})
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v; want it to say %q", id, err, want)
		}
	}
}
