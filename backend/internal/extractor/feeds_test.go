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

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

// A feed shaped like a real podcast's, cut down to the parts the extractor
// reads and carrying the traps that decide whether it reads them properly:
//
//   - the newest item's enclosure is wrapped by an analytics redirector, so
//     the publisher's own filename is the last path segment and the tracker's
//     "redirect.mp3" is the first;
//   - that item also carries a <media:content>, which is a near-identical
//     decoy with a url, a type and a size of its own;
//   - a title with a slash in it, which is prose rather than a path;
//   - an item with no enclosure at all, which is an article;
//   - an enclosure whose length is "0" and one with no length attribute;
//   - a file that is plainly .m4a declared as audio/mpeg, which is the
//     mistake publishing tools make most;
//   - two episodes sharing a title, and an item with no title at all;
//   - a bare &nbsp; and an unescaped ampersand, both fatal to a strict parser.
//
// Items are newest first, as feeds are.
const feedRSSFixture = `<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0" xmlns:itunes="http://www.itunes.com/dtds/podcast-1.0.dtd" xmlns:media="http://search.yahoo.com/mrss/">
<channel>
  <title>The Example Show</title>
  <link>https://show.example.test/</link>
  <image><title>The Example Show artwork</title><url>https://show.example.test/art.png</url></image>
  <item>
    <title>S2E4: Vinyl / Digital</title>
    <description>Under &pound;5 &amp; fun&nbsp;now, R&D too</description>
    <media:content url="https://media.example.test/previews/s2e4-clip.mp3" type="audio/mpeg" fileSize="99"/>
    <enclosure url="https://analytics.example.test/redirect.mp3/tracker.example.test/e/media.example.test/episodes/s2e4" length="12345678" type="audio/mpeg"/>
  </item>
  <item>
    <title>Interlude</title>
    <enclosure url="https://media.example.test/episodes/interlude.m4a" length="0" type="audio/mpeg"/>
  </item>
  <item>
    <title>An article, nothing attached</title>
    <link>https://show.example.test/notes</link>
  </item>
  <item>
    <title>Bonus</title>
    <enclosure url="https://media.example.test/episodes/bonus-2.mp3" type="audio/mpeg"/>
  </item>
  <item>
    <title>Bonus</title>
    <enclosure url="https://media.example.test/episodes/bonus-1.mp3" length="7654321" type="audio/mpeg"/>
  </item>
  <item>
    <enclosure url="https://media.example.test/episodes/pilot.mp3?updated=1699" length="555" type="audio/mpeg"/>
  </item>
</channel>
</rss>`

// An Atom feed with the relation trap: an entry's links are mostly the
// article, and one with no rel at all means "alternate" rather than nothing.
// The oldest entry writes the relation as its full IANA URI, which Atom
// permits and a few generators use.
const feedAtomFixture = `<?xml version="1.0" encoding="utf-8"?>
<feed xmlns="http://www.w3.org/2005/Atom">
  <title type="text">Example Atom Cast</title>
  <updated>2026-01-02T03:04:05Z</updated>
  <entry>
    <title>Second &amp; last</title>
    <link rel="alternate" href="https://show.example.test/2"/>
    <link href="https://show.example.test/2/transcript"/>
    <link rel="enclosure" type="audio/ogg" length="4096" href="https://media.example.test/atom/two.ogg"/>
  </entry>
  <entry>
    <title>Only a page</title>
    <link href="https://show.example.test/1"/>
  </entry>
  <entry>
    <title>First</title>
    <link rel="http://www.iana.org/assignments/relation/enclosure" type="audio/mpeg" length="0" href="https://media.example.test/atom/one"/>
  </entry>
</feed>`

func feedTestURL(t *testing.T) *url.URL {
	t.Helper()
	u, err := url.Parse("https://feeds.example.test/the-example-show")
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func TestFeedRSS(t *testing.T) {
	res, err := feedResult([]byte(feedRSSFixture), feedTestURL(t))
	if err != nil {
		t.Fatalf("feedResult: %v", err)
	}
	if res.Title != "The Example Show" {
		t.Errorf("title = %q, want the channel title (not the artwork's)", res.Title)
	}

	// Five enclosures from six items, oldest first: the article is skipped
	// and the newest episode is last.
	want := []struct {
		name string
		size int64
		url  string
	}{
		{"pilot.mp3", 555, "https://media.example.test/episodes/pilot.mp3?updated=1699"},
		{"Bonus.mp3", 7654321, "https://media.example.test/episodes/bonus-1.mp3"},
		{"Bonus (2).mp3", -1, "https://media.example.test/episodes/bonus-2.mp3"},
		{"Interlude.m4a", -1, "https://media.example.test/episodes/interlude.m4a"},
		{"S2E4: Vinyl - Digital.mp3", 12345678,
			"https://analytics.example.test/redirect.mp3/tracker.example.test/e/media.example.test/episodes/s2e4"},
	}
	if len(res.Files) != len(want) {
		t.Fatalf("found %d files, want %d: %+v", len(res.Files), len(want), res.Files)
	}
	for i, w := range want {
		got := res.Files[i]
		if got.Name != w.name {
			t.Errorf("file %d name = %q, want %q", i, got.Name, w.name)
		}
		if got.Size != w.size {
			t.Errorf("file %d size = %d, want %d", i, got.Size, w.size)
		}
		if got.URL != w.url {
			t.Errorf("file %d url = %q, want %q", i, got.URL, w.url)
		}
		// A length attribute is a byte count, never a rounded display
		// figure, so nothing here may be marked approximate — that is what
		// lets an episode already on disk be skipped before connecting.
		if got.SizeApprox {
			t.Errorf("file %d is marked approximate; an enclosure length is exact", i)
		}
	}

	// The decoy in the newest item states a size of its own and would have
	// been queued as a sixth file had <media:content> been read as an
	// enclosure.
	for _, f := range res.Files {
		if strings.Contains(f.URL, "/previews/") {
			t.Errorf("the media:content decoy was queued: %s", f.URL)
		}
		if strings.HasPrefix(f.Name, "redirect.") {
			t.Errorf("file named after the analytics prefix: %q", f.Name)
		}
	}
}

func TestFeedAtom(t *testing.T) {
	res, err := feedResult([]byte(feedAtomFixture), feedTestURL(t))
	if err != nil {
		t.Fatalf("feedResult: %v", err)
	}
	if res.Title != "Example Atom Cast" {
		t.Errorf("title = %q, want the feed title", res.Title)
	}
	if len(res.Files) != 2 {
		t.Fatalf("found %d files, want 2 (an alternate link is a web page, not an attachment): %+v",
			len(res.Files), res.Files)
	}

	// Oldest first, and the oldest declared its relation as the IANA URI.
	first := res.Files[0]
	if first.Name != "First.mp3" {
		t.Errorf("first name = %q, want the title plus the extension the type implies", first.Name)
	}
	if first.Size != -1 {
		t.Errorf("first size = %d, want -1: a length of 0 is no length at all", first.Size)
	}
	second := res.Files[1]
	if second.Name != "Second & last.ogg" {
		t.Errorf("second name = %q, want the decoded title and the .ogg the type implies", second.Name)
	}
	if second.Size != 4096 {
		t.Errorf("second size = %d, want the exact length 4096", second.Size)
	}
	for _, f := range res.Files {
		if strings.Contains(f.URL, "show.example.test") {
			t.Errorf("an article link was queued as an enclosure: %s", f.URL)
		}
	}
}

// TestFeedReversesAndCaps pins both halves of what a long-running show needs:
// the cut is made at the newest end of the feed, and what survives it is
// ordered oldest first.
func TestFeedReversesAndCaps(t *testing.T) {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0"?><rss version="2.0"><channel><title>Long</title>`)
	total := config.MaxListingFiles + 100
	for i := range total {
		fmt.Fprintf(&b, `<item><title>Ep %d</title>`+
			`<enclosure url="https://media.example.test/e/%d.mp3" length="%d" type="audio/mpeg"/></item>`,
			i, i, i+1)
	}
	b.WriteString(`</channel></rss>`)

	res, err := feedResult([]byte(b.String()), feedTestURL(t))
	if err != nil {
		t.Fatalf("feedResult: %v", err)
	}
	if len(res.Files) != config.MaxListingFiles {
		t.Fatalf("kept %d files, want the cap of %d", len(res.Files), config.MaxListingFiles)
	}
	// Item 0 is the newest, so the kept window is items 0..cap-1 and, once
	// reversed, it starts at the oldest of those and ends at the newest.
	if got, want := res.Files[0].Name, fmt.Sprintf("Ep %d.mp3", config.MaxListingFiles-1); got != want {
		t.Errorf("first file = %q, want %q — the oldest episode that survived the cap", got, want)
	}
	if got := res.Files[len(res.Files)-1].Name; got != "Ep 0.mp3" {
		t.Errorf("last file = %q, want the newest episode", got)
	}
}

// TestFeedDeclaredCharset covers the older feeds that still announce a
// single-byte encoding: encoding/xml refuses those outright unless it is
// handed a reader for them, so without feedCharset this parses to nothing.
func TestFeedDeclaredCharset(t *testing.T) {
	doc := "<?xml version=\"1.0\" encoding=\"ISO-8859-1\"?>" +
		"<rss version=\"2.0\"><channel><title>Caf\xe9 Radio</title>" +
		"<item><title>Cr\xe8me</title>" +
		`<enclosure url="https://media.example.test/e/1.mp3" length="9" type="audio/mpeg"/>` +
		"</item></channel></rss>"

	res, err := feedResult([]byte(doc), feedTestURL(t))
	if err != nil {
		t.Fatalf("feedResult: %v", err)
	}
	if res.Title != "Café Radio" {
		t.Errorf("title = %q, want the latin-1 title decoded", res.Title)
	}
	if res.Files[0].Name != "Crème.mp3" {
		t.Errorf("name = %q, want the latin-1 title decoded", res.Files[0].Name)
	}
}

// TestFeedTolerantParsing is why the decoder is not strict: every one of
// these is a hard error to a strict parser, and none is anywhere near an
// enclosure.
func TestFeedTolerantParsing(t *testing.T) {
	doc := `<?xml version="1.0"?><rss version="2.0"><channel><title>Messy</title>` +
		`<item><title>Fine &amp; dandy</title>` +
		`<description>Half <b>bold, 100% &nbsp; raw & loose</description>` +
		`<enclosure url="https://media.example.test/e/1.mp3" length="9" type="audio/mpeg"/>` +
		`</item></channel></rss>`

	res, err := feedResult([]byte(doc), feedTestURL(t))
	if err != nil {
		t.Fatalf("feedResult: %v", err)
	}
	if len(res.Files) != 1 || res.Files[0].Name != "Fine & dandy.mp3" {
		t.Fatalf("got %+v, want one file named after the item", res.Files)
	}
}

// TestFeedTruncatedDocument covers a fetch that ended early. The decoder
// fills the document as it goes and an enclosure is one self-closed element,
// so what was recovered is whole entries — worth keeping rather than turning
// a dropped connection into no download at all.
func TestFeedTruncatedDocument(t *testing.T) {
	cut := strings.Index(feedRSSFixture, "<title>Bonus</title>")
	if cut < 0 {
		t.Fatal("fixture no longer contains the item the cut is made at")
	}
	res, err := feedResult([]byte(feedRSSFixture[:cut]), feedTestURL(t))
	if err != nil {
		t.Fatalf("feedResult: %v", err)
	}
	if len(res.Files) != 2 {
		t.Errorf("recovered %d files, want the 2 that arrived in full: %+v", len(res.Files), res.Files)
	}
}

func TestFeedUnreadableDocumentIsAnError(t *testing.T) {
	if _, err := feedResult([]byte(`<?xml version="1.0"?><rss><chan`), feedTestURL(t)); err == nil {
		t.Fatal("a document that yielded nothing resolved to something")
	}
}

// TestFeedWithoutEnclosuresIsAnError covers an ordinary blog feed, which
// links to pages rather than attaching files.
func TestFeedWithoutEnclosuresIsAnError(t *testing.T) {
	doc := `<?xml version="1.0"?><rss version="2.0"><channel><title>Blog</title>` +
		`<item><title>A post</title><link>https://blog.example.test/1</link></item>` +
		`</channel></rss>`
	if _, err := feedResult([]byte(doc), feedTestURL(t)); err == nil {
		t.Fatal("an article feed resolved to something downloadable")
	}
}

// TestFeedish pins the recognition, which decides between reading a document
// and handing the URL to the fallback extractor as a plain file.
func TestFeedish(t *testing.T) {
	sitemap := `<?xml version="1.0" encoding="UTF-8"?><urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">` +
		`<url><loc>https://example.test/</loc></url></urlset>`
	page := `<!DOCTYPE html><html><head><title>Subscribe</title>` +
		`<link rel="alternate" type="application/rss+xml" href="/feed"></head><body>x</body></html>`

	tests := []struct {
		name        string
		contentType string
		body        string
		want        bool
	}{
		{"rss served as text/xml", "text/xml; charset=utf-8", feedRSSFixture, true},
		{"atom served as octet-stream", "application/octet-stream", feedAtomFixture, true},
		{"rss with no prolog", "", `<rss version="2.0"><channel><title>x</title></channel></rss>`, true},
		// The type settles it even when the body does not: a host that says
		// RSS and serves rubbish should fail loudly rather than have its
		// rubbish quietly downloaded as a file.
		{"declared rss, served junk", "application/rss+xml", "not xml at all", true},
		// Both of these are perfectly good XML that is not a feed, which is
		// the whole reason the body is looked at.
		{"sitemap", "application/xml", sitemap, false},
		// A page advertising a feed mentions the type in an attribute; only
		// the root element tells the two apart.
		{"page linking to a feed", "text/html; charset=utf-8", page, false},
		{"an actual media file", "audio/mpeg", "\x00\x01ID3", false},
	}
	for _, tt := range tests {
		if got := feedish(tt.contentType, []byte(tt.body)); got != tt.want {
			t.Errorf("%s: feedish = %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestFeedExtension(t *testing.T) {
	tests := []struct {
		base, mediaType, want string
	}{
		// A wrapped URL ends in a bare id, so the declared type is all there
		// is to go on.
		{"s2e4", "audio/mpeg", ".mp3"},
		{"s2e4", "audio/mpeg; charset=binary", ".mp3"},
		// The URL wins over the type, because the type is the field
		// publishers are careless with.
		{"s2e4.m4a", "audio/mpeg", ".m4a"},
		{"clip.mp3", "video/mp4", ".mp3"},
		// A suffix that is not an extension is dropped, not carried.
		{"episode.php", "audio/mpeg", ".mp3"},
		{"1234.5678", "", ""},
		{"download", "application/octet-stream", ""},
		// Unusual but plausible suffixes are kept, since no table will name
		// every format a publisher attaches.
		{"talk.spx", "", ".spx"},
		{"", "audio/x-m4a", ".m4a"},
	}
	for _, tt := range tests {
		if got := feedExtension(tt.base, tt.mediaType); got != tt.want {
			t.Errorf("feedExtension(%q, %q) = %q, want %q", tt.base, tt.mediaType, got, tt.want)
		}
	}
}

func TestFeedLength(t *testing.T) {
	cases := map[string]int64{
		"12345678": 12345678,
		" 42 ":     42,
		"0":        -1,
		"":         -1,
		"unknown":  -1,
		"-3":       -1,
	}
	for raw, want := range cases {
		if got := feedLength(raw); got != want {
			t.Errorf("feedLength(%q) = %d, want %d", raw, got, want)
		}
	}
}

func TestFeedIsEnclosure(t *testing.T) {
	cases := map[string]bool{
		"enclosure":   true,
		"ENCLOSURE":   true,
		" enclosure ": true,
		"http://www.iana.org/assignments/relation/enclosure": true,
		// An Atom link with no rel means alternate: the article's own page.
		"":          false,
		"alternate": false,
		"self":      false,
		"related":   false,
	}
	for rel, want := range cases {
		if got := feedIsEnclosure(rel); got != want {
			t.Errorf("feedIsEnclosure(%q) = %v, want %v", rel, got, want)
		}
	}
}

// feedServer serves one document, and records what was asked for.
func feedServer(t *testing.T, contentType, body string) (*httptest.Server, *string) {
	t.Helper()
	var accept string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		accept = r.Header.Get(httpx.HeaderAccept)
		if contentType != "" {
			w.Header().Set(httpx.HeaderContentType, contentType)
		}
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv, &accept
}

func testFeeds(t *testing.T) *Feeds {
	t.Helper()
	return NewFeeds(httpx.New("test-agent", "en-US", 0, 5*time.Second))
}

func TestFeedsExtract(t *testing.T) {
	srv, accept := feedServer(t, "application/xml; charset=utf-8", feedRSSFixture)
	u, err := ParseURL(srv.URL + "/feed.xml")
	if err != nil {
		t.Fatal(err)
	}

	res, err := testFeeds(t).Extract(context.Background(), u, Options{})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if res.Title != "The Example Show" || len(res.Files) != 5 {
		t.Errorf("got %q with %d files, want the show and its 5 enclosures", res.Title, len(res.Files))
	}
	if !strings.Contains(*accept, "application/rss+xml") {
		t.Errorf("Accept was %q, want a feed asked for by name", *accept)
	}
}

// TestFeedsExtractFallsBackToTheFile covers the cost of matching on the shape
// of a URL: a .xml that is not a feed must still download as the file it is,
// exactly as it would have had this extractor never matched.
func TestFeedsExtractFallsBackToTheFile(t *testing.T) {
	const sitemap = `<?xml version="1.0" encoding="UTF-8"?>` +
		`<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">` +
		`<url><loc>https://example.test/</loc></url></urlset>`
	srv, _ := feedServer(t, "application/xml", sitemap)
	u, err := ParseURL(srv.URL + "/sitemap.xml")
	if err != nil {
		t.Fatal(err)
	}

	res, err := testFeeds(t).Extract(context.Background(), u, Options{})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if len(res.Files) != 1 {
		t.Fatalf("got %d files, want the one file the URL names", len(res.Files))
	}
	if res.Files[0].Name != "sitemap.xml" || res.Files[0].URL != u.String() {
		t.Errorf("got %q at %s, want the document itself", res.Files[0].Name, res.Files[0].URL)
	}
}

func TestFeedsMatch(t *testing.T) {
	f := NewFeeds(nil)
	yes := []string{
		"https://feeds.example.test/the-example-show",
		"https://rss.example.test/shows/one",
		"https://example.test/blog/feed/",
		"https://example.test/feed",
		"https://example.test/s/12345/podcast/rss",
		"https://example.test/podcast.xml",
		"https://example.test/show.rss",
		"https://example.test/updates.atom",
		"https://example.test/?feed=rss2",
	}
	no := []string{
		"https://example.test/",
		"https://example.test/feedback/a-story",
		"https://example.test/videos/1/a-clip/",
		"https://example.test/files/episode.mp3",
	}
	for _, raw := range yes {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		if !f.Match(u) {
			t.Errorf("Match(%q) = false", raw)
		}
	}
	for _, raw := range no {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		if f.Match(u) {
			t.Errorf("Match(%q) = true, want the URL left to the other extractors", raw)
		}
	}
}
