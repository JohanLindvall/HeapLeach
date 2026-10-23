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
)

func noodleCard(link, title string) string {
	return fmt.Sprintf(`<div class="item" data-video-title-base="%s"><a class="item_link" href="%s"><div data-trailer_url="/preview.mp4"></div><div class="title" data-video-title-text>%s</div></a></div>`, html.EscapeString(title), html.EscapeString(link), html.EscapeString(title))
}

func noodleListing(cards, pager string) string {
	return `<h1>Harbour Videos</h1><div class="list_videos" id="list_videos">` + cards + `</div>` + pager +
		`<aside><div class="list_videos">` + noodleCard("/watch/9_9", "Unrelated") + `</div></aside>`
}

func TestNoodleListingWalksOnlySearchCardsAndDefersPlayers(t *testing.T) {
	var pages, videos atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/watch/-1_2" {
			videos.Add(1)
			fmt.Fprint(w, `<meta property="og:title" content="Second &amp; Friends"><script>window.playlist={"sources":[{"file":"/second.mp4","type":"mp4"}]};</script>`)
			return
		}
		pages.Add(1)
		if r.URL.EscapedPath() != "/video/harbour%2Blights" || r.URL.Query().Get("sort") != "0" || r.URL.Query().Get("hd") != "1" || r.URL.Query().Get("len") != "long" {
			t.Errorf("lost query escaping or filters: %s", r.URL)
		}
		switch r.URL.Query().Get("p") {
		case "":
			fmt.Fprint(w, noodleListing(noodleCard("/watch/-1_1", "First - Part 2")+noodleCard("/watch/-1_2", "Second & Friends")+
				noodleCard("https://other.example.test/watch/3_3", "Foreign"), `<div class="more" data-page="1"></div>`))
		case "1":
			fmt.Fprint(w, noodleListing(noodleCard("/watch/-1_2?duplicate=yes", "Repeated")+noodleCard("/watch/-1_3", "Third"),
				`<div class="more" data-action="prev" data-page="0"></div><div class="more" data-page="2"></div>`))
		case "2":
			// An exhausted search still returns a full page of other videos.
			fmt.Fprint(w, `<span data-i18n="nothing_found">Nothing Found</span><div class="list_videos">`+noodleCard("/watch/9_9", "Unrelated")+`</div>`)
		default:
			t.Errorf("unexpected page: %s", r.URL)
		}
	}))
	defer srv.Close()
	n := NewNoodleMagazine(noodleTestClient(t))
	res, err := n.Extract(context.Background(), mustParse(t, srv.URL+"/video/harbour%2Blights?p=8&sort=0&hd=1&len=long#results"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, f := range res.Files {
		names = append(names, f.Name)
		if f.URL != "" || f.Resolve == nil || f.Refresh == nil {
			t.Fatalf("listing did not defer its media: %+v", f)
		}
	}
	if !reflect.DeepEqual(names, []string{"First - Part 2.mp4", "Second & Friends.mp4", "Third.mp4"}) || res.Note != "" || res.Title != "Harbour Videos" || pages.Load() != 3 || videos.Load() != 0 {
		t.Fatalf("wrong listing: %+v, names=%v, pages=%d, videos=%d", res, names, pages.Load(), videos.Load())
	}
	if _, err := res.Files[1].Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if pages.Load() != 3 || videos.Load() != 1 {
		t.Fatal("recovery reopened the listing instead of its video")
	}
}

func TestNoodleListingCapsAndIncompletePages(t *testing.T) {
	for _, tc := range []struct {
		name, second, pager, note string
		limits                    Limits
		files, pages              int
	}{
		{name: "source-cap", limits: Limits{Sources: 1}, files: 1, pages: 1, note: "source limit"},
		{name: "file-cap", limits: Limits{Files: 1}, files: 1, pages: 1, note: "file limit"},
		{name: "exact-cap-more", limits: Limits{Sources: 2}, files: 2, pages: 1, note: "source limit"},
		{name: "exact-cap-complete", limits: Limits{Sources: 2}, pager: "none", files: 2, pages: 1},
		{name: "hidden-next", pager: `<div class="more" data-page="1" hidden></div>`, files: 2, pages: 1},
		{name: "related-next", pager: `<div class="more" data-page="1" data-action="related"></div>`, files: 2, pages: 1},
		{name: "repeated", files: 2, pages: 2, note: "repeated or empty"},
		{name: "missing", second: "missing", files: 2, pages: 2, note: "listing is missing"},
		{name: "failure", second: "failed", files: 2, pages: 2, note: "could not fetch"},
		{name: "redirect", second: "redirect", files: 2, pages: 3, note: "redirected"},
		{name: "bad-page", pager: `<div class="more" data-page="invalid"></div>`, files: 2, pages: 1, note: "invalid next page"},
		{name: "backwards", pager: `<div class="more" data-page="0"></div>`, files: 2, pages: 1, note: "invalid next page"},
		{name: "skipped-page", pager: `<div class="more" data-page="2"></div>`, files: 2, pages: 1, note: "invalid next page"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if calls.Add(1) > 3 {
					t.Error("listing failed to stop")
					http.NotFound(w, r)
					return
				}
				if r.URL.Query().Get("p") == "1" {
					switch tc.second {
					case "missing":
						fmt.Fprint(w, "<html>Missing listing</html>")
						return
					case "failed":
						http.NotFound(w, r)
						return
					case "redirect":
						http.Redirect(w, r, "/video/another", http.StatusFound)
						return
					}
				}
				pager := tc.pager
				if pager == "" {
					pager = `<div class="more" data-page="1"></div>`
				}
				fmt.Fprint(w, noodleListing(noodleCard("/watch/1_1", "First")+noodleCard("/watch/1_2", "Second"), pager))
			}))
			defer srv.Close()
			n := NewNoodleMagazine(noodleTestClient(t))
			res, err := n.Extract(context.Background(), mustParse(t, srv.URL+"/video/synthetic"), Options{limits: tc.limits})
			if err != nil {
				t.Fatal(err)
			}
			if len(res.Files) != tc.files || int(calls.Load()) != tc.pages || !strings.Contains(res.Note, tc.note) || (tc.note == "" && res.Note != "") || res.Title != "Harbour Videos" {
				t.Fatalf("result=%+v, calls=%d", res, calls.Load())
			}
		})
	}
}

func TestNoodleListingNeverUsesRecommendationsForEmptySearch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<span data-i18n="nothing_found">Nothing Found</span><div class="list_videos">`+noodleCard("/watch/9_9", "Unrelated")+`</div>`)
	}))
	defer srv.Close()
	_, err := NewNoodleMagazine(noodleTestClient(t)).Extract(context.Background(), mustParse(t, srv.URL+"/video/synthetic"), Options{})
	if err == nil || !strings.Contains(err.Error(), "no videos found") {
		t.Fatalf("empty search became recommendations: %v", err)
	}
}

func TestNoodleListingCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("p") == "1" {
			cancel()
			return
		}
		fmt.Fprint(w, noodleListing(noodleCard("/watch/1_1", "First"), `<div class="more" data-page="1"></div>`))
	}))
	defer srv.Close()
	_, err := NewNoodleMagazine(noodleTestClient(t)).Extract(ctx, mustParse(t, srv.URL+"/video/synthetic"), Options{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("partial listing hid cancellation: %v", err)
	}
}
