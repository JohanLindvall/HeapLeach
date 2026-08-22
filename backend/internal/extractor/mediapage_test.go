package extractor

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/httpx"
	"golang.org/x/net/html"
)

// Every fixture here is written by hand on reserved domains. What is under
// test is which markup counts as the film and which is furniture around it,
// and none of that cares whose host a path belongs to.

// mediaPagePlayerHTML carries the two traps a real video page carries at
// once: a <picture> whose <source> elements are photographs, and sharing
// metadata pointing at a promo clip that is not the film. The player's own
// sources disagree with their labels the way pornone's do — the largest file
// is marked 720p and its own name says 1920x1080.
const mediaPagePlayerHTML = `<html><head>
<meta property="og:title" content="An interview with the director">
<title>An interview with the director | Example Tube</title>
<meta property="og:video" content="https://cdn.example.test/promo/teaser.mp4">
<meta property="og:video:height" content="1080">
</head><body>
<picture>
  <source srcset="/img/header_1920x1080.jpg" type="image/jpeg">
  <img src="/img/header.jpg" alt="">
</picture>
<video id="player" poster="/img/poster_1920x1080_still.jpg" controls>
  <source src="/media/clip_1920x1080_4000k.mp4" type="video/mp4" label="720p" res="720">
  <source src="https://cdn.example.test/media/clip_640x360_800k.mp4" type="video/mp4" label="1080p" res="1080">
</video>
</body></html>`

func mustParse(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := ParseURL(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func mustParseDoc(t *testing.T, doc string) *html.Node {
	t.Helper()
	root, err := parseHTML(doc)
	if err != nil {
		t.Fatal(err)
	}
	return root
}

// TestMediaPageElementsReadOnlyThePlayer pins the restriction that makes this
// safe to run on an arbitrary page: <source> is also how a responsive image
// is spelled, so anything outside a <video> or <audio> is not media.
func TestMediaPageElementsReadOnlyThePlayer(t *testing.T) {
	base := mustParse(t, "https://example.test/watch/an-interview")
	candidates := mediaPageElements(mustParseDoc(t, mediaPagePlayerHTML), base)

	if len(candidates) != 2 {
		t.Fatalf("collected %d candidates, want the player's two: %+v", len(candidates), candidates)
	}
	for _, c := range candidates {
		if strings.Contains(c.URL, "header") || strings.Contains(c.URL, "poster") {
			t.Errorf("a photograph was collected as a rendition: %s", c.URL)
		}
	}
}

// TestMediaPageElementsTrustTheFilename covers the disagreement pornone
// documents: the player labels its largest file 720p while the encoder wrote
// 1920x1080 into the name, and the name is the one that is right.
func TestMediaPageElementsTrustTheFilename(t *testing.T) {
	base := mustParse(t, "https://example.test/watch/an-interview")
	best, ok := bestCandidate(mediaPageElements(mustParseDoc(t, mediaPagePlayerHTML), base))
	if !ok {
		t.Fatal("no rendition was chosen")
	}
	if best.Quality != 1080 {
		t.Errorf("chose the %d rendition; the labels are the other way round", best.Quality)
	}
	// And the relative reference was resolved against the page.
	if want := "https://example.test/media/clip_1920x1080_4000k.mp4"; best.URL != want {
		t.Errorf("chose %q, want %q", best.URL, want)
	}
}

// TestMediaPageFindPrefersThePlayerOverTheMetadata is the false-positive
// guard stated as behaviour: the page advertises a promo clip in og:video and
// the film is in its player, and the player wins.
//
// A nil client is deliberate. Reaching a step that fetches would itself be
// the regression this pins, so it should fail loudly rather than quietly.
func TestMediaPageFindPrefersThePlayerOverTheMetadata(t *testing.T) {
	base := mustParse(t, "https://example.test/watch/an-interview")
	found, ok := mediaPageFind(context.Background(), nil,
		mustParseDoc(t, mediaPagePlayerHTML), mediaPagePlayerHTML, base)
	if !ok {
		t.Fatal("nothing was found on a page with a player on it")
	}
	if strings.Contains(found.URL, "promo") {
		t.Errorf("resolved to the sharing metadata's promo clip: %s", found.URL)
	}
}

// TestMediaPageElementsStopAtTheFirstPlayer covers the page shape that made
// ranking across players wrong: the article's own film sits above a rail of
// previews for other ones, and the preview here is the larger file.
func TestMediaPageElementsStopAtTheFirstPlayer(t *testing.T) {
	const page = `<html><body>
<video id="main" controls>
  <source src="/media/the_film_640x360_800k.mp4" type="video/mp4">
</video>
<aside class="related">
  <video id="preview" muted loop>
    <source src="/media/another_film_1920x1080_4000k.mp4" type="video/mp4">
  </video>
</aside>
</body></html>`

	base := mustParse(t, "https://example.test/news/an-article")
	best, ok := bestCandidate(mediaPageElements(mustParseDoc(t, page), base))
	if !ok {
		t.Fatal("no rendition was chosen")
	}
	if !strings.Contains(best.URL, "the_film") {
		t.Errorf("chose %q, want the page's own player rather than the taller preview beside it", best.URL)
	}
}

// TestMediaPageElementsStepOverAPlayerWithNothingInIt is the other half of
// that rule: the first player is preferred, but an empty one is not an answer.
func TestMediaPageElementsStepOverAPlayerWithNothingInIt(t *testing.T) {
	const page = `<html><body>
<video id="mse" src="blob:https://example.test/8f0e-4a1b"></video>
<video id="fallback"><source src="/media/the_film.mp4" type="video/mp4"></video>
</body></html>`

	base := mustParse(t, "https://example.test/watch/a-clip")
	best, ok := bestCandidate(mediaPageElements(mustParseDoc(t, page), base))
	if !ok {
		t.Fatal("the player below the one with nothing in it was not read")
	}
	if !strings.HasSuffix(best.URL, "/media/the_film.mp4") {
		t.Errorf("chose %q", best.URL)
	}
}

// TestMediaPageElementsSkipWhatCannotBeFetched covers the player shapes that
// look like a source and are not one: an MSE player's blob: URL names an
// object inside the browser, and a typed source says outright when it is not
// media.
func TestMediaPageElementsSkipWhatCannotBeFetched(t *testing.T) {
	const page = `<html><body>
<video src="blob:https://example.test/8f0e-4a1b" controls>
  <source src="data:video/mp4;base64,AAAA" type="video/mp4">
  <source src="/media/stream.mpd" type="application/dash+xml">
  <source src="/img/frame.jpg" type="image/jpeg">
</video>
</body></html>`

	base := mustParse(t, "https://example.test/watch/a-clip")
	if got := mediaPageElements(mustParseDoc(t, page), base); len(got) != 0 {
		t.Errorf("collected %d candidates from a page with nothing fetchable on it: %+v", len(got), got)
	}
}

func TestMediaPageOpenGraph(t *testing.T) {
	base := mustParse(t, "https://example.test/watch/a-clip")

	t.Run("the secure link is preferred", func(t *testing.T) {
		const page = `<html><head>
<meta property="og:video:secure_url" content="https://cdn.example.test/v/secure.mp4">
<meta name="og:video:url" content="http://cdn.example.test/v/plain.mp4">
<meta property="og:video" content="http://cdn.example.test/v/legacy.mp4">
<meta property="og:video:height" content="720">
</head></html>`

		found, ok := mediaPageOpenGraph(mustParseDoc(t, page), base)
		if !ok {
			t.Fatal("nothing was read from the sharing metadata")
		}
		if !strings.HasSuffix(found.URL, "/secure.mp4") {
			t.Errorf("read %q, want the https spelling", found.URL)
		}
		if found.Quality != 720 {
			t.Errorf("quality = %d, want the height the page states", found.Quality)
		}
		if found.Size != -1 {
			t.Errorf("size = %d, want -1: the page stated no length", found.Size)
		}
	})

	// The trap that makes this step untrustworthy on its own: a player
	// advertises its own embed page here, and says so in the type.
	t.Run("a player page is declined", func(t *testing.T) {
		const page = `<html><head>
<meta property="og:video" content="https://example.test/embed/a-clip">
<meta property="og:video:type" content="text/html">
</head></html>`

		if found, ok := mediaPageOpenGraph(mustParseDoc(t, page), base); ok {
			t.Errorf("took %q, which the page calls a web page", found.URL)
		}
	})

	t.Run("a relative reference is resolved", func(t *testing.T) {
		const page = `<html><head><meta property="og:video" content="/v/clip.mp4"></head></html>`
		found, ok := mediaPageOpenGraph(mustParseDoc(t, page), base)
		if !ok {
			t.Fatal("nothing was read from the sharing metadata")
		}
		if want := "https://example.test/v/clip.mp4"; found.URL != want {
			t.Errorf("read %q, want %q", found.URL, want)
		}
	})
}

// TestMediaPageTwitterStreamIgnoresThePlayerPage pins which of the two card
// properties is media: twitter:player is the page the card frames.
func TestMediaPageTwitterStreamIgnoresThePlayerPage(t *testing.T) {
	const page = `<html><head>
<meta name="twitter:player" content="https://example.test/embed/a-clip">
<meta name="twitter:player:stream" content="https://cdn.example.test/v/card.mp4">
<meta name="twitter:player:stream:content_type" content="video/mp4">
</head></html>`

	base := mustParse(t, "https://example.test/watch/a-clip")
	found, ok := mediaPageTwitterStream(mustParseDoc(t, page), base)
	if !ok {
		t.Fatal("the card's stream was not read")
	}
	if !strings.HasSuffix(found.URL, "/card.mp4") {
		t.Errorf("read %q, want the stream rather than the player page", found.URL)
	}
	if found.Type != "video/mp4" {
		t.Errorf("type = %q, want the declared one", found.Type)
	}
}

// TestMediaPageLinkedData covers the three shapes publishers emit, each
// carrying the trap that makes the type check necessary: an ImageObject with
// a contentUrl of its own, listed before the video.
func TestMediaPageLinkedData(t *testing.T) {
	const graph = `<html><head><script type="application/ld+json">
{"@context":"https://schema.org","@graph":[
 {"@type":"ImageObject","contentUrl":"https://cdn.example.test/img/lead.jpg","height":800},
 {"@type":"NewsArticle","headline":"Something happened"},
 {"@type":"VideoObject","name":"The clip","height":720,
  "contentUrl":"https://cdn.example.test/v/graph.mp4","encodingFormat":"video/mp4"}
]}
</script></head></html>`

	const array = `<html><head><script type="application/ld+json">
[{"@type":"ImageObject","contentUrl":"https://cdn.example.test/img/lead.jpg"},
 {"@type":["VideoObject"],"contentUrl":"https://cdn.example.test/v/array.mp4","height":720}]
</script></head></html>`

	// "image" sorts before "video", so the walk meets the photograph first:
	// only the type keeps it from being taken.
	const nested = `<html><head><script type="application/ld+json">
{"@type":"NewsArticle",
 "image":{"@type":"ImageObject","contentUrl":"https://cdn.example.test/img/lead.jpg"},
 "video":{"@type":"VideoObject","contentUrl":"https://cdn.example.test/v/nested.mp4","height":"720"}}
</script></head></html>`

	base := mustParse(t, "https://example.test/news/something-happened")
	tests := map[string]string{
		"@graph": graph,
		"array":  array,
		"nested": nested,
	}
	for name, page := range tests {
		t.Run(name, func(t *testing.T) {
			found, ok := mediaPageLinkedData(mustParseDoc(t, page), base)
			if !ok {
				t.Fatal("no media object was found")
			}
			if strings.HasSuffix(found.URL, ".jpg") {
				t.Fatalf("resolved to the article's photograph: %s", found.URL)
			}
			if found.Quality != 720 {
				t.Errorf("quality = %d, want the stated height (which one shape writes as a string)", found.Quality)
			}
		})
	}

	t.Run("a video object naming a page is declined", func(t *testing.T) {
		const page = `<html><head><script type="application/ld+json">
{"@type":"VideoObject","contentUrl":"https://example.test/embed/a-clip","encodingFormat":"text/html"}
</script></head></html>`
		if found, ok := mediaPageLinkedData(mustParseDoc(t, page), base); ok {
			t.Errorf("took %q, which the document calls a web page", found.URL)
		}
	})

	t.Run("markup that is not JSON is stepped over", func(t *testing.T) {
		const page = `<html><head><script type="application/ld+json">{ this is not json </script></head></html>`
		if _, ok := mediaPageLinkedData(mustParseDoc(t, page), base); ok {
			t.Error("a broken document produced a finding")
		}
	})
}

// TestMediaPageTitle pins where the name comes from and that the site's own
// name comes off it either way. The third case is why og:title is not exempt
// from the trimming: it is what Wikipedia writes into that property.
func TestMediaPageTitle(t *testing.T) {
	tests := map[string]string{
		mediaPagePlayerHTML: "An interview with the director",

		// No og:title: the document's own is read, and the pipe is cut before
		// the dash, so a dash inside the title survives.
		`<html><head><title>Another interview - part two | Example Tube</title></head></html>`: "Another interview - part two",

		// og:title carrying the site's name, exactly as observed.
		`<html><head><meta property="og:title" content="Big Buck Bunny - Wikipedia"></head></html>`: "Big Buck Bunny",

		// Nothing to read, so the caller falls back to the URL.
		`<html><body>nothing</body></html>`: "",
	}
	for doc, want := range tests {
		if got := mediaPageTitle(mustParseDoc(t, doc), doc); got != want {
			t.Errorf("title = %q, want %q\n  from %s", got, want, doc)
		}
	}
}

func TestMediaPageName(t *testing.T) {
	tests := []struct {
		name  string
		found mediaFinding
		want  string
	}{
		{
			name:  "the URL names the file",
			found: mediaFinding{mediaCandidate: mediaCandidate{URL: "https://cdn.example.test/v/clip.webm"}},
			want:  "A clip.webm",
		},
		{
			name:  "a query does not become the extension",
			found: mediaFinding{mediaCandidate: mediaCandidate{URL: "https://cdn.example.test/v/clip.mp4?e=1&hash=SIGNATURE"}},
			want:  "A clip.mp4",
		},
		{
			name:  "a signed link with no extension takes the declared type",
			found: mediaFinding{mediaCandidate: mediaCandidate{URL: "https://cdn.example.test/get/abc123"}, Type: "video/webm"},
			want:  "A clip.webm",
		},
		{
			name:  "a dot that is not an extension is left alone",
			found: mediaFinding{mediaCandidate: mediaCandidate{URL: "https://cdn.example.test/v1.2/stream"}},
			want:  "A clip.mp4",
		},
		{
			name:  "a playlist is saved as the stream it concatenates into",
			found: mediaFinding{mediaCandidate: mediaCandidate{URL: "https://cdn.example.test/hls/master.m3u8", IsHLS: true}},
			want:  "A clip.ts",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := mediaPageName("A clip", tc.found); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// --- jw player ---------------------------------------------------------------

// TestJWPlayerEmbedFindsTheMediaID covers every shape an embed takes, and the
// near misses that must not be read as one. The ids are invented; what is
// pinned is the shape around them.
func TestJWPlayerEmbedFindsTheMediaID(t *testing.T) {
	tests := map[string]string{
		`<script src="https://cdn.jwplayer.com/players/AbCd1234-XyZw9876.js"></script>`:                        "AbCd1234",
		`<script src="//content.jwplatform.com/players/AbCd1234-XyZw9876.js"></script>`:                        "AbCd1234",
		`<iframe src="https://content.jwplatform.com/players/AbCd1234"></iframe>`:                              "AbCd1234",
		`jwplayer("p").setup({playlist:"https://cdn.jwplayer.com/v2/media/AbCd1234"});`:                        "AbCd1234",
		`<meta property="og:image" content="https://cdn.jwplayer.com/v2/media/AbCd1234/poster.jpg?width=720">`: "AbCd1234",
		`<video><source src="https://cdn.jwplayer.com/manifests/AbCd1234.m3u8"></video>`:                       "AbCd1234",

		// A longer run of characters is not an id with a tail; it is
		// something else, and truncating it would invent a plausible id.
		`<script src="https://cdn.jwplayer.com/players/AbCd12345-XyZw9876.js"></script>`: "",
		`<script src="https://cdn.jwplayer.com/players/AbCd12.js"></script>`:             "",
		// Another host laid out the same way is another host.
		`<script src="https://cdn.example.test/players/AbCd1234-XyZw9876.js"></script>`: "",
	}
	for doc, want := range tests {
		got := ""
		if m := jwPlayerEmbed.FindStringSubmatch(doc); m != nil {
			got = m[1]
		}
		if got != want {
			t.Errorf("%s\n  found %q, want %q", doc, got, want)
		}
	}
}

// jwPlayerFeedJSON is the shape the media endpoint answers with, kept in the
// order the live one uses: the manifests first, the MP4s ascending, and an
// audio-only rendition last. Both traps are here — the DASH manifest this
// cannot assemble, and the audio track that carries no height and would win a
// naive ranking.
const jwPlayerFeedJSON = `{
 "title":"Feed title",
 "playlist":[{
  "title":"The film",
  "mediaid":"AbCd1234",
  "sources":[
   {"file":"https://cdn.example.test/manifests/AbCd1234.mpd","type":"application/dash+xml"},
   {"file":"https://cdn.example.test/manifests/AbCd1234.m3u8","type":"application/vnd.apple.mpegurl"},
   {"file":"https://cdn.example.test/videos/AbCd1234-low.mp4","type":"video/mp4","height":180,"width":320,"label":"H.264 320px","filesize":25195664},
   {"file":"https://cdn.example.test/videos/AbCd1234-high.mp4","type":"video/mp4","height":1080,"width":1920,"label":"H.264 1920px","filesize":251956640},
   {"file":"https://cdn.example.test/videos/AbCd1234-audio.m4a","type":"audio/mp4","label":"AAC Audio","filesize":8577303}
  ]
 }]
}`

func TestJWPlayerBestRanksVideoByHeight(t *testing.T) {
	found, ok := jwPlayerBest(decodeJWFeed(t, jwPlayerFeedJSON))
	if !ok {
		t.Fatal("no rendition was chosen")
	}
	if !strings.HasSuffix(found.URL, "-high.mp4") {
		t.Errorf("chose %q, want the tallest MP4", found.URL)
	}
	if found.Size != 251956640 {
		t.Errorf("size = %d, want the byte count the host states exactly", found.Size)
	}
	if found.Title != "The film" {
		t.Errorf("title = %q, want the media's own", found.Title)
	}
	if found.IsHLS {
		t.Error("the chosen file was taken for a playlist")
	}
}

// TestJWPlayerBestFallsBackThroughTheKinds pins the two orderings that matter:
// a playlist is only taken when no file is offered, and the audio track only
// when there is no video at all — otherwise a film resolves to its soundtrack.
func TestJWPlayerBestFallsBackThroughTheKinds(t *testing.T) {
	t.Run("no MP4 leaves the playlist", func(t *testing.T) {
		const feed = `{"playlist":[{"title":"The film","sources":[
   {"file":"https://cdn.example.test/manifests/AbCd1234.mpd","type":"application/dash+xml"},
   {"file":"https://cdn.example.test/manifests/AbCd1234.m3u8","type":"application/vnd.apple.mpegurl"},
   {"file":"https://cdn.example.test/videos/AbCd1234-audio.m4a","type":"audio/mp4"}]}]}`

		found, ok := jwPlayerBest(decodeJWFeed(t, feed))
		if !ok {
			t.Fatal("no rendition was chosen")
		}
		if !found.IsHLS || !strings.HasSuffix(found.URL, ".m3u8") {
			t.Errorf("chose %q, want the playlist rather than the soundtrack or the .mpd", found.URL)
		}
	})

	t.Run("audio only is still a download", func(t *testing.T) {
		const feed = `{"playlist":[{"title":"An episode","sources":[
   {"file":"https://cdn.example.test/manifests/AbCd1234.mpd","type":"application/dash+xml"},
   {"file":"https://cdn.example.test/videos/AbCd1234-audio.m4a","type":"audio/mp4"}]}]}`

		found, ok := jwPlayerBest(decodeJWFeed(t, feed))
		if !ok {
			t.Fatal("a media with only audio resolved to nothing")
		}
		if !strings.HasSuffix(found.URL, ".m4a") {
			t.Errorf("chose %q, want the audio", found.URL)
		}
	})

	t.Run("nothing usable", func(t *testing.T) {
		const feed = `{"playlist":[{"sources":[
   {"file":"https://cdn.example.test/manifests/AbCd1234.mpd","type":"application/dash+xml"}]}]}`
		if _, ok := jwPlayerBest(decodeJWFeed(t, feed)); ok {
			t.Error("a media offering only DASH produced a finding")
		}
	})

	t.Run("an empty feed", func(t *testing.T) {
		if _, ok := jwPlayerBest(decodeJWFeed(t, `{"playlist":[]}`)); ok {
			t.Error("an empty feed produced a finding")
		}
	})
}

func decodeJWFeed(t *testing.T, doc string) *jwPlayerFeed {
	t.Helper()
	var feed jwPlayerFeed
	if err := json.Unmarshal([]byte(doc), &feed); err != nil {
		t.Fatal(err)
	}
	return &feed
}

// --- the sniff end to end ----------------------------------------------------

// mediaPageServer serves the fixtures a sniff walks through, and counts what
// was asked for so a test can pin what was *not*.
func mediaPageServer(t *testing.T, pages map[string]mediaPageResponse) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		page, found := pages[r.URL.Path]
		if !found {
			http.NotFound(w, r)
			return
		}
		w.Header().Set(httpx.HeaderContentType, page.kind)
		_, _ = w.Write([]byte(page.body))
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

type mediaPageResponse struct {
	kind string
	body string
}

func mediaPageClient() *httpx.Client {
	return httpx.New("test-agent", "en-US", 0, 5*time.Second)
}

func TestMediaPageSniffResolvesAPage(t *testing.T) {
	srv, _ := mediaPageServer(t, map[string]mediaPageResponse{
		"/watch/an-interview": {kind: "text/html; charset=utf-8", body: mediaPagePlayerHTML},
	})

	res, ok := mediaPageSniff(context.Background(), mediaPageClient(),
		mustParse(t, srv.URL+"/watch/an-interview"))
	if !ok {
		t.Fatal("a page with a player on it was not recognised")
	}
	if res.Title != "An interview with the director" {
		t.Errorf("title = %q", res.Title)
	}
	if len(res.Files) != 1 {
		t.Fatalf("found %d files, want one", len(res.Files))
	}
	file := res.Files[0]
	if file.Name != "An interview with the director.mp4" {
		t.Errorf("name = %q", file.Name)
	}
	if want := srv.URL + "/media/clip_1920x1080_4000k.mp4"; file.URL != want {
		t.Errorf("url = %q, want %q", file.URL, want)
	}
	if file.Size != -1 {
		t.Errorf("size = %d, want -1: nothing stated a length", file.Size)
	}
	if got := file.Headers[httpx.HeaderReferer]; got != srv.URL+"/watch/an-interview" {
		t.Errorf("referer = %q, want the page the link came off", got)
	}
}

// TestMediaPageSniffLeavesAURLThatNamesAFile is the first guard, and the
// counter is the assertion that matters: a file must not be fetched at all to
// find out it is one.
func TestMediaPageSniffLeavesAURLThatNamesAFile(t *testing.T) {
	srv, hits := mediaPageServer(t, map[string]mediaPageResponse{
		"/downloads/clip.mp4": {kind: "text/html; charset=utf-8", body: mediaPagePlayerHTML},
	})

	if _, ok := mediaPageSniff(context.Background(), mediaPageClient(),
		mustParse(t, srv.URL+"/downloads/clip.mp4")); ok {
		t.Error("a URL naming a file was sniffed as a page")
	}
	if n := hits.Load(); n != 0 {
		t.Errorf("made %d requests, want none: the URL alone settles this", n)
	}
}

// TestMediaPageSniffLeavesWhatIsNotServedAsHTML is the second guard. The body
// here is a page, and it is still not read, because the host said it was
// sending a video — which is what an extensionless media link looks like.
func TestMediaPageSniffLeavesWhatIsNotServedAsHTML(t *testing.T) {
	srv, _ := mediaPageServer(t, map[string]mediaPageResponse{
		"/get/abc123": {kind: "video/mp4", body: mediaPagePlayerHTML},
	})

	if _, ok := mediaPageSniff(context.Background(), mediaPageClient(),
		mustParse(t, srv.URL+"/get/abc123")); ok {
		t.Error("a response the host called a video was parsed as a page")
	}
}

// TestMediaPageSniffFallsThroughQuietly covers the contract the caller
// depends on: a page carrying no media is not an error, it is a no.
func TestMediaPageSniffFallsThroughQuietly(t *testing.T) {
	srv, _ := mediaPageServer(t, map[string]mediaPageResponse{
		"/about": {kind: "text/html", body: `<html><head><title>About us</title></head>
<body><picture><source srcset="/img/team.jpg"></picture><p>Nothing to download here.</p></body></html>`},
		"/gone": {kind: "text/html", body: "<html><body>Not found</body></html>"},
	})

	for _, path := range []string{"/about", "/missing", "/gone"} {
		if res, ok := mediaPageSniff(context.Background(), mediaPageClient(),
			mustParse(t, srv.URL+path)); ok {
			t.Errorf("%s produced %+v, want a quiet no", path, res)
		}
	}
}

// TestMediaPageSniffResolvesAPlaylist covers the other half of the contract:
// an adaptive stream has no file to range over, so it arrives as segments.
func TestMediaPageSniffResolvesAPlaylist(t *testing.T) {
	const page = `<html><head><meta property="og:title" content="A live set"></head><body>
<video controls>
  <source src="/hls/master.m3u8" type="application/vnd.apple.mpegurl">
</video></body></html>`

	const master = `#EXTM3U
#EXT-X-STREAM-INF:BANDWIDTH=800000,RESOLUTION=1280x720,CODECS="avc1.64001F,mp4a.40.2"
/hls/720/media.m3u8
`
	const media = `#EXTM3U
#EXTINF:6.0,
/hls/720/1.ts
#EXTINF:6.0,
/hls/720/2.ts
#EXT-X-ENDLIST
`

	srv, _ := mediaPageServer(t, map[string]mediaPageResponse{
		"/watch/a-live-set":   {kind: "text/html", body: page},
		"/hls/master.m3u8":    {kind: "application/vnd.apple.mpegurl", body: master},
		"/hls/720/media.m3u8": {kind: "application/vnd.apple.mpegurl", body: media},
	})

	res, ok := mediaPageSniff(context.Background(), mediaPageClient(),
		mustParse(t, srv.URL+"/watch/a-live-set"))
	if !ok {
		t.Fatal("a page offering a playlist was not recognised")
	}
	file := res.Files[0]
	if file.URL != "" {
		t.Errorf("url = %q, want empty: a playlist has no single file to fetch", file.URL)
	}
	if file.Name != "A live set.ts" {
		t.Errorf("name = %q", file.Name)
	}
	if len(file.Segments) != 2 {
		t.Fatalf("collected %d segments, want the playlist's two: %v", len(file.Segments), file.Segments)
	}
	if file.Segments[0] != srv.URL+"/hls/720/1.ts" {
		t.Errorf("first segment = %q", file.Segments[0])
	}
}

// TestMediaPageSniffThroughJWPlayer is the whole JW path: a page with no
// player markup at all, one embed reference in a script tag, and the media
// endpoint answering for it.
func TestMediaPageSniffThroughJWPlayer(t *testing.T) {
	const page = `<html><head><title>A talk | Example Conference</title>
<meta property="og:video" content="https://example.test/embed/AbCd1234">
<meta property="og:video:type" content="text/html">
</head><body>
<div id="player"></div>
<script src="https://cdn.jwplayer.com/players/AbCd1234-XyZw9876.js"></script>
</body></html>`

	srv, _ := mediaPageServer(t, map[string]mediaPageResponse{
		"/talks/a-talk":      {kind: "text/html", body: page},
		"/v2/media/AbCd1234": {kind: "application/json", body: jwPlayerFeedJSON},
	})

	restore := jwPlayerMediaAPI
	jwPlayerMediaAPI = srv.URL + "/v2/media/"
	t.Cleanup(func() { jwPlayerMediaAPI = restore })

	res, ok := mediaPageSniff(context.Background(), mediaPageClient(),
		mustParse(t, srv.URL+"/talks/a-talk"))
	if !ok {
		t.Fatal("a page embedding the player was not resolved")
	}
	file := res.Files[0]
	if !strings.HasSuffix(file.URL, "-high.mp4") {
		t.Errorf("url = %q, want the tallest MP4 the media offers", file.URL)
	}
	if file.Size != 251956640 {
		t.Errorf("size = %d, want the exact length the host stated", file.Size)
	}
	if res.Title != "A talk" {
		t.Errorf("title = %q, want the page's own title trimmed of the site", res.Title)
	}
}

// TestDirectExtractSniffsAPage pins the wiring rather than the parsing: the
// fallback extractor has to reach for this before it treats a URL as a file,
// or the whole point is lost — the page would land on disk under its own name
// and be recorded as a finished download.
func TestDirectExtractSniffsAPage(t *testing.T) {
	srv, _ := mediaPageServer(t, map[string]mediaPageResponse{
		"/watch/an-interview": {kind: "text/html; charset=utf-8", body: mediaPagePlayerHTML},
	})

	res, err := NewDirect(mediaPageClient()).Extract(context.Background(),
		mustParse(t, srv.URL+"/watch/an-interview"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if got := res.Files[0].URL; got != srv.URL+"/media/clip_1920x1080_4000k.mp4" {
		t.Errorf("the fallback resolved to %q, want the page's own rendition", got)
	}
}

// TestDirectExtractStillWrapsAFile is the other half of that contract: what
// the sniff declines has to come out exactly as it did before, since every
// ordinary direct link goes through the same path.
func TestDirectExtractStillWrapsAFile(t *testing.T) {
	res, err := NewDirect(mediaPageClient()).Extract(context.Background(),
		mustParse(t, "https://cdn.example.test/files/archive.zip"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Files) != 1 || res.Files[0].URL != "https://cdn.example.test/files/archive.zip" {
		t.Fatalf("a plain file came out as %+v", res.Files)
	}
	if res.Files[0].Name != "archive.zip" {
		t.Errorf("name = %q", res.Files[0].Name)
	}
}
