// SPDX-License-Identifier: MIT

package extractor

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

// A known install's video must receive the browse reader's representation
// on its first GET. A navigation request can expose a preview instead, and
// a page consumed by that visit cannot be asked for again with new headers.
func TestKVSVideoProbeUsesReaderReferer(t *testing.T) {
	for _, pagePath := range []string{"/videos/synthetic-clip/", "/watch/synthetic-clip/"} {
		for _, once := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/consumable=%t", pagePath, once), func(t *testing.T) {
				var reads, invited, mediaReads atomic.Int32
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path != pagePath {
						mediaReads.Add(1)
						http.NotFound(w, r)
						return
					}
					n := reads.Add(1)
					link := "https://media.example.test/preview.mp4"
					if r.Referer() == "http://"+r.Host+"/" {
						invited.Add(1)
						link = "https://media.example.test/complete.mp4"
					}
					if once && n != 1 {
						http.Error(w, "page already consumed", http.StatusGone)
						return
					}
					w.Header().Set(httpx.HeaderContentType, "text/html")
					w.Header().Set(httpx.HeaderVary, httpx.HeaderReferer)
					fmt.Fprintf(w, `<html><head><meta name="generator" content="KVS"></head><body><script>var flashvars = {video_title: 'Synthetic Clip', video_url: '%s'};</script></body></html>`, link)
				}))
				t.Cleanup(srv.Close)
				client := testClient()
				t.Cleanup(client.CloseIdleConnections)
				u := mustParse(t, srv.URL+pagePath)
				cfg := &config.Config{ExtraHosts: map[string][]string{config.FamilyKVS: {u.Hostname()}}}
				res, ex, err := NewRegistry(cfg, client).Extract(t.Context(), u.String(), Options{})
				if err != nil || res == nil || ex.Name() != "kvs" || len(res.Files) != 1 || res.Files[0].URL != "https://media.example.test/complete.mp4" {
					t.Fatalf("result=%+v extractor=%v error=%v; want the complete video", res, ex, err)
				}
				if reads.Load() != 1 || invited.Load() != 1 || mediaReads.Load() != 0 {
					t.Fatalf("page reads=%d reader Referers=%d media reads=%d; want 1, 1, 0", reads.Load(), invited.Load(), mediaReads.Load())
				}
			})
		}
	}
}

// The header policy does not force KVS recognition. A different response
// still belongs to its reader. Canonical member/search listings use their
// self-Referer; noncanonical forms and unfamiliar hosts keep navigation.
// Categories and custom videos use the browse reader's origin Referer.
func TestKVSProbeKeepsRecognitionAndRouteBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name, path, referer string
		known               bool
	}{
		{"known_video", "/videos/synthetic-clip/", "/", true},
		{"known_singular_video", "/video/synthetic-clip/", "/", true},
		{"unknown_video", "/videos/synthetic-clip/", "", false},
		{"member_listing", "/members/42/public_videos/", "/members/42/public_videos/", true},
		{"member_video_section", "/members/42/videos/", "/members/42/videos/", true},
		{"member_later_page", "/members/42/videos/2/", "", true},
		{"member_profile", "/members/42/", "", true},
		{"member_query", "/members/42/videos/?page=2", "", true},
		{"unknown_member", "/members/42/public_videos/", "", false},
		{"search", "/search/synthetic/", "/search/synthetic/", true},
		{"search_escaped_term", "/search/synthetic%2Bclip/", "/search/synthetic%2Bclip/", true},
		{"search_video_term", "/search/videos/2/", "", true},
		{"search_query", "/search/?q=synthetic", "", true},
		{"unknown_search", "/search/synthetic/", "", false},
		{"custom_video_route", "/watch/synthetic-clip/", "/", true},
		{"category", "/categories/synthetic/", "/", true},
		{"front_page", "/", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var reads atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				reads.Add(1)
				want := ""
				if tc.referer != "" {
					want = "http://" + r.Host + tc.referer
				}
				if r.Referer() != want {
					t.Errorf("Referer=%q, want %q", r.Referer(), want)
				}
				w.Header().Set(httpx.HeaderContentType, "application/rss+xml")
				fmt.Fprint(w, `<rss version="2.0"><channel><title>Synthetic Feed</title></channel></rss>`)
			}))
			t.Cleanup(srv.Close)
			client := testClient()
			t.Cleanup(client.CloseIdleConnections)
			u := mustParse(t, srv.URL+tc.path)
			cfg := &config.Config{}
			if tc.known {
				cfg.ExtraHosts = map[string][]string{config.FamilyKVS: {u.Hostname()}}
			}
			ex, page := NewRegistry(cfg, client).identify(t.Context(), u)
			if ex.Name() != "feed" || page == nil || page.Err != nil || reads.Load() != 1 {
				t.Fatalf("extractor=%v page=%+v reads=%d; want the feed after one read", ex, page, reads.Load())
			}
		})
	}
}

// A canonical listing must receive its reader representation before a
// navigation request can spend the only usable response. Noncanonical forms
// keep navigation on their submitted URL and read the listing separately.
func TestKVSListingProbeUsesReaderReferer(t *testing.T) {
	for _, tc := range []struct {
		name, submitted, listing string
		browse                   bool
	}{
		{"canonical_member", "/members/42/public_videos/", "/members/42/public_videos/", false},
		{"canonical_member_section", "/members/42/videos/", "/members/42/videos/", false},
		{"canonical_search", "/search/synthetic/", "/search/synthetic/", false},
		{"escaped_search", "/search/synthetic%2Bclip/", "/search/synthetic%2Bclip/", false},
		{"canonical_browse", "/categories/synthetic/", "/categories/synthetic/", true},
		{"noncanonical_member", "/members/42/", "/members/42/public_videos/", false},
		{"noncanonical_search", "/search/synthetic/2/", "/search/synthetic/", false},
	} {
		for _, once := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/consumable=%t", tc.name, once), func(t *testing.T) {
				var listingReads, readerReads, videoReads, otherReads atomic.Int32
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set(httpx.HeaderContentType, "text/html")
					switch {
					case r.URL.EscapedPath() == tc.listing:
						n := listingReads.Add(1)
						want := "http://" + r.Host + tc.listing
						if tc.browse {
							want = "http://" + r.Host + "/"
						}
						invited := r.Referer() == want
						if invited {
							readerReads.Add(1)
						}
						if once && n > 1 {
							http.Error(w, "listing already consumed", http.StatusGone)
							return
						}
						w.Header().Set(httpx.HeaderVary, httpx.HeaderReferer)
						fmt.Fprint(w, `<html><head><meta name="generator" content="KVS"></head><body><h2>Synthetic Member's Public Videos</h2><div id="list_videos_common"><div id="list_videos_common_items"><a href="/videos/first-synthetic/">First Synthetic Clip</a>`)
						if invited {
							fmt.Fprint(w, `<a href="/videos/second-synthetic/">Second Synthetic Clip</a>`)
						}
						fmt.Fprint(w, `</div><div class="pagination"><ul><li class="next"><span>Next</span></li></ul></div></div></body></html>`)
					case r.URL.EscapedPath() == tc.submitted && tc.submitted != tc.listing:
						otherReads.Add(1)
						if r.Referer() != "" {
							t.Errorf("noncanonical navigation Referer=%q, want none", r.Referer())
						}
						fmt.Fprint(w, `<html><head><meta name="generator" content="KVS"></head><body><div id="list_videos_common"><div id="list_videos_common_items"></div></div></body></html>`)
					case strings.HasPrefix(r.URL.Path, "/videos/"):
						videoReads.Add(1)
						id := strings.Trim(strings.TrimPrefix(r.URL.Path, "/videos/"), "/")
						fmt.Fprintf(w, `<html><script>var flashvars={video_title:'Synthetic Clip',video_url:'https://media.example.test/%s.mp4'};</script></html>`, id)
					default:
						otherReads.Add(1)
						http.Error(w, "unexpected request", http.StatusNotFound)
					}
				}))
				t.Cleanup(srv.Close)
				client := testClient()
				t.Cleanup(client.CloseIdleConnections)
				u := mustParse(t, srv.URL+tc.submitted)
				cfg := &config.Config{ExtraHosts: map[string][]string{config.FamilyKVS: {u.Hostname()}}}
				res, ex, err := NewRegistry(cfg, client).Extract(t.Context(), u.String(), Options{})
				if err != nil || res == nil || ex.Name() != "kvs" || len(res.Files) != 2 {
					t.Fatalf("result=%+v extractor=%v error=%v; want both listing videos", res, ex, err)
				}
				var wantOther int32
				if tc.submitted != tc.listing {
					wantOther = 1
				}
				if listingReads.Load() != 1 || readerReads.Load() != 1 || videoReads.Load() != 2 || otherReads.Load() != wantOther {
					t.Fatalf("listing=%d reader Referers=%d videos=%d other=%d; want 1, 1, 2, %d", listingReads.Load(), readerReads.Load(), videoReads.Load(), otherReads.Load(), wantOther)
				}
				for i, suffix := range []string{"first-synthetic", "second-synthetic"} {
					if res.Files[i].URL != "https://media.example.test/"+suffix+".mp4" {
						t.Errorf("unexpected media %q", res.Files[i].URL)
					}
				}
			})
		}
	}
}
