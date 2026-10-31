// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

// Everything below is a synthetic wiki. The API is the thing under test and
// it does not care whose files it is describing, so the pages, the titles
// and the media hosts are all invented — with the traps the real API has
// built into them, since a fixture without those pins nothing.

// --- the fixtures ---------------------------------------------------------

// mediaWikiCategoryBatch1 is deliberately out of order.
//
// The API sorts each batch by page id whatever order the generator walked
// in, so the file that sorts last by name comes first here. It also carries
// a red link: a file page some article names that nobody ever uploaded,
// which arrives with no imageinfo at all.
const mediaWikiCategoryBatch1 = `{
  "batchcomplete": true,
  "continue": {"gcmcontinue": "file|second", "continue": "gcmcontinue||"},
  "query": {"pages": [
    {"pageid": 300, "ns": 6, "title": "File:Zebra crossing.jpg", "imagerepository": "local",
     "imageinfo": [{"size": 300300, "width": 100, "height": 100,
       "url": "https://media.example.test/z/Zebra_crossing.jpg"}]},
    {"pageid": 400, "ns": 6, "title": "File:Arch bridge.jpg", "imagerepository": "local",
     "imageinfo": [{"size": 400400, "width": 100, "height": 100,
       "url": "https://media.example.test/a/Arch_bridge.jpg"}]},
    {"ns": 6, "title": "File:Never uploaded.png", "missing": true, "imagerepository": ""}
  ]}
}`

// mediaWikiCategoryBatch2 holds the trap that matters most: a file marked
// missing that is not missing. It lives on a shared repository, so the local
// page does not exist while the imageinfo is complete — which is the state
// of nearly every image on any Wikipedia article.
const mediaWikiCategoryBatch2 = `{
  "batchcomplete": true,
  "query": {"pages": [
    {"ns": 6, "title": "File:Beam bridge.jpg", "missing": true, "known": true,
     "imagerepository": "shared",
     "imageinfo": [{"size": 100100, "width": 100, "height": 100,
       "url": "https://media.example.test/b/Beam_bridge.jpg"}]}
  ]}
}`

// mediaWikiSubcats answers the subcategory listing. The child names its own
// parent, because a category graph loops and a walk that does not remember
// where it has been never finishes; and one child will not list, which the
// walk leaves out and says so.
const mediaWikiSubcats = `{
  "batchcomplete": true,
  "query": {"categorymembers": [
    {"pageid": 11, "ns": 14, "title": "Category:Stone bridges"},
    {"pageid": 12, "ns": 14, "title": "Category:Synthetic bridges"},
    {"pageid": 13, "ns": 14, "title": "Category:Nonexistent"}
  ]}
}`

const mediaWikiStoneFiles = `{
  "batchcomplete": true,
  "query": {"pages": [
    {"pageid": 500, "ns": 6, "title": "File:Old stone.jpg", "imagerepository": "local",
     "imageinfo": [{"size": 500500, "width": 100, "height": 100,
       "url": "https://media.example.test/o/Old_stone.jpg"}]}
  ]}
}`

// mediaWikiArticleImages is what an article yields: its own picture, and the
// site furniture its templates draw, which the API cannot tell apart.
const mediaWikiArticleImages = `{
  "batchcomplete": true,
  "query": {"pages": [
    {"pageid": 700, "ns": 6, "title": "File:Synthetic diagram.svg", "imagerepository": "local",
     "imageinfo": [{"size": 7070, "width": 10, "height": 10,
       "url": "https://media.example.test/s/Synthetic_diagram.svg"}]},
    {"ns": 6, "title": "File:Project-logo.svg", "missing": true, "known": true,
     "imagerepository": "shared",
     "imageinfo": [{"size": 808, "width": 10, "height": 10,
       "url": "https://media.example.test/p/Project-logo.svg"}]}
  ]}
}`

// mediaWikiFarmUploads comes from a wiki on the farm whose CDN re-encodes
// what it serves, and whose every media path ends in the same segment.
const mediaWikiFarmUploads = `{
  "batchcomplete": true,
  "query": {"pages": [
    {"pageid": 900, "ns": 6, "title": "File:Painted sign.jpg", "imagerepository": "local",
     "imageinfo": [{"size": 82135, "width": 901, "height": 549,
       "url": "https://static.wikia.nocookie.net/synthwiki/images/b/bf/Painted_sign.jpg/revision/latest?cb=20120911024129"}]}
  ]}
}`

const mediaWikiSiteInfo = `{
  "batchcomplete": true,
  "query": {"general": {"generator": "MediaWiki 1.43.0", "sitename": "Synthetic Wiki"}}
}`

// --- a wiki to talk to ----------------------------------------------------

// mediaWikiServer stands up a wiki with the layout of one on a farm: the API
// at /api.php, and an empty 200 at /w/api.php. That second one is not
// padding — Fandom answers exactly that way, so a probe that trusts a status
// code settles on the wrong endpoint and every query after it fails.
func mediaWikiServer(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()

	var calls atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/w/api.php", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK) // and nothing else, exactly as Fandom does
	})
	// The wiki's own pages, as the software renders them, advertising the
	// API where this one keeps it.
	mux.HandleFunc("/wiki/", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set(httpx.HeaderContentType, "text/html; charset=utf-8")
		_, _ = io.WriteString(w, strings.Replace(mediaWikiRenderedPage, "/custom/api.php", "/api.php", 1))
	})
	mux.HandleFunc("/api.php", func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		q := r.URL.Query()
		w.Header().Set(httpx.HeaderContentType, httpx.ContentTypeJSON)

		body := ""
		switch {
		case q.Get("meta") == "siteinfo":
			body = mediaWikiSiteInfo

		case q.Get("list") == "categorymembers":
			if q.Get("cmtitle") == "Category:Synthetic bridges" {
				body = mediaWikiSubcats
			} else {
				body = `{"batchcomplete":true,"query":{"categorymembers":[]}}`
			}

		case q.Get("generator") == "categorymembers":
			// Matched on the name rather than the whole title, because a
			// category reached through the namespace lookup arrives under
			// whatever prefix the wiki calls its own.
			category := q.Get("gcmtitle")
			switch {
			case strings.HasSuffix(category, ":Stone bridges"):
				body = mediaWikiStoneFiles
			case strings.HasSuffix(category, ":Empty shelf"):
				body = `{"batchcomplete":true,"query":{"pages":[]}}`
			case strings.HasSuffix(category, ":Nonexistent"):
				// An API error arrives with a perfectly good 200.
				body = `{"error":{"code":"invalidcategory","info":"The category name you entered is not valid."}}`
			case q.Get("gcmcontinue") != "":
				body = mediaWikiCategoryBatch2
			default:
				body = mediaWikiCategoryBatch1
			}

		case q.Get("generator") == "images":
			body = mediaWikiArticleImages

		case q.Get("generator") == "allimages":
			body = mediaWikiFarmUploads

		case q.Get("prop") == "imageinfo" && q.Get("titles") != "":
			body = `{"batchcomplete":true,"query":{"pages":[
			  {"pageid": 600, "ns": 6, "title": "File:Single: file.jpg", "imagerepository": "local",
			   "imageinfo": [{"size": 6060, "width": 10, "height": 10,
			     "url": "https://media.example.test/s/Single_file.jpg"}]}]}}`

		case q.Get("titles") != "":
			// The namespace lookup. The wiki is the only thing that can say
			// what a prefix in its own language means, and here it says that
			// Kategorie: is the category namespace and everything else is an
			// ordinary page.
			ns := 0
			if strings.HasPrefix(q.Get("titles"), "Kategorie:") {
				ns = 14
			}
			body = fmt.Sprintf(`{"batchcomplete":true,"query":{"pages":[
			  {"pageid": 42, "ns": %d, "title": %q}]}}`, ns, q.Get("titles"))

		default:
			t.Errorf("unexpected API call: %s", r.URL.RawQuery)
			body = `{}`
		}
		_, _ = io.WriteString(w, body)
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &calls
}

func testMediaWiki(t *testing.T) *MediaWiki {
	t.Helper()
	// No interval: the courtesy spacing is for real hosts, and a second
	// between requests would be the whole runtime of this file.
	m := NewMediaWiki(testClient())
	m.interval = 0
	return m
}

func mediaWikiExtract(t *testing.T, m *MediaWiki, raw string) (*Result, error) {
	t.Helper()
	u := mustParse(t, raw)
	return m.Extract(context.Background(), u, Options{})
}

// --- the listing ----------------------------------------------------------

// TestMediaWikiCategory is the whole path in one: probing past the blank
// endpoint, paging on the continuation token, keeping a file the API called
// missing, dropping one that really is, and sorting what is left.
func TestMediaWikiCategory(t *testing.T) {
	srv, calls := mediaWikiServer(t)
	m := testMediaWiki(t)

	res, err := mediaWikiExtract(t, m, srv.URL+"/wiki/Category:Synthetic_bridges")
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if res.Title != "Synthetic bridges" {
		t.Errorf("title = %q, want the category without its namespace", res.Title)
	}

	want := []struct {
		name string
		size int64
	}{
		{"Arch bridge.jpg", 400400},
		{"Beam bridge.jpg", 100100},
		{"Zebra crossing.jpg", 300300},
	}
	if len(res.Files) != len(want) {
		t.Fatalf("got %d files, want %d — the red link is not one and the shared-repository file is",
			len(res.Files), len(want))
	}
	for i, w := range want {
		got := res.Files[i]
		if got.Name != w.name {
			t.Errorf("file %d is %q, want %q (the batch arrives in page-id order)", i, got.Name, w.name)
		}
		if got.Size != w.size {
			t.Errorf("%s size = %d, want %d", got.Name, got.Size, w.size)
		}
		// The exact byte count is the point of this host: it is what lets a
		// second run skip a file already on disk.
		if got.SizeApprox {
			t.Errorf("%s is marked approximate, but the API states an exact length", got.Name)
		}
	}

	// Once for the probe of this endpoint, then two pages of the listing.
	if n := calls.Load(); n != 3 {
		t.Errorf("made %d API calls, want 3 (one probe, two pages)", n)
	}

	// A second URL on the same host must not probe again.
	if _, err := mediaWikiExtract(t, m, srv.URL+"/wiki/Category:Synthetic_bridges"); err != nil {
		t.Fatal(err)
	}
	if n := calls.Load(); n != 5 {
		t.Errorf("made %d API calls in total, want 5 — the API base should be remembered", n)
	}
}

// TestMediaWikiCategoryDepth covers the recursion: off unless asked for, and
// proof against the loops a category graph is full of.
func TestMediaWikiCategoryDepth(t *testing.T) {
	srv, _ := mediaWikiServer(t)

	shallow, err := mediaWikiExtract(t, testMediaWiki(t), srv.URL+"/wiki/Category:Synthetic_bridges")
	if err != nil {
		t.Fatal(err)
	}
	if len(shallow.Files) != 3 {
		t.Fatalf("got %d files without ?depth=, want only the category's own 3", len(shallow.Files))
	}

	deep, err := mediaWikiExtract(t, testMediaWiki(t), srv.URL+"/wiki/Category:Synthetic_bridges?depth=1")
	if err != nil {
		t.Fatal(err)
	}
	if len(deep.Files) != 4 {
		t.Fatalf("got %d files at depth 1, want 4 — the subcategory's file should be there", len(deep.Files))
	}
	// The subcategory lists its own parent as a child. Having come back at
	// all means that loop was not followed.
	if deep.Files[2].Name != "Old stone.jpg" {
		t.Errorf("files are %v, want the subcategory's file sorted in among the rest",
			fileNames(deep.Files))
	}
	if deep.Note != mediaWikiSkipped {
		t.Errorf("note = %q, want the subcategory that would not list said", deep.Note)
	}
	if shallow.Note != "" {
		t.Errorf("note = %q without ?depth=, want none", shallow.Note)
	}
}

// TestMediaWikiCategoryWithNoFiles covers the commonest disappointment on
// Commons: a container category, which holds nothing but subcategories. The
// error has to say what to do about it, since there is no other way to find
// out that descending is even possible.
func TestMediaWikiCategoryWithNoFiles(t *testing.T) {
	srv, _ := mediaWikiServer(t)

	_, err := mediaWikiExtract(t, testMediaWiki(t), srv.URL+"/wiki/Category:Empty_shelf")
	if err == nil {
		t.Fatal("a category with no files resolved to something")
	}
	if !strings.Contains(err.Error(), "depth") {
		t.Errorf("error %q does not mention how to descend into subcategories", err)
	}
}

// TestMediaWikiSurfacesAPIErrors pins the one that looks like success: the
// API reports a bad request with HTTP 200 and an error object, so nothing in
// the transport will notice it.
func TestMediaWikiSurfacesAPIErrors(t *testing.T) {
	srv, _ := mediaWikiServer(t)

	_, err := mediaWikiExtract(t, testMediaWiki(t), srv.URL+"/wiki/Category:Nonexistent")
	if err == nil {
		t.Fatal("an API error carried in a 200 was taken for a result")
	}
	if !strings.Contains(err.Error(), "not valid") {
		t.Errorf("error %q does not carry what the wiki said", err)
	}
}

// TestMediaWikiArticle covers an article's images, decoration included.
func TestMediaWikiArticle(t *testing.T) {
	srv, _ := mediaWikiServer(t)

	res, err := mediaWikiExtract(t, testMediaWiki(t), srv.URL+"/wiki/A_synthetic_article")
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if res.Title != "A synthetic article" {
		t.Errorf("title = %q, want the article's own", res.Title)
	}
	if len(res.Files) != 2 {
		t.Fatalf("got %d files, want both images the page uses: %v", len(res.Files), fileNames(res.Files))
	}
}

// TestMediaWikiFilePage covers a single File: page, and a filename with a
// colon of its own — cutting at the last one would lose half the name.
func TestMediaWikiFilePage(t *testing.T) {
	srv, _ := mediaWikiServer(t)

	res, err := mediaWikiExtract(t, testMediaWiki(t), srv.URL+"/wiki/File:Single:_file.jpg")
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if len(res.Files) != 1 {
		t.Fatalf("got %d files, want 1", len(res.Files))
	}
	if res.Files[0].Name != "Single: file.jpg" {
		t.Errorf("name = %q, want the title with only the namespace removed", res.Files[0].Name)
	}
}

// TestMediaWikiUploads covers a wiki's own upload list, and with it the farm
// CDN that hands back something other than what it was asked for.
func TestMediaWikiUploads(t *testing.T) {
	srv, _ := mediaWikiServer(t)

	res, err := mediaWikiExtract(t, testMediaWiki(t), srv.URL+"/wiki/Special:ListFiles")
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if res.Title != "Synthetic Wiki files" {
		t.Errorf("title = %q, want the wiki's own name from siteinfo", res.Title)
	}
	if len(res.Files) != 1 {
		t.Fatalf("got %d files, want 1", len(res.Files))
	}

	file := res.Files[0]
	// Named from the title: every file on this CDN sits at a path ending
	// /revision/latest, so the URL would name them all the same thing.
	if file.Name != "Painted sign.jpg" {
		t.Errorf("name = %q, want the title's", file.Name)
	}
	if !strings.Contains(file.URL, "format=original") {
		t.Errorf("url = %q, which is the one that comes back re-encoded as WebP", file.URL)
	}
	if !file.SizeApprox {
		t.Error("the size is marked exact, but it describes the file before the CDN re-encoded it")
	}
	if file.Size != 82135 {
		t.Errorf("size = %d, want the API's figure kept for the job total", file.Size)
	}
}

// TestMediaWikiRefusesTheWholeOfCommons covers the one special page that
// cannot mean what it says: an alphabetical slice of a hundred million files
// is not a download anybody asked for.
func TestMediaWikiRefusesTheWholeOfCommons(t *testing.T) {
	m := testMediaWiki(t)

	_, err := mediaWikiExtract(t, m, "https://commons.wikimedia.org/wiki/Special:ListFiles")
	if err == nil {
		t.Fatal("the whole file list of a Wikimedia project was accepted")
	}
	if !strings.Contains(err.Error(), "NewFiles") {
		t.Errorf("error %q does not offer the bounded alternative", err)
	}
	// Nothing was fetched to find that out: no request left this process.
	if !strings.Contains(err.Error(), "far more files") {
		t.Errorf("error %q does not say what the problem is", err)
	}
}

// TestMediaWikiAsksAboutAnUnknownPrefix covers a wiki that names its
// namespaces in its own language: the prefix means nothing here, so the wiki
// is asked, and its answer decides which generator runs.
func TestMediaWikiAsksAboutAnUnknownPrefix(t *testing.T) {
	srv, _ := mediaWikiServer(t)

	res, err := mediaWikiExtract(t, testMediaWiki(t), srv.URL+"/wiki/Kategorie:Synthetic_bridges")
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	// A category, resolved as one, despite nothing here knowing German.
	if len(res.Files) != 3 {
		t.Fatalf("got %d files, want the category's 3: %v", len(res.Files), fileNames(res.Files))
	}
}

// --- the pieces -----------------------------------------------------------

// mediaWikiRenderedPage is an article as the software renders it: its generator, the
// ResourceLoader configuration, and the EditURI link naming its API.
const mediaWikiRenderedPage = `<!DOCTYPE html><html><head><title>A synthetic page - A Wiki</title>
<meta name="generator" content="MediaWiki 1.43.0">
<script>RLCONF={"wgPageName":"A_synthetic_page"};</script>
<link rel="EditURI" type="application/rsd+xml" href="/custom/api.php?action=rsd">
</head><body class="mediawiki ltr"></body></html>`

// TestMediaWikiShape pins what a link alone can claim: a namespace nothing
// else on the web uses. An article path is every site's that files content
// under /wiki/, so it waits for the page.
func TestMediaWikiShape(t *testing.T) {
	m := NewMediaWiki(nil)
	for raw, want := range map[string]bool{
		"https://wiki.example.test/wiki/Category:Odds_and_ends":  true,
		"https://wiki.example.test/wiki/File:Thing.png":          true,
		"https://wiki.example.test/index.php?title=File:A.png":   true,
		"https://wiki.example.test/wiki/Special:NewFiles":        true,
		"https://wiki.example.test/wiki/A_synthetic_article":     false,
		"https://wiki.example.test/some/file.bin":                false,
		"https://wiki.example.test/":                             false,
		"https://wiki.example.test/w/index.php?title=An_article": false,
	} {
		expectShape(t, m, raw, want)
	}
}

// TestMediaWikiRecognise: the software's own page, on any host and at any
// title, and nothing that only looks like one.
func TestMediaWikiRecognise(t *testing.T) {
	m := NewMediaWiki(nil)
	expectRecognised(t, m, htmlPage(t, "https://wiki.example.test/wiki/A_synthetic_page", mediaWikiRenderedPage), true)
	expectRecognised(t, m, htmlPage(t, "https://wiki.example.test/wiki/Category:Odds_and_ends", mediaWikiRenderedPage), true)
	// A missing page is still the wiki's.
	expectRecognised(t, m, fixturePage(t, "https://wiki.example.test/wiki/Category:Gone", http.StatusNotFound, "text/html", mediaWikiRenderedPage), true)
	// The generator and the site's name, without the software's structure.
	expectRecognised(t, m, htmlPage(t, "https://wiki.example.test/wiki/A_synthetic_page",
		`<html><head><title>A synthetic page - A Wiki</title><meta name="generator" content="MediaWiki 1.43.0"></head><body></body></html>`), false)
	// An ordinary page filed under /wiki/.
	expectRecognised(t, m, htmlPage(t, "https://site.example.test/wiki/Some_page", `<html><title>Some page</title></html>`), false)
	// The software's page at the site root names no title.
	expectRecognised(t, m, htmlPage(t, "https://wiki.example.test/", mediaWikiRenderedPage), false)
}

// An install with an article path of its own names the title in the
// configuration its page carries, which is read in place of the URL.
func TestMediaWikiTakesTheTitleFromThePage(t *testing.T) {
	srv, _ := mediaWikiServer(t)
	m := testMediaWiki(t)
	raw := srv.URL + "/w/Category:Synthetic_bridges"
	page := htmlPage(t, raw, strings.Replace(mediaWikiRenderedPage, "A_synthetic_page", `Category:Synthetic_bridges`, 1))
	res, err := m.Extract(withPage(context.Background(), page), page.URL, Options{})
	if err != nil || len(res.Files) == 0 {
		t.Fatalf("a category under a custom article path: %v, %v", res, err)
	}
}

// A wiki without recognised markup is asked its API, but only for a form naming a
// namespace, and only once however often the page is consulted.
func TestMediaWikiRecogniseThroughTheAPI(t *testing.T) {
	for _, status := range []int{http.StatusForbidden, http.StatusOK} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			srv, calls := mediaWikiServer(t)
			m := NewMediaWiki(testClient())
			page := func(path string) *Page {
				return fixturePage(t, srv.URL+path, status, "text/html", `<title>A custom wiki</title><p>Browse files.</p>`)
			}
			p := page("/wiki/Category:Synthetic_bridges")
			for range 2 {
				if !m.Recognise(context.Background(), p) {
					t.Fatal("a category whose API answers was not recognised")
				}
			}
			if n := calls.Load(); n != 1 {
				t.Errorf("the API was asked %d times, want once", n)
			}
			if m.Recognise(context.Background(), page("/wiki/An_article")) {
				t.Error("an unmarked article path was probed and claimed")
			}
		})
	}
}

func TestMediaWikiTitle(t *testing.T) {
	tests := []struct {
		raw      string
		title    string
		explicit bool
	}{
		{"https://a.example.test/wiki/Category:Some_bridges", "Category:Some bridges", true},
		{"https://a.example.test/w/index.php?title=File:A_b.png", "File:A b.png", true},
		{"https://a.example.test/index.php/File:A_b.png", "File:A b.png", true},
		// Percent-encoding is the URL's business, not the API's.
		{"https://a.example.test/wiki/File:%C3%85s.jpg", "File:Ås.jpg", true},
		// Titles have subpages, so the remainder is not split on slashes.
		{"https://a.example.test/wiki/Special:ListFiles/Someone", "Special:ListFiles/Someone", true},
		// A wiki whose pages sit at the root looks like every other site.
		{"https://a.example.test/Category:Some_bridges", "Category:Some bridges", false},
		{"https://a.example.test/", "", false},
	}
	for _, tc := range tests {
		u := mustParse(t, tc.raw)
		title, explicit := mediaWikiTitle(u)
		if title != tc.title || explicit != tc.explicit {
			t.Errorf("mediaWikiTitle(%s) = %q/%v, want %q/%v", tc.raw, title, explicit, tc.title, tc.explicit)
		}
	}
}

func TestMediaWikiKindOf(t *testing.T) {
	tests := map[string]mediaWikiKind{
		"Category:Bridges":      mediaWikiCategory,
		"category:bridges":      mediaWikiCategory,
		"File:A.jpg":            mediaWikiFilePage,
		"Image:A.jpg":           mediaWikiFilePage, // the pre-2005 alias
		"Special:ListFiles":     mediaWikiSpecial,
		"Bridge":                mediaWikiArticle,
		"Kategorie:Brücken":     mediaWikiArticle, // only the wiki can say
		"Bridge: The Reckoning": mediaWikiArticle,
		"Category:":             mediaWikiArticle, // a prefix with nothing behind it
	}
	for title, want := range tests {
		if got := mediaWikiKindOf(title); got != want {
			t.Errorf("mediaWikiKindOf(%q) = %d, want %d", title, got, want)
		}
	}
}

func TestMediaWikiName(t *testing.T) {
	tests := map[string]string{
		"File:Arch bridge.jpg": "Arch bridge.jpg",
		// A colon in the name itself: the namespace is what precedes the
		// first one, so the rest of the name has to survive.
		"File:Single: file.jpg": "Single: file.jpg",
		"Datei:Brücke.jpg":      "Brücke.jpg",
		"No namespace.jpg":      "No namespace.jpg",
	}
	for title, want := range tests {
		if got := mediaWikiName(title); got != want {
			t.Errorf("mediaWikiName(%q) = %q, want %q", title, got, want)
		}
	}
}

// TestMediaWikiSource covers the CDN rewrite and, as much, the hosts it must
// leave alone: appending a parameter to a signed or plain link elsewhere
// would be a change for nothing.
func TestMediaWikiSource(t *testing.T) {
	tests := []struct {
		raw   string
		want  string
		exact bool
	}{
		{
			raw:  "https://static.wikia.nocookie.net/w/images/b/bf/A.jpg/revision/latest?cb=2012",
			want: "https://static.wikia.nocookie.net/w/images/b/bf/A.jpg/revision/latest?cb=2012&format=original",
		},
		{
			// Already asked for: left as it stands rather than doubled.
			raw:  "https://images.wikia.nocookie.net/w/images/A.jpg?format=original",
			want: "https://images.wikia.nocookie.net/w/images/A.jpg?format=original",
		},
		{
			raw:   "https://upload.wikimedia.org/wikipedia/commons/a/ab/A.jpg?utm_source=x",
			want:  "https://upload.wikimedia.org/wikipedia/commons/a/ab/A.jpg?utm_source=x",
			exact: true,
		},
		{
			raw:   "https://a.example.test/images/A.jpg",
			want:  "https://a.example.test/images/A.jpg",
			exact: true,
		},
	}
	for _, tc := range tests {
		got, exact := mediaWikiSource(tc.raw)
		if got != tc.want || exact != tc.exact {
			t.Errorf("mediaWikiSource(%q) = %q/%v, want %q/%v", tc.raw, got, exact, tc.want, tc.exact)
		}
	}
}

func TestMediaWikiDepth(t *testing.T) {
	tests := map[string]int{
		"https://a.example.test/wiki/Category:A":          0,
		"https://a.example.test/wiki/Category:A?depth=2":  2,
		"https://a.example.test/wiki/Category:A?depth=-1": 0,
		"https://a.example.test/wiki/Category:A?depth=x":  0,
		// Bounded whatever was asked for: the graph has no bottom.
		"https://a.example.test/wiki/Category:A?depth=99": config.MaxFolderDepth,
	}
	for raw, want := range tests {
		u := mustParse(t, raw)
		if got := mediaWikiDepth(u); got != want {
			t.Errorf("mediaWikiDepth(%s) = %d, want %d", raw, got, want)
		}
	}
}

func TestMediaWikiScriptPath(t *testing.T) {
	tests := []struct {
		path string
		want string
		ok   bool
	}{
		{path: "/w/index.php", want: "/w/api.php", ok: true},
		{path: "/index.php/File:A.jpg", want: "/api.php", ok: true},
		{path: "/mediawiki/index.php", want: "/mediawiki/api.php", ok: true},
		{path: "/wiki/File:A.jpg"},
	}
	for _, tc := range tests {
		got, ok := mediaWikiScriptPath(tc.path)
		if got != tc.want || ok != tc.ok {
			t.Errorf("mediaWikiScriptPath(%q) = %q/%v, want %q/%v", tc.path, got, ok, tc.want, tc.ok)
		}
	}
}

func TestMediaWikiSpecialPage(t *testing.T) {
	tests := map[string]string{
		"Special:ListFiles":         "listfiles",
		"Special:ListFiles/Someone": "listfiles",
		"Special:New files":         "newfiles",
		"Special:Random":            "random",
	}
	for title, want := range tests {
		if got := mediaWikiSpecialPage(title); got != want {
			t.Errorf("mediaWikiSpecialPage(%q) = %q, want %q", title, got, want)
		}
	}
}

// TestMediaWikiPaceSpacesCallsOut covers the courtesy limit. Wikimedia has no
// key to throttle instead, so this is the only thing keeping the promise.
func TestMediaWikiPaceSpacesCallsOut(t *testing.T) {
	m := NewMediaWiki(nil)
	m.interval = 40 * time.Millisecond
	ctx := context.Background()

	start := time.Now()
	for range 3 {
		if err := m.pace(ctx, "https://a.example.test/w/api.php"); err != nil {
			t.Fatal(err)
		}
	}
	// Three calls, two gaps. The first goes out at once.
	if elapsed := time.Since(start); elapsed < 80*time.Millisecond {
		t.Errorf("three calls took %s, want at least two intervals between them", elapsed)
	}

	// Another wiki is not made to wait for this one's turn.
	start = time.Now()
	if err := m.pace(ctx, "https://b.example.test/w/api.php"); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > 20*time.Millisecond {
		t.Errorf("an unrelated wiki waited %s on another's pacing", elapsed)
	}
}
