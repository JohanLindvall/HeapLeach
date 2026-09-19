package extractor

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

// balbumsPageHTML builds a result page in the shape the site serves: a grid
// of cards, each an anchor wrapping a thumbnail, above the site's own
// navigation — which is on the index's own host and must never be followed,
// since following it is how a search becomes a walk of the whole catalogue.
func balbumsPageHTML(albumHost string, page, pages int, albums ...string) string {
	var b strings.Builder
	fmt.Fprintf(&b, `<html><head><title>Bunkr Albums</title></head><body>
<nav><a href="/">Home</a> <a href="/topalbums">Top albums</a> <a href="/?search=Something">Something</a></nav>
<div>results for <span>Driftwood</span> page <span>%d</span> of <span>%d</span></div>
<section class="grid">`, page, pages)
	for _, a := range albums {
		fmt.Fprintf(&b, `<a href="https://%s/a/%s" target="_blank" class="card">
			<img src="https://static.example.test/thumbs/%s.png" alt="">
			<h3>%s</h3><span>3 files</span></a>`, albumHost, a, a, a)
	}
	fmt.Fprintf(&b, `</section>
<section><div>Page %d of %d</div><a href="/?search=Driftwood&page=%d">Next</a></section>
</body></html>`, page, pages, page+1)
	return b.String()
}

// balbumsIndex serves pages and returns an extractor wired to a registry
// holding only itself and stub, so nothing in the test reaches a real host.
// The index claims the test server's own host, which is what lets the test
// prove the site's navigation is not followed.
func balbumsIndex(t *testing.T, stub Extractor, pages map[int]string) (*Balbums, string, func() []string) {
	t.Helper()

	var (
		mu    sync.Mutex
		asked []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		asked = append(asked, r.URL.RequestURI())
		mu.Unlock()
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		doc, ok := pages[page]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set(httpx.HeaderContentType, "text/html; charset=utf-8")
		_, _ = io.WriteString(w, doc)
	}))
	t.Cleanup(srv.Close)

	base, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	reg := &Registry{fallback: NewDirect(nil)}
	index := &Balbums{
		hostSet:  hostSet{base.Hostname()},
		client:   httpx.New("test-agent", "en-US", 0, 5*time.Second),
		registry: reg,
	}
	reg.extractors = []Extractor{index, stub}
	return index, srv.URL, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), asked...)
	}
}

func balbumsExtract(t *testing.T, index *Balbums, raw string) (*Result, error) {
	t.Helper()
	u, err := ParseURL(raw)
	if err != nil {
		t.Fatal(err)
	}
	return index.Extract(context.Background(), u, Options{})
}

func TestBalbumsSearchKeepsTheNarrowingAndDropsThePaging(t *testing.T) {
	cases := []struct{ raw, query, want string }{
		{
			raw:   "https://balbums.st/?search=Driftwood",
			query: "Driftwood",
			want:  "per=100&search=Driftwood",
		},
		// A pasted later page means the whole search, and the site's own
		// per-page choice is replaced by the one it honours.
		{
			raw:   "https://balbums.st/?search=Driftwood&mode=broad&per=20&sort=latest&page=7",
			query: "Driftwood",
			want:  "mode=broad&per=100&search=Driftwood&sort=latest",
		},
		// Somebody who narrowed the search before pasting it meant that.
		{
			raw:   "https://balbums.st/?search=A+Name&mode=exact",
			query: "A Name",
			want:  "mode=exact&per=100&search=A+Name",
		},
	}
	for _, tc := range cases {
		u, err := url.Parse(tc.raw)
		if err != nil {
			t.Fatal(err)
		}
		query, first, ok := balbumsSearch(u)
		if !ok {
			t.Errorf("balbumsSearch(%q) found no search", tc.raw)
			continue
		}
		if query != tc.query {
			t.Errorf("balbumsSearch(%q) query = %q, want %q", tc.raw, query, tc.query)
		}
		if first.RawQuery != tc.want {
			t.Errorf("balbumsSearch(%q) first page = %q, want %q", tc.raw, first.RawQuery, tc.want)
		}
	}
}

func TestBalbumsSearchRejectsEverythingElse(t *testing.T) {
	for _, raw := range []string{
		// The front page is the same grid over the whole catalogue.
		"https://balbums.st/",
		"https://balbums.st/?search=",
		"https://balbums.st/?search=%20",
		// Built in the browser, so an anonymous fetch sees no links at all.
		"https://balbums.st/topalbums",
		"https://balbums.st/live",
	} {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, ok := balbumsSearch(u); ok {
			t.Errorf("balbumsSearch(%q) claimed a search", raw)
		}
	}
}

func TestBalbumsPageNamesOnePageOfTheResults(t *testing.T) {
	u, err := url.Parse("https://balbums.st/?search=Driftwood&mode=broad")
	if err != nil {
		t.Fatal(err)
	}
	_, first, ok := balbumsSearch(u)
	if !ok {
		t.Fatal("no search")
	}
	want := "https://balbums.st/?mode=broad&page=3&per=100&search=Driftwood"
	if got := balbumsPage(first, 3); got != want {
		t.Errorf("balbumsPage = %q, want %q", got, want)
	}
}

// The whole route: every page of results walked, each album resolved by the
// host that has it, one folder per album, and the index's own navigation
// left alone.
func TestBalbumsWalksEveryPageAndFoldersEachAlbum(t *testing.T) {
	const albumHost = "albums.example.test"
	stub := &linksStub{host: albumHost, files: 2, title: "Album"}
	index, root, asked := balbumsIndex(t, stub, map[int]string{
		1: balbumsPageHTML(albumHost, 1, 2, "AAAA", "BBBB"),
		2: balbumsPageHTML(albumHost, 2, 2, "CCCC"),
	})

	res, err := balbumsExtract(t, index, root+"/?search=Driftwood")
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if res.Title != "Driftwood" {
		t.Errorf("title = %q, want the query", res.Title)
	}
	if len(res.Files) != 6 {
		t.Fatalf("got %d files, want 6 (three albums of two): %+v", len(res.Files), res.Files)
	}
	// Page order, whatever order the requests finished in, and one folder
	// per album.
	for i, want := range []string{
		"Album a_AAAA", "Album a_AAAA", "Album a_BBBB", "Album a_BBBB", "Album a_CCCC", "Album a_CCCC",
	} {
		if res.Files[i].Dir != want {
			t.Errorf("file[%d] landed in %q, want %q", i, res.Files[i].Dir, want)
		}
	}
	// A signed host resolves per attempt, and that closure has to survive
	// being merged into the search's result.
	if res.Files[0].Resolve == nil {
		t.Fatal("the file lost its resolver")
	}

	got := asked()
	if len(got) != 2 {
		t.Errorf("asked for %d pages, want 2: %v", len(got), got)
	}
	for _, uri := range got {
		if !strings.Contains(uri, "per=100") {
			t.Errorf("asked %q without the page size the site honours", uri)
		}
		if strings.Contains(uri, "page=3") {
			t.Errorf("asked past the last page the search stated: %s", uri)
		}
		if strings.Contains(uri, "search=Something") || strings.Contains(uri, "topalbums") {
			t.Errorf("followed the index's own navigation: %s", uri)
		}
	}
}

// A search wider than anybody meant stops at the cap, and says so where the
// job is named rather than quietly returning a prefix.
func TestBalbumsCapsTheAlbumsItFollows(t *testing.T) {
	const albumHost = "albums.example.test"
	stub := &linksStub{host: albumHost, files: 1, title: "Album"}

	pages := make(map[int]string)
	for page := 1; page <= 8; page++ {
		albums := make([]string, 0, 100)
		for i := range 100 {
			albums = append(albums, fmt.Sprintf("p%02dn%02d", page, i))
		}
		pages[page] = balbumsPageHTML(albumHost, page, 8, albums...)
	}
	index, root, asked := balbumsIndex(t, stub, pages)

	res, err := balbumsExtract(t, index, root+"/?search=Driftwood")
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if len(res.Files) != maxExpandedSources {
		t.Errorf("got %d files, want the cap of %d", len(res.Files), maxExpandedSources)
	}
	if want := fmt.Sprintf("%d of 800 albums", maxExpandedSources); !strings.Contains(res.Title, want) {
		t.Errorf("title = %q, want it to admit %q", res.Title, want)
	}
	// The walk stops at the cap rather than reading eight pages to throw
	// three away.
	if got := asked(); len(got) != 5 {
		t.Errorf("asked for %d pages, want 5 (the cap reached on the fifth): %v", len(got), got)
	}
}

func TestBalbumsReportsASearchWithNothingInIt(t *testing.T) {
	stub := &linksStub{host: "albums.example.test", files: 1, title: "Album"}
	index, root, _ := balbumsIndex(t, stub, map[int]string{
		1: `<html><body><div>results for <span>nothing</span></div>
			<div>Page 1 of 0</div></body></html>`,
	})

	_, err := balbumsExtract(t, index, root+"/?search=nothing")
	if err == nil {
		t.Fatal("an empty search was accepted")
	}
	if !strings.Contains(err.Error(), "nothing") {
		t.Errorf("error = %q, want it to name the query", err)
	}
}

func TestBalbumsRefusesAPageThatIsNotASearch(t *testing.T) {
	stub := &linksStub{host: "albums.example.test", files: 1, title: "Album"}
	index, root, asked := balbumsIndex(t, stub, map[int]string{1: "<html></html>"})

	_, err := balbumsExtract(t, index, root+"/topalbums")
	if err == nil {
		t.Fatal("a non-search page was accepted")
	}
	if !strings.Contains(err.Error(), "search") {
		t.Errorf("error = %q, want it to say what to paste instead", err)
	}
	if got := asked(); len(got) != 0 {
		t.Errorf("fetched %v for a page that names no search", got)
	}
}
