// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
	"github.com/JohanLindvall/HeapLeach/internal/tools"
)

// withoutHelpers hides the metadata helper, so a handoff's extraction ends
// with "not installed" instead of running one: what is under test is which
// extractor the registry chose.
func withoutHelpers(t *testing.T) {
	t.Helper()
	t.Setenv("PATH", t.TempDir())
	tools.Reset()
	t.Cleanup(tools.Reset)
}

// registryAt extracts path from a server answering with handle, and returns
// the extractor the registry chose.
func registryAt(t *testing.T, path string, handle http.HandlerFunc) (Extractor, *Result, error) {
	t.Helper()
	srv := httptest.NewServer(handle)
	t.Cleanup(srv.Close)
	cfg := &config.Config{UserAgent: config.DefaultUserAgent, Language: config.DefaultLanguage}
	res, ex, err := NewRegistry(cfg, testClient()).Extract(t.Context(), srv.URL+path, Options{})
	return ex, res, err
}

// servePage answers every request with the page.
func servePage(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set(httpx.HeaderContentType, "text/html; charset=utf-8")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}
}

// ytPage is a page of the video site's web client.
const ytPage = `<html><head><title>A Page - YouTube</title><meta property="og:site_name" content="YouTube"></head>` +
	`<body><script>ytcfg.set({"A":1});var ytInitialData = {};</script></body></html>`

// The video site's forms beyond the watch page: a clip, a search, a
// channel's live and podcast tabs are links; a hashtag and a music browse
// page are recognised by their page.
func TestYouTubeWiderForms(t *testing.T) {
	yt := handoffNamed(t, "youtube")
	for _, link := range []string{
		site + "/clip/UgkxAbCdEfGhIjKlMnOpQrStUvWxYz012345",
		site + "/results?search_query=a+search",
		site + "/@a-channel/live",
		site + "/@a-channel/podcasts",
	} {
		expectShape(t, yt, link, true)
		expectRecognised(t, yt, htmlPage(t, link, ytPage), true)
	}
	for _, link := range []string{site + "/hashtag/atag", site + "/browse/VLabcdef", site + "/@a-channel/community"} {
		expectShape(t, yt, link, false)
		expectRecognised(t, yt, htmlPage(t, link, ytPage), true)
		expectRecognised(t, yt, htmlPage(t, link, `<html><head><title>A Blog</title></head></html>`), false)
	}
	expectShape(t, yt, site+"/results", false)
	expectShape(t, yt, site+"/clip/short", false)
}

// A video link whose probe was refused, challenged or failed is handed to
// yt-dlp rather than saved as a page: the probe read nothing to say it is
// not the site's, and yt-dlp may be let in where the probe was not. Only
// forms no other site shares are taken so.
func TestYouTubeFailedProbeIsTheHandoffs(t *testing.T) {
	yt := handoffNamed(t, "youtube")
	watch := site + "/watch?v=abcdefghijk"
	for name, p := range map[string]*Page{
		"429":        fixturePage(t, watch, http.StatusTooManyRequests, "text/html", "slow down"),
		"403":        fixturePage(t, watch, http.StatusForbidden, "text/html", ""),
		"challenged": challengedPage(t, watch),
		"transport":  {URL: mustURL(t, watch), Final: mustURL(t, watch), Err: errors.New("connection reset")},
		"shorts":     fixturePage(t, site+"/shorts/abcdefghijk", http.StatusServiceUnavailable, "", ""),
		"playlist":   fixturePage(t, site+"/playlist?list=PLabcdefghijkl", http.StatusServiceUnavailable, "", ""),
		"liked":      fixturePage(t, site+"/playlist?list=LLabcdefghijkl", http.StatusServiceUnavailable, "", ""),
		"embed":      fixturePage(t, site+"/embed/abcdefghijk", http.StatusTooManyRequests, "text/html", ""),
		"channel id": fixturePage(t, site+"/channel/UCabcdefghijklmnopqrstuv", http.StatusTooManyRequests, "text/html", ""),
		"channel tab": fixturePage(t, site+"/channel/UCabcdefghijklmnopqrstuv/videos", http.StatusTooManyRequests,
			"text/html", ""),
	} {
		t.Run(name, func(t *testing.T) { expectRecognised(t, yt, p, true) })
	}
	// A redirect to a rate-limit page that names the video it continues to.
	sorry := fixturePage(t, "https://sorry.example.test/sorry/index?continue="+url.QueryEscape(watch),
		http.StatusTooManyRequests, "text/html", "")
	sorry.URL = mustURL(t, "https://other.example.test/x")
	expectRecognised(t, yt, sorry, true)

	// A channel, a fediverse profile's form, is not taken on a failed probe.
	expectRecognised(t, yt, fixturePage(t, site+"/@someone", http.StatusServiceUnavailable, "", ""), false)
	expectRecognised(t, yt, fixturePage(t, site+"/watch?v=short", http.StatusServiceUnavailable, "", ""), false)
	// Another player's numeric embed is not the site's.
	expectRecognised(t, yt, fixturePage(t, site+"/embed/12345678901", http.StatusServiceUnavailable, "", ""), false)
	// A page that answered is judged by what it carries.
	expectRecognised(t, yt, htmlPage(t, watch, `<html><head><title>A Blog</title></head></html>`), false)
}

// The music client's pages carry the web client's configuration under the
// music site's name, without the www client's page-data variables; a page
// of the same form under another name is not taken.
func TestYouTubeMusicPageRecognised(t *testing.T) {
	yt := handoffNamed(t, "youtube")
	music := `<html><head><title>A Song</title><meta property="og:site_name" content="YouTube Music"></head>` +
		`<body><script>ytcfg.set({"A":1});</script></body></html>`
	for _, link := range []string{site + "/watch?v=abcdefghijk&cbrd=1", site + "/playlist?list=OLAK5uy_abcdefghijkl",
		site + "/browse/MPREb_abcdefghijk"} {
		expectRecognised(t, yt, htmlPage(t, link, music), true)
	}
	expectRecognised(t, yt, htmlPage(t, site+"/watch?v=abcdefghijk", strings.ReplaceAll(music, "YouTube Music", "A Blog")), false)
}

// Through the registry, a refused watch page reaches the handoff and
// fails in its words, not as a page queued as a file.
func TestYouTubeRefusedWatchReachesTheHandoff(t *testing.T) {
	withoutHelpers(t)
	ex, res, err := registryAt(t, "/watch?v=abcdefghijk", servePage(http.StatusForbidden, "go away"))
	if ex == nil || ex.Name() != "youtube" {
		t.Fatalf("routed via %v (res %+v, err %v)", ex, res, err)
	}
	if err == nil || !strings.Contains(err.Error(), "yt-dlp") {
		t.Fatalf("err = %v, want the handoff's", err)
	}
}

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// The other handoff sites' collections — an artist, a user, a channel, a
// playlist, a space — are recognised by the site's own page, as they were
// handed over whole before.
func TestHandoffCollections(t *testing.T) {
	for _, c := range []struct {
		site, link, page string
		want             bool
	}{
		{"soundcloud", site + "/an-artist", head("An Artist", "og:site_name=SoundCloud") + `<script>window.__sc_hydration = [];</script></body></html>`, true},
		{"soundcloud", site + "/an-artist/tracks", head("An Artist", "og:site_name=SoundCloud") + `<script>window.__sc_hydration = [];</script></body></html>`, true},
		{"soundcloud", site + "/an-artist", head("An Artist", "og:site_name=SoundCloud") + `</body></html>`, false},
		{"soundcloud", site + "/an-artist/a-song/s-AbCdEfGhIjK", head("A Song", "og:site_name=SoundCloud") + `<script>window.__sc_hydration = [];</script></body></html>`, true},
		{"soundcloud", site + "/an-artist/sets/a-set/s-AbCdEfGhIjK", head("A Set", "og:site_name=SoundCloud") + `<script>window.__sc_hydration = [];</script></body></html>`, true},
		{"soundcloud", site + "/an-artist/a-song/s-AbCdEfGhIjK", head("A Song", "og:site_name=A Blog") + `<script>window.__sc_hydration = [];</script></body></html>`, false},
		{"mixcloud", site + "/a-user/a-show", head("A Show by A User | Mixcloud", "og:site_name=Mixcloud") + `<script>var s = {cloudcastQuery: {}}</script></body></html>`, true},
		{"rumble", site + "/c/AChannel", `<html><head><title>A Channel</title><script src="/rum-ui/r/l/index.js"></script></head></html>`, true},
		{"rumble", site + "/user/someone", `<html><head><title>Someone</title><script src="/rum-ui/r/l/index.js"></script></head></html>`, true},
		{"rumble", site + "/playlists/abc123", `<html><head><script src="/rum-ui/r/l/index.js"></script></head></html>`, true},
		{"rumble", site + "/c/AChannel", `<html><head><title>A Channel</title></head></html>`, false},
		{"dailymotion", site + "/a-user", `<html><head><title>Dailymotion</title><script src="/neon-user-ssr/app.js"></script></head></html>`, true},
		{"dailymotion", site + "/us", `<html><head><title>Dailymotion</title><script src="/neon-user-ssr/app.js"></script></head></html>`, false},
		{"dailymotion", site + "/player/xabc.html?video=xabc123", `<html><head><title>Dailymotion Video Player</title><script src="/static/cdn/dmp.photon_boot.1.js"></script></head></html>`, true},
		{"dailymotion", site + "/player/xabc.html?video=xabc123", `<html><head><title>A Player</title></head></html>`, false},
		{"dailymotion", site + "/player.html?video=xabc123", `<html><head><title>Dailymotion Video Player</title><script src="/static/cdn/dmp.photon_boot.1.js"></script></head></html>`, true},
		{"dailymotion", site + "/video/x8abcde_a-title_news", `<html><head><title>A Video - Dailymotion</title><meta property="og:site_name" content="Dailymotion"><script src="/neon-user-ssr/app.js"></script></head></html>`, true},
		{"bilibili", site + "/123456", `<html><head><title>A User - bilibili</title><script src="//cdn.example.test/bfs/seed/log/report.js"></script></head></html>`, true},
		{"bilibili", site + "/bangumi/media/md12345", `<html><head><title>A Series - bilibili</title><script>window.__INITIAL_STATE__={}</script></head></html>`, true},
		{"bilibili", site + "/festival/an-event?bvid=BV1aB2cD3eF4", `<html><head><title>An Event - bilibili</title><script>window.__INITIAL_STATE__={}</script></head></html>`, true},
		{"bilibili", site + "/123456", `<html><head><title>A User - A Blog</title><script>window.__INITIAL_STATE__={}</script></head></html>`, false},
		{"odysee", site + "/$/playlist/abc123", head("A List", "og:site_name=Odysee") + `<script src="/public/assets/ui.js"></script></body></html>`, true},
	} {
		h := handoffNamed(t, c.site)
		t.Run(c.site+" "+c.link, func(t *testing.T) { expectRecognised(t, h, htmlPage(t, c.link, c.page), c.want) })
	}
	expectShape(t, handoffNamed(t, "dailymotion"), site+"/player/xabc.html?video=xabc123", true)
	expectShape(t, handoffNamed(t, "dailymotion"), site+"/player.html?video=xabc123", true)
	expectShape(t, handoffNamed(t, "dailymotion"), site+"/video/x8abcde_a-title_news", true)
	expectShape(t, handoffNamed(t, "odysee"), site+"/$/playlist/abc123", true)
	// A private link is recognised by its page, not followed by its form,
	// which other sites' navigation shares.
	expectShape(t, handoffNamed(t, "soundcloud"), site+"/gifts/catalog/s-housewarming", false)
	expectShape(t, handoffNamed(t, "soundcloud"), site+"/an-artist/a-song", false)
}

// A channel of the comment-video site is served by its channel front end,
// under its portal's name, and yt-dlp lists it.
func TestNiconicoChannelRecognised(t *testing.T) {
	n := NewNiconico(nil)
	page := `<html><head><title>A Channel - ニコニコチャンネル</title>` +
		`<script src="https://cdn.example.test/nicochannel/chfront/js/a.js"></script></head></html>`
	expectRecognised(t, n, htmlPage(t, site+"/a-channel", page), true)
	expectRecognised(t, n, htmlPage(t, site+"/a-channel", `<html><head><title>A Channel - ニコニコチャンネル</title></head></html>`), false)
}

// A search preview naming its film in the query, and the touch preview, are
// pointer pages as the numbered preview is.
func TestYandexFilmForms(t *testing.T) {
	y := pointerNamed(t, "yandex")
	page := `<html><head><script src="/fiji-static/a.js"></script></head><body><a class="VideoViewer-Source" href="https://tube.example.test/v/1">src</a></body></html>`
	for _, link := range []string{
		site + "/video/search?text=a&filmId=12345",
		site + "/video/preview/?filmId=12345",
		site + "/video/touch/preview/12345",
	} {
		expectShape(t, y, link, true)
		expectRecognised(t, y, htmlPage(t, link, page), true)
	}
	expectShape(t, y, site+"/video/search?text=a", false)
}

// TestBroadcasterRestPagesReachTheHandoff: pages of broadcasters outside
// their native catalogue — news, audio, radio, live channels — are still
// recognised, by path family and name, and handed to yt-dlp.
func TestBroadcasterRestPagesReachTheHandoff(t *testing.T) {
	for _, c := range []struct {
		name, link, page string
		r                Recogniser
		want             bool
	}{
		// yt-dlp refuses news articles; the fallback reads what they link.
		{"dr news", "https://dr.example.test/nyheder/indland/a-story", `<html><head><meta property="og:site_name" content="DR"><script src="/hydra/resources/a.js"></script></head></html>`, NewDRTV(nil), false},
		{"dr audio", "https://dr.example.test/lyd/p3/a-show-123", `<html><head><meta property="og:site_name" content="DR LYD"><script src="/lyd/_next/static/a.js"></script></head></html>`, NewDRTV(nil), true},
		{"dr unnamed", "https://dr.example.test/lyd/p3/a-show-123", `<html><head><meta property="og:site_name" content="A Blog"><script src="/lyd/_next/static/a.js"></script></head></html>`, NewDRTV(nil), false},
		{"dr other section", "https://dr.example.test/kontakt/a-form", `<html><head><meta property="og:site_name" content="DR"><script src="/lyd/_next/a.js"></script></head></html>`, NewDRTV(nil), false},
		{"orf radio", "https://sound.example.test/radio/oe1/20260101/a-broadcast", `<html><head><title>A Broadcast | ORF Sound</title></head></html>`, NewORF(nil), true},
		{"orf story", "https://fm4.example.test/stories/3012345/", `<html><head><title>A Story - FM4</title></head></html>`, NewORF(nil), true},
		{"orf unnamed", "https://sound.example.test/radio/oe1/20260101/a-broadcast", `<html><head><title>A Broadcast | A Blog</title></head></html>`, NewORF(nil), false},
		{"orf programme broadcast", "https://sound.example.test/radio/oe1/sendung/123456/a-broadcast", `<html><head><title>A Broadcast | ORF Sound</title></head></html>`, NewORF(nil), true},
		{"orf station player", "https://fm4.example.test/player/20260101/4AB", `<html><head><title>A Show - FM4</title></head></html>`, NewORF(nil), true},
		{"orf player unnamed", "https://fm4.example.test/player/20260101/4AB", `<html><head><title>A Show - A Blog</title></head></html>`, NewORF(nil), false},
		{"francetv live", "https://tv.example.test/france-2/direct.html", `<html><head><title>Direct | France TV</title><script src="/_next/static/chunks/a.js"></script></head></html>`, NewFranceTV(nil), true},
		{"francetv no app", "https://tv.example.test/france-2/direct.html", `<html><head><title>Direct | France TV</title></head></html>`, NewFranceTV(nil), false},
	} {
		t.Run(c.name, func(t *testing.T) { expectRecognised(t, c.r, htmlPage(t, c.link, c.page), c.want) })
	}
}

// Through the registry, a news article of the broadcaster is read by the
// fallback for the media it links, not handed to yt-dlp, which refuses
// such pages as unsupported.
func TestBroadcasterNewsPageIsReadForItsMedia(t *testing.T) {
	withoutHelpers(t)
	page := `<html><head><meta property="og:site_name" content="DR"><script src="/hydra/resources/a.js"></script></head>` +
		`<body><p>A story</p><video src="/media/a-story-clip.mp4"></video></body></html>`
	ex, res, err := registryAt(t, "/nyheder/indland/a-story", servePage(http.StatusOK, page))
	if ex == nil || ex.Name() != "direct" || err != nil || len(res.Files) != 1 || !strings.HasSuffix(res.Files[0].URL, "/media/a-story-clip.mp4") {
		t.Fatalf("routed via %v: %+v, %v", ex, res, err)
	}
}

// Link forms of the broadcasters are kept specific: an ordinary news site's
// slug-number article, live page or channel link is no broadcaster's, and
// is not probed as one.
func TestBroadcasterShapesSkipOrdinaryNavigation(t *testing.T) {
	z, r, f := NewZDF(nil), NewRTBF(nil), NewFranceTV(nil)
	for _, link := range []string{
		"https://news.example.test/sport/story-201",
		"https://news.example.test/news/other-story-101",
	} {
		expectShape(t, z, link, false)
	}
	expectShape(t, z, "https://media.example.test/video/dokus/a-show-100/an-episode-100", true)
	expectShape(t, z, "https://media.example.test/serien/a-show-100", true)
	// The Mediathek's own page under another section is still recognised.
	expectRecognised(t, z, htmlPage(t, "https://media.example.test/sport/a-match-100",
		broadcastDoc("ZDF", `{"ptmdTemplate":"/tmd/2/{playerId}/a-match-100"}`)), true)

	for _, link := range []string{"https://news.example.test/live", "https://news.example.test/direct",
		"https://news.example.test/chaine/a-channel"} {
		expectShape(t, r, link, false)
	}
	expectShape(t, r, "https://auvio.example.test/media/an-episode-12345", true)
	expectShape(t, r, "https://auvio.example.test/emission/a-programme-12345", true)

	expectShape(t, f, "https://news.example.test/2026/1234567-a-story.html", false)
	expectShape(t, f, "https://tv.example.test/documentaires/a-collection/1234567-an-episode.html", true)
}

// Media console links name their media outright and need no page; a
// /media-<id> link needs the site's name or a probe that read nothing.
func TestRadioCanadaLinkFormsNeedNoDataLayer(t *testing.T) {
	r := NewRadioCanada(nil)
	console := "https://ici.example.test/widgets/mediaconsole/medianet/1234567"
	expectRecognised(t, r, htmlPage(t, console, `<html><body>a widget</body></html>`), true)
	expectRecognised(t, r, fixturePage(t, console, http.StatusNotFound, "", ""), true)
	media := "https://ici.example.test/info/a-story/media-1234567"
	expectRecognised(t, r, htmlPage(t, media, broadcastDoc("Radio-Canada", "")), true)
	expectRecognised(t, r, fixturePage(t, media, http.StatusServiceUnavailable, "", ""), true)
	expectRecognised(t, r, htmlPage(t, media, broadcastDoc("A Blog", "")), false)
}

// A show's sub-pages are read at the show's own address.
func TestPBSShowSubPages(t *testing.T) {
	p := NewPBS(nil)
	listing := broadcastDoc("PBS", `<a href="/video/a-title-abc123/">1</a>`)
	for _, link := range []string{"https://pbs.example.test/show/a-programme/episodes/",
		"https://pbs.example.test/show/a-programme/season/2/"} {
		expectRecognised(t, p, htmlPage(t, link, listing), true)
		expectShape(t, p, link, false)
		if target := pbsTargetOf(mustURL(t, link)); target == nil || target.show != "a-programme" {
			t.Errorf("%s reads as %+v", link, target)
		}
	}
	expectShape(t, p, "https://pbs.example.test/show/a-programme/", true)
}

// Case is the site's to vary: links spelled with capitals are the same
// programmes.
func TestBroadcasterFormsIgnoreCase(t *testing.T) {
	expectShape(t, NewBBCSounds(nil), "https://bbc.example.test/Sounds/play/m000abcd", true)
	expectShape(t, NewRTPPlay(nil), "https://rtp.example.test/Play/P1234/E56789/a-programme", true)
	expectShape(t, NewRUV(nil), "https://tv.example.test/a/b/spila/a-programme/12345", true)
}

// TestZDFRefusedVideosAreRefusals: a listed video ZDF offers no stream for
// is left out as refused, in ZDF's words, not counted as unreadable.
func TestZDFRefusedVideosAreRefusals(t *testing.T) {
	err := error(&itemRefusal{site: "zdf", title: "An Episode", reason: "each " + (&zdfNoStream{why: " (not yet)"}).Error()})
	if reason, ok := refusalReason(err); !ok || reason != "each offers no stream (not yet)" {
		t.Fatalf("reason %q, %v", reason, ok)
	}
	z := NewZDF(nil)
	_, err = z.file(context.Background(), &zdfDocument{}, newZDFReach(nil))
	if _, ok := errors.AsType[*zdfNoStream](err); !ok {
		t.Fatalf("a document with nothing to fetch gave %T %v", err, err)
	}
}

// The three series that once followed the file cap follow it again where it
// is set past the episode budget, and keep the budget below it.
func TestEpisodeBudgetFollowsTheFileCap(t *testing.T) {
	if got := (Options{limits: Limits{Files: 5}}).episodeBudget(); got != config.MaxResolvedEpisodes {
		t.Errorf("budget under a small cap = %d", got)
	}
	if got := (Options{limits: Limits{Files: 5000}}).episodeBudget(); got != 5000 {
		t.Errorf("budget under a large cap = %d", got)
	}
}
