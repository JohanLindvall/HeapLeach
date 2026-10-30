// SPDX-License-Identifier: MIT

package extractor

import "testing"

// broadcastDoc is a page of an invented programme on a site of this name,
// carrying the marker in its body.
func broadcastDoc(site, marker string) string {
	return `<html><head><title>An Invented Programme | ` + site + `</title></head><body>` + marker + `</body></html>`
}

// broadcastCase is one broadcaster's recognition: a page of the right form
// carrying its marker, the same page without the marker, and the marked
// page at a form this does not read. All three carry the site's name, so a
// name alone is shown not to be enough.
type broadcastCase struct {
	r               Recogniser
	site, marker    string
	page, elsewhere string
}

func (c broadcastCase) check(t *testing.T) {
	t.Helper()
	expectRecognised(t, c.r, htmlPage(t, c.page, broadcastDoc(c.site, c.marker)), true)
	expectRecognised(t, c.r, htmlPage(t, c.page, broadcastDoc(c.site, "")), false)
	expectRecognised(t, c.r, htmlPage(t, c.elsewhere, broadcastDoc(c.site, c.marker)), false)
	expectRecognised(t, c.r, htmlPage(t, c.page, broadcastDoc("A Blog", c.marker)), false)
}

func TestSVTPlayRecognise(t *testing.T) {
	s := &SVTPlay{}
	broadcastCase{s, "SVT Play", `{"videoSvtId":"a1b2C3d"}`,
		"https://play.example.test/video/a1b2C3d/a-programme/an-episode", "https://play.example.test/genre/a-genre/more"}.check(t)
	expectRecognised(t, s, htmlPage(t, "https://play.example.test/a-programme", broadcastDoc("SVT Play", `{"videoSvtId":"a1b2C3d"}`)), true)
	expectShape(t, s, "https://play.example.test/video/a1b2C3d/a-programme", true)
	expectShape(t, s, "https://play.example.test/video/1234567890", false)
	expectShape(t, s, "https://play.example.test/video/preview/1234567890", false)
	expectShape(t, s, "https://play.example.test/a-programme", false)
}

func TestRUVRecognise(t *testing.T) {
	r := &RUV{}
	broadcastCase{r, "Spilari RÚV", `<script>window._GQL_ = "/gql/";</script>`,
		"https://tv.example.test/sjonvarp/spila/a-programme/12345/abc1de", "https://tv.example.test/sjonvarp/dagskra"}.check(t)
	expectShape(t, r, "https://tv.example.test/utvarp/spila/a-programme/12345", true)
	expectShape(t, r, "https://tv.example.test/frettir/a-story", false)
}

func TestARDRecognise(t *testing.T) {
	a := &ARD{}
	broadcastCase{a, "ARD Mediathek", `"https://api.example.test/page-gateway/pages/ard/item/x"`,
		"https://media.example.test/video/a-show/an-episode/abc/Y3JpZDovL2V4YW1wbGUudGVzdC9hLXZpZGVv",
		"https://media.example.test/tv-programm/"}.check(t)
	expectShape(t, a, "https://media.example.test/sendung/a-show/Y3JpZDovL2V4YW1wbGUudGVzdC9hLXNob3c", true)
	expectShape(t, a, "https://media.example.test/video/1234567890", false)
}

func TestZDFRecognise(t *testing.T) {
	z := &ZDF{}
	broadcastCase{z, "ZDF", `{"ptmdTemplate":"/tmd/2/{playerId}/a-programme-100"}`,
		"https://media.example.test/video/dokus/a-show-100/an-episode-100", "https://media.example.test/live-tv"}.check(t)
	expectShape(t, z, "https://media.example.test/dokus/a-show-100", true)
	expectShape(t, z, "https://media.example.test/a-show-100", false)
}

func TestSRFRecognise(t *testing.T) {
	s := &SRF{}
	broadcastCase{s, "Play SRF", `"urn:srf:video:1a2b"`,
		"https://play.example.test/play/tv/a-show/video/a-title?urn=urn:srf:video:1a2b", "https://play.example.test/play/tv"}.check(t)
	expectShape(t, s, "https://play.example.test/play/tv/sendung/a-show?id=eb4f3b13-0362-4ea8-b7b3-325382c86ef2", true)
	expectShape(t, s, "https://play.example.test/play/tv/sendung/a-show", false)
}

func TestArteRecognise(t *testing.T) {
	a := &Arte{}
	broadcastCase{a, "ARTE", `"https://api.example.test/api/player/v2/config/en/123456-000-A"`,
		"https://tv.example.test/en/videos/123456-000-A/a-programme/", "https://tv.example.test/en/"}.check(t)
	expectShape(t, a, "https://tv.example.test/embeds/de/RC-012345", true)
	expectShape(t, a, "https://tv.example.test/en/videos/a-programme/", false)
}

func TestORFRecognise(t *testing.T) {
	o := &ORF{}
	broadcastCase{o, "ORF ON", `<script>window.__NUXT__={config:{}}</script>`,
		"https://on.example.test/video/12345678/an-episode", "https://on.example.test/"}.check(t)
	expectShape(t, o, "https://on.example.test/sendereihe/1234/a-series", true)
	expectShape(t, o, "https://on.example.test/video/12345678", false)
}

func TestYleRecognise(t *testing.T) {
	y := &Yle{}
	broadcastCase{y, "Yle Areena", `{"appIdFrontend":"areena-web-items"}`,
		"https://areena.example.test/1-1234567", "https://areena.example.test/tv"}.check(t)
	expectShape(t, y, "https://areena.example.test/podcastit/1-1234567", true)
	expectShape(t, y, "https://areena.example.test/tv", false)
}

func TestDRTVRecognise(t *testing.T) {
	d := &DRTV{}
	broadcastCase{d, "DRTV", `<script>` + drShell + `:{}}}</script>`,
		"https://tv.example.test/drtv/episode/a-show_-an-episode_12345", "https://tv.example.test/nyheder/a-story"}.check(t)
	expectShape(t, d, "https://tv.example.test/drtv/serie/a-show_678", true)
	expectShape(t, d, "https://tv.example.test/drtv/", false)
}

func TestRTBFRecognise(t *testing.T) {
	r := &RTBF{}
	broadcastCase{r, "RTBF Auvio", `{"contentPath":"https://api.example.test/auvio/v1.23/widgets/1"}`,
		"https://auvio.example.test/emission/a-programme-12345", "https://auvio.example.test/categorie/a-category"}.check(t)
	// RTBF's own pages are read for the Auvio players they embed.
	expectRecognised(t, r, htmlPage(t, "https://news.example.test/article/a-story-123",
		broadcastDoc("RTBF", `<div data-asset-id="12345"></div>`)), true)
	expectRecognised(t, r, htmlPage(t, "https://news.example.test/article/a-story-123", broadcastDoc("RTBF", "")), false)
	expectShape(t, r, "https://auvio.example.test/media/an-episode-12345", true)
	expectShape(t, r, "https://auvio.example.test/categorie/a-category", false)
}

func TestFranceTVRecognise(t *testing.T) {
	f := &FranceTV{}
	marker := `<script>self.__next_f.push([1,"{\"video_factory_id\":\"00000000-0000-0000-0000-000000000000\"}"])</script>`
	video := "https://tv.example.test/documentaires/a-collection/1234567-an-episode.html"
	listing := "https://tv.example.test/documentaires/a-collection/"
	expectRecognised(t, f, htmlPage(t, listing, broadcastDoc("France TV", marker)), true)
	expectRecognised(t, f, htmlPage(t, listing, broadcastDoc("France TV", "")), false)
	expectRecognised(t, f, htmlPage(t, listing, broadcastDoc("A Blog", marker)), false)
	expectRecognised(t, f, htmlPage(t, "https://tv.example.test/", broadcastDoc("France TV", marker)), false)
	// A video page's title names the video alone; its form says the rest.
	expectRecognised(t, f, htmlPage(t, video, `<html><head><title>An Episode</title></head><body>`+marker+`</body></html>`), true)
	expectRecognised(t, f, htmlPage(t, video, `<html><head><title>An Episode</title></head><body></body></html>`), false)
	expectShape(t, f, "https://tv.example.test/a-programme/toutes-les-videos/", true)
	expectShape(t, f, "https://tv.example.test/a-programme/", false)
}

func TestRadioCanadaRecognise(t *testing.T) {
	r := &RadioCanada{}
	broadcastCase{r, "Radio-Canada", `<script>{"IdsCollection":"MedianetId:1234567|EpisodeId:89|"}</script>`,
		"https://ici.example.test/ohdio/balados/123/a-podcast", "https://ici.example.test/"}.check(t)
	expectShape(t, r, "https://ici.example.test/info/videos/1-123456/a-video", true)
	expectShape(t, r, "https://ici.example.test/widgets/mediaconsole/medianet/1234567", true)
	expectShape(t, r, "https://ici.example.test/nouvelle/123/a-story", false)
}

func TestVRTMaxRecognise(t *testing.T) {
	v := &VRTMax{}
	broadcastCase{v, "VRT MAX", `<link href="/vrtnu-resources/styles/main.css" rel="stylesheet">`,
		"https://vrt.example.test/vrtmax/a-z/a-programme/", "https://vrt.example.test/vrtnws/a-story/"}.check(t)
	expectShape(t, v, "https://vrt.example.test/vrtmax/a-z/a-programme/1/an-episode/", true)
	expectShape(t, v, "https://vrt.example.test/vrtmax/", false)
}

func TestNPOStartRecognise(t *testing.T) {
	n := &NPOStart{}
	broadcastCase{n, "NPO Start", `<script src="/start/_next/static/chunks/a.js"></script>`,
		"https://npo.example.test/start/serie/a-series", "https://npo.example.test/start"}.check(t)
	expectShape(t, n, "https://npo.example.test/start/afspelen/an-episode", true)
	expectShape(t, n, "https://npo.example.test/overnpo/", false)
}

func TestRaiPlayRecognise(t *testing.T) {
	r := &RaiPlay{}
	broadcastCase{r, "RaiPlay", `<rai-player video-url="/video/2026/01/An-Episode-0000.json"></rai-player>`,
		"https://rai.example.test/video/2026/01/An-Episode-0000.html", "https://rai.example.test/dirette/rai1"}.check(t)
	expectShape(t, r, "https://rai.example.test/programmi/unprogramma", true)
	expectShape(t, r, "https://rai.example.test/dirette/rai1", false)
}

func TestRTVERecognise(t *testing.T) {
	r := &RTVE{}
	broadcastCase{r, "RTVE.es", `<div class="striplayer" data-idasset=1234567></div>`,
		"https://tve.example.test/play/videos/a-programme/an-episode/1234567/", "https://tve.example.test/noticias/"}.check(t)
	// A programme's page is read by the id its deferred requests carry.
	expectRecognised(t, r, htmlPage(t, "https://tve.example.test/play/videos/a-programme/",
		broadcastDoc("RTVE.es", `<div data-src="/api/programas/12345/videos"></div>`)), true)
	expectShape(t, r, "https://tve.example.test/v/1234567", true)
	expectShape(t, r, "https://tve.example.test/noticias/a-story", false)
}

func TestRTPPlayRecognise(t *testing.T) {
	r := &RTPPlay{}
	broadcastCase{r, "RTP Play", `<script>var player = new RTPPlayer({});</script>`,
		"https://rtp.example.test/play/p1234/e56789/a-programme", "https://rtp.example.test/noticias/a-story"}.check(t)
	season := "https://rtp.example.test/play/p1234/a-programme"
	expectRecognised(t, r, htmlPage(t, season, broadcastDoc("RTP Play", `<a href="/play/p1234/e56789/a-programme">1</a>`)), true)
	expectRecognised(t, r, htmlPage(t, season, broadcastDoc("RTP Play", "")), false)
	expectShape(t, r, season, true)
	expectShape(t, r, "https://rtp.example.test/play/", false)
}

func TestPBSRecognise(t *testing.T) {
	p := &PBS{}
	broadcastCase{p, "PBS", `<iframe src="https://player.example.test/portalplayer/3100000001/"></iframe>`,
		"https://pbs.example.test/video/a-title-abc123/", "https://pbs.example.test/about/"}.check(t)
	show := "https://pbs.example.test/show/a-programme/"
	expectRecognised(t, p, htmlPage(t, show, broadcastDoc("PBS", `<a href="/video/a-title-abc123/">1</a>`)), true)
	expectRecognised(t, p, htmlPage(t, show, broadcastDoc("PBS", "")), false)
	player := "https://player.example.test/portalplayer/3100000001/"
	expectRecognised(t, p, htmlPage(t, player, `<script>window.videoBridge = {};</script>`), true)
	expectRecognised(t, p, htmlPage(t, player, `<html></html>`), false)
	expectShape(t, p, "https://pbs.example.test/video/a-title-abc123/", true)
	expectShape(t, p, "https://pbs.example.test/video/1234567890", false)
}

func TestNPRRecognise(t *testing.T) {
	n := &NPR{}
	broadcastCase{n, "NPR", `<div data-audio='{"audioUrl":"https://media.example.test/a.mp3"}'></div>`,
		"https://npr.example.test/2026/01/02/1234567890/a-story", "https://npr.example.test/"}.check(t)
	expectShape(t, n, "https://npr.example.test/programs/a-programme/", true)
	expectShape(t, n, "https://npr.example.test/2026/01/02/a-blog-post/", false)
}

func TestABCListenRecognise(t *testing.T) {
	a := &ABCListen{}
	broadcastCase{a, "ABC listen", `<script id="__NEXT_DATA__" type="application/json">{"props":{"pageProps":{}}}</script>`,
		"https://abc.example.test/listen/programs/a-programme/an-episode/12345678", "https://abc.example.test/news/a-story"}.check(t)
	expectShape(t, a, "https://abc.example.test/listen/programs/a-programme", true)
	expectShape(t, a, "https://abc.example.test/listen", false)
}

func TestBBCSoundsRecognise(t *testing.T) {
	b := &BBCSounds{}
	broadcastCase{b, "BBC Sounds", `<script src="/sounds/web/sounds-web-spa/_next/static/chunks/a.js"></script>`,
		"https://bbc.example.test/sounds/play/m000abcd", "https://bbc.example.test/sounds/my"}.check(t)
	expectShape(t, b, "https://bbc.example.test/sounds/brand/b000abcd", true)
	expectShape(t, b, "https://bbc.example.test/sounds/podcasts", false)
}

func TestNRKRecognise(t *testing.T) {
	n := &NRK{}
	broadcastCase{n, "NRK TV", `<script>{"prfId":"ABCD12345678"}</script>`,
		"https://tv.example.test/serie/a-series", "https://tv.example.test/direkte/nrk1"}.check(t)
	expectShape(t, n, "https://tv.example.test/se?v=ABCD12345678", true)
	expectShape(t, n, "https://tv.example.test/program/ABCD12345678", true)
	expectShape(t, n, "https://tv.example.test/se", false)
}
