// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"errors"
	"fmt"
	"html"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

func knuthingCard(id, title, link string) string {
	return fmt.Sprintf(`<div class="video-latest-list video-wrapper" data-id="%s"><a href="%s">thumbnail</a><a href="%s"><h4>%s</h4></a></div>`, id, html.EscapeString(link), html.EscapeString(link), html.EscapeString(title))
}

func knuthingListingPage(cards, pager string) string {
	return `<aside><div class="videos-latest-list">` + knuthingCard("99", "Sidebar", "/watch/sidebar.html") + `</div></aside>
	<div id="container_content"><h4 class="page_head">Test Collection</h4>
	<div class="videos-latest-list row">` + cards + `</div><ul class="pagination">` + pager + `</ul></div>`
}

func TestKnuthingListingWalksOnlyItsCardsAndPages(t *testing.T) {
	for _, section := range []string{"/videos/free", "/videos/latest", "/videos/category/synthetic"} {
		t.Run(section, func(t *testing.T) {
			var pages, videos atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == section {
					pages.Add(1)
					if r.URL.Query().Get("sort") != "top" || r.URL.Query().Get("last_page") != "" {
						t.Errorf("lost filter or kept last-page shortcut: %s", r.URL)
					}
					var cards, pager string
					switch r.URL.Query().Get("page_id") {
					case "1":
						cards = knuthingCard("1", "First - Part 2", "/watch/first.html") + knuthingCard("2", "Second & Friends", "/watch/second.html?keep=yes") +
							knuthingCard("77", "Foreign", "https://other.example.test/watch/foreign.html")
						pager = `<a title="Next Page" href="?page_id=2">Next</a><a title="Last Page" href="?page_id=99&amp;last_page=1">Last</a>`
					case "2":
						cards = knuthingCard("2", "Duplicate", "/watch/renamed.html") + knuthingCard("3", "Third", "/watch/third.html")
						pager = `<a href="?page_id=1">1</a><a href="?page_id=3">3</a>`
					case "3":
						cards = knuthingCard("4", "Fourth", "/watch/fourth.html")
						pager = `<a title="First Page" href="?page_id=1">First</a>`
					default:
						t.Errorf("unexpected listing page: %s", r.URL)
					}
					fmt.Fprint(w, knuthingListingPage(cards, pager))
					return
				}
				switch r.URL.Path {
				case "/watch/second.html":
					videos.Add(1)
					if r.URL.Query().Get("keep") != "yes" {
						t.Error("lost video's own query")
					}
					fmt.Fprint(w, `<title>Second &amp; Friends</title><iframe class="bunnyframe" src="/player"></iframe>`)
				case "/player":
					fmt.Fprint(w, `<bunny-stream-video content-src="/playlist.m3u8"></bunny-stream-video>`)
				case "/playlist.m3u8":
					fmt.Fprint(w, "#EXTM3U\n#EXTINF:4,\npart.ts\n#EXT-X-ENDLIST\n")
				default:
					t.Errorf("unexpected request: %s", r.URL)
					http.NotFound(w, r)
				}
			}))
			defer srv.Close()
			k := NewKnuthing(httpx.New("test", "en", 0, time.Second))
			res, err := k.Extract(context.Background(), mustParse(t, srv.URL+section+"?page_id=8&last_page=1&sort=top"), Options{})
			if err != nil {
				t.Fatal(err)
			}
			var names []string
			for _, f := range res.Files {
				names = append(names, f.Name)
				if f.URL != "" || f.Resolve == nil || f.Refresh == nil {
					t.Fatalf("listing did not defer video resolution: %+v", f)
				}
			}
			want := []string{"First - Part 2.ts", "Second & Friends.ts", "Third.ts", "Fourth.ts"}
			if !reflect.DeepEqual(names, want) || res.Title != "Test Collection" || res.Note != "" || pages.Load() != 3 || videos.Load() != 0 {
				t.Fatalf("listing: %+v, names=%v, pages=%d, videos=%d", res, names, pages.Load(), videos.Load())
			}
			if _, err := res.Files[1].Refresh(context.Background()); err != nil {
				t.Fatal(err)
			}
			if pages.Load() != 3 || videos.Load() != 1 {
				t.Fatal("recovery reopened the listing instead of its video")
			}
		})
	}
}

func TestKnuthingListingCapsAndPartialPages(t *testing.T) {
	for _, tc := range []struct {
		name, second, next, note string
		limits                   Limits
		files, pages             int
	}{
		{name: "source-cap", limits: Limits{Sources: 1}, files: 1, pages: 1, note: "source limit"},
		{name: "file-cap", limits: Limits{Files: 1}, files: 1, pages: 1, note: "file limit"},
		{name: "exact-cap", limits: Limits{Sources: 2}, files: 2, pages: 1, note: "source limit"},
		{name: "repeat", second: "repeat", files: 2, pages: 2, note: "repeated or empty"},
		{name: "missing", second: "missing", files: 2, pages: 2, note: "listing is missing"},
		{name: "failed", second: "failed", files: 2, pages: 2, note: "could not fetch"},
		{name: "other-section", next: "/videos/latest?page_id=2", files: 2, pages: 1, note: "invalid next page"},
		{name: "other-origin", next: "https://other.example.test/videos/free?page_id=2", files: 2, pages: 1, note: "invalid next page"},
		{name: "other-filter", next: "?page_id=2&sort=oldest", files: 2, pages: 1, note: "invalid next page"},
		{name: "backwards", next: "?page_id=1", files: 2, pages: 1, note: "invalid next page"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				n := calls.Add(1)
				if r.URL.Path != "/videos/free" || n > 2 {
					t.Errorf("walk escaped its listing or did not stop: %s", r.URL)
					http.NotFound(w, r)
					return
				}
				if n == 2 {
					switch tc.second {
					case "missing":
						fmt.Fprint(w, "<html>Listing gone</html>")
						return
					case "failed":
						http.NotFound(w, r)
						return
					}
				}
				next := tc.next
				if next == "" {
					next = "?page_id=2"
				}
				fmt.Fprint(w, knuthingListingPage(knuthingCard("1", "First", "/watch/first.html")+knuthingCard("2", "Second", "/watch/second.html"), `<a title="Next Page" href="`+html.EscapeString(next)+`">Next</a>`))
			}))
			defer srv.Close()
			k := NewKnuthing(httpx.New("test", "en", 0, time.Second))
			res, err := k.Extract(context.Background(), mustParse(t, srv.URL+"/videos/free?sort=top"), Options{limits: tc.limits})
			if err != nil {
				t.Fatal(err)
			}
			if len(res.Files) != tc.files || int(calls.Load()) != tc.pages || !strings.Contains(res.Note, tc.note) || res.Title != "Test Collection" {
				t.Fatalf("result=%+v, pages=%d", res, calls.Load())
			}
		})
	}
}

func TestKnuthingListingCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page_id") == "2" {
			cancel()
			return
		}
		fmt.Fprint(w, knuthingListingPage(knuthingCard("1", "First", "/watch/first.html"), `<a title="Next Page" href="?page_id=2">Next</a>`))
	}))
	defer srv.Close()
	k := NewKnuthing(httpx.New("test", "en", 0, time.Second))
	if _, err := k.Extract(ctx, mustParse(t, srv.URL+"/videos/free"), Options{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("partial result hid cancellation: %v", err)
	}
}
