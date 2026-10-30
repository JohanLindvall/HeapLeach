// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
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

func TestNiconicoLink(t *testing.T) {
	for raw, want := range map[string]struct {
		kind int
		id   string
	}{
		"https://www.nicovideo.jp/watch/sm1001":            {niconicoVideo, "sm1001"},
		"https://sp.nicovideo.jp/watch/so2002?ref=example": {niconicoVideo, "so2002"},
		"https://www.nicovideo.jp/watch/3003":              {niconicoVideo, "3003"},
		"https://nico.ms/sm1001":                           {niconicoVideo, "sm1001"},
		"https://www.nicovideo.jp/mylist/4004":             {niconicoMylist, "4004"},
		"https://www.nicovideo.jp/user/5005/mylist/4004":   {niconicoMylist, "4004"},
		"https://www.nicovideo.jp/series/6006":             {niconicoSeries, "6006"},
		"https://www.nicovideo.jp/user/5005/series/6006":   {niconicoSeries, "6006"},
		"https://www.nicovideo.jp/user/5005":               {niconicoUploads, "5005"},
		"https://www.nicovideo.jp/user/5005/video":         {niconicoUploads, "5005"},
		// Pages still handed to yt-dlp.
		"https://www.nicovideo.jp/tag/an-invented-tag": {0, ""},
		"https://www.nicovideo.jp/ranking":             {0, ""},
		"https://www.nicovideo.jp/watch/not-an-id":     {0, ""},
	} {
		u, _ := url.Parse(raw)
		if kind, id := niconicoLink(u); kind != want.kind || id != want.id {
			t.Errorf("%s: %d %q, want %d %q", raw, kind, id, want.kind, want.id)
		}
	}
}

// niconicoWatchPage is a watch page describing a video as the site does, in
// the escaped JSON of a meta tag, behind the given status.
func niconicoWatchPage(w http.ResponseWriter, status int, description string) {
	w.Header().Set(httpx.HeaderContentType, "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, `<!doctype html><html><head><meta name="server-response" content="%s"></head><body></body></html>`,
		html.EscapeString(description))
}

// niconicoSite stands in for the watch pages, the API and the stream: a
// grant for the pair asked for, a master naming its audio apart, and AES-128
// fragmented MP4 whose key is refused without the grant's cookie.
func niconicoSite(t *testing.T) (*httptest.Server, *[][]string) {
	t.Helper()
	var asked [][]string
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch path := r.URL.Path; path {
		case "/watch/sm1001":
			niconicoWatchPage(w, http.StatusOK, `{"meta": {"status": 200}, "data": {"response": {
				"video": {"id": "sm1001", "title": "An Invented Video"},
				"client": {"watchTrackId": "track-1"},
				"media": {"domand": {"accessRightKey": "an-invented-key",
					"videos": [{"id": "video-h264-1080p", "isAvailable": false, "height": 1080, "bitRate": 4000000},
						{"id": "video-h264-720p", "isAvailable": true, "height": 720, "bitRate": 2000000},
						{"id": "video-h264-360p", "isAvailable": true, "height": 360, "bitRate": 600000}],
					"audios": [{"id": "audio-aac-64kbps", "isAvailable": true, "bitRate": 64000},
						{"id": "audio-aac-128kbps", "isAvailable": true, "bitRate": 128000}]}}}}}`)
		case "/watch/sm404":
			niconicoWatchPage(w, http.StatusNotFound, `{"meta": {"status": 404, "code": "NOT_FOUND"},
				"data": {"response": {"errorCode": "NOT_FOUND"}}}`)
		case "/watch/sm400":
			niconicoWatchPage(w, http.StatusBadRequest, `{"meta": {"status": 400, "code": "FORBIDDEN"},
				"data": {"response": {"errorCode": "FORBIDDEN", "reasonCode": "AN_INVENTED_REASON"}}}`)
		case "/watch/sm500":
			niconicoWatchPage(w, http.StatusOK, `{"meta": {"status": 200}, "data": {"response": {
				"video": {"title": "A Members' Video"}, "media": {"domand": null}}}}`)
		case "/v1/watch/sm1001/access-rights/hls":
			var body struct {
				Outputs [][]string `json:"outputs"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if r.Method != http.MethodPost || r.URL.Query().Get("actionTrackId") != "track-1" ||
				r.Header.Get("X-Access-Right-Key") != "an-invented-key" || r.Header.Get("X-Frontend-Id") != "6" {
				http.Error(w, "refused", http.StatusBadRequest)
				return
			}
			asked = append(asked, body.Outputs...)
			http.SetCookie(w, &http.Cookie{Name: "domand_bid", Value: "an-invented-cookie", Path: "/"})
			w.WriteHeader(http.StatusCreated)
			_, _ = fmt.Fprintf(w, `{"meta": {"status": 201}, "data": {"contentUrl": "%s/hls/master.m3u8?session=1"}}`, srv.URL)
		case "/hls/master.m3u8":
			_, _ = io.WriteString(w, "#EXTM3U\n"+
				`#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="audio-aac-128kbps",NAME="Main Audio",DEFAULT=YES,URI="audio.m3u8"`+"\n"+
				`#EXT-X-STREAM-INF:BANDWIDTH=2500000,CODECS="avc1.4d401e,mp4a.40.2",RESOLUTION=1280x720,AUDIO="audio-aac-128kbps"`+
				"\nvideo.m3u8\n")
		case "/hls/video.m3u8", "/hls/audio.m3u8":
			kind := strings.TrimSuffix(strings.TrimPrefix(path, "/hls/"), ".m3u8")
			_, _ = io.WriteString(w, "#EXTM3U\n#EXT-X-TARGETDURATION:6\n#EXT-X-MEDIA-SEQUENCE:1\n"+
				`#EXT-X-MAP:URI="`+kind+`-init.cmfv"`+"\n"+
				`#EXT-X-KEY:METHOD=AES-128,URI="/keys/`+kind+`.key",IV=0x28CD62225B4E1B736063663FA69385D2`+"\n"+
				"#EXTINF:6,\n"+kind+"-1.cmfv\n#EXTINF:6,\n"+kind+"-2.cmfv\n#EXT-X-ENDLIST\n")
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &asked
}

func niconicoWith(srv *httptest.Server) *Niconico {
	n := NewNiconico(httpx.New("test-agent", "en-US", 0, 5*time.Second))
	n.watch, n.api = srv.URL+"/watch/", srv.URL
	return n
}

// A video is read off its watch page when added, and its stream minted when
// the transfer starts: the best quality of each kind offered without an
// account, granted together, with the cookie the key server wants carried
// on every request of the transfer.
func TestNiconicoMintsTheStreamWhenTheTransferStarts(t *testing.T) {
	srv, asked := niconicoSite(t)
	u, _ := url.Parse("https://www.nicovideo.jp/watch/sm1001")
	res, err := niconicoWith(srv).Extract(context.Background(), u, Options{})
	if err != nil {
		t.Fatal(err)
	}
	f := res.Files[0]
	if res.Title != "An Invented Video" || f.Name != "An Invented Video.mp4" || len(f.Segments) != 0 || f.Resolve == nil {
		t.Fatalf("result %q, file %+v; want the video named, its stream left to the transfer", res.Title, f)
	}
	if len(*asked) != 0 {
		t.Errorf("a stream was granted before the transfer asked: %v", *asked)
	}

	target, err := f.Resolve(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprint(*asked); got != "[[video-h264-720p audio-aac-128kbps]]" {
		t.Errorf("asked for %s, want the best available video with the best audio", got)
	}
	if target.URL != srv.URL+"/hls/master.m3u8?session=1" || target.Headers[httpx.HeaderCookie] != "domand_bid=an-invented-cookie" {
		t.Errorf("target URL %q, headers %v", target.URL, target.Headers)
	}
	if len(target.Segments) != 3 || target.SegmentKey == nil || target.SegmentKey.Clear != 1 || len(target.SegmentKey.IV) != 16 {
		t.Errorf("segments %v, key %+v; want the initialisation segment and two parts, the first in the clear",
			target.Segments, target.SegmentKey)
	}
	if len(target.Audio) != 1 || target.Audio[0].Name != "Main Audio" || target.Audio[0].Key == nil ||
		target.Audio[0].Key.Clear != 1 {
		t.Errorf("audio = %+v", target.Audio)
	}
}

// A video that cannot be had says why, in the page's own words where it
// gives them.
func TestNiconicoSaysWhyAVideoCannotBeHad(t *testing.T) {
	srv, _ := niconicoSite(t)
	for id, want := range map[string]string{
		"sm404": "sm404 does not exist",
		"sm400": "cannot be watched: AN_INVENTED_REASON",
		"sm500": errNiconicoNoStream.Error(),
	} {
		u, _ := url.Parse("https://www.nicovideo.jp/watch/" + id)
		_, err := niconicoWith(srv).Extract(context.Background(), u, Options{})
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v; want it to say %q", id, err, want)
		}
	}
}

// A listing is read a page at a time off the API, one file per video named
// from the listing itself, so no watch page is fetched until a transfer
// starts. A video gone from the site, or one only a payment fetches, is
// left out, and the job's note says how many and how much of the listing
// the cap kept.
func TestNiconicoListsMylistsSeriesAndUploads(t *testing.T) {
	var watchPages atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Frontend-Id") != "6" {
			http.Error(w, "refused", http.StatusBadRequest)
			return
		}
		page := r.URL.Query().Get("page")
		switch {
		case strings.HasPrefix(r.URL.Path, "/watch/"):
			watchPages.Add(1)
		case r.URL.Path == "/v2/mylists/4004" && page == "1":
			_, _ = io.WriteString(w, `{"data": {"mylist": {"name": "An Invented Mylist", "totalItemCount": 5, "hasNext": true,
				"items": [{"status": "public", "video": {"id": "sm1", "title": "First"}},
					{"status": "deleted", "video": {"id": "sm2", "title": "Gone"}},
					{"status": "public", "video": {"id": "sm3", "title": "Paid", "isPaymentRequired": true}}]}}}`)
		case r.URL.Path == "/v2/mylists/4004" && page == "2":
			_, _ = io.WriteString(w, `{"data": {"mylist": {"name": "An Invented Mylist", "totalItemCount": 5, "hasNext": false,
				"items": [{"status": "public", "video": {"id": "sm4", "title": "Fourth"}},
					{"status": "public", "video": {"id": "sm5", "title": "Fifth"}}]}}}`)
		case r.URL.Path == "/v2/series/6006":
			_, _ = io.WriteString(w, `{"data": {"detail": {"title": "An Invented Series"}, "totalCount": 2,
				"items": [{"video": {"id": "sm7", "title": "Part One"}}, {"video": {"id": "sm8", "title": "Part Two"}}]}}`)
		case r.URL.Path == "/v3/users/5005/videos":
			_, _ = io.WriteString(w, `{"data": {"totalCount": 1,
				"items": [{"essential": {"id": "sm9", "title": "An Upload", "owner": {"name": "an-invented-user"}}}]}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	n := niconicoWith(srv)

	for _, tc := range []struct {
		link   string
		limits Limits
		title  string
		files  []string
		note   string
	}{
		{"https://www.nicovideo.jp/mylist/4004", Limits{}, "An Invented Mylist",
			[]string{"First.mp4", "Fourth.mp4", "Fifth.mp4"},
			"1 left out that need an account or a payment; 1 no longer on the site"},
		{"https://www.nicovideo.jp/user/5005/mylist/4004", Limits{Files: 2}, "An Invented Mylist",
			[]string{"First.mp4", "Fourth.mp4"},
			"2 of 5 videos; 1 left out that need an account or a payment; 1 no longer on the site"},
		{"https://www.nicovideo.jp/series/6006", Limits{}, "An Invented Series",
			[]string{"Part One.mp4", "Part Two.mp4"}, ""},
		{"https://www.nicovideo.jp/user/5005/video", Limits{}, "an-invented-user", []string{"An Upload.mp4"}, ""},
	} {
		u, _ := url.Parse(tc.link)
		res, err := n.Extract(context.Background(), u, Options{limits: tc.limits})
		if err != nil {
			t.Errorf("%s: %v", tc.link, err)
			continue
		}
		var names []string
		for _, f := range res.Files {
			names = append(names, f.Name)
			if f.Resolve == nil {
				t.Errorf("%s: %s has no resolver", tc.link, f.Name)
			}
		}
		if res.Title != tc.title || strings.Join(names, ",") != strings.Join(tc.files, ",") || res.Note != tc.note {
			t.Errorf("%s: %q %v note %q; want %q %v note %q", tc.link, res.Title, names, res.Note, tc.title, tc.files, tc.note)
		}
	}
	if n := watchPages.Load(); n != 0 {
		t.Errorf("listing fetched %d watch pages, want none", n)
	}

	u, _ := url.Parse("https://www.nicovideo.jp/mylist/9999")
	if _, err := n.Extract(context.Background(), u, Options{}); err == nil || !strings.Contains(err.Error(), "private, or does not exist") {
		t.Errorf("a missing mylist: %v", err)
	}
}
