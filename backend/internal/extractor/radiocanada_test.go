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
			page(w, "Un balado inventé | OHdio", `{"episodes":[`+
				`{"typename":"EpisodeListItem","isPlayable":true,"mediaIds":[1002],"summary":"<p>Un résumé.</p>","title":"Le premier&nbsp;: inventé","url":"/balados/2000/x/2001/y"},`+
				`{"typename":"EpisodeListItem","isPlayable":true,"mediaIds":[1004],"summary":"","title":"Le deuxième","url":"/balados/2000/x/2002/z"}`+
				`],"pagination":{"pageNumber":1,"pageSize":2,"totalCount":30}}`)
		case path == "/media/validation/v2/":
			if q.Get("appCode") != "medianet" || q.Get("deviceType") == "" || q.Get("connectionType") == "" {
				http.Error(w, "missing parameters", http.StatusBadRequest)
				return
			}
			switch q.Get("idMedia") {
			case "1001", "1003":
				_, _ = fmt.Fprintf(w, `{"url": "%s/out/v1/video/index.m3u8?aws.manifestfilter=video_bitrate:0-1500000", "message": null, "errorCode": 0}`, srv.URL)
			case "1002", "1004":
				_, _ = fmt.Fprintf(w, `{"url": "%s/audio/master.m3u8", "message": null, "errorCode": 0}`, srv.URL)
			default:
				_, _ = io.WriteString(w, `{"url": null, "message": "Le contenu sélectionné n'est pas disponible dans votre pays", "errorCode": 1}`)
			}
		case path == "/media/meta/v1/index.ashx":
			_, _ = io.WriteString(w, `{"Metas": [{"name": "Title", "text": "Un titre inventé"}, {"name": "Emission", "text": "Une émission inventée"}]}`)
		case path == "/out/v1/video/index.m3u8":
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

// An OHdio programme is the episodes its page lists, named as they resolve,
// and the note says how many more it has.
func TestRadioCanadaListsAProgrammesEpisodes(t *testing.T) {
	srv := radioCanadaFakeSite(t)
	res, err := radioCanadaExtract(t, srv, "/ohdio/balados/2000/un-balado-invente")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, f := range res.Files {
		names = append(names, f.Name)
	}
	want := "Un balado inventé - Le premier : inventé.ts|Un balado inventé - Le deuxième.ts"
	if res.Title != "Un balado inventé" || strings.Join(names, "|") != want || res.Note != "2 of 30 episodes" {
		t.Errorf("%q: %v, note %q", res.Title, names, res.Note)
	}
	target, err := res.Files[1].Resolve(context.Background())
	if err != nil || target.Name != "Un balado inventé - Le deuxième.ts" {
		t.Errorf("target %+v, %v", target, err)
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
