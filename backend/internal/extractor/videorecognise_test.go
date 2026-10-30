// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

// head builds a document head from a title and meta elements, key=value.
func head(title string, metas ...string) string {
	var b strings.Builder
	b.WriteString("<html><head><title>" + title + "</title>")
	for _, m := range metas {
		k, v, _ := strings.Cut(m, "=")
		b.WriteString(`<meta property="` + k + `" content="` + v + `">`)
	}
	b.WriteString("</head><body>")
	return b.String()
}

const site = "https://site.example.test"

func TestVideoRecognition(t *testing.T) {
	handoff := func(name string) Recogniser {
		for _, e := range NewHandoffs(nil) {
			if e.Name() == name {
				return e.(Recogniser)
			}
		}
		t.Fatalf("no handoff %s", name)
		return nil
	}
	ytWatch := head("First Clip - YouTube", "og:site_name=YouTube") +
		`<script>ytcfg.set({"A":1});var ytInitialPlayerResponse = {};</script></body></html>`
	cases := []recogniseCase{
		{
			name: "youtube", r: handoff("youtube"),
			good: site + "/watch?v=abcdefghijk", wrongForm: site + "/about",
			page:     ytWatch,
			unmarked: head("First Clip - YouTube", "og:site_name=YouTube") + "</body></html>",
			shapes: []string{site + "/watch?v=abcdefghijk", site + "/shorts/abcdefghijk", site + "/embed/abcdefghijk",
				site + "/playlist?list=PLabc", site + "/@a-channel"},
			notShapes: []string{site + "/watch?v=short", site + "/", site + "/abcdefghijk"},
			extraGood: []*recogniseExtra{
				{site + "/embed/abcdefghijk", `<html><script>ytcfg.set({});var x = {"embedded_player_response":1}</script></html>`},
				// A consent page judged by where it continues to.
				{"https://consent.example.test/m?continue=" + "https%3A%2F%2Fvideo.example.test%2Fshorts%2Fabcdefghijk",
					`<html><form action="/save"><input type="hidden" name="set_ytc" value="true"></form></html>`},
			},
			extraWrong: []*recogniseExtra{
				{"https://consent.example.test/m?continue=" + "https%3A%2F%2Fvideo.example.test%2Fabout",
					`<html><form action="/save"><input type="hidden" name="set_ytc" value="true"></form></html>`},
			},
		},
		{
			name: "vimeo watch", r: NewVimeo(nil),
			good: site + "/123456789", wrongForm: site + "/features",
			page:      head("First Clip", "og:site_name=Vimeo", "og:video:url=https://player.example.test/video/123456789?h=ab12") + "</body></html>",
			unmarked:  head("First Clip", "og:site_name=Vimeo") + "</body></html>",
			shapes:    []string{site + "/123456789", site + "/123456789/abcdef1234", site + "/video/123456789", site + "/channels/staff/123456789"},
			notShapes: []string{site + "/", site + "/features"},
		},
		{
			name: "vimeo player", r: NewVimeo(nil),
			good: site + "/video/123456789", wrongForm: site + "/watch/123456789",
			page:     `<html><script>window.playerConfig = {"view":1}</script></html>`,
			unmarked: head("First Clip on Vimeo", "og:site_name=Vimeo") + "</body></html>",
		},
		{
			name: "niconico", r: NewNiconico(nil),
			// Every page of the site that carries its description or its web
			// client is the extractor's, the rest handed to yt-dlp, so no form
			// of the site's is wrong.
			good:      site + "/watch/sm1234",
			page:      head("First Clip - ニコニコ動画", "og:site_name=ニコニコ動画") + `<meta name="server-response" content="{}"></body></html>`,
			unmarked:  head("First Clip - ニコニコ動画", "og:site_name=ニコニコ動画") + "</body></html>",
			shapes:    []string{site + "/watch/sm1234", site + "/sm1234", site + "/mylist/12", site + "/user/12/video"},
			notShapes: []string{site + "/", site + "/tag/a-tag"},
			extraGood: []*recogniseExtra{
				{site + "/user/12", head("ユーザー - ニコニコ", "og:site_name=ニコニコ") + `<script src="/web/scripts/bundle/vendor.js"></script></body></html>`},
			},
			extraWrong: []*recogniseExtra{
				// The description without the site's name is someone else's.
				{site + "/watch/sm1234", `<html><head><title>A blog</title><meta name="server-response" content="{}"></head></html>`},
			},
		},
		{
			name: "yandex", r: pointerNamed(t, "yandex"),
			good: site + "/video/preview/1234567890", wrongForm: site + "/video/search",
			page:      `<html><head><title>Video search</title><script src="/s3/fiji-static/_/runtime.js"></script></head><body><a class="VideoViewer-Source" href="https://clips.example.test/1">x</a></body></html>`,
			unmarked:  `<html><head><title>Video search</title><script src="/s3/fiji-static/_/runtime.js"></script></head><body></body></html>`,
			shapes:    []string{site + "/video/preview/1234567890"},
			notShapes: []string{site + "/video/", site + "/search/"},
		},
		{
			name: "pornhub", r: NewPlayer(nil, &pornhubPlayer),
			good: site + "/view_video.php?viewkey=abc123def", wrongForm: site + "/video?page=2",
			page:      head("First Clip - Pornhub.com", "og:site_name=Pornhub") + `<script>var flashvars_123 = {"mediaDefinitions":[]};</script></body></html>`,
			unmarked:  head("First Clip - Pornhub.com", "og:site_name=Pornhub") + "</body></html>",
			shapes:    []string{site + "/view_video.php?viewkey=abc123def"},
			notShapes: []string{site + "/view_video.php", site + "/"},
		},
		{
			name: "xhamster", r: NewPlayer(nil, &xhamsterPlayer),
			good: site + "/videos/first-clip-xhAb12c", wrongForm: site + "/categories/first",
			page:      head("First Clip | xHamster", "og:site_name=xHamster") + `<script>window.initials = {"xplayerSettings":{}};</script></body></html>`,
			unmarked:  head("First Clip | xHamster", "og:site_name=xHamster") + "</body></html>",
			shapes:    []string{site + "/videos/first-clip-xhAb12c"},
			notShapes: []string{site + "/categories/first", site + "/"},
		},
		{
			name: "tnaflix", r: NewPlayer(nil, &tnaflixPlayer),
			good: site + "/amateur/first-clip/video123", wrongForm: site + "/amateur",
			page:      `<html><head><title>First Clip - Tnaflix.com</title></head><body><video><source src="https://cdn.example.test/1/first-480p.mp4" type="video/mp4"></video></body></html>`,
			unmarked:  `<html><head><title>First Clip - Tnaflix.com</title></head><body></body></html>`,
			shapes:    []string{site + "/amateur/first-clip/video123"},
			notShapes: []string{site + "/amateur", site + "/de"},
		},
		{
			name: "pornone", r: NewPlayer(nil, &pornonePlayer),
			good: site + "/funny/first-clip/12345678/", wrongForm: site + "/funny/",
			page:      `<html><head><title>first clip - PornOne ex vPorn</title></head><body><video><source src="https://cdn.example.test/1/12345678_1920x1080_4000k.mp4"></video></body></html>`,
			unmarked:  `<html><head><title>first clip - PornOne ex vPorn</title></head><body><video><source src="https://cdn.example.test/1/first.mp4"></video></body></html>`,
			shapes:    []string{site + "/funny/first-clip/12345678/"},
			notShapes: []string{site + "/de/", site + "/funny/first-clip/1/"},
		},
		{
			name: "drtuber", r: NewPlayer(nil, &drtuberPlayer),
			good: site + "/video/1234567/first-clip", wrongForm: site + "/categories/first",
			page:      `<html><head><title>First Clip at DrTuber</title></head><body><script>var configUrl = "/player_config_json/?vid=1234567";</script></body></html>`,
			unmarked:  `<html><head><title>First Clip at DrTuber</title></head><body></body></html>`,
			shapes:    []string{site + "/video/1234567/first-clip", site + "/embed/1234567"},
			notShapes: []string{site + "/2", site + "/categories/first"},
		},
		{
			name: "eporner", r: NewPlayer(nil, &epornerPlayer),
			good: site + "/video-AbC123/first-clip/", wrongForm: site + "/cat/first/",
			page: head("First Clip - EPORNER", "og:site_name=EPORNER.COM") +
				`<script>EP.video.player.vid = 'AbC123'; EP.video.player.hash = '0123456789abcdef0123456789abcdef';</script></body></html>`,
			unmarked:  head("First Clip - EPORNER", "og:site_name=EPORNER.COM") + "</body></html>",
			shapes:    []string{site + "/video-AbC123/first-clip/", site + "/hd-porn/AbC123/First/", site + "/profile/someone/uploaded-videos/"},
			notShapes: []string{site + "/login/", site + "/profile/someone/pictures/"},
			extraGood: []*recogniseExtra{
				{site + "/profile/someone/videos/", head("someone's videos - EPORNER", "og:site_name=EPORNER.COM") + `<script src="/js/1/profile.js"></script></body></html>`},
			},
		},
		{
			name: "bitchute", r: NewBitChute(nil),
			good: site + "/video/AbC123xyz/", wrongForm: site + "/search/",
			page:      head("First Clip", "og:site_name=BitChute", "twitter:player=https://player.example.test/embed/AbC123xyz") + `<script src="/js/app.abc.js"></script></body></html>`,
			unmarked:  head("First Clip", "og:site_name=BitChute") + `<script src="/js/app.abc.js"></script></body></html>`,
			shapes:    []string{site + "/video/AbC123xyz/", site + "/channel/first/"},
			notShapes: []string{site + "/", site + "/search/"},
			extraGood: []*recogniseExtra{
				{site + "/channel/first/", head("BitChute", "og:site_name=BitChute") + `<script src="/js/app.abc.js"></script></body></html>`},
			},
		},
		{
			name: "streamable", r: NewStreamable(nil),
			good: site + "/abc12", wrongForm: site + "/about/us",
			page:      head("Watch first.mp4", "og:site_name=Streamable", "twitter:player=https://site.example.test/t/abc12") + "</body></html>",
			unmarked:  head("Watch first.mp4", "og:site_name=Streamable") + "</body></html>",
			notShapes: []string{site + "/abc12", site + "/e/abc12"},
		},
		{
			name: "vidmoly", r: NewPlayer(nil, &vidmolyPlayer),
			good: site + "/v/abcdefghijkl", wrongForm: site + "/make-money",
			page:      `<html><head><title>First - Cinema</title></head><body><script>window.__NUXT__={}</script><a href="/embed-abcdefghijkl.html">embed</a></body></html>`,
			unmarked:  `<html><head><title>First - Cinema</title></head><body><script>window.__NUXT__={}</script></body></html>`,
			shapes:    []string{site + "/v/abcdefghijkl", site + "/embed-abcdefghijkl.html", site + "/w/abcdefghijkl"},
			notShapes: []string{site + "/", site + "/register"},
			extraGood: []*recogniseExtra{
				{site + "/embed-abcdefghijkl.html", `<html><title>VidMoly</title><script>player.setup({sources: [{file:"https://cdn.example.test/m.m3u8"}]})</script></html>`},
			},
			extraWrong: []*recogniseExtra{
				// The same player setup on another host's player is not this one.
				{site + "/embed-abcdefghijkl.html", `<html><title>Player</title><script>player.setup({sources: [{file:"https://cdn.example.test/m.m3u8"}]})</script></html>`},
			},
		},
		{
			name: "ok.ru", r: NewPlayer(nil, &okruPlayer),
			good: site + "/video/1234567890", wrongForm: site + "/discovery",
			page:      `<html><head><title>OK</title></head><body><div data-module="OKVideo" data-options="{}"></div></body></html>`,
			unmarked:  head("OK", "og:site_name=OK.RU") + "</body></html>",
			shapes:    []string{site + "/video/1234567890", site + "/videoembed/1234567890"},
			notShapes: []string{site + "/video", site + "/discovery"},
		},
		{
			name: "streamtape", r: NewPlayer(nil, &streamtapePlayer),
			good: site + "/v/AbCdEf123/first.mp4", wrongForm: site + "/support",
			page:      `<html><head><title>first.mp4 - Streamtape</title></head><body><script>document.getElementById('robotlink').innerHTML = '//cdn.example.test/get_video?id=1&expires=2' + ('xyz&token=3').substring(3);</script></body></html>`,
			unmarked:  `<html><head><title>first.mp4 - Streamtape</title></head><body></body></html>`,
			shapes:    []string{site + "/v/AbCdEf123/first.mp4", site + "/e/AbCdEf123"},
			notShapes: []string{site + "/support"},
		},
		{
			name: "doodstream", r: NewPlayer(nil, &doodPlayer),
			good: site + "/e/abcdef123", wrongForm: site + "/premium",
			page:      `<html><head><title>first - DoodStream</title></head><body><script>$.get('/pass_md5/12-34/abcdef', function(d){})</script></body></html>`,
			unmarked:  `<html><head><title>first - DoodStream</title></head><body></body></html>`,
			shapes:    []string{site + "/e/abcdef123", site + "/d/abcdef123"},
			notShapes: []string{site + "/premium"},
			extraGood: []*recogniseExtra{
				{site + "/d/abcdef123", `<html><head><title>first - DoodStream</title></head><body><iframe src="/e/abcdef123"></iframe></body></html>`},
			},
		},
		{
			name: "mixdrop", r: NewPlayer(nil, &mixdropPlayer),
			good: site + "/e/abcdef123", wrongForm: site + "/copyright/",
			page:      `<html><head><title>MixDrop</title></head><body><script>MDCore.ref = "abcdef123";</script></body></html>`,
			unmarked:  `<html><head><title>MixDrop</title></head><body></body></html>`,
			shapes:    []string{site + "/e/abcdef123", site + "/f/abcdef123"},
			notShapes: []string{site + "/copyright/"},
			extraGood: []*recogniseExtra{
				{site + "/f/abcdef123", `<html><head><title>MixDrop</title></head><body><iframe src="/e/abcdef123"></iframe></body></html>`},
			},
		},
		{
			name: "lulustream", r: NewPlayer(nil, &luluPlayer),
			good: site + "/e/abcdefghijkl", wrongForm: site + "/premium",
			page:      `<html><head><title>first - LuluStream</title></head><body><script>jwplayer("v").setup({sources: [{file:"https://cdn.example.test/hls/master.m3u8?t=1"}]})</script></body></html>`,
			unmarked:  `<html><head><title>first - LuluStream</title></head><body></body></html>`,
			shapes:    []string{site + "/e/abcdefghijkl", site + "/d/abcdefghijkl"},
			notShapes: []string{site + "/abcdefghijkl", site + "/premium"},
			extraWrong: []*recogniseExtra{
				{site + "/e/abcdefghijkl", `<html><head><title>Player</title></head><body><script>jwplayer("v").setup({sources: [{file:"https://cdn.example.test/hls/master.m3u8?t=1"}]})</script></body></html>`},
			},
		},
		{
			name: "vids.st", r: NewPlayer(nil, &vidsPlayer),
			good: site + "/v/123", wrongForm: site + "/register",
			page:      `<html><body><script>var playerConfig = {"videoUrl":"/media/first.mp4","videoName":"first.mp4"};</script></body></html>`,
			unmarked:  `<html><head><title>VIDS.ST</title></head><body></body></html>`,
			shapes:    []string{site + "/v/123", site + "/e/123"},
			notShapes: []string{site + "/register", site + "/e/clip"},
		},
		{
			name: "sendvid", r: NewPlayer(nil, &sendvidPlayer),
			good: site + "/abc123xy", wrongForm: site + "/help/faq",
			page:      `<html><head><title>first - Sendvid</title></head><body><video><source id="video_source" src="https://cdn.example.test/first.mp4"></video></body></html>`,
			unmarked:  `<html><head><title>first - Sendvid</title></head><body></body></html>`,
			shapes:    []string{site + "/embed/abc123xy"},
			notShapes: []string{site + "/abc123xy", site + "/help/faq"},
		},
		{
			name: "knuthing", r: NewPlayer(nil, &knuthingPlayer),
			good: site + "/watch/first-clip_Ab12.html", wrongForm: site + "/affiliates",
			page:      `<html><head><title>First</title><script src="/themes/knuthing/js/jquery.js"></script></head><body><iframe class="bunnyframe" src="https://player.example.test/1"></iframe></body></html>`,
			unmarked:  `<html><head><title>First</title><script src="/themes/knuthing/js/jquery.js"></script></head><body></body></html>`,
			shapes:    []string{site + "/watch/first-clip_Ab12.html", site + "/videos/category/12"},
			notShapes: []string{site + "/videos/1/some-clip/", site + "/@someone"},
			extraGood: []*recogniseExtra{
				{site + "/videos/category/12", `<html><head><script src="/themes/knuthing/js/jquery.js"></script></head><body><div id="container_content"><div class="videos-latest-list"></div></div></body></html>`},
			},
		},
		{
			name: "noodlemagazine", r: NewPlayer(nil, &noodlePlayer),
			good: site + "/watch/-123_456", wrongForm: site + "/home",
			page:      head("First watch online", "og:site_name=noodlemagazine") + `<script>window.playlist = {"sources":[]};</script></body></html>`,
			unmarked:  head("First watch online", "og:site_name=noodlemagazine") + "</body></html>",
			shapes:    []string{site + "/watch/-123_456", site + "/video/first+query"},
			notShapes: []string{site + "/home", site + "/videos/1/x/"},
			extraGood: []*recogniseExtra{
				{site + "/video/first+query", `<html><head><script src="/static/js/search.min.abc.js"></script></head><body><div id="list_videos"><div class="item"><a class="item_link" href="/watch/-1_2">x</a></div></div></body></html>`},
			},
		},
		{
			name: "redgifs", r: NewRedGifs(nil),
			good: site + "/watch/firstclip", wrongForm: site + "/site.webmanifest",
			page:      head("First by someone", "og:site_name=RedGIFs", "twitter:player=https://site.example.test/ifr/firstclip") + `<script src="/client_data/abc/script.js"></script></body></html>`,
			unmarked:  head("First by someone", "og:site_name=RedGIFs") + `<script src="/client_data/abc/script.js"></script></body></html>`,
			shapes:    []string{site + "/watch/firstclip", site + "/users/someone"},
			notShapes: []string{site + "/", site + "/watch/first1"},
		},
		{
			name: "odysee", r: handoff("odysee"),
			good: site + "/@someone:1/first-clip:2", wrongForm: site + "/$/settings",
			page:      head("First Clip", "og:site_name=Odysee", "og:video=https://site.example.test/%24/embed/%40someone%3A1%2Ffirst-clip%3A2") + "</body></html>",
			unmarked:  head("First Clip", "og:site_name=Odysee") + "</body></html>",
			shapes:    []string{site + "/@someone:1/first-clip:2", site + "/@someone:1"},
			notShapes: []string{site + "/", site + "/$/settings"},
		},
		{
			name: "dailymotion", r: handoff("dailymotion"),
			good: site + "/video/xabc123", wrongForm: site + "/us",
			page:      `<html><head><title>Dailymotion</title><script src="/neon-user-ssr/static/app.js"></script></head></html>`,
			unmarked:  `<html><head><title>Dailymotion</title></head></html>`,
			shapes:    []string{site + "/video/xabc123", site + "/playlist/xabc1"},
			notShapes: []string{site + "/us", site + "/xabc123"},
		},
		{
			name: "bilibili", r: handoff("bilibili"),
			good: site + "/video/BV1aB2cD3eF4", wrongForm: site + "/en",
			page:      head("First Clip_哔哩哔哩_bilibili", "og:site_name=哔哩哔哩") + `<script>window.__playinfo__={}</script><script>window.__INITIAL_STATE__={}</script></body></html>`,
			unmarked:  head("First Clip_哔哩哔哩_bilibili", "og:site_name=哔哩哔哩") + "</body></html>",
			shapes:    []string{site + "/video/BV1aB2cD3eF4", site + "/en/video/BV1aB2cD3eF4"},
			notShapes: []string{site + "/en", site + "/BV1aB2cD3eF4"},
		},
		{
			name: "soundcloud", r: handoff("soundcloud"),
			good: site + "/an-artist/a-song", wrongForm: site + "/pages/privacy/more",
			page:      head("Stream A Song by An Artist", "og:site_name=SoundCloud", "twitter:player=https://widget.example.test/player/?url=x") + `<script>window.__sc_hydration = [];</script></body></html>`,
			unmarked:  head("Stream A Song by An Artist", "og:site_name=SoundCloud") + "</body></html>",
			shapes:    []string{site + "/an-artist/sets/a-set"},
			notShapes: []string{site + "/an-artist/a-song", site + "/discover"},
		},
		{
			name: "mixcloud", r: handoff("mixcloud"),
			good: site + "/a-user/a-show/", wrongForm: site + "/a-user/",
			page:      head("A Show by A User | Mixcloud", "og:site_name=Mixcloud") + `<script>var s = {cloudcastQuery: {lookup: {}}}</script></body></html>`,
			unmarked:  head("A Show by A User | Mixcloud", "og:site_name=Mixcloud") + "</body></html>",
			notShapes: []string{site + "/a-user/a-show/"},
		},
	}
	for _, c := range cases {
		c.run(t)
	}
}

// TestRumbleRecognisedByItsOEmbed: the site's pages sit behind a challenge,
// so the page's own origin is asked, once, whether its oEmbed endpoint
// describes the link — and only once the link has the form of a video.
func TestRumbleRecognisedByItsOEmbed(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.URL.Path != rumbleOEmbedPath {
			http.NotFound(w, r)
			return
		}
		w.Header().Set(httpx.HeaderContentType, httpx.ContentTypeJSON)
		if strings.Contains(r.URL.Query().Get("url"), "v1abcd-gone") {
			_, _ = w.Write([]byte(`{}`))
			return
		}
		_, _ = w.Write([]byte(`{"type":"video","provider_name":"Rumble.com","html":"<iframe></iframe>"}`))
	}))
	defer srv.Close()
	client := httpx.New("test", "en", 0, 5*time.Second)
	var rumble *Handoff
	for _, e := range NewHandoffs(client) {
		if e.Name() == "rumble" {
			rumble = e.(*Handoff)
		}
	}
	challenged := func(raw string) *Page {
		p := fixturePage(t, raw, 0, "", "")
		p.Challenged = true
		return p
	}
	p := challenged(srv.URL + "/v1abcd-a-clip.html")
	for range 2 {
		if !rumble.Recognise(context.Background(), p) {
			t.Fatal("a video the origin describes was not recognised")
		}
	}
	if hits.Load() != 1 {
		t.Errorf("the origin was asked %d times, want once", hits.Load())
	}
	if rumble.Recognise(context.Background(), challenged(srv.URL+"/v1abcd-gone.html")) {
		t.Error("a link the origin does not describe was recognised")
	}
	before := hits.Load()
	if rumble.Recognise(context.Background(), challenged(srv.URL+"/login.php")) {
		t.Error("a page of another form was recognised")
	}
	if hits.Load() != before {
		t.Error("a page of another form cost a request")
	}
	expectShape(t, rumble, "https://site.example.test/v1abcd-a-clip.html", true)
	expectShape(t, rumble, "https://site.example.test/register.php", false)
}
