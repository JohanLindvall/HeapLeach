package extractor

import (
	"context"
	"errors"
	"fmt"
	"github.com/JohanLindvall/HeapLeach/internal/config"
	"io"
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

// Nothing here came off a live install. The fixtures are written from the
// platform's own templates, and every trap they carry is one those templates
// produce: the tile's image is the thumbnail, the viewer's image is the medium
// rendition, a video's og:image is its poster frame, the pagination anchor is
// printed on the last page too, and the two major versions mark an album tile
// differently.

// cheveretoTile renders one file's list item. The JSON is URL-encoded into the
// attribute exactly as the platform writes it, and the tile's own image is the
// thumbnail rather than the file — which is the whole reason the metadata is
// what gets read.
func cheveretoTile(object, extraAttrs, thumb string) string {
	return fmt.Sprintf(`<div class="list-item fixed-size c8" data-type="image"%s data-object='%s'>
  <div class="list-item-image fixed-size">
    <a href="https://pics.example.test/i/anything" class="image-container">
      <img src="%s" alt="a picture">
    </a>
  </div>
</div>`, extraAttrs, url.QueryEscape(object), thumb)
}

// A version 3 tile: no data-size attribute, and the length written as a JSON
// string rather than a number.
const cheveretoObjectV3 = `{
  "id_encoded": "AAA111",
  "image": {"filename": "AAA111.png", "url": "https://cdn.example.test/AAA111.png", "size": "53525"},
  "medium": {"url": "https://cdn.example.test/AAA111.md.png"},
  "thumb": {"url": "https://cdn.example.test/AAA111.th.png"},
  "name": "holiday",
  "title": "holiday",
  "display_url": "https://cdn.example.test/AAA111.md.png",
  "filename": "AAA111.png",
  "size_formatted": "53.5 KB",
  "url": "https://cdn.example.test/AAA111.png",
  "url_viewer": "https://pics.example.test/i/holiday.AAA111"
}`

// A version 4 tile: the length is a number at the top level, and repeated on
// the element as an attribute.
const cheveretoObjectV4 = `{
  "id_encoded": "BBB222",
  "image": {"filename": "BBB222.jpg", "url": "https://cdn.example.test/BBB222.jpg", "size": 90210},
  "filename": "BBB222.jpg",
  "size": 90210,
  "display_url": "https://cdn.example.test/BBB222.md.jpg",
  "url": "https://cdn.example.test/BBB222.jpg"
}`

// A video tile, and the decoy this file exists to refuse: the nested object
// describes the poster frame taken off the video, not the video, so its length
// belongs to a different file and must not become the item's Size.
const cheveretoObjectVideo = `{
  "id_encoded": "CCC333",
  "image": {"filename": "CCC333.fr.jpg", "url": "https://cdn.example.test/CCC333.fr.jpg", "size": 8192},
  "filename": "CCC333.mp4",
  "display_url": "https://cdn.example.test/CCC333.fr.jpg",
  "url": "https://cdn.example.test/CCC333.mp4"
}`

// cheveretoPage wraps tiles in the surrounding page, with the pagination block
// the platform prints. The "next" anchor is always there; only its href says
// whether a next page exists.
func cheveretoPage(title, tiles, nextHref, more string) string {
	href := ""
	if nextHref != "" {
		href = fmt.Sprintf(` href="%s"`, nextHref)
	}
	return fmt.Sprintf(`<!DOCTYPE html><html><head>
<meta name="generator" content="Chevereto 3">
<meta property="og:title" content="%s">
<meta property="og:image" content="https://cdn.example.test/AAA111.md.png">
</head><body>
<div class="content-listing">
  <div class="pad-content-listing">%s</div>
  %s
  <ul class="content-listing-pagination">
    <li class="pagination-prev"><a data-pagination="prev"></a></li>
    <li class="pagination-page"><a data-pagination="0" href="?page=1">1</a></li>
    <li class="pagination-next"><a data-pagination="next"%s></a></li>
  </ul>
</div>
<script>var auth_token = "deadbeefcafe0001"; CHV.obj.config = {};</script>
</body></html>`, title, tiles, more, href)
}

func cheveretoBase(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func cheveretoParse(t *testing.T, doc string) *html.Node {
	t.Helper()
	root, err := parseHTML(doc)
	if err != nil {
		t.Fatal(err)
	}
	return root
}

// TestCheveretoItemsReadTheMetadataNotTheThumbnail is the central claim: one
// listing fetch yields the full-size link, the stored filename and an exact
// length, none of which the visible markup carries.
func TestCheveretoItemsReadTheMetadataNotTheThumbnail(t *testing.T) {
	doc := cheveretoPage("Holiday album",
		cheveretoTile(cheveretoObjectV3, "", "https://cdn.example.test/AAA111.th.png")+
			cheveretoTile(cheveretoObjectV4, ` data-size="90210"`, "https://cdn.example.test/BBB222.th.jpg"),
		"", "")

	files := cheveretoItems(cheveretoParse(t, doc), cheveretoBase(t, "https://pics.example.test/album/holiday.XYZ"))
	if len(files) != 2 {
		t.Fatalf("read %d files, want 2", len(files))
	}

	want := []File{
		{Name: "AAA111.png", URL: "https://cdn.example.test/AAA111.png", Size: 53525},
		{Name: "BBB222.jpg", URL: "https://cdn.example.test/BBB222.jpg", Size: 90210},
	}
	for i, w := range want {
		got := files[i]
		if got.Name != w.Name || got.URL != w.URL || got.Size != w.Size {
			t.Errorf("file %d = {%q %q %d}, want {%q %q %d}",
				i, got.Name, got.URL, got.Size, w.Name, w.URL, w.Size)
		}
		if got.SizeApprox {
			t.Errorf("file %d marked approximate: the listing states an exact byte count", i)
		}
	}
}

// TestCheveretoSizeIsRefusedWhenItDescribesAnotherFile pins the one place an
// exact length would be actively harmful. A video's nested object is its
// poster frame; taking that length would let the downloader decide a
// part-finished video on disk was already complete.
func TestCheveretoSizeIsRefusedWhenItDescribesAnotherFile(t *testing.T) {
	doc := cheveretoPage("clips",
		cheveretoTile(cheveretoObjectVideo, "", "https://cdn.example.test/CCC333.th.jpg"), "", "")

	files := cheveretoItems(cheveretoParse(t, doc), cheveretoBase(t, "https://pics.example.test/album/clips.XYZ"))
	if len(files) != 1 {
		t.Fatalf("read %d files, want 1", len(files))
	}
	if files[0].URL != "https://cdn.example.test/CCC333.mp4" {
		t.Errorf("URL = %q, want the video rather than its frame", files[0].URL)
	}
	if files[0].Name != "CCC333.mp4" {
		t.Errorf("Name = %q, want the video's own filename", files[0].Name)
	}
	if files[0].Size != -1 {
		t.Errorf("Size = %d, want -1: the only length on offer belongs to the poster frame", files[0].Size)
	}
}

// TestCheveretoAlbumLinksHandleBothVersions covers the markup difference
// between the two: version 4 names the album in an attribute, version 3 only
// in the text of its title link. Both tiles mark themselves as albums, which
// is why that is what is matched on.
func TestCheveretoAlbumLinksHandleBothVersions(t *testing.T) {
	doc := `<!DOCTYPE html><html><body>
<div class="list-item" data-type="album" data-id="V4">
  <div class="list-item-image"><a href="/album/summer.V4" class="image-container"><img src="x.th.jpg"></a></div>
  <div class="list-item-desc"><div class="list-item-desc-title">
    <a class="list-item-desc-title-link" href="/album/summer.V4" data-text="album-name">Summer</a>
  </div></div>
</div>
<div class="list-item" data-type="album" data-id="V3">
  <div class="list-item-image"><a href="/album/winter.V3" class="image-container"><img src="y.th.jpg"></a></div>
  <div class="list-item-desc"><div class="list-item-desc-title">
    <a class="list-item-desc-title-link" href="/album/winter.V3">Winter</a>
  </div></div>
</div>
<div class="list-item" data-type="image" data-object=''>
  <a href="/i/not-an-album">an item, which is not an album</a>
</div>
</body></html>`

	// Only version 4 prints the name as an attribute; the tile above carries
	// it so the attribute is preferred, and the version 3 tile falls back to
	// the link text.
	doc = strings.Replace(doc, `data-type="album" data-id="V4"`,
		`data-type="album" data-id="V4" data-name="Summer 2026"`, 1)

	albums := cheveretoAlbumLinks(cheveretoParse(t, doc), cheveretoBase(t, "https://pics.example.test/user/someone/albums"))
	if len(albums) != 2 {
		t.Fatalf("found %d albums, want 2 (and never the image tile)", len(albums))
	}
	if albums[0].URL != "https://pics.example.test/album/summer.V4" || albums[0].Name != "Summer 2026" {
		t.Errorf("album 0 = %+v, want the version 4 attribute name", albums[0])
	}
	if albums[1].URL != "https://pics.example.test/album/winter.V3" || albums[1].Name != "Winter" {
		t.Errorf("album 1 = %+v, want the version 3 link text as the name", albums[1])
	}
}

// TestCheveretoNextPageNeedsAnHref is the pagination trap. The anchor is
// printed on the last page as well, without an href; matching the element
// alone would walk the final page over and over to the page cap.
func TestCheveretoNextPageNeedsAnHref(t *testing.T) {
	base := cheveretoBase(t, "https://pics.example.test/album/holiday.XYZ")

	withNext := cheveretoParse(t, cheveretoPage("a", "", "?page=2&seek=ZZZ", ""))
	if got := cheveretoNextPage(withNext, base); got != "https://pics.example.test/album/holiday.XYZ?page=2&seek=ZZZ" {
		t.Errorf("next page = %q, want the anchor's own href", got)
	}

	last := cheveretoParse(t, cheveretoPage("a", "", "", ""))
	if got := cheveretoNextPage(last, base); got != "" {
		t.Errorf("next page = %q on the last page, want none: the anchor there has no href", got)
	}
}

func TestCheveretoPageAtKeepsTheQueryAndDropsTheCursor(t *testing.T) {
	base := cheveretoBase(t, "https://pics.example.test/album/x.XYZ?sort=date_desc&seek=ABC&page=1")
	got := cheveretoPageAt(base, 3)

	u, err := url.Parse(got)
	if err != nil {
		t.Fatal(err)
	}
	query := u.Query()
	if query.Get("page") != "3" {
		t.Errorf("page = %q, want 3", query.Get("page"))
	}
	if query.Get("sort") != "date_desc" {
		t.Errorf("sort = %q, want the caller's own ordering kept", query.Get("sort"))
	}
	if _, ok := query["seek"]; ok {
		t.Error("the seek cursor was kept; it points into the page already read")
	}
}

func TestCheveretoHasMore(t *testing.T) {
	full := cheveretoParse(t, cheveretoPage("a", "", "", `<div class="content-listing-loading"></div>`))
	if !cheveretoHasMore(full) {
		t.Error("a page carrying the marker reported no more")
	}
	short := cheveretoParse(t, cheveretoPage("a", "", "", ""))
	if cheveretoHasMore(short) {
		t.Error("a page without the marker reported more")
	}
}

// --- the single item ---------------------------------------------------------

// A video's viewer page. og:image is the poster frame and the viewer's own
// element shows that same frame, so both of the obvious sources are wrong and
// only og:video names the file.
const cheveretoVideoViewer = `<!DOCTYPE html><html><head>
<meta name="generator" content="Chevereto 4">
<meta property="og:type" content="video.other">
<meta property="og:title" content="a clip">
<meta property="og:image" content="https://cdn.example.test/CCC333.fr.jpg">
<meta property="og:video" content="https://cdn.example.test/CCC333.mp4">
<meta property="og:video:type" content="video/mp4">
</head><body>
<div id="image-viewer" class="image-viewer full-viewer">
  <img draggable="false" data-media="video" class="media"
       src="https://cdn.example.test/CCC333.fr.jpg" data-load="full">
  <div id="image-viewer-loader" class="glass-button" data-size="7340032"></div>
</div>
</body></html>`

// An image's viewer page: og:image is the file, while the element on the page
// is the medium rendition. This one names the software only in its footer
// credit, which is all a version 4 install leaves.
const cheveretoImageViewer = `<!DOCTYPE html><html><head>
<meta property="og:title" content="holiday">
<meta property="og:image" content="https://cdn.example.test/AAA111.png">
</head><body>
<div id="image-viewer" class="image-viewer full-viewer">
  <img class="media" src="https://cdn.example.test/AAA111.md.png" data-load="full">
  <div id="image-viewer-loader" data-size="53525"></div>
</div>
<footer><a href="https://chevereto.com" rel="generator">Powered by Chevereto</a></footer>
</body></html>`

func TestCheveretoSingleReadsTheVideoNotItsPosterFrame(t *testing.T) {
	root := cheveretoParse(t, cheveretoVideoViewer)
	res, err := cheveretoSingle(root, cheveretoBase(t, "https://pics.example.test/i/a-clip.CCC333"), "chevereto", "a clip")
	if err != nil {
		t.Fatal(err)
	}
	if res.Files[0].URL != "https://cdn.example.test/CCC333.mp4" {
		t.Errorf("URL = %q, want the video: og:image and the viewer both show the frame",
			res.Files[0].URL)
	}
	if res.Files[0].Name != "CCC333.mp4" {
		t.Errorf("Name = %q, want the video's own filename", res.Files[0].Name)
	}
	if res.Files[0].Size != 7340032 {
		t.Errorf("Size = %d, want the exact length the viewer's loader states", res.Files[0].Size)
	}
}

func TestCheveretoSingleReadsTheOriginalNotTheRendition(t *testing.T) {
	root := cheveretoParse(t, cheveretoImageViewer)
	res, err := cheveretoSingle(root, cheveretoBase(t, "https://pics.example.test/i/holiday.AAA111"), "chevereto", "holiday")
	if err != nil {
		t.Fatal(err)
	}
	if res.Files[0].URL != "https://cdn.example.test/AAA111.png" {
		t.Errorf("URL = %q, want the original rather than the .md. rendition on the page",
			res.Files[0].URL)
	}
	if res.Files[0].Size != 53525 {
		t.Errorf("Size = %d, want 53525", res.Files[0].Size)
	}
}

// TestCheveretoSingleFallsBackToTheViewerAndStrips covers an install whose
// metadata is missing: the element is all there is, and it is the rendition,
// so the marker has to come out of the name.
func TestCheveretoSingleFallsBackToTheViewerAndStrips(t *testing.T) {
	root := cheveretoParse(t, `<html><body><div id="image-viewer">
	  <img class="media" src="https://cdn.example.test/AAA111.md.png"></div></body></html>`)

	res, err := cheveretoSingle(root, cheveretoBase(t, "https://pics.example.test/i/holiday.AAA111"), "chevereto", "")
	if err != nil {
		t.Fatal(err)
	}
	if res.Files[0].URL != "https://cdn.example.test/AAA111.png" {
		t.Errorf("URL = %q, want the medium marker stripped", res.Files[0].URL)
	}
	if res.Files[0].Size != -1 {
		t.Errorf("Size = %d, want -1 when no length is stated", res.Files[0].Size)
	}
}

// TestCheveretoSingleRefusesAPageShowingNoItem is what keeps a front page or
// an empty profile from resolving to the operator's cover art or a user's
// avatar, both of which such pages publish as og:image.
func TestCheveretoSingleRefusesAPageShowingNoItem(t *testing.T) {
	root := cheveretoParse(t, `<!DOCTYPE html><html><head>
	  <meta name="generator" content="Chevereto 3">
	  <meta property="og:title" content="Free image hosting">
	  <meta property="og:image" content="https://pics.example.test/content/images/system/home_cover.png">
	  </head><body><div class="content-listing"></div></body></html>`)

	if _, err := cheveretoSingle(root, cheveretoBase(t, "https://pics.example.test/explore/recent"), "chevereto", ""); err == nil {
		t.Fatal("a page with no viewer resolved to its own cover image")
	}
}

// --- the rendition mapping ---------------------------------------------------

func TestCheveretoOriginal(t *testing.T) {
	tests := map[string]string{
		"https://cdn.example.test/AAA111.md.png":  "https://cdn.example.test/AAA111.png",
		"https://cdn.example.test/AAA111.th.png":  "https://cdn.example.test/AAA111.png",
		"https://cdn.example.test/CCC333.fr.jpg":  "https://cdn.example.test/CCC333.jpg",
		"https://cdn.example.test/AAA111.png":     "https://cdn.example.test/AAA111.png",
		"https://cdn.example.test/a/b/c.md.webp":  "https://cdn.example.test/a/b/c.webp",
		"https://cdn.example.test/my.holiday.jpg": "https://cdn.example.test/my.holiday.jpg",
		"https://cdn.example.test/AAA111.mdx.png": "https://cdn.example.test/AAA111.mdx.png",
		"https://cdn.example.test/noextension":    "https://cdn.example.test/noextension",
		"":                                        "",
		"/relative/AAA111.md.png":                 "",
	}
	for in, want := range tests {
		if got := cheveretoOriginal(in); got != want {
			t.Errorf("cheveretoOriginal(%q) = %q, want %q", in, got, want)
		}
	}
}

// --- recognising the platform ------------------------------------------------

func TestCheveretoFingerprint(t *testing.T) {
	tests := []struct {
		name string
		doc  string
		want bool
	}{
		{
			name: "version 3 states it in the metadata",
			doc:  `<html><head><meta name="generator" content="Chevereto 3"></head><body></body></html>`,
			want: true,
		},
		{
			// Version 4 prints no generator meta at all; the credit in its
			// footer is what is left.
			name: "version 4 leaves only the footer credit",
			doc:  `<html><body><a href="https://chevereto.com" rel="generator">Powered by Chevereto</a></body></html>`,
			want: true,
		},
		{
			// A licence that removed the credit still ships the scripts.
			name: "an install with neither still ships the namespace",
			doc:  `<html><body><script>CHV.obj.config = {json_api: 1};</script></body></html>`,
			want: true,
		},
		{
			name: "an unrelated page",
			doc:  `<html><head><meta name="generator" content="WordPress 6.5"></head><body></body></html>`,
			want: false,
		},
		{
			// The word alone is not the fingerprint: a page may merely
			// mention the software.
			name: "a page that only talks about it",
			doc:  `<html><body><p>We run Chevereto here.</p></body></html>`,
			want: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := cheveretoFingerprint(cheveretoParse(t, tc.doc), tc.doc); got != tc.want {
				t.Errorf("cheveretoFingerprint = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestCheveretoPagePath(t *testing.T) {
	tests := map[string]bool{
		"https://pics.example.test/i/holiday.AAA111":     true,
		"https://pics.example.test/image/holiday.AAA111": true,
		"https://pics.example.test/img/AAA111":           true,
		"https://pics.example.test/album/holiday.XYZ":    true,
		"https://pics.example.test/a/XYZ":                true,
		// A user's album tab leads with their name, so the route follows
		// rather than leads. It is the one shape recognisable without
		// knowing who they are.
		"https://pics.example.test/someone/albums": true,
		"https://pics.example.test/someone":        false,
		"https://pics.example.test/":               false,
		"https://pics.example.test/i":              false,
		// A file that happens to sit under one of those segments is a
		// download already, not a page to read.
		"https://pics.example.test/images/AAA111.jpg": false,
		"https://pics.example.test/i/AAA111.png":      false,
	}
	for raw, want := range tests {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		if got := cheveretoPagePath(u); got != want {
			t.Errorf("cheveretoPagePath(%q) = %v, want %v", raw, got, want)
		}
	}
}

func TestCheveretoMatch(t *testing.T) {
	sites := NewCheveretoSites(nil, nil)
	byName := make(map[string]Extractor, len(sites))
	for _, site := range sites {
		byName[site.Name()] = site
	}

	tests := map[string]string{
		"https://imgbb.com/album/holiday.XYZ":     "imgbb",
		"https://ibb.co/AAA111":                   "imgbb",
		"https://freeimage.host/i/holiday.AAA111": "freeimage",
		"https://gifyu.com/album/x.XYZ":           "gifyu",
	}
	for raw, want := range tests {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		ex, ok := byName[want]
		if !ok {
			t.Fatalf("no extractor named %q was built", want)
		}
		if !ex.Match(u) {
			t.Errorf("%s did not match %q", want, raw)
		}
	}

	other, _ := url.Parse("https://nobody.example.test/i/AAA111")
	for _, site := range sites {
		if site.Match(other) {
			t.Errorf("%s matched an unrelated host", site.Name())
		}
	}
}

// TestCheveretoAcceptsExtraHosts covers the escape hatch for software whose
// installs outrun any release.
func TestCheveretoAcceptsExtraHosts(t *testing.T) {
	cfg := &config.Config{ExtraHosts: map[string][]string{
		config.FamilyChevereto: {"pics.example.test", "another.example.test"},
	}}

	u, err := url.Parse("https://pics.example.test/album/holiday.XYZ")
	if err != nil {
		t.Fatal(err)
	}
	var matched Extractor
	for _, site := range NewCheveretoSites(cfg, nil) {
		if site.Match(u) {
			matched = site
		}
	}
	if matched == nil {
		t.Fatal("a host added through HEAPLEACH_EXTRA_HOSTS was not matched")
	}
	if matched.Name() != "pics" {
		t.Errorf("the extra host is named %q, want its own first label", matched.Name())
	}
}

// --- against a server --------------------------------------------------------

func cheveretoTestClient() *httpx.Client {
	return httpx.New("test-agent", "en-US", 0, 5*time.Second)
}

// TestCheveretoWalksEveryPage covers the pagination end to end, including the
// stop: the second page's anchor carries no href, so a third request would be
// the bug this pins.
func TestCheveretoWalksEveryPage(t *testing.T) {
	var hits atomic.Int32
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		switch r.URL.Query().Get("page") {
		case "", "1":
			_, _ = io.WriteString(w, cheveretoPage("Holiday album",
				cheveretoTile(cheveretoObjectV3, "", "https://cdn.example.test/AAA111.th.png"),
				srv.URL+"/album/holiday.XYZ?page=2&seek=AAA111", `<div class="content-listing-loading"></div>`))
		default:
			_, _ = io.WriteString(w, cheveretoPage("Holiday album",
				cheveretoTile(cheveretoObjectV4, ` data-size="90210"`, "https://cdn.example.test/BBB222.th.jpg"),
				"", ""))
		}
	}))
	defer srv.Close()

	u, err := url.Parse(srv.URL + "/album/holiday.XYZ")
	if err != nil {
		t.Fatal(err)
	}
	res, err := cheveretoExtract(context.Background(), cheveretoTestClient(), u, "chevereto", Options{}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Files) != 2 {
		t.Fatalf("resolved %d files, want both pages", len(res.Files))
	}
	if res.Title != "Holiday album" {
		t.Errorf("title = %q, want the album's own name", res.Title)
	}
	if n := hits.Load(); n != 2 {
		t.Errorf("fetched %d pages, want 2: the last page's next anchor has no href", n)
	}
}

// TestCheveretoStopsWhenAPageRepeats covers the other end of the walk. A
// listing asked for a page past its last answers with the last one again, and
// the guessed page number is only reached when the anchor is missing.
func TestCheveretoStopsWhenAPageRepeats(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		// No pagination anchor at all, but the page reports itself full, so
		// the numbered fallback is tried — and answers with the same item.
		_, _ = io.WriteString(w, cheveretoPage("Holiday album",
			cheveretoTile(cheveretoObjectV3, "", "https://cdn.example.test/AAA111.th.png"),
			"", `<div class="content-listing-loading"></div>`))
	}))
	defer srv.Close()

	u, err := url.Parse(srv.URL + "/album/holiday.XYZ")
	if err != nil {
		t.Fatal(err)
	}
	res, err := cheveretoExtract(context.Background(), cheveretoTestClient(), u, "chevereto", Options{}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Files) != 1 {
		t.Errorf("resolved %d files, want the one distinct item", len(res.Files))
	}
	if n := hits.Load(); n != 2 {
		t.Errorf("fetched %d pages, want 2: the repeat has to end the walk", n)
	}
}

// TestCheveretoExpandsAProfilesAlbums covers the page that lists albums rather
// than files: each is opened, and each album's files land in a folder named
// after it so two albums cannot collide.
func TestCheveretoExpandsAProfilesAlbums(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/album/summer"):
			_, _ = io.WriteString(w, cheveretoPage("Summer",
				cheveretoTile(cheveretoObjectV3, "", "https://cdn.example.test/AAA111.th.png"), "", ""))
		case strings.HasPrefix(r.URL.Path, "/album/winter"):
			_, _ = io.WriteString(w, cheveretoPage("Winter",
				cheveretoTile(cheveretoObjectV4, "", "https://cdn.example.test/BBB222.th.jpg"), "", ""))
		default:
			_, _ = io.WriteString(w, fmt.Sprintf(`<!DOCTYPE html><html><head>
			<meta name="generator" content="Chevereto 4">
			<meta property="og:title" content="someone">
			</head><body>
			<div class="list-item" data-type="album" data-name="Summer">
			  <div class="list-item-desc-title"><a class="list-item-desc-title-link"
			     href="%s/album/summer.V4">Summer</a></div>
			</div>
			<div class="list-item" data-type="album">
			  <div class="list-item-desc-title"><a class="list-item-desc-title-link"
			     href="%s/album/winter.V3">Winter</a></div>
			</div>
			</body></html>`, srv.URL, srv.URL))
		}
	}))
	defer srv.Close()

	u, err := url.Parse(srv.URL + "/someone/albums")
	if err != nil {
		t.Fatal(err)
	}
	res, err := cheveretoExtract(context.Background(), cheveretoTestClient(), u, "chevereto", Options{}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Files) != 2 {
		t.Fatalf("resolved %d files, want one from each album", len(res.Files))
	}
	// In the profile's own order, whatever order the requests finished in.
	if res.Files[0].Dir != "Summer" || res.Files[1].Dir != "Winter" {
		t.Errorf("directories are %q and %q, want each album's own name in the page's order",
			res.Files[0].Dir, res.Files[1].Dir)
	}
}

// TestCheveretoPasswordGate covers a protected album: nothing is guessed at,
// the caller is told what is needed, and the token the form carries is posted
// back with the password.
func TestCheveretoPasswordGate(t *testing.T) {
	const (
		token    = "deadbeefcafe0001"
		password = "open sesame"
	)
	gate := fmt.Sprintf(`<!DOCTYPE html><html><head>
	<meta name="generator" content="Chevereto 4"></head><body>
	<div class="content-password-gate"><form method="post" data-action="validate">
	  <input type="hidden" name="auth_token" value="%s">
	  <input type="password" id="content-password" name="content-password" required>
	</form></div></body></html>`, token)

	album := cheveretoPage("Locked album",
		cheveretoTile(cheveretoObjectV3, "", "https://cdn.example.test/AAA111.th.png"), "", "")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			_, _ = io.WriteString(w, gate)
			return
		}
		if err := r.ParseForm(); err != nil {
			t.Errorf("the posted form did not parse: %v", err)
		}
		if got := r.PostForm.Get("auth_token"); got != token {
			t.Errorf("posted auth_token %q, want the one the form carried", got)
		}
		if r.PostForm.Get("content-password") != password {
			// A wrong password is answered with the gate again, which is how
			// the site itself refuses.
			_, _ = io.WriteString(w, gate)
			return
		}
		_, _ = io.WriteString(w, album)
	}))
	defer srv.Close()

	u, err := url.Parse(srv.URL + "/album/locked.XYZ")
	if err != nil {
		t.Fatal(err)
	}
	client := cheveretoTestClient()

	if _, err := cheveretoExtract(context.Background(), client, u, "chevereto", Options{}, false); !errors.Is(err, ErrPasswordRequired) {
		t.Fatalf("without a password the error was %v, want ErrPasswordRequired", err)
	}

	_, err = cheveretoExtract(context.Background(), client, u, "chevereto", Options{Password: "wrong"}, false)
	if err == nil {
		t.Fatal("a wrong password resolved the album")
	}
	if errors.Is(err, ErrPasswordRequired) {
		t.Error("a refused password was reported as no password having been given")
	}

	res, err := cheveretoExtract(context.Background(), client, u, "chevereto", Options{Password: password}, false)
	if err != nil {
		t.Fatalf("the right password did not unlock the album: %v", err)
	}
	if len(res.Files) != 1 {
		t.Errorf("resolved %d files behind the gate, want 1", len(res.Files))
	}
}

// TestCheveretoSniffNeedsTheFingerprint is the whole bargain of sniffing an
// unregistered host: the shape is worth one request, and a page that does not
// identify itself as Chevereto is left to the handling that would have run
// anyway.
func TestCheveretoSniffNeedsTheFingerprint(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if strings.Contains(r.URL.Path, "impostor") {
			_, _ = io.WriteString(w, `<html><head><title>not it</title></head>
			<body><div id="image-viewer"><img src="https://cdn.example.test/x.md.png"></div></body></html>`)
			return
		}
		_, _ = io.WriteString(w, cheveretoImageViewer)
	}))
	defer srv.Close()

	client := cheveretoTestClient()

	// A page that is not laid out like one is not even fetched.
	notShaped, _ := url.Parse(srv.URL + "/some/file.bin")
	if _, ok := cheveretoSniff(context.Background(), client, notShaped, Options{}); ok {
		t.Error("a URL of another shape was claimed")
	}
	if n := hits.Load(); n != 0 {
		t.Errorf("made %d requests for a URL of the wrong shape, want none", n)
	}

	// One that is, but whose page says nothing about the software, is
	// fetched once and then declined.
	impostor, _ := url.Parse(srv.URL + "/i/impostor.AAA111")
	if _, ok := cheveretoSniff(context.Background(), client, impostor, Options{}); ok {
		t.Error("a page carrying no Chevereto fingerprint was claimed")
	}

	real, _ := url.Parse(srv.URL + "/i/holiday.AAA111")
	res, ok := cheveretoSniff(context.Background(), client, real, Options{})
	if !ok {
		t.Fatal("a Chevereto page on an unregistered host was not recognised")
	}
	if res.Files[0].URL != "https://cdn.example.test/AAA111.png" {
		t.Errorf("URL = %q, want the original", res.Files[0].URL)
	}
}

// TestCheveretoRefusesABareDomain keeps a mis-paste from resolving to the
// site's own cover art, which its front page publishes as og:image.
func TestCheveretoRefusesABareDomain(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("the front page was fetched; a bare domain should be refused without a request")
		_, _ = io.WriteString(w, "")
	}))
	defer srv.Close()

	u, err := url.Parse(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cheveretoExtract(context.Background(), cheveretoTestClient(), u, "chevereto", Options{}, false); err == nil {
		t.Fatal("a bare domain resolved to something")
	}
}
