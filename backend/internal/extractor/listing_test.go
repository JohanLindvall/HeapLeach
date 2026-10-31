// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// testListingPage reads the synthetic listing listing_test serves: links
// with class=item, a rel=next pager, and markers for a missing or ended
// listing.
func testListingPage(root *html.Node, page, _ *url.URL) listingPage {
	if findFirst(root, func(n *html.Node) bool { return attr(n, "id") == "end" }) != nil {
		return listingPage{end: true}
	}
	list := findFirst(root, func(n *html.Node) bool { return attr(n, "id") == "list" })
	if list == nil {
		return listingPage{missing: true}
	}
	p := listingPage{title: firstText(root, atom.H1)}
	for _, a := range findAll(list, func(n *html.Node) bool { return isElem(n, atom.A) }) {
		if link := webLink(page, attr(a, "href")); link != nil {
			p.links = append(p.links, listingLink{key: link.Path, url: link.String()})
		}
	}
	if a := findFirst(root, func(n *html.Node) bool { return attr(n, "rel") == "next" }); a != nil {
		p.next = webLink(page, attr(a, "href"))
	}
	return p
}

func TestWalkListing(t *testing.T) {
	// Each case serves page n of /list from body(n): "" is a 404, and
	// "redirect" sends the request to another listing.
	listPage := func(next string, items ...string) string {
		var b strings.Builder
		b.WriteString(`<h1>Synthetic Listing</h1><div id="list">`)
		for _, item := range items {
			fmt.Fprintf(&b, `<a href="/item/%s">%s</a>`, item, item)
		}
		b.WriteString(`</div>`)
		if next != "" {
			fmt.Fprintf(&b, `<a rel="next" href="%s">Next</a>`, next)
		}
		return b.String()
	}
	for _, tc := range []struct {
		name   string
		body   func(n int) string
		limit  int
		items  int
		note   string
		failed string
		// quiet walks the listing as one that ends where it stops, with a
		// failed hook naming a rate limit.
		quiet bool
		// cancel is the page whose request cancels the walk as it is
		// answered.
		cancel int
	}{
		{name: "complete", items: 3, body: func(n int) string {
			if n == 1 {
				return listPage("?page=2", "a", "b")
			}
			return listPage("", "b", "c")
		}},
		{name: "ended", items: 2, body: func(n int) string {
			if n == 1 {
				return listPage("?page=2", "a", "b")
			}
			return `<div id="end"></div>`
		}},
		{name: "page limit", items: config.MaxAlbumPages, note: "partial — page limit", body: func(n int) string {
			return listPage("?page="+strconv.Itoa(n+1), strconv.Itoa(n))
		}},
		{name: "repeated page", items: 2, note: "partial — repeated page", body: func(n int) string {
			if n == 1 {
				return listPage("?page=2", "a")
			}
			return listPage("/list", "b")
		}},
		{name: "repeated or empty", items: 1, note: "partial — repeated or empty page", body: func(int) string {
			return listPage("?page=2", "a")
		}},
		{name: "fetch failed", items: 1, note: "partial — could not fetch next page", body: func(n int) string {
			if n == 1 {
				return listPage("?page=2", "a")
			}
			return ""
		}},
		{name: "missing", items: 1, note: "partial — listing is missing on next page", body: func(n int) string {
			if n == 1 {
				return listPage("?page=2", "a")
			}
			return "<p>gone</p>"
		}},
		{name: "redirected", items: 1, note: "partial — next page redirected to another listing", body: func(n int) string {
			if n == 1 {
				return listPage("?page=2", "a")
			}
			return "redirect"
		}},
		{name: "capped mid-page", limit: 2, items: 2, note: "partial — source limit", body: func(int) string {
			return listPage("?page=2", "a", "b", "c")
		}},
		{name: "capped at page end", limit: 2, items: 2, note: "partial — source limit", body: func(n int) string {
			return listPage("?page="+strconv.Itoa(n+1), "a", "b")
		}},
		{name: "exact cap at the end", limit: 2, items: 2, body: func(int) string {
			return listPage("", "a", "b")
		}},
		{name: "quiet end at a missing page", quiet: true, items: 1, body: func(n int) string {
			if n == 1 {
				return listPage("?page=2", "a")
			}
			return ""
		}},
		{name: "quiet rate limit", quiet: true, items: 1, note: "partial — rate limited", body: func(n int) string {
			if n == 1 {
				return listPage("?page=2", "a")
			}
			return "429"
		}},
		{name: "quiet repeated page", quiet: true, items: 2, body: func(n int) string {
			if n == 1 {
				return listPage("?page=2", "a")
			}
			return listPage("/list", "b")
		}},
		{name: "quiet page adding nothing", quiet: true, items: 3, body: func(n int) string {
			if n == 1 {
				return listPage("?page=2", "a", "b")
			}
			return listPage("?page=3", "b", "c")
		}},
		{name: "quiet page limit", quiet: true, items: config.MaxAlbumPages, body: func(n int) string {
			return listPage("?page="+strconv.Itoa(n+1), strconv.Itoa(n))
		}},
		{name: "quiet missing", quiet: true, items: 1, body: func(n int) string {
			if n == 1 {
				return listPage("?page=2", "a")
			}
			return "<p>gone</p>"
		}},
		{name: "quiet redirected", quiet: true, items: 1, body: func(n int) string {
			if n == 1 {
				return listPage("?page=2", "a")
			}
			return "redirect"
		}},
		{name: "quiet cap keeps its note", quiet: true, limit: 2, items: 2, note: "partial — source limit", body: func(int) string {
			return listPage("?page=2", "a", "b", "c")
		}},
		{name: "cancelled", cancel: 2, failed: context.Canceled.Error(), body: func(n int) string {
			return listPage("?page="+strconv.Itoa(n+1), strconv.Itoa(n))
		}},
		{name: "first page missing", failed: "test: no video listing on this page", body: func(int) string {
			return "<p>gone</p>"
		}},
		{name: "first page failed", failed: "test: fetch listing", body: func(int) string { return "" }},
		{name: "first page redirected", failed: "test: listing redirected to another page", body: func(int) string {
			return "redirect"
		}},
		{name: "nothing listed", failed: "test: nothing here", body: func(int) string {
			return `<div id="end"></div>`
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/elsewhere" {
					fmt.Fprint(w, listPage("", "z"))
					return
				}
				n, _ := strconv.Atoi(r.URL.Query().Get("page"))
				if tc.cancel != 0 && max(n, 1) == tc.cancel {
					cancel()
				}
				switch body := tc.body(max(n, 1)); body {
				case "":
					http.NotFound(w, r)
				case "429":
					w.Header().Set(httpx.HeaderRetryAfter, "0")
					w.WriteHeader(http.StatusTooManyRequests)
				case "redirect":
					http.Redirect(w, r, "/elsewhere", http.StatusFound)
				default:
					fmt.Fprint(w, body)
				}
			}))
			defer srv.Close()
			limit := tc.limit
			if limit == 0 {
				limit = 1000
			}
			walk := listingWalk{
				name: "test", title: "fallback", none: "nothing here", limit: limit, limitNote: "partial — source limit", read: testListingPage,
			}
			if tc.quiet {
				walk.quiet = true
				walk.failed = func(err error) (string, bool) {
					if httpx.HasStatus(err, http.StatusTooManyRequests) {
						return "partial — rate limited", true
					}
					return "", true
				}
			}
			links, title, note, err := walkListing(ctx, testClient(), mustParse(t, srv.URL+"/list"), walk)
			if tc.failed != "" {
				if err == nil || !strings.Contains(err.Error(), tc.failed) {
					t.Fatalf("err = %v, want %q", err, tc.failed)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(links) != tc.items || note != tc.note || title != "Synthetic Listing" {
				t.Fatalf("items=%d title=%q note=%q", len(links), title, note)
			}
		})
	}
}

// TestWalkPages covers what the engine owns beyond walkListing's pages: a
// stated total, an end page, a stop page and its note, and a failed hook.
func TestWalkPages(t *testing.T) {
	errGone := fmt.Errorf("gone")
	for _, tc := range []struct {
		name  string
		pages []pageOf[string]
		quiet bool
		items string
		note  string
		asked int
	}{
		{name: "stated total", pages: []pageOf[string]{
			{items: []string{"a", "b"}, next: true, total: 3}, {items: []string{"c"}, next: true, total: 3}, {items: []string{"d"}},
		}, items: "a,b,c", asked: 2},
		{name: "a short page goes on", pages: []pageOf[string]{
			{items: []string{"a", "b"}, next: true}, {items: []string{"c"}, next: true}, {end: true},
		}, items: "a,b,c", asked: 3},
		{name: "end page takes its items", pages: []pageOf[string]{
			{items: []string{"a"}, next: true}, {items: []string{"a", "b"}, end: true},
		}, items: "a,b", asked: 2},
		{name: "last page note", pages: []pageOf[string]{
			{items: []string{"a"}, note: "partial — invalid next page"},
		}, quiet: true, items: "a", note: "partial — invalid next page", asked: 1},
		{name: "stop page", pages: []pageOf[string]{
			{items: []string{"a"}, next: true}, {items: []string{"b"}, stop: "partial — redirected"},
		}, items: "a", note: "partial — redirected", asked: 2},
		{name: "quiet stop page keeps its own note", pages: []pageOf[string]{
			{items: []string{"a"}, next: true}, {stop: "partial — redirected", note: "partial — gone"},
		}, quiet: true, items: "a", note: "partial — gone", asked: 2},
		{name: "failed hook", pages: []pageOf[string]{
			{items: []string{"a"}, next: true}, {},
		}, items: "a", note: "partial — gone", asked: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			asked := 0
			items, note, err := walkPages(context.Background(), pageWalk[string]{
				key: func(s string) string { return s }, limit: -1, quiet: tc.quiet,
				failed: func(err error) (string, bool) { return "partial — " + err.Error(), true },
				fetch: func(_ context.Context, n int) (pageOf[string], error) {
					asked++
					p := tc.pages[n]
					if p.items == nil && !p.end && p.stop == "" {
						return p, errGone
					}
					return p, nil
				},
			})
			if err != nil || strings.Join(items, ",") != tc.items || note != tc.note || asked != tc.asked {
				t.Fatalf("items=%v note=%q asked=%d err=%v", items, note, asked, err)
			}
		})
	}
	if _, _, err := walkPages(context.Background(), pageWalk[string]{
		key: func(s string) string { return s }, limit: -1,
		fetch: func(context.Context, int) (pageOf[string], error) { return pageOf[string]{}, errGone },
	}); err != errGone {
		t.Fatalf("first page failure = %v, want it as it stands", err)
	}
}

func TestWebLink(t *testing.T) {
	base := mustParse(t, "https://example.test/list/page?x=1")
	for ref, want := range map[string]string{
		"":                              "",
		"  /watch/a#frag ":              "https://example.test/watch/a",
		"next":                          "https://example.test/list/next",
		"//other.example.test/a":        "https://other.example.test/a",
		"mailto:someone@example.test":   "",
		"https://user@example.test/a":   "",
		"javascript:void(0)":            "",
		"http://example.test/plain?q=1": "http://example.test/plain?q=1",
	} {
		got := ""
		if link := webLink(base, ref); link != nil {
			got = link.String()
		}
		if got != want {
			t.Errorf("webLink(%q) = %q, want %q", ref, got, want)
		}
	}
}

func TestExpansionNote(t *testing.T) {
	listed := func(n int) outcomeSummary { return outcomeSummary{listed: n, attempted: n} }
	counted := sourceWords{noun: "videos", counted: true, own: true}
	byReason := sourceWords{noun: "albums"}
	for _, tc := range []struct {
		walk  string
		e     expansion
		words sourceWords
		want  string
	}{
		{"", expansion{outcomeSummary: listed(3), used: 3}, counted, ""},
		{"partial — source limit", expansion{outcomeSummary: listed(3), used: 3}, counted, "partial — source limit"},
		{"", expansion{outcomeSummary: listed(3), used: 2, full: true}, counted, "partial — file limit; 2 of 3 videos resolved"},
		{"partial — repeated page", expansion{outcomeSummary: listed(3), used: 1}, counted, "partial — repeated page; 1 of 3 videos resolved"},
		{"partial — repeated page", expansion{outcomeSummary: listed(3), used: 1, full: true}, counted, "partial — repeated page; file limit; 1 of 3 videos resolved"},
		// A link no extractor recognised was never a source, unless the
		// sources are the collection's own pages.
		{"", expansion{outcomeSummary: outcomeSummary{listed: 3, attempted: 3, unrecognised: 1}, used: 2}, sourceWords{noun: "links", counted: true}, ""},
		{"", expansion{outcomeSummary: outcomeSummary{listed: 3, attempted: 3, unrecognised: 1}, used: 2}, counted, "2 of 3 videos resolved"},
		// Per reason: what the host refused and what could not be read.
		{"", expansion{outcomeSummary: outcomeSummary{listed: 4, attempted: 4, failures: 1, refusals: []string{"private"}}, used: 2}, byReason,
			"1 left out: private; 1 left out: could not be read"},
		{"partial — invalid next page", expansion{outcomeSummary: outcomeSummary{listed: 3, attempted: 3, failures: 1}, used: 2, full: true}, byReason,
			"partial — invalid next page; file limit; 1 left out: could not be read"},
		// Sources that said their own results were incomplete.
		{"", expansion{outcomeSummary: listed(3), used: 3, partial: 1}, byReason, "1 of 3 albums incomplete"},
		{"", expansion{outcomeSummary: listed(3), used: 3, partial: 2}, sourceWords{noun: "posts", counted: true, children: "posts have incomplete downloads"},
			"2 posts have incomplete downloads"},
		// Caps counted against what was found leave the omissions per reason.
		{"", expansion{outcomeSummary: outcomeSummary{listed: 2, attempted: 2, failures: 1}, used: 1}, sourceWords{noun: "links", found: 5, counted: true},
			"1 of 5 links; 1 left out: could not be read"},
		{"", expansion{outcomeSummary: outcomeSummary{listed: 2, attempted: 2, failures: 1}, used: 1}, sourceWords{noun: "links", found: 2, counted: true},
			"1 of 2 links resolved"},
	} {
		if got := tc.e.note(tc.walk, tc.words); got != tc.want {
			t.Errorf("note(%q, %+v, %+v) = %q, want %q", tc.walk, tc.e, tc.words, got, tc.want)
		}
	}
}

// A caller that has already read the first page hands it over rather than
// have it fetched again, and a listing whose key folds tracking parameters
// spots a page it has read under another spelling. An empty listing without
// a none error is the caller's to judge.
func TestWalkListingCallerOwnedFirstPage(t *testing.T) {
	var mu sync.Mutex
	asked := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		asked[r.URL.RequestURI()]++
		mu.Unlock()
		if r.URL.Query().Get("page") != "2" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, `<div id="list"><a href="/item/b">b</a></div><a rel="next" href="/list?utm_source=x">Next</a>`)
	}))
	defer srv.Close()
	client := testClient()
	root := mustParseDoc(t, `<h1>Owned</h1><div id="list"><a href="/item/a">a</a></div><a rel="next" href="/list?page=2">Next</a>`)
	key := func(u *url.URL) string {
		k := *u
		q := k.Query()
		q.Del("utm_source")
		k.RawQuery = q.Encode()
		return k.String()
	}
	links, title, note, err := walkListing(context.Background(), client, mustParse(t, srv.URL+"/list"), listingWalk{
		name: "test", limit: -1, read: testListingPage, root: root, key: key,
	})
	if err != nil || len(links) != 2 || title != "Owned" || note != "partial — repeated page" {
		t.Fatalf("links=%d title=%q note=%q err=%v", len(links), title, note, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if asked["/list"] != 0 || asked["/list?utm_source=x"] != 0 || asked["/list?page=2"] != 1 {
		t.Fatalf("asked %v", asked)
	}

	empty := mustParseDoc(t, `<h1>Nothing</h1><div id="list"></div>`)
	links, title, note, err = walkListing(context.Background(), client, mustParse(t, srv.URL+"/list"), listingWalk{
		name: "test", limit: -1, quiet: true, read: testListingPage, root: empty,
	})
	if err != nil || len(links) != 0 || title != "Nothing" || note != "" {
		t.Fatalf("empty listing: links=%d title=%q note=%q err=%v", len(links), title, note, err)
	}
}
