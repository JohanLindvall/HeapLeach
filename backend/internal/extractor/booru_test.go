package extractor

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

func testBooru(t *testing.T) *Booru {
	t.Helper()
	return &Booru{client: httpx.New("test-agent", "en-US", 0, 5*time.Second)}
}

// ------------------------------------------------------------- szurubooru

// A szurubooru page, shaped like the real one. Two things in it are traps:
// contentUrl is a path relative to the site root with no leading slash to
// say so, and thumbnailUrl sits right beside it looking just as much like a
// file.
const szurubooruPage = `{
  "query": "a_tag",
  "offset": 0,
  "limit": 100,
  "total": 2,
  "results": [
    {
      "id": 93155,
      "type": "image",
      "mimeType": "image/jpeg",
      "checksum": "6fe0de471b74c14b4afcd842f7ba1f9b386c72d1",
      "fileSize": 104913,
      "canvasWidth": 620,
      "canvasHeight": 309,
      "contentUrl": "data/posts/93155_f7817490558c9fe6.jpg",
      "thumbnailUrl": "data/generated-thumbnails/93155_f7817490558c9fe6.jpg",
      "tags": [{"names": ["a_tag"], "category": "general", "usages": 12}]
    },
    {
      "id": 93156,
      "type": "animation",
      "mimeType": "image/gif",
      "fileSize": 2200000,
      "contentUrl": "data/posts/93156_0011223344556677.gif",
      "thumbnailUrl": "data/generated-thumbnails/93156_0011223344556677.jpg",
      "tags": []
    }
  ]
}`

// TestSzurubooruFetch covers the family end to end, including the header
// without which the server answers 406 rather than JSON.
func TestSzurubooruFetch(t *testing.T) {
	var (
		asked  string
		accept string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked, accept = r.URL.RequestURI(), r.Header.Get(httpx.HeaderAccept)
		if !strings.Contains(accept, "application/json") {
			// What the real server does, and the reason apiHeaders exists.
			w.WriteHeader(http.StatusNotAcceptable)
			return
		}
		_, _ = io.WriteString(w, szurubooruPage)
	}))
	t.Cleanup(srv.Close)

	site := &booruSite{name: "szuru", root: srv.URL, api: apiSzurubooru, domains: []string{"example.test"}}
	files, err := testBooru(t).fetch(context.Background(), site, "a_tag", "")
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}

	if want := "/api/posts/?limit=100&offset=0&query=a_tag"; asked != want {
		t.Errorf("asked for %q, want %q", asked, want)
	}
	if len(files) != 2 {
		t.Fatalf("got %d files, want 2", len(files))
	}

	first := files[0]
	if want := srv.URL + "/data/posts/93155_f7817490558c9fe6.jpg"; first.URL != want {
		t.Errorf("url = %q, want the relative content path resolved against the site root", first.URL)
	}
	if first.Name != "93155.jpg" {
		t.Errorf("name = %q, want the post id and the file's own extension", first.Name)
	}
	// The board states a real byte count, so it is exact and a file already
	// on disk at that length may be skipped.
	if first.Size != 104913 || first.SizeApprox {
		t.Errorf("size = %d approx = %v, want the exact 104913", first.Size, first.SizeApprox)
	}
	if files[1].Name != "93156.gif" {
		t.Errorf("second name = %q, want the animation's own extension", files[1].Name)
	}
	// The file requests must not carry the API's Accept header.
	if _, ok := first.Headers[httpx.HeaderAccept]; ok {
		t.Error("the image request asks for JSON")
	}
}

// TestSzurubooruSinglePostDoesNotPage pins that a post link resolves in one
// request: the id search matches exactly one post, and asking for a second
// page would only repeat it.
func TestSzurubooruSinglePostDoesNotPage(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		_, _ = io.WriteString(w, szurubooruPage)
	}))
	t.Cleanup(srv.Close)

	site := &booruSite{name: "szuru", root: srv.URL, api: apiSzurubooru, domains: []string{"example.test"}}
	if _, err := testBooru(t).fetch(context.Background(), site, "id:93155", "93155"); err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if calls != 1 {
		t.Errorf("made %d requests for one post, want 1", calls)
	}
}

// ----------------------------------------------------------- Gelbooru 0.1

// A listing page shaped like a board's, cut to what is read. The hosts are
// synthetic; the layout is not. Three of the images here are not posts — the
// board's own header, an advert, and a tag-sidebar icon — and every one of
// them would be queued as one by anything that just collected <img> tags.
const gelbooru01ListPage = `<!DOCTYPE html><html><head><title>A Board</title></head><body>
<div id="header"><img src="https://static.example.test/logo.png" alt="banner"></div>
<div id="content"><div id="post-list">
<div class="sidebar"><ul><li><img src="https://static.example.test/tag.gif"> a tag 1234</li></ul></div>
<div class="content">
<div class="divTable"><img src="https://ads.example.test/banner.jpg" alt="advert"></div>
<span class="thumb"><a id="p159110" href="index.php?page=post&amp;s=view&amp;id=159110"><img src="https://thumbs.example.test/aboard/thumbnails//158/thumbnail_1e76e301bf6babe256a0d98b1ba2efcaa6aab475.jpg" alt="post" title=" dress long_hair score:0 rating:Safe"/></a>
<script type="text/javascript">posts[159110] = {'tags':'dress long_hair'.split(/ /g)}</script></span>
<span class="thumb"><a id="p159109" href="index.php?page=post&amp;s=view&amp;id=159109"><img src="https://thumbs.example.test/aboard/thumbnails//158/thumbnail_959e6811c4df2c65dff8c4569d71daae0051aae6.png" alt="post" title=" blue_eyes score:0 rating:Safe"/></a></span>
<span class="thumb"><a id="p159108" href="index.php?page=post&amp;s=view&amp;id=159108"><img src="https://thumbs.example.test/aboard/thumbnails//64/thumbnail_905a7970b1665881d7da76479b97680ff3cfb1a3.gif" alt="post" title=" animated_gif score:0 rating:Safe"/></a></span>
</div>
<div id="paginator"><b>1</b><a href="?page=post&amp;s=list&amp;tags=all&amp;pid=20">2</a></div>
</div></div></body></html>`

// A post's own page. The image to take is the one the board marks; the others
// are the "similar posts" strip, which is thumbnails of other posts.
const gelbooru01PostPage = `<!DOCTYPE html><html><head><title>A Board</title></head><body>
<div id="content">
<img alt="img" src="https://img.example.test/aboard//images/158/1e76e301bf6babe256a0d98b1ba2efcaa6aab475.jpg" id="image" onclick="Note.toggle();"/>
<div id="similar"><span class="thumb"><a id="p159000" href="index.php?page=post&amp;s=view&amp;id=159000"><img src="https://thumbs.example.test/aboard/thumbnails//158/thumbnail_ffffffffffffffffffffffffffffffffffffffff.jpg"/></a></span></div>
</div></body></html>`

func TestGelbooru01Listing(t *testing.T) {
	var asked string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = r.URL.RequestURI()
		_, _ = io.WriteString(w, gelbooru01ListPage)
	}))
	t.Cleanup(srv.Close)

	site := &booruSite{name: "aboard", root: srv.URL, api: apiGelbooru01, domains: []string{"example.test"}}
	files, err := testBooru(t).fetch(context.Background(), site, "dress", "")
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}

	if want := "/index.php?page=post&pid=0&s=list&tags=dress"; asked != want {
		t.Errorf("asked for %q, want %q", asked, want)
	}
	if len(files) != 3 {
		t.Fatalf("got %d files, want the 3 posts and none of the furniture", len(files))
	}
	if want := "https://img.example.test/aboard/images/158/1e76e301bf6babe256a0d98b1ba2efcaa6aab475.jpg"; files[0].URL != want {
		t.Errorf("url = %q, want %q", files[0].URL, want)
	}
	if files[0].Name != "159110.jpg" {
		t.Errorf("name = %q, want the post id and the image's extension", files[0].Name)
	}
	// An animation keeps its own extension, which is what makes deriving the
	// original from the thumbnail safe.
	if !strings.HasSuffix(files[2].URL, ".gif") || files[2].Name != "159108.gif" {
		t.Errorf("third file = %q at %q, want the gif", files[2].Name, files[2].URL)
	}
	// A listing states no size, and a guess would be worse than none.
	if files[0].Size != -1 {
		t.Errorf("size = %d, want -1 for a length the board never stated", files[0].Size)
	}
}

// TestGelbooru01ListingStopsAtAShortPage guards the paging arithmetic: a
// board's page holds twenty, so a page holding fewer is the last one. Getting
// this wrong against the API families' hundred would page forever.
func TestGelbooru01ListingStopsAtAShortPage(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		_, _ = io.WriteString(w, gelbooru01ListPage)
	}))
	t.Cleanup(srv.Close)

	site := &booruSite{name: "aboard", root: srv.URL, api: apiGelbooru01, domains: []string{"example.test"}}
	if _, err := testBooru(t).fetch(context.Background(), site, "dress", ""); err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if calls != 1 {
		t.Errorf("made %d requests for a page of 3 posts, want 1", calls)
	}
}

// TestGelbooru01KeepsWhatItGot covers a board that stops answering partway
// through a long walk, which is how these end in practice: the rate limit
// runs out and the next page is a 429. What was already read is the job.
func TestGelbooru01KeepsWhatItGot(t *testing.T) {
	var full strings.Builder
	full.WriteString(`<html><body><div class="content">`)
	for i := range gelbooru01PageSize {
		fmt.Fprintf(&full, `<span class="thumb"><a id="p%d" href="index.php?page=post&s=view&id=%d">`+
			`<img src="https://thumbs.example.test/aboard/thumbnails//1/thumbnail_%02d.jpg"></a></span>`,
			1000+i, 1000+i, i)
	}
	full.WriteString(`</div></body></html>`)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("pid") == "0" {
			_, _ = io.WriteString(w, full.String())
			return
		}
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	t.Cleanup(srv.Close)

	site := &booruSite{name: "aboard", root: srv.URL, api: apiGelbooru01, domains: []string{"example.test"}}
	files, err := testBooru(t).fetch(context.Background(), site, "dress", "")
	if err != nil {
		t.Fatalf("a refused second page threw away the first: %v", err)
	}
	if len(files) != gelbooru01PageSize {
		t.Errorf("kept %d files, want the %d the board did answer with", len(files), gelbooru01PageSize)
	}
}

// TestBooruFirstPageFailureIsAnError is the other half: nothing read at all
// is a failure, not an empty job.
func TestBooruFirstPageFailureIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	t.Cleanup(srv.Close)

	site := &booruSite{name: "aboard", root: srv.URL, api: apiGelbooru01, domains: []string{"example.test"}}
	if _, err := testBooru(t).fetch(context.Background(), site, "dress", ""); err == nil {
		t.Fatal("a board that answered nothing resolved to something")
	}
}

func TestGelbooru01SinglePost(t *testing.T) {
	var asked string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = r.URL.RequestURI()
		_, _ = io.WriteString(w, gelbooru01PostPage)
	}))
	t.Cleanup(srv.Close)

	site := &booruSite{name: "aboard", root: srv.URL, api: apiGelbooru01, domains: []string{"example.test"}}
	files, err := testBooru(t).fetch(context.Background(), site, "id:159110", "159110")
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if want := "/index.php?id=159110&page=post&s=view"; asked != want {
		t.Errorf("asked for %q, want %q", asked, want)
	}
	if len(files) != 1 {
		t.Fatalf("got %d files, want only the post's own image", len(files))
	}
	if want := "https://img.example.test/aboard//images/158/1e76e301bf6babe256a0d98b1ba2efcaa6aab475.jpg"; files[0].URL != want {
		t.Errorf("url = %q, want the image the page marks as the post's", files[0].URL)
	}
}

func TestGelbooru01Image(t *testing.T) {
	cases := map[string]string{
		// The board writes the doubled separator; the rebuilt URL does not
		// need to, and the host serves either.
		"https://thumbs.example.test/aboard/thumbnails//158/thumbnail_abc.jpg": "https://img.example.test/aboard/images/158/abc.jpg",
		"https://thumbs.example.test/aboard/thumbnails/158/thumbnail_abc.png":  "https://img.example.test/aboard/images/158/abc.png",
		// Not thumbnails: the board's furniture and an advert.
		"https://static.example.test/logo.png":                      "",
		"https://thumbs.example.test/aboard/other/158/abc.jpg":      "",
		"https://thumbs.example.test/aboard/thumbnails/158/abc.jpg": "",
		"https://img.example.test/aboard/images/158/abc.jpg":        "",
		"/relative/thumbnails/158/thumbnail_abc.jpg":                "",
		"://not a url": "",
	}
	for thumb, want := range cases {
		if got := gelbooru01Image(thumb); got != want {
			t.Errorf("gelbooru01Image(%q) = %q, want %q", thumb, got, want)
		}
	}
}

// TestBooruMultiTenantSite covers the one entry that stands for hundreds of
// boards: everything it needs comes from the link.
func TestBooruMultiTenantSite(t *testing.T) {
	u, err := ParseURL("https://example.booru.org/index.php?page=post&s=list&tags=dress")
	if err != nil {
		t.Fatal(err)
	}
	site := siteFor(u)
	if site == nil {
		t.Fatal("a booru.org board was not matched")
	}
	board := site.forURL(u)
	if board.root != "https://example.booru.org" {
		t.Errorf("root = %q, want the board the link named", board.root)
	}
	if board.name != "example" {
		t.Errorf("name = %q, want the board's own label", board.name)
	}
	// The table entry itself must be left alone, or the next board inherits
	// this one's root.
	if site.root != "" {
		t.Errorf("the shared entry was modified: root = %q", site.root)
	}
}

func TestBooruParseTargetNewFamilies(t *testing.T) {
	tests := []struct {
		raw         string
		api         booruAPI
		wantTags    string
		wantPost    string
		wantListing bool
	}{
		{raw: "https://example.test/posts?query=a_tag", api: apiSzurubooru, wantTags: "a_tag", wantListing: true},
		{raw: "https://example.test/posts", api: apiSzurubooru, wantListing: true},
		{raw: "https://example.test/post/93155", api: apiSzurubooru, wantPost: "93155", wantListing: true},
		{raw: "https://example.test/", api: apiSzurubooru},
		{raw: "https://example.test/index.php?page=post&s=list&tags=dress", api: apiGelbooru01,
			wantTags: "dress", wantListing: true},
		{raw: "https://example.test/index.php?page=post&s=view&id=159110", api: apiGelbooru01,
			wantPost: "159110", wantListing: true},
		// A bare board front page is a mis-paste, not a request for
		// everything it has.
		{raw: "https://example.test/index.php", api: apiGelbooru01},
	}
	for _, tc := range tests {
		u, err := ParseURL(tc.raw)
		if err != nil {
			t.Fatal(err)
		}
		site := &booruSite{name: "test", root: "https://example.test", api: tc.api}
		tags, post, listing := site.parseTarget(u)
		if tags != tc.wantTags || post != tc.wantPost || listing != tc.wantListing {
			t.Errorf("parseTarget(%s) = %q, %q, %v; want %q, %q, %v",
				tc.raw, tags, post, listing, tc.wantTags, tc.wantPost, tc.wantListing)
		}
	}
}

func TestBooruMatchCoversTheNewBoards(t *testing.T) {
	b := NewBooru(nil)
	for _, raw := range []string{
		"https://snootbooru.com/posts?query=a_tag",
		"https://booru.foalcon.com/post/1",
		"https://example.booru.org/index.php?page=post&s=list",
		"https://second.booru.org/index.php?page=post&s=view&id=1",
	} {
		u, err := ParseURL(raw)
		if err != nil {
			t.Fatal(err)
		}
		if !b.Match(u) {
			t.Errorf("Match(%q) = false", raw)
		}
	}
	other, _ := ParseURL("https://booru.org.example.test/index.php")
	if b.Match(other) {
		t.Error("matched a lookalike host")
	}
}
