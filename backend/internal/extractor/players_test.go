// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

// TestPlayerSitesAreCompleteAndRegistered checks every row: each fills in
// what the engine calls, and each is registered once, in the table's order,
// under its own name.
func TestPlayerSitesAreCompleteAndRegistered(t *testing.T) {
	reg := NewRegistry(&config.Config{}, nil)
	var registered []string
	for _, e := range reg.extractors {
		if p, ok := e.(*Player); ok {
			registered = append(registered, p.Name())
		}
	}
	var want []string
	for _, site := range playerSites {
		if site.name == "" || site.recognises == "" || site.shape == nil || site.recognise == nil ||
			site.read == nil || site.listing != nil && site.listing.shape == nil ||
			site.listing != nil && (site.listing.walk == nil) == (site.listing.links == nil) {
			t.Errorf("row %q leaves out something the engine calls", site.name)
		}
		want = append(want, site.name)
	}
	if !slices.Equal(registered, want) {
		t.Errorf("registered %v, want the table's %v", registered, want)
	}
}

// TestPlayerPages covers each row's canonical page: the form whose page
// serves the player, whichever form was pasted.
func TestPlayerPages(t *testing.T) {
	for _, tc := range []struct {
		site     *playerSite
		in, want string
	}{
		{&luluPlayer, "https://lulu.example.test/e/abcd1234wxyz", "https://lulu.example.test/abcd1234wxyz"},
		{&luluPlayer, "https://lulu.example.test/d/abcd1234wxyz", "https://lulu.example.test/abcd1234wxyz"},
		{&mixdropPlayer, "https://mix.example.test/f/abcdef123", "https://mix.example.test/e/abcdef123"},
		{&vidsPlayer, "https://vids.example.test/e/123", "https://vids.example.test/v/123"},
		{&sendvidPlayer, "https://sv.example.test/embed/abcd1234", "https://sv.example.test/abcd1234"},
		{&vidmolyPlayer, "https://vm.example.test/embed-embed-abcdefghijkl.html", "https://vidmoly.biz/embed-abcdefghijkl.html"},
		{&drtuberPlayer, "https://dt.example.test/video/1234567/clip", drtuberRoot + "/player_config_json/?vid=1234567"},
		{&luluPlayer, "https://lulu.example.test/a/b/c", ""},
		{&sendvidPlayer, "https://sv.example.test/help/faq", ""},
	} {
		u, _ := url.Parse(tc.in)
		got, err := tc.site.page(u)
		if tc.want == "" {
			if err == nil {
				t.Errorf("%s %s: page %q, want an error", tc.site.name, tc.in, got)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("%s %s: page %q, %v; want %q", tc.site.name, tc.in, got, err, tc.want)
		}
	}
}

// The player lists its largest file first and marks it 720p, while the
// file's own name says 1920x1080. That disagreement is the reason the
// filename decides, and the reason this fixture exists.
const pornonePlayerPage = `<html><body>
<h1>a synthetic clip</h1>
<video id="pornone-video-player" class="video-js" controls preload="none">
  <source src="https://s1.example.test/vid2/TOKEN/1787378460/21/1/1_1920x1080_4000k.mp4?lang=en"
          type="video/mp4" label="720p" res="720"/>
  <source src="https://s2.example.test/vid2/TOKEN/1787378460/21/1/1_720x406_500k.mp4?lang=en"
          type="video/mp4" label="480p" res="480"/>
</video>
</body></html>`

func TestPornOneRenditionsTrustTheFilename(t *testing.T) {
	renditions := pornoneRenditions(mustParseDoc(t, pornonePlayerPage))
	if len(renditions) != 2 {
		t.Fatalf("read %d renditions, want 2", len(renditions))
	}
	if renditions[0].Quality != 1080 {
		t.Errorf("the 1920x1080 file ranks as %d, but the player labels it 720p",
			renditions[0].Quality)
	}
	if renditions[1].Quality != 406 {
		t.Errorf("the 720x406 file ranks as %d", renditions[1].Quality)
	}

	best, ok := bestCandidate(renditions)
	if !ok {
		t.Fatal("no rendition was chosen")
	}
	if best.Quality != 1080 {
		t.Errorf("chose the %d rendition, want the largest", best.Quality)
	}
}

// TestPornOneRenditionsFallBackToTheAttribute covers a page whose filenames
// carry no dimensions, where the player's own attribute is all there is.
func TestPornOneRenditionsFallBackToTheAttribute(t *testing.T) {
	const page = `<html><body><video>
  <source src="https://s1.example.test/vid2/TOKEN/1/1/low.mp4" label="480p" res="480"/>
  <source src="https://s1.example.test/vid2/TOKEN/1/1/high.mp4" label="1080p" res="1080"/>
</video></body></html>`

	best, ok := bestCandidate(pornoneRenditions(mustParseDoc(t, page)))
	if !ok {
		t.Fatal("no rendition was chosen")
	}
	if best.Quality != 1080 {
		t.Errorf("chose the %d rendition, want the one the player calls largest", best.Quality)
	}
}

func TestPornOneRenditionsOnAPageWithNoPlayer(t *testing.T) {
	if got := pornoneRenditions(mustParseDoc(t, `<html><body>This video has been removed.</body></html>`)); len(got) != 0 {
		t.Errorf("found %d renditions on a page with no player", len(got))
	}
}

func TestFirstHeading(t *testing.T) {
	if got := firstHeading(pornonePlayerPage); got != "a synthetic clip" {
		t.Errorf("firstHeading = %q", got)
	}
	if got := firstHeading(`<html><body><p>no heading</p></body></html>`); got != "" {
		t.Errorf("firstHeading = %q, want empty", got)
	}
}

// The configuration lists every rendition the site knows of, present or
// not: an absent one is an empty string or a JSON null, and null decodes to
// the same empty string.
func TestDrTuberRenditionsSkipTheAbsentOnes(t *testing.T) {
	got := drtuberRenditions(map[string]string{
		"lq": "https://cdn.example.test/mp4_lq/synthetic.mp4?expire=1",
		"hq": "",
		"4k": "",
	})
	if len(got) != 1 {
		t.Fatalf("read %d renditions, want 1", len(got))
	}
	if got[0].Quality != 320 {
		t.Errorf("the lq rendition ranks as %d, want 320", got[0].Quality)
	}
}

// The rendition names are names, not resolutions, so sorting them as text
// would put "lq" above "4k".
func TestDrTuberRenditionsRankByQualityNotName(t *testing.T) {
	best, ok := bestCandidate(drtuberRenditions(map[string]string{
		"lq": "https://cdn.example.test/mp4_lq/synthetic.mp4",
		"hq": "https://cdn.example.test/mp4_hq/synthetic.mp4",
		"4k": "https://cdn.example.test/mp4_4k/synthetic.mp4",
	}))
	if !ok {
		t.Fatal("no rendition was chosen")
	}
	if want := "https://cdn.example.test/mp4_4k/synthetic.mp4"; best.URL != want {
		t.Errorf("chose %q, want the 4k rendition", best.URL)
	}
}

// A name the map does not know still has to rank, or it would sort below
// every named one and never be chosen.
func TestDrTuberRenditionsReadAnUnknownName(t *testing.T) {
	best, ok := bestCandidate(drtuberRenditions(map[string]string{
		"lq":    "https://cdn.example.test/mp4_lq/synthetic.mp4",
		"1080p": "https://cdn.example.test/mp4_1080p/synthetic.mp4",
	}))
	if !ok {
		t.Fatal("no rendition was chosen")
	}
	if want := "https://cdn.example.test/mp4_1080p/synthetic.mp4"; best.URL != want {
		t.Errorf("chose %q, want the 1080p rendition", best.URL)
	}
}

// Equal-quality renditions must be chosen the same way every time: the
// resolve at queue time and the one at download time have to agree.
func TestDrTuberRenditionsAreOrderedDeterministically(t *testing.T) {
	files := map[string]string{
		"a": "https://cdn.example.test/a/synthetic.mp4",
		"b": "https://cdn.example.test/b/synthetic.mp4",
		"c": "https://cdn.example.test/c/synthetic.mp4",
	}
	first := drtuberRenditions(files)
	for range 20 {
		got := drtuberRenditions(files)
		for j := range got {
			if got[j].URL != first[j].URL {
				t.Fatalf("rendition %d came back as %q, first read %q", j, got[j].URL, first[j].URL)
			}
		}
	}
}

func TestDrTuberRenditionsOnAVideoWithNoFiles(t *testing.T) {
	if got := drtuberRenditions(nil); len(got) != 0 {
		t.Errorf("found %d renditions in an empty list", len(got))
	}
	if _, ok := bestCandidate(drtuberRenditions(map[string]string{"lq": "", "hq": ""})); ok {
		t.Error("a video with no rendition was accepted")
	}
}

// The id pattern is what separates a video from the rest of the site.
func TestDrTuberID(t *testing.T) {
	ids := map[string]string{
		"/video/1234567/a-synthetic-clip": "1234567",
		"/video/1234567":                  "1234567",
		"/embed/1234567":                  "1234567",
		"/videos/1234567/x":               "",
		"/categories/something":           "",
		"/":                               "",
	}
	for path, want := range ids {
		var got string
		if m := drtuberID.FindStringSubmatch(path); m != nil {
			got = m[1]
		}
		if got != want {
			t.Errorf("drtuberID(%q) = %q, want %q", path, got, want)
		}
	}
}

// vidmolyEmbedPage is an embed page in the shape the site serves, and it is
// built around the trap rather than around the happy path.
//
// Three of the four file: keys on it are not the video. The sprite sheet for
// the seek bar comes first, which is what a pattern anchored on nothing worse
// than file: would take; the source list opens with a DASH manifest, which
// nothing here can join; and the thumbnail track follows the media, so
// preferring the last match would be wrong as well. Hosts are reserved
// domains — what is under test is which key is the media, which cares nothing
// for whose CDN it names.
const vidmolyEmbedPage = `<html><head><title>A Synthetic Clip - Vidmoly</title></head><body>
<div id="vplayer"></div>
<script type="text/javascript">
  var player = jwplayer("vplayer");
  player.setup({
    image: "https://s1.example.test/i/01/00001/abcdefghijkl.jpg",
    file: "/api/v1/slides?v=abcdefghijkl&e=1750000000&h=SIGNATURE.jpg",
    sources: [
      {file:"https://s1.example.test/dash/abcdefghijkl/index.mpd",type:"dash"},
      {file:"https://s1.example.test/hls2/01/00001/abcdefghijkl/master.m3u8?t=TOKEN&s=1750000000"}
    ],
    tracks: [{file:"/api/v1/thumbnails?v=abcdefghijkl",kind:"thumbnails"}],
    aspectratio: "16:9"
  });
</script></body></html>`

func vidmolyTestBase(t *testing.T) *url.URL {
	t.Helper()
	return vidmolyEmbed("abcdefghijkl")
}

// TestVidmolyMediaIgnoresTheThumbnailSprite is the one that matters. The
// sprite URL answers with a real body, so taking it produces a JPEG on disk
// under the video's name and a job that reports success.
func TestJWSourceIgnoresTheThumbnailSprite(t *testing.T) {
	got, err := jwSource(vidmolyEmbedPage, vidmolyTestBase(t))
	if err != nil {
		t.Fatalf("jwSource: %v", err)
	}
	if strings.Contains(got, "slides") || strings.Contains(got, "thumbnails") {
		t.Fatalf("a player thumbnail was taken for the media: %s", got)
	}
	if !strings.Contains(got, "master.m3u8") {
		t.Errorf("got %s, want the HLS master playlist", got)
	}
}

// TestVidmolyMediaSkipsDASH pins the other half of the choice: the source
// list names the DASH manifest first, and taking it would leave the
// downloader with a manifest it cannot turn into segments.
func TestJWSourceSkipsDASH(t *testing.T) {
	got, err := jwSource(vidmolyEmbedPage, vidmolyTestBase(t))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got, ".mpd") {
		t.Errorf("the DASH manifest was chosen: %s", got)
	}
}

// TestVidmolyMediaReportsADASHOnlyPage covers the file that genuinely offers
// nothing else, where the useful answer is why rather than "no media".
func TestJWSourceReportsADASHOnlyPage(t *testing.T) {
	const doc = `<html><body><script>player.setup({
	  file: "/api/v1/slides?v=abcdefghijkl.jpg",
	  sources: [{file:"https://s1.example.test/dash/abcdefghijkl/index.mpd",type:"dash"}]
	});</script></body></html>`

	_, err := jwSource(doc, vidmolyTestBase(t))
	if err == nil {
		t.Fatal("a page offering only DASH was accepted")
	}
	if !strings.Contains(err.Error(), "DASH") {
		t.Errorf("error %q does not say what the page offered", err)
	}
}

// TestVidmolyMediaResolvesARelativeLink matters because the player writes
// some of its links relative — the sprite on every page is one — so the media
// key cannot be assumed absolute either.
func TestJWSourceResolvesARelativeLink(t *testing.T) {
	const doc = `<script>player.setup({sources:[{file:"/hls2/abcdefghijkl/master.m3u8?t=TOKEN"}]});</script>`

	got, err := jwSource(doc, vidmolyTestBase(t))
	if err != nil {
		t.Fatal(err)
	}
	const want = "https://vidmoly.biz/hls2/abcdefghijkl/master.m3u8?t=TOKEN"
	if got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

// TestVidmolyMediaRejectsAPageWithNoSourceList is the failure that must stay a
// failure: a page carrying the sprite and nothing else must not resolve to it.
func TestJWSourceRejectsAPageWithNoSourceList(t *testing.T) {
	for _, doc := range []string{
		`<html><body>File not found, deleted or being converted</body></html>`,
		`<script>player.setup({file:"/api/v1/slides?v=abcdefghijkl.jpg"});</script>`,
	} {
		if got, err := jwSource(doc, vidmolyTestBase(t)); err == nil {
			t.Errorf("a page with no source list resolved to %s", got)
		}
	}
}

// TestVidmolyCode covers every shape a link can arrive in, including the one
// the site's own redirect produces.
func TestVidmolyCode(t *testing.T) {
	tests := map[string]string{
		"https://vidmoly.biz/embed-abcdefghijkl.html": "abcdefghijkl",
		"https://vidmoly.me/embed-abcdefghijkl.html":  "abcdefghijkl",
		"https://vidmoly.to/embed-abcdefghijkl.html":  "abcdefghijkl",
		"https://vidmoly.net/embed-abcdefghijkl.html": "abcdefghijkl",
		// The bare shape, which .net redirects to .biz and .biz then refuses.
		"https://vidmoly.net/abcdefghijkl": "abcdefghijkl",
		"https://vidmoly.biz/abcdefghijkl": "abcdefghijkl",
		// What somebody lands on after following the site's own redirect.
		"https://vidmoly.biz/embed-embed-abcdefghijkl.html": "abcdefghijkl",
		// Query strings and a trailing slash are the player's, not ours.
		"https://vidmoly.biz/embed-abcdefghijkl.html?autoplay=1": "abcdefghijkl",
		"https://vidmoly.biz/abcdefghijkl/":                      "abcdefghijkl",
		// Nothing that could be a file code.
		"https://vidmoly.biz/":     "",
		"https://vidmoly.biz/tos":  "",
		"https://vidmoly.biz/i.js": "",
	}
	for raw, want := range tests {
		u, err := ParseURL(raw)
		if err != nil {
			t.Fatal(err)
		}
		if got := vidmolyCode(u); got != want {
			t.Errorf("vidmolyCode(%s) = %q, want %q", raw, got, want)
		}
	}
}

// TestVidmolyEmbedAlwaysUsesTheHostThatAnswers is the whole reason this
// extractor rebuilds the URL instead of fetching what it was handed: the
// other three domains answer a video path with a redirect that reaches an ad
// page, a 404, or a doubled path that 404s.
func TestVidmolyEmbedAlwaysUsesTheHostThatAnswers(t *testing.T) {
	const want = "https://vidmoly.biz/embed-abcdefghijkl.html"
	for _, raw := range []string{
		"https://vidmoly.me/embed-abcdefghijkl.html",
		"https://vidmoly.to/embed-abcdefghijkl.html",
		"https://vidmoly.net/abcdefghijkl",
		"https://www.vidmoly.biz/embed-abcdefghijkl.html",
	} {
		u, err := ParseURL(raw)
		if err != nil {
			t.Fatal(err)
		}
		if got := vidmolyEmbed(vidmolyCode(u)).String(); got != want {
			t.Errorf("%s became %s, want %s", raw, got, want)
		}
	}
}

func TestVidmolyExtension(t *testing.T) {
	tests := map[string]string{
		"https://s1.example.test/v/abcdefghijkl.mp4?t=TOKEN": ".mp4",
		"https://s1.example.test/v/abcdefghijkl.mkv":         ".mkv",
		"https://s1.example.test/v/abcdefghijkl":             ".mp4",
	}
	for link, want := range tests {
		if got := vidmolyExtension(link); got != want {
			t.Errorf("vidmolyExtension(%s) = %q, want %q", link, got, want)
		}
	}
}

// TestStreamtapeAssemble covers the one thing that actually breaks about
// this host: the link is delivered in two halves, and how much of the second
// half is padding is decided by the page, not by us.
func TestStreamtapeAssemble(t *testing.T) {
	const page = "https://streamtape.example.test/e/SYNTHETIC"
	doc := `<div id="robotlink" style="display:none;">/get_video?id=1&amp;token=abc</div>
<script>document.getElementById('robotlink').innerHTML = ` +
		`'//streamtape.example.test/get_video?id=1&token=abc' + ('xxxdef456').substring(3);</script>`

	got, err := streamtapeAssemble(doc, page)
	if err != nil {
		t.Fatalf("streamtapeAssemble: %v", err)
	}
	const want = "https://streamtape.example.test/get_video?id=1&token=abcdef456&stream=1"
	if got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

// TestStreamtapeAssembleFollowsTheOffsetOnThePage is the same fixture with a
// different offset, which is exactly the change the host makes when it wants
// to break scrapers that hard-coded one.
func TestStreamtapeAssembleFollowsTheOffsetOnThePage(t *testing.T) {
	const page = "https://streamtape.example.test/e/SYNTHETIC"
	doc := `<script>document.getElementById('robotlink').innerHTML = ` +
		`'//streamtape.example.test/get_video?id=1&token=abc' + ('xxxxxxdef456').substring(6);</script>`

	got, err := streamtapeAssemble(doc, page)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(strings.TrimSuffix(got, "&stream=1"), "abcdef456") {
		t.Errorf("got %s, want the tail joined from offset 6", got)
	}
}

func TestStreamtapeAssembleReportsAMissingLink(t *testing.T) {
	_, err := streamtapeAssemble("<html><body>File not found</body></html>",
		"https://streamtape.example.test/e/SYNTHETIC")
	if err == nil {
		t.Fatal("a page with no link was accepted")
	}
	if !strings.Contains(err.Error(), "no media link") {
		t.Errorf("error %q does not say the link is missing", err)
	}
}

// TestUnpackJS covers the p,a,c,k,e,d packer these players ship instead of
// readable source. The fixture is hand-built: the payload's tokens are
// indices into the word list, written in the base the call states.
func TestUnpackJS(t *testing.T) {
	const packed = `eval(function(p,a,c,k,e,d){while(c--)if(k[c])` +
		`p=p.replace(new RegExp('\\b'+c.toString(a)+'\\b','g'),k[c]);return p}` +
		`('0.1="2://4.5/6.7"',36,8,'MDCore|wurl|https|unused|example|test|clip|mp4'.split('|'),0,{}))`

	got, ok := unpackJS(packed)
	if !ok {
		t.Fatal("a packed script was not recognised")
	}
	const want = `MDCore.wurl="https://example.test/clip.mp4"`
	if got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

func TestUnpackJSLeavesUnpackedScriptsAlone(t *testing.T) {
	if _, ok := unpackJS(`<script>var MDCore = {}; MDCore.wurl = "//example.test/clip.mp4";</script>`); ok {
		t.Error("an ordinary script was taken for a packed one")
	}
}

// A payload word long enough to overflow the index accumulator — a hex token
// inside a URL is the ordinary case — must pass through verbatim rather than
// wrap negative and panic the word lookup.
func TestUnpackJSSurvivesLongPayloadWords(t *testing.T) {
	const packed = `eval(function(p,a,c,k,e,d){}` +
		`('0="deadbeefdeadbeefdeadbeefdeadbeef"',36,2,'MDCore|unused'.split('|'),0,{}))`

	got, ok := unpackJS(packed)
	if !ok {
		t.Fatal("a packed script was not recognised")
	}
	if !strings.Contains(got, "deadbeefdeadbeefdeadbeefdeadbeef") {
		t.Errorf("the long token did not survive: %s", got)
	}
}

// TestPackerIndex covers the alphabet, whose third range only appears once a
// word list outgrows base 36 — the case that is easy to get wrong and rare
// enough not to notice.
func TestPackerIndex(t *testing.T) {
	tests := []struct {
		word  string
		base  int
		want  int
		valid bool
	}{
		{word: "0", base: 36, want: 0, valid: true},
		{word: "9", base: 36, want: 9, valid: true},
		{word: "a", base: 36, want: 10, valid: true},
		{word: "z", base: 36, want: 35, valid: true},
		{word: "10", base: 36, want: 36, valid: true},
		{word: "A", base: 62, want: 36, valid: true},
		{word: "Z", base: 62, want: 61, valid: true},
		{word: "z", base: 20, valid: false}, // a digit the base does not have
		{word: "-", base: 36, valid: false}, // not a digit at all
		{word: "", base: 36, valid: false},
		// A long run of base digits is an ordinary word — a hex token in a
		// URL, say — not an index. Unbounded it overflows the accumulator,
		// and a wrapped-negative value would panic the caller's word lookup.
		{word: "deadbeefdeadbeefdeadbeefdeadbeef", base: 36, valid: false},
		{word: strings.Repeat("z", 64), base: 62, valid: false},
	}
	for _, tc := range tests {
		got, ok := packerIndex(tc.word, tc.base)
		if ok != tc.valid || (ok && got != tc.want) {
			t.Errorf("packerIndex(%q, %d) = %d, %v; want %d, %v",
				tc.word, tc.base, got, ok, tc.want, tc.valid)
		}
	}
}

// luluSite serves the shape a lulustream page has: a heading naming the
// video, a packed player setup naming a signed master playlist, one muxed
// rendition, and AES-128 segments under one key. The packer is given no
// words, so the payload — the test server's own address — passes through it
// untouched.
func luluSite(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var pages atomic.Int32
	mux := http.NewServeMux()
	var srv *httptest.Server
	mux.HandleFunc("/abcd1234wxyz", func(w http.ResponseWriter, _ *http.Request) {
		pages.Add(1)
		setup := fmt.Sprintf(`jwplayer("vplayer").setup({sources:[{file:"%s/hls/master.m3u8?t=signed&e=28800"}],image:"%s/thumb.jpg"});`,
			srv.URL, srv.URL)
		_, _ = fmt.Fprintf(w, `<html><head><title>First Clip - Lulustream.mp4</title></head><body>
<h1 class="h5">First Clip</h1>
<script>eval(function(p,a,c,k,e,d){while(c--)if(k[c])p=p.replace(new RegExp('\\b'+c.toString(a)+'\\b','g'),k[c]);return p}('%s',36,0,''.split('|')))</script>
</body></html>`, setup)
	})
	mux.HandleFunc("/hls/master.m3u8", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=3000000,RESOLUTION=720x1280,CODECS=\"avc1.64001f,mp4a.40.2\"\n"+
			"index-v1-a1.m3u8?t=signed\n")
	})
	mux.HandleFunc("/hls/index-v1-a1.m3u8", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "#EXTM3U\n#EXT-X-TARGETDURATION:12\n#EXT-X-PLAYLIST-TYPE:VOD\n"+
			`#EXT-X-KEY:METHOD=AES-128,URI="encryption.key?t=signed"`+"\n"+
			"#EXTINF:10,\nseg-1-v1-a1.ts?t=signed\n#EXTINF:8,\nseg-2-v1-a1.ts?t=signed\n#EXT-X-ENDLIST\n")
	})
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &pages
}

func TestLuluStreamFollowsThePackedPlayerToEncryptedSegments(t *testing.T) {
	srv, pages := luluSite(t)
	l := NewPlayer(httpx.New("test-agent", "en-US", 0, 5*time.Second), &luluPlayer)

	u, _ := url.Parse(srv.URL + "/e/abcd1234wxyz")
	res, err := l.Extract(context.Background(), u, Options{})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if res.Title != "First Clip" {
		t.Errorf("title = %q, want the page's heading", res.Title)
	}
	f := res.Files[0]
	if f.Name != "First Clip.ts" {
		t.Errorf("name = %q, want the title and the segments' container", f.Name)
	}
	if len(f.Segments) != 2 || f.Segments[0] != srv.URL+"/hls/seg-1-v1-a1.ts?t=signed" {
		t.Errorf("segments = %v", f.Segments)
	}
	if f.SegmentKey == nil || f.SegmentKey.URI != srv.URL+"/hls/encryption.key?t=signed" {
		t.Errorf("key = %+v, want the playlist's own", f.SegmentKey)
	}

	// The page is read again at transfer time, segments and key with it.
	tg, err := f.Resolve(context.Background())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(tg.Segments) != 2 || tg.SegmentKey == nil || pages.Load() != 2 {
		t.Errorf("resolve gave %d segments, key %v, after %d page reads", len(tg.Segments), tg.SegmentKey, pages.Load())
	}
}

func TestLuluID(t *testing.T) {
	for raw, want := range map[string]string{
		"https://luluvido.com/abcd1234wxyz":     "abcd1234wxyz",
		"https://luluvdo.com/e/abcd1234wxyz":    "abcd1234wxyz",
		"https://lulustream.com/d/abcd1234wxyz": "abcd1234wxyz",
		"https://lulustream.com/":               "",
		"https://lulustream.com/a/b/c":          "",
	} {
		u, _ := url.Parse(raw)
		if got := luluID(u); got != want {
			t.Errorf("luluID(%s) = %q, want %q", raw, got, want)
		}
	}
}

// vidsSite serves video pages shaped like the site's: the player
// configuration as one object in a script, followed by the rest of the
// script, which the reader must stop short of.
func vidsSite(t *testing.T, configs map[string]string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	for id, cfg := range configs {
		mux.HandleFunc("/v/"+id, func(w http.ResponseWriter, _ *http.Request) {
			_, _ = fmt.Fprintf(w, `<html><head><title>x - VIDS.ST</title></head><body><div id="player"></div>
<script>
const playerConfig = %s;
(function(){ if (playerConfig.videoUrl) { new Artplayer({url: playerConfig.videoUrl}); } })();
</script></body></html>`, cfg)
		})
	}
	mux.HandleFunc("/hls/index.m3u8", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "#EXTM3U\n#EXT-X-TARGETDURATION:6\n"+`#EXT-X-KEY:METHOD=AES-128,URI="key.bin"`+
			"\n#EXTINF:6,\nseg0.ts\n#EXTINF:6,\nseg1.ts\n#EXT-X-ENDLIST\n")
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func vidsExtract(t *testing.T, raw string) (*Result, error) {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return NewPlayer(httpx.New("test-agent", "en-US", 0, 5*time.Second), &vidsPlayer).Extract(context.Background(), u, Options{})
}

func TestVidsStTakesTheUploadedFileAsItIs(t *testing.T) {
	srv := vidsSite(t, map[string]string{
		"101": `{"videoUrl":"/storage/uploads/video101/first-clip.mp4","videoName":"First Clip.mp4","thumbnail":"/t.jpg","videoId":101,"subtitleTracks":[],"audioTracks":[]}`,
	})
	res, err := vidsExtract(t, srv.URL+"/e/101")
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	f := res.Files[0]
	if res.Title != "First Clip" || f.Name != "First Clip.mp4" {
		t.Errorf("title %q, name %q", res.Title, f.Name)
	}
	if f.URL != srv.URL+"/storage/uploads/video101/first-clip.mp4" {
		t.Errorf("url = %q, want the uploaded file, resolved against the page", f.URL)
	}
	if len(f.Segments) != 0 {
		t.Error("a plain file was treated as a playlist")
	}
}

func TestVidsStFollowsAPlaylistNatively(t *testing.T) {
	srv := vidsSite(t, map[string]string{
		"202": `{"videoUrl":"/hls/index.m3u8","videoName":"Second Clip.mp4"}`,
	})
	res, err := vidsExtract(t, srv.URL+"/v/202")
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	f := res.Files[0]
	if f.Name != "Second Clip.ts" || len(f.Segments) != 2 || f.SegmentKey == nil {
		t.Errorf("name %q, %d segments, key %v; want the playlist joined and decrypted as TS", f.Name, len(f.Segments), f.SegmentKey)
	}
}

func TestVidsStSaysWhenAVideoHasBeenRemoved(t *testing.T) {
	srv := vidsSite(t, nil)
	_, err := vidsExtract(t, srv.URL+"/v/303")
	if err == nil || !strings.Contains(err.Error(), "removed") {
		t.Errorf("err = %v, want it to say the video is gone", err)
	}
}

func TestVidsID(t *testing.T) {
	for raw, want := range map[string]string{
		"https://vids.st/v/123": "123",
		"https://vids.st/e/123": "123",
		"https://vids.st/":      "",
		"https://vids.st/faq":   "",
	} {
		u, _ := url.Parse(raw)
		if got := vidsID(u); got != want {
			t.Errorf("vidsID(%s) = %q, want %q", raw, got, want)
		}
	}
}

// sendvidSite serves watch pages shaped like the site's: the title in the
// page metadata, og:video naming a link that is not the media, and the
// player's <source> signed afresh on every visit.
func sendvidSite(t *testing.T, titles map[string]string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var visits atomic.Int32
	mux := http.NewServeMux()
	for id, title := range titles {
		mux.HandleFunc("/"+id, func(w http.ResponseWriter, _ *http.Request) {
			n := visits.Add(1)
			meta := ""
			if title != "" {
				meta = fmt.Sprintf(`<meta name="twitter:title" content="%s"/><meta property="og:title" content="%s"/>`, title, title)
			}
			_, _ = fmt.Fprintf(w, `<!DOCTYPE html><html><head><title>%s - Sendvid</title>%s
<meta property="og:video" content="https://sendvid.example.test/%s.mp4"/>
<meta property="og:video:type" content="video/mp4"/></head><body>
<video id="video-js-video" class="video-js" controls preload="none">
<source src="https://videos9.media.example.test/ab/cd/%s.mp4?validfrom=1000&amp;validto=15400&amp;rate=250k&amp;hash=visit%d" type="video/mp4" id="video_source"/>
</video></body></html>`, title, meta, id, id, n)
		})
	}
	mux.HandleFunc("/gone0000", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "<html><title>Upload and Share Videos - Sendvid</title>Not Found</html>", http.StatusNotFound)
	})
	mux.HandleFunc("/empty000", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `<html><head><meta property="og:title" content="Nothing Here"/></head><body></body></html>`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &visits
}

func sendvidExtract(t *testing.T, raw string) (*Result, error) {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return NewPlayer(httpx.New("test-agent", "en-US", 0, 5*time.Second), &sendvidPlayer).Extract(context.Background(), u, Options{})
}

func TestSendvidTakesThePlayersSignedSourceAndSignsItAgainLater(t *testing.T) {
	srv, visits := sendvidSite(t, map[string]string{"abcd1234": "Harbour Walk"})
	res, err := sendvidExtract(t, srv.URL+"/abcd1234")
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	f := res.Files[0]
	if res.Title != "Harbour Walk" || f.Name != "Harbour Walk.mp4" {
		t.Errorf("title %q, name %q", res.Title, f.Name)
	}
	want := "https://videos9.media.example.test/ab/cd/abcd1234.mp4?validfrom=1000&validto=15400&rate=250k&hash=visit1"
	if f.URL != want {
		t.Errorf("url = %q, want the player's source with its signature intact", f.URL)
	}
	if f.Headers["Referer"] != srv.URL+"/abcd1234" {
		t.Errorf("headers = %v, want the page as referer", f.Headers)
	}
	if f.Resolve == nil {
		t.Fatal("no resolver: a link signed for hours would expire in a long queue")
	}
	target, err := f.Resolve(context.Background())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !strings.HasSuffix(target.URL, "hash=visit2") || visits.Load() != 2 {
		t.Errorf("resolved %q after %d visits, want a link signed on a second visit", target.URL, visits.Load())
	}
}

// The embed page and the og:video link name the same video as the watch
// page, which is the one read.
func TestSendvidReadsEveryLinkShapeAsTheWatchPage(t *testing.T) {
	srv, _ := sendvidSite(t, map[string]string{"abcd1234": "Harbour Walk"})
	for _, raw := range []string{srv.URL + "/embed/abcd1234", srv.URL + "/abcd1234.mp4"} {
		res, err := sendvidExtract(t, raw)
		if err != nil {
			t.Fatalf("%s: %v", raw, err)
		}
		if res.Files[0].Name != "Harbour Walk.mp4" || !strings.Contains(res.Files[0].URL, "videos9.media.example.test") {
			t.Errorf("%s: %q from %q", raw, res.Files[0].Name, res.Files[0].URL)
		}
	}
}

func TestSendvidNamesAnUntitledVideoByItsID(t *testing.T) {
	srv, _ := sendvidSite(t, map[string]string{"untitled9": ""})
	res, err := sendvidExtract(t, srv.URL+"/untitled9")
	if err != nil {
		t.Fatal(err)
	}
	if res.Files[0].Name != "untitled9.mp4" {
		t.Errorf("name %q, want the id", res.Files[0].Name)
	}
}

func TestSendvidSaysWhenAVideoIsGone(t *testing.T) {
	srv, _ := sendvidSite(t, nil)
	_, err := sendvidExtract(t, srv.URL+"/gone0000")
	if err == nil || !strings.Contains(err.Error(), "not found or has been removed") {
		t.Errorf("err = %v, want the video reported gone", err)
	}
	_, err = sendvidExtract(t, srv.URL+"/empty000")
	if err == nil || !strings.Contains(err.Error(), "no video on") {
		t.Errorf("err = %v, want a page without a player refused", err)
	}
}

func TestSendvidID(t *testing.T) {
	for raw, want := range map[string]string{
		"https://sendvid.com/abcd1234":          "abcd1234",
		"https://sendvid.com/abcd1234.mp4":      "abcd1234",
		"https://www.sendvid.com/embed/Ab12Cd":  "Ab12Cd",
		"https://sendvid.com/embed/abcd1234?x=": "abcd1234",
		"https://sendvid.com/":                  "",
		"https://sendvid.com/help/faq":          "",
		"https://sendvid.com/users/sign_in":     "",
		"https://sendvid.com/not-an-id":         "",
	} {
		u, _ := url.Parse(raw)
		if got := sendvidID(u); got != want {
			t.Errorf("sendvidID(%s) = %q, want %q", raw, got, want)
		}
	}
}

// TestPlayerEngineOptions covers what a row can ask of the engine beyond
// its read: the media referer a read names over the row's or none at all,
// a playlist still being written refused unless the row serves live
// streams, and the proxy fallback carried onto the file.
func TestPlayerEngineOptions(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/open.m3u8":
			fmt.Fprint(w, "#EXTM3U\n#EXTINF:4,\n/part.ts\n")
		case "/closed.m3u8":
			fmt.Fprint(w, "#EXTM3U\n#EXTINF:4,\n/part.ts\n#EXT-X-ENDLIST\n")
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	for _, tc := range []struct {
		name          string
		rowReferer    func(string) string
		readReferer   string
		link          string
		live, proxied bool
		wantReferer   string
		wantErr       string
	}{
		{name: "row referer", rowReferer: samePage, link: "/file.mp4", wantReferer: "https://player.example.test/v/1"},
		{name: "read referer wins", rowReferer: samePage, readReferer: "https://embed.example.test/p", link: "/file.mp4",
			wantReferer: "https://embed.example.test/p"},
		{name: "no referer", link: "/file.mp4"},
		{name: "proxied", link: "/file.mp4", proxied: true},
		{name: "finished playlist", link: "/closed.m3u8"},
		{name: "unfinished playlist", link: "/open.m3u8", wantErr: "still being written"},
		{name: "live playlist", link: "/open.m3u8", live: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			site := &playerSite{
				name: "synthetic", referer: tc.rowReferer, signed: true, live: tc.live, proxyFallback: tc.proxied,
				read: func(context.Context, *httpx.Client, string) (*playerVideo, error) {
					return &playerVideo{title: "First Clip", candidates: playerLink(srv.URL + tc.link), referer: tc.readReferer}, nil
				},
			}
			p := NewPlayer(httpx.New("test-agent", "en-US", 0, 5*time.Second), site)
			res, err := p.Extract(context.Background(), mustParse(t, "https://player.example.test/v/1"), Options{})
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			f := res.Files[0]
			if got := f.Headers[httpx.HeaderReferer]; got != tc.wantReferer || (tc.wantReferer == "" && f.Headers != nil) {
				t.Errorf("referer = %q (headers %v), want %q", got, f.Headers, tc.wantReferer)
			}
			if f.ProxyFallback != tc.proxied || p.ProxyFallback() != tc.proxied {
				t.Errorf("proxy fallback = %v/%v, want %v", f.ProxyFallback, p.ProxyFallback(), tc.proxied)
			}
			if f.Resolve == nil {
				t.Error("a signed row's file is not read again at transfer time")
			}
		})
	}
}

// TestPlayerGoneKeepsTheStatus checks that a page answering 404 reads as a
// video that has gone and still carries the status underneath.
func TestPlayerGoneKeepsTheStatus(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	site := &playerSite{name: "synthetic", read: func(ctx context.Context, client *httpx.Client, page string) (*playerVideo, error) {
		_, err := playerFetch(ctx, client, page, page)
		return nil, err
	}}
	_, err := NewPlayer(httpx.New("test-agent", "en-US", 0, 5*time.Second), site).
		Extract(context.Background(), mustParse(t, srv.URL+"/v/1"), Options{})
	if err == nil || !strings.Contains(err.Error(), "is not there") || !httpx.HasStatus(err, http.StatusNotFound) {
		t.Fatalf("err = %v, want a gone video still carrying its 404", err)
	}
}
