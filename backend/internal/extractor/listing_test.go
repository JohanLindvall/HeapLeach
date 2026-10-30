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
	"testing"
	"time"

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
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/elsewhere" {
					fmt.Fprint(w, listPage("", "z"))
					return
				}
				n, _ := strconv.Atoi(r.URL.Query().Get("page"))
				switch body := tc.body(max(n, 1)); body {
				case "":
					http.NotFound(w, r)
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
			links, title, note, err := walkListing(context.Background(), httpx.New("test", "en", 0, time.Second), mustParse(t, srv.URL+"/list"), listingWalk{
				name: "test", title: "fallback", none: "nothing here", limit: limit, limitNote: "partial — source limit", read: testListingPage,
			})
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
