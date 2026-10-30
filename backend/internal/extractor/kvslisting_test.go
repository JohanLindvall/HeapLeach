// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

// A listing that pages in script, as a search does on every install seen
// and the member listing does on some. The "next" control is rendered but
// its anchor points at "#", so there is nothing to follow, and the page also
// carries a second listing — the albums matching the query — with a block
// and a pager of its own, placed FIRST so that anything choosing either by
// position picks the wrong one. Its pager says the albums end on this page,
// which must not be taken to mean the videos do.
const kvsScriptedListing = `<!DOCTYPE html><html><head>
<title>Search - Example Tube</title>
</head><body>
<nav><a href="/">Home</a> <a href="/categories/">Categories</a> <a href="/search/other-query/">Other</a></nav>
<h1>Videos for: some-query</h1>
<div class="box" id="list_albums_albums_list_search_result" data-block-id="list_albums_albums_list_search_result">
  <a href="/albums/an-album/" class="item">An album</a>
  <div class="pagination" id="list_albums_albums_list_search_result_pagination"><ul>
    <li class="prev"><span>Back</span></li>
    <li class="page-current"><span class="pages-selector">1</span></li>
    <li class="next"><span class="pages-selector">Next</span></li>
  </ul></div>
</div>
<div class="box" id="list_videos_videos_list_search_result" data-block-id="list_videos_videos_list_search_result">
  <a href="/video/first-clip/" class="item">First</a>
  <a href="/video/second-clip/" class="item">Second</a>
  <div class="pagination" id="list_videos_videos_list_search_result_pagination"><ul>
    <li class="prev"><span>Back</span></li>
    <li class="page-current"><span class="pages-selector">1</span></li>
    <li class="page"><a href="#search" data-action="ajax" data-block-id="list_videos_videos_list_search_result" data-parameters="q:some-query;from_videos+from_albums:02" class="pages-selector">2</a></li>
    <li class="next"><a href="#search" data-action="ajax" data-block-id="list_videos_videos_list_search_result" data-parameters="q:some-query;from_videos+from_albums:2">Next</a></li>
  </ul></div>
</div>
</body></html>`

// The last page, as the block request returns it: a bare fragment whose
// pager renders the "next" item with a span where the anchor was.
const kvsScriptedLastPage = `<div class="box" id="list_videos_videos_list_search_result" data-block-id="list_videos_videos_list_search_result">
  <a href="/video/third-clip/" class="item">Third</a>
  <div class="pagination" id="list_videos_videos_list_search_result_pagination"><ul>
    <li class="prev"><a href="#search" data-action="ajax" data-parameters="q:some-query;from_videos+from_albums:1">Back</a></li>
    <li class="page"><a href="#search" data-action="ajax" data-parameters="q:some-query;from_videos+from_albums:01" class="pages-selector">1</a></li>
    <li class="page-current"><span class="pages-selector">2</span></li>
    <li class="next"><span class="pages-selector">Next</span></li>
  </ul></div>
</div>`

// kvsScriptedVideo is the page behind one tile, signed rather than
// scrambled, named for its slug so the order of the result can be checked.
func kvsScriptedVideo(slug string) string {
	title := strings.ReplaceAll(slug, "-", " ")
	return `<html><head><title>` + title + ` - Example Tube</title></head><body>
<script>var flashvars = { video_id: '1', video_title: '` + title + `', license_code: '` + kvsLicense + `', ` +
		`video_url: 'https://tube.example.test/get_file/4/0123456789abcdef0123456789abcdef/3000/1/` + slug + `.mp4/?v-acctoken=MTIzfDF8MHxhYmM', ` +
		`postfix: '.mp4' };</script></body></html>`
}

// A scripted control is not a page to follow: the walk must neither follow
// "#" back to where it is nor read the control's presence as the end.
func TestKVSNextPageIgnoresAScriptedControl(t *testing.T) {
	const base = "https://tube.example.test/search/some-query/"
	root, err := parseHTML(kvsScriptedListing)
	if err != nil {
		t.Fatal(err)
	}
	if got := kvsNextPage(root, base); got != "" {
		t.Errorf("next page = %q, want none: the anchor points at \"#\"", got)
	}
	if kvsListingEnds(root) {
		t.Error("the first page of a scripted listing was read as the last")
	}
}

// The last page of a scripted listing is announced in its pager, and that
// is the only signal there is: asking for one more page is a 404.
func TestKVSListingEndsOnTheLastScriptedPage(t *testing.T) {
	root, err := parseHTML(kvsScriptedLastPage)
	if err != nil {
		t.Fatal(err)
	}
	if !kvsListingEnds(root) {
		t.Error("a pager whose next item holds no anchor was not read as the end")
	}
	if got := kvsNextPage(root, "https://tube.example.test/search/some-query/"); got != "" {
		t.Errorf("next page on the last page = %q, want none", got)
	}
}

// The themes that page by URL say nothing in the pager on the last page —
// the next control is simply absent — and that absence is not the end
// signal here, since a theme without any next control looks the same. The
// walk asks for one more page on those, as it always has.
func TestKVSListingEndsSaysNothingWithoutANextControl(t *testing.T) {
	for name, doc := range map[string]string{
		"first page": kvsMemberListing,
		"last page":  kvsMemberLastPage,
		"no pager":   `<html><body><a href="/videos/1/x/">x</a></body></html>`,
		"a carousel": `<html><body><div class="next"><span>›</span></div><a href="/videos/1/x/">x</a></body></html>`,
	} {
		root, err := parseHTML(doc)
		if err != nil {
			t.Fatal(err)
		}
		if kvsListingEnds(root) {
			t.Errorf("%s: read as the last page", name)
		}
	}
}

// Both the block and the pager are chosen by name: the albums block comes
// first in the fixture, and its pager says the albums end on page one.
func TestKVSListingPrefersTheVideosBlockAndPager(t *testing.T) {
	root, err := parseHTML(kvsScriptedListing)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := kvsListingBlock(root), "list_videos_videos_list_search_result"; got != want {
		t.Errorf("kvsListingBlock = %q, want %q", got, want)
	}
	if got, want := attr(kvsPager(root), "id"), "list_videos_videos_list_search_result_pagination"; got != want {
		t.Errorf("kvsPager chose %q, want %q", got, want)
	}
}

// The whole route, against a server standing in for the install: a search
// pasted as its third page resolves to the entire search, the walk asks
// for page two the way the site's script does, stops on the page that says
// it is the last rather than asking for a third, and every tile is resolved
// to its signed link in the listing's own order.
func TestKVSSearchResolvesEveryPageOfResults(t *testing.T) {
	var (
		mu    sync.Mutex
		asked []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		asked = append(asked, r.URL.RequestURI())
		mu.Unlock()
		q := r.URL.Query()
		switch {
		case r.URL.Path == "/search/some-query/" && q.Get("mode") == "":
			io.WriteString(w, kvsScriptedListing)
		case r.URL.Path == "/search/some-query/" && q.Get("mode") == "async":
			if q.Get("function") != "get_block" ||
				q.Get("block_id") != "list_videos_videos_list_search_result" ||
				q.Get("from_videos") != "2" {
				http.NotFound(w, r)
				return
			}
			io.WriteString(w, kvsScriptedLastPage)
		case strings.HasPrefix(r.URL.Path, "/video/"):
			io.WriteString(w, kvsScriptedVideo(strings.Trim(strings.TrimPrefix(r.URL.Path, "/video/"), "/")))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	k := NewKVS(httpx.New("test-agent", "en-US", 0, 5*time.Second))
	u, err := ParseURL(srv.URL + "/search/some-query/3/")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	res, err := k.Extract(ctx, u, Options{})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if res.Title != "some-query" {
		t.Errorf("title = %q, want the query", res.Title)
	}
	want := []string{"first clip.mp4", "second clip.mp4", "third clip.mp4"}
	if len(res.Files) != len(want) {
		t.Fatalf("got %d files, want %d: %+v", len(res.Files), len(want), res.Files)
	}
	for i, f := range res.Files {
		if f.Name != want[i] {
			t.Errorf("file %d = %q, want %q", i, f.Name, want[i])
		}
		if !strings.Contains(f.URL, "?v-acctoken=") {
			t.Errorf("file %d lost its token: %s", i, f.URL)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if asked[0] != "/search/some-query/" {
		t.Errorf("the walk began at %q, want the whole search", asked[0])
	}
	for _, uri := range asked {
		if strings.Contains(uri, "from_videos=3") {
			t.Errorf("asked for a page past the one that said it was last: %s", uri)
		}
		if strings.HasPrefix(uri, "/albums/") {
			t.Errorf("opened an album as if it were a video: %s", uri)
		}
	}
}

// The walk's end conditions, each where the listing itself says it ends:
// none of them is an error once the first page has been read. The stated
// total is checked ahead of the cap, so a page that reaches the total keeps
// every tile it has.
func TestKVSListingPagesEnds(t *testing.T) {
	page := func(showing, next string, slugs ...string) string {
		var b strings.Builder
		b.WriteString(`<h1>Example Videos</h1>` + showing + `<div id="list_videos_common">`)
		for _, slug := range slugs {
			b.WriteString(`<a href="/video/` + slug + `/">` + slug + `</a>`)
		}
		b.WriteString(`</div>`)
		if next != "" {
			b.WriteString(`<div class="pagination"><ul><li class="next"><a href="` + next + `">Next</a></li></ul></div>`)
		}
		return b.String()
	}
	for _, tc := range []struct {
		name  string
		pages map[string]string
		limit int
		want  int
		never string
	}{
		{"stated total reached", map[string]string{
			"/list/":   page("Showing 1 - 2 of 3", "/list/2/", "a", "b"),
			"/list/2/": page("", "/list/3/", "c"),
			"/list/3/": page("", "", "d"),
		}, 0, 3, "/list/3/"},
		{"missing page ends it", map[string]string{
			"/list/": page("", "/list/2/", "a", "b"),
		}, 0, 2, ""},
		{"page adding nothing ends it", map[string]string{
			"/list/":   page("", "/list/2/", "a", "b"),
			"/list/2/": page("", "/list/3/", "a", "b"),
			"/list/3/": page("", "", "c"),
		}, 0, 2, "/list/3/"},
		{"last page says so", map[string]string{
			"/list/":   page("", "/list/2/", "a", "b"),
			"/list/2/": page("", "", "c") + `<div class="pagination"><ul><li class="next"><span>Next</span></li></ul></div>`,
		}, 0, 3, ""},
		{"cap truncates", map[string]string{
			"/list/":   page("", "/list/2/", "a", "b"),
			"/list/2/": page("", "/list/3/", "c", "d"),
			"/list/3/": page("", "", "e"),
		}, 3, 3, "/list/3/"},
		{"total ahead of the cap", map[string]string{
			"/list/":   page("Showing 1 - 2 of 4", "/list/2/", "a", "b"),
			"/list/2/": page("", "/list/3/", "c", "d"),
		}, 3, 4, "/list/3/"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var (
				mu    sync.Mutex
				asked []string
			)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				asked = append(asked, r.URL.RequestURI())
				mu.Unlock()
				if doc, ok := tc.pages[r.URL.RequestURI()]; ok {
					io.WriteString(w, doc)
					return
				}
				http.NotFound(w, r)
			}))
			defer srv.Close()
			l := kvsListingForPage(srv.URL + "/list/")
			l.limit = tc.limit
			pages, title, err := kvsListingPages(context.Background(), httpx.New("test-agent", "en-US", 0, 5*time.Second), l)
			if err != nil || len(pages) != tc.want || title != "Example" {
				t.Fatalf("pages=%d title=%q err=%v, want %d", len(pages), title, err, tc.want)
			}
			mu.Lock()
			defer mu.Unlock()
			for _, uri := range asked {
				if tc.never != "" && uri == tc.never {
					t.Errorf("asked past the end: %s", uri)
				}
			}
		})
	}
}
