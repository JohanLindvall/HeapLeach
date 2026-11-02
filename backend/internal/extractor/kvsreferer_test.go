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

// A valid navigation listing can contain fewer entries than the reader's
// self-Referer representation. Only a matching probe may replace that read.
func TestKVSCanonicalListingHonorsReferer(t *testing.T) {
	for _, listingPath := range []string{"/members/42/public_videos/", "/search/synthetic/"} {
		for _, matchingProbe := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/matching-probe=%t", listingPath, matchingProbe), func(t *testing.T) {
				var listingReads, invited, mediaReads atomic.Int32
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set(httpx.HeaderContentType, "text/html")
					switch {
					case r.URL.Path == listingPath:
						listingReads.Add(1)
						w.Header().Set(httpx.HeaderVary, httpx.HeaderReferer)
						fmt.Fprint(w, `<html><head><meta name="generator" content="KVS"></head><body><h2>Synthetic Member's Public Videos</h2><div id="list_videos_common"><div id="list_videos_common_items"><a href="/videos/first-synthetic/">First Synthetic Clip</a>`)
						if r.Referer() == "http://"+r.Host+listingPath {
							invited.Add(1)
							fmt.Fprint(w, `<a href="/videos/second-synthetic/">Second Synthetic Clip</a>`)
						}
						fmt.Fprint(w, `</div><div class="pagination"><ul><li class="next"><span>Next</span></li></ul></div></div></body></html>`)
					case strings.HasPrefix(r.URL.Path, "/videos/"):
						name := strings.Trim(strings.TrimPrefix(r.URL.Path, "/videos/"), "/")
						fmt.Fprintf(w, `<html><script>var flashvars = {video_title: '%s', video_url: 'http://%s/media/%s.mp4'};</script></html>`, name, r.Host, name)
					default:
						mediaReads.Add(1)
						http.NotFound(w, r)
					}
				}))
				t.Cleanup(srv.Close)
				client := testClient()
				t.Cleanup(client.CloseIdleConnections)
				u := mustParse(t, srv.URL+listingPath)
				var res *Result
				var err error
				wantReads := int32(2)
				if matchingProbe {
					// Record an actual self-Referer response with the client
					// identity that Page.answers requires, then consume it.
					p := &Page{URL: u, Final: u, via: client,
						agent: httpx.RequestAgent(t.Context(), client), request: httpx.RequestKey(t.Context(), client, u)}
					if !probeGet(t.Context(), client, p, httpx.Referer(u.String())) {
						t.Fatalf("matching probe failed: %v", p.Err)
					}
					p.session = httpx.RequestKey(t.Context(), client, u)
					res, err = NewKVS(client).Extract(withPage(t.Context(), p), u, Options{})
					wantReads = 1
				} else {
					// An unfamiliar install navigates first. Its preview
					// must not replace the reader's self-Referer response.
					res, _, err = NewRegistry(&config.Config{}, client).Extract(t.Context(), u.String(), Options{})
				}
				if err != nil || res == nil || len(res.Files) != 2 || res.Note != "" {
					t.Fatalf("listing=%+v error=%v; want both files from the complete representation", res, err)
				}
				if listingReads.Load() != wantReads || invited.Load() != 1 || mediaReads.Load() != 0 {
					t.Fatalf("listing reads=%d self-Referer reads=%d media reads=%d; want %d, 1, 0", listingReads.Load(), invited.Load(), mediaReads.Load(), wantReads)
				}
			})
		}
	}
}
