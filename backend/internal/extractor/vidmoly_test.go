package extractor

import (
	"net/url"
	"strings"
	"testing"
)

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
func TestVidmolyMediaIgnoresTheThumbnailSprite(t *testing.T) {
	got, err := vidmolyMedia(vidmolyEmbedPage, vidmolyTestBase(t))
	if err != nil {
		t.Fatalf("vidmolyMedia: %v", err)
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
func TestVidmolyMediaSkipsDASH(t *testing.T) {
	got, err := vidmolyMedia(vidmolyEmbedPage, vidmolyTestBase(t))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got, ".mpd") {
		t.Errorf("the DASH manifest was chosen: %s", got)
	}
}

// TestVidmolyMediaReportsADASHOnlyPage covers the file that genuinely offers
// nothing else, where the useful answer is why rather than "no media".
func TestVidmolyMediaReportsADASHOnlyPage(t *testing.T) {
	const doc = `<html><body><script>player.setup({
	  file: "/api/v1/slides?v=abcdefghijkl.jpg",
	  sources: [{file:"https://s1.example.test/dash/abcdefghijkl/index.mpd",type:"dash"}]
	});</script></body></html>`

	_, err := vidmolyMedia(doc, vidmolyTestBase(t))
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
func TestVidmolyMediaResolvesARelativeLink(t *testing.T) {
	const doc = `<script>player.setup({sources:[{file:"/hls2/abcdefghijkl/master.m3u8?t=TOKEN"}]});</script>`

	got, err := vidmolyMedia(doc, vidmolyTestBase(t))
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
func TestVidmolyMediaRejectsAPageWithNoSourceList(t *testing.T) {
	for _, doc := range []string{
		`<html><body>File not found, deleted or being converted</body></html>`,
		`<script>player.setup({file:"/api/v1/slides?v=abcdefghijkl.jpg"});</script>`,
	} {
		if got, err := vidmolyMedia(doc, vidmolyTestBase(t)); err == nil {
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

func TestVidmolyMatch(t *testing.T) {
	v := NewVidmoly(nil)
	for _, host := range []string{
		"vidmoly.me", "vidmoly.to", "vidmoly.biz", "vidmoly.net", "www.vidmoly.biz",
	} {
		if !v.Match(&url.URL{Scheme: "https", Host: host, Path: "/embed-abcdefghijkl.html"}) {
			t.Errorf("%s was not matched", host)
		}
	}
	for _, host := range []string{"vidmoly.example.test", "notvidmoly.me", "vidmoly.com"} {
		if v.Match(&url.URL{Scheme: "https", Host: host, Path: "/embed-abcdefghijkl.html"}) {
			t.Errorf("%s was matched", host)
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
