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

// radioCanadaFakeSite stands in for Radio-Canada: pages naming their own
// media in their data layer among other media's ids, an OHdio programme's
// page listing its episodes, the media API, and the CDN, which serves a
// master's every rendition only when not asked to filter them.
func radioCanadaFakeSite(t *testing.T) *httptest.Server {
	t.Helper()
	var srv *httptest.Server
	page := func(w http.ResponseWriter, title, data string) {
		_, _ = fmt.Fprintf(w, `<html><head><title>%s | Radio-Canada</title></head><body><script>%s</script></body></html>`, title, data)
	}
	recommended := `{"media":{"__typename":"Video","appCode":"medianet","id":9001}}`
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		switch path := r.URL.Path; {
		case path == "/info/videos/1-1001/une-video-inventee":
			page(w, "Une vidéo inventée", recommended+`{"IdsCollection":"|ProgramId:24-1|MedianetId:1001|","Emission":"Vidéos"}`)
		case path == "/ohdio/balados/2000/un-balado-invente/2001/un-episode":
			page(w, "Un épisode inventé | OHdio", `{"Emission":"Un balado inventé","IdsCollection":"|ProgramId:2000|EpisodeId:2001|MedianetId:1002|"}`)
		case path == "/ohdio/balados/2000/un-balado-invente":
			// Two episodes a page, five in all, the next page asked for by number.
			episodes := map[string][]string{"": {"1002", "1004"}, "1": {"1002", "1004"}, "2": {"1006", "1008"}, "3": {"1010"}}[q.Get("pageNumber")]
			var items []string
			for _, id := range episodes {
				items = append(items, fmt.Sprintf(`{"typename":"EpisodeListItem","isPlayable":true,"mediaIds":[%s],"summary":"","title":"L\u0027épisode %s&nbsp;: inventé","url":"/balados/2000/x/%s/y"}`, id, id, id))
			}
			page(w, "Un balado inventé | OHdio", `{"episodes":[`+strings.Join(items, ",")+`],"pagination":{"pageNumber":1,"pageSize":2,"totalCount":5}}`)
		case strings.HasPrefix(path, "/ohdio/balados/3000/un-balado-a-saisons"):
			// The programme's page shows its latest season, and links to each.
			season := strings.TrimPrefix(path, "/ohdio/balados/3000/un-balado-a-saisons")
			if season == "" {
				season = "/saison-2"
			}
			id := map[string]string{"/saison-1": "1012", "/saison-2": "1014"}[season]
			name := map[string]string{"/saison-1": "La première", "/saison-2": "La deuxième"}[season]
			page(w, "Un balado à saisons | OHdio", `<a href=/ohdio/balados/3000/un-balado-a-saisons/saison-1>Saison 1</a>`+
				`{"numberOfSeasons":2}{"episodes":[{"typename":"EpisodeListItem","isPlayable":true,"mediaIds":[`+id+`],"title":"Un épisode `+id+`","url":"/x"}],`+
				`"pagination":{"pageNumber":1,"pageSize":50,"totalCount":1},"selectedSeason":{"fullName":"`+name+`","name":"Saison `+season[len(season)-1:]+`","summary":""}}`)
		case path == "/media/validation/v2/":
			if q.Get("appCode") != "medianet" || q.Get("deviceType") == "" || q.Get("connectionType") == "" {
				http.Error(w, "missing parameters", http.StatusBadRequest)
				return
			}
			switch q.Get("idMedia") {
			case "1001", "1003":
				_, _ = fmt.Fprintf(w, `{"url": "%s/out/v1/video/index.m3u8?aws.manifestfilter=video_bitrate:0-1500000", "message": null, "errorCode": 0}`, srv.URL)
			case "1002", "1004", "1006", "1008", "1010", "1012", "1014":
				_, _ = fmt.Fprintf(w, `{"url": "%s/audio/master.m3u8", "message": null, "errorCode": 0}`, srv.URL)
			case "1005":
				_, _ = fmt.Fprintf(w, `{"url": "%s/out/v1/strict/index.m3u8?aws.manifestfilter=video_bitrate:0-1500000", "message": null, "errorCode": 0}`, srv.URL)
			default:
				_, _ = io.WriteString(w, `{"url": null, "message": "Le contenu sélectionné n'est pas disponible dans votre pays", "errorCode": 1}`)
			}
		case path == "/media/meta/v1/index.ashx":
			_, _ = io.WriteString(w, `{"Metas": [{"name": "Title", "text": "Un titre inventé"}, {"name": "Emission", "text": "Une émission inventée"}]}`)
		case path == "/out/v1/strict/index.m3u8" && !q.Has("aws.manifestfilter"):
			http.Error(w, "refused", http.StatusForbidden)
		case path == "/out/v1/video/index.m3u8", path == "/out/v1/strict/index.m3u8":
			master := "#EXTM3U\n" + `#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="audio_0",CHANNELS="2",NAME="audio1_fr",LANGUAGE="fra",DEFAULT=YES,AUTOSELECT=YES,URI="audio.m3u8"` + "\n" +
				`#EXT-X-STREAM-INF:BANDWIDTH=1503268,RESOLUTION=360x640,CODECS="avc1.64001E,mp4a.40.2",AUDIO="audio_0"` + "\nlow.m3u8\n"
			if !q.Has("aws.manifestfilter") {
				master += `#EXT-X-STREAM-INF:BANDWIDTH=5251549,RESOLUTION=576x1024,CODECS="avc1.64001F,mp4a.40.2",AUDIO="audio_0"` + "\nhigh.m3u8\n"
			}
			_, _ = io.WriteString(w, master)
		case strings.HasSuffix(path, ".m3u8"):
			name := strings.TrimSuffix(path[strings.LastIndex(path, "/")+1:], ".m3u8")
			if name == "master" {
				_, _ = io.WriteString(w, "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=124333,CODECS=\"mp4a.40.2\"\nindex_1_a.m3u8\n")
				return
			}
			_, _ = fmt.Fprintf(w, "#EXTM3U\n#EXT-X-TARGETDURATION:2\n#EXTINF:2.002,\n%s_0.ts\n#EXT-X-ENDLIST\n", name)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func radioCanadaWith(srv *httptest.Server) *RadioCanada {
	r := NewRadioCanada(httpx.New("test-agent", "en-US", 0, 5*time.Second))
	r.api = srv.URL + "/media/"
	return r
}

func radioCanadaExtract(t *testing.T, srv *httptest.Server, path string) (*Result, error) {
	t.Helper()
	u, _ := url.Parse(srv.URL + path)
	return radioCanadaWith(srv).Extract(context.Background(), u, Options{})
}

// A video page's own media is the one its data layer names, not the first
// id on the page, and its stream is taken whole, without the filter the API
// puts on its renditions.
func TestRadioCanadaReadsThePagesOwnMediaUnfiltered(t *testing.T) {
	srv := radioCanadaFakeSite(t)
	res, err := radioCanadaExtract(t, srv, "/info/videos/1-1001/une-video-inventee")
	if err != nil {
		t.Fatal(err)
	}
	f := res.Files[0]
	if res.Title != "Une vidéo inventée" || f.Name != "Une vidéo inventée.mp4" {
		t.Errorf("title %q, name %q", res.Title, f.Name)
	}
	if strings.Contains(f.URL, "manifestfilter") || len(f.Segments) != 1 || !strings.HasSuffix(f.Segments[0], "/high_0.ts") {
		t.Errorf("url %q, segments %v; want the unfiltered master's top rendition", f.URL, f.Segments)
	}
	if len(f.Audio) != 1 || f.Audio[0].Name != "" || f.Audio[0].Language != "fra" {
		t.Errorf("audio %+v; want the French track, the packager's name left off", f.Audio)
	}
}

// An OHdio episode is named for its programme, and the older links name
// their media outright, named by the API.
func TestRadioCanadaReadsEpisodesAndOlderLinks(t *testing.T) {
	srv := radioCanadaFakeSite(t)
	res, err := radioCanadaExtract(t, srv, "/ohdio/balados/2000/un-balado-invente/2001/un-episode")
	if err != nil {
		t.Fatal(err)
	}
	if f := res.Files[0]; f.Name != "Un balado inventé - Un épisode inventé.ts" || len(f.Audio) != 0 {
		t.Errorf("name %q, audio %+v", f.Name, f.Audio)
	}
	res, err = radioCanadaExtract(t, srv, "/widgets/mediaconsole/medianet/1003")
	if err != nil {
		t.Fatal(err)
	}
	if name := res.Files[0].Name; name != "Une émission inventée - Un titre inventé.mp4" {
		t.Errorf("name %q", name)
	}
}

// An OHdio programme is every page of its list, asked for by number as the
// site's own loader asks, its episodes named as they resolve.
func TestRadioCanadaListsEveryPageOfAProgramme(t *testing.T) {
	srv := radioCanadaFakeSite(t)
	res, err := radioCanadaExtract(t, srv, "/ohdio/balados/2000/un-balado-invente")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, f := range res.Files {
		names = append(names, f.Name)
	}
	want := []string{
		"Un balado inventé - L'épisode 1002 : inventé.ts", "Un balado inventé - L'épisode 1004 : inventé.ts",
		"Un balado inventé - L'épisode 1006 : inventé.ts", "Un balado inventé - L'épisode 1008 : inventé.ts",
		"Un balado inventé - L'épisode 1010 : inventé.ts",
	}
	if res.Title != "Un balado inventé" || strings.Join(names, "|") != strings.Join(want, "|") || res.Note != "" {
		t.Errorf("%q (note %q):\n%s", res.Title, res.Note, strings.Join(names, "\n"))
	}
	target, err := res.Files[4].Resolve(context.Background())
	if err != nil || target.Name != want[4] {
		t.Errorf("target %+v, %v", target, err)
	}
}

// A programme with seasons is each season its page links to, in a folder
// of its own, though its page shows the latest alone; a season's own link
// is that season alone.
func TestRadioCanadaListsEverySeason(t *testing.T) {
	srv := radioCanadaFakeSite(t)
	res, err := radioCanadaExtract(t, srv, "/ohdio/balados/3000/un-balado-a-saisons")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, f := range res.Files {
		got = append(got, f.Dir+"/"+f.Name)
	}
	want := "Saison 1 - La première/Un balado à saisons - Un épisode 1012.ts|Saison 2 - La deuxième/Un balado à saisons - Un épisode 1014.ts"
	if strings.Join(got, "|") != want {
		t.Errorf("%s\nwant %s", strings.Join(got, "|"), want)
	}
	res, err = radioCanadaExtract(t, srv, "/ohdio/balados/3000/un-balado-a-saisons/saison-1")
	if err != nil {
		t.Fatal(err)
	}
	if res.Title != "Un balado à saisons - Saison 1 - La première" || len(res.Files) != 1 || res.Files[0].Dir != "" ||
		!strings.HasSuffix(res.Files[0].Name, "Un épisode 1012.ts") {
		t.Errorf("%q: %+v; want the first season alone, under its name", res.Title, res.Files)
	}
}

// Where the CDN will not serve a master unfiltered, the API's own link is
// taken, filter and all.
func TestRadioCanadaFallsBackToTheFilteredMaster(t *testing.T) {
	srv := radioCanadaFakeSite(t)
	res, err := radioCanadaExtract(t, srv, "/widgets/mediaconsole/medianet/1005")
	if err != nil {
		t.Fatal(err)
	}
	if f := res.Files[0]; !strings.Contains(f.URL, "manifestfilter") || !strings.HasSuffix(f.Segments[0], "/low_0.ts") {
		t.Errorf("url %q, segments %v; want the filtered master's", f.URL, f.Segments)
	}
}

// A media the API names no stream for says why, in the API's words.
func TestRadioCanadaPassesOnTheAPIsRefusal(t *testing.T) {
	srv := radioCanadaFakeSite(t)
	_, err := radioCanadaExtract(t, srv, "/widgets/mediaconsole/medianet/1009")
	if err == nil || !strings.Contains(err.Error(), "n'est pas disponible dans votre pays") {
		t.Errorf("err = %v", err)
	}
}
