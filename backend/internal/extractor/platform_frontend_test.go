// SPDX-License-Identifier: MIT

package extractor

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

func TestPlatformRecognisesOrdinaryFrontendThroughAPI(t *testing.T) {
	for _, row := range []struct {
		name, path, generator, api, response string
		wiki                                 bool
	}{
		{"wiki custom article", "/wiki/Example_article", "", "/api.php", mediaWikiSiteInfo, true},
		{"wiki generator article", "/wiki/Example_article", "MediaWiki 1.43", "/api.php", mediaWikiSiteInfo, true},
		{"wiki generator bare title", "/Example_article", "MediaWiki 1.43", "/api.php", mediaWikiSiteInfo, true},
		{"wiki script route", "/custom/index.php?title=Example_article", "", "/custom/api.php", mediaWikiSiteInfo, true},
		{"archive custom board", "/b/", "", foolFuukaIndexAPI, foolFuukaIndexJSON, false},
		{"archive generator board", "/b/", "FoolFuuka 2.2", foolFuukaIndexAPI, foolFuukaIndexJSON, false},
		{"archive paged board", "/b/page/2/", "", foolFuukaIndexAPI, foolFuukaIndexJSON, false},
	} {
		for _, answer := range []string{"platform", "unrelated JSON", "missing API"} {
			t.Run(row.name+"/"+answer, func(t *testing.T) {
				var calls atomic.Int32
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					if r.URL.Path != row.api || answer == "missing API" {
						http.NotFound(w, r)
						return
					}
					w.Header().Set(httpx.HeaderContentType, httpx.ContentTypeJSON)
					if answer == "unrelated JSON" {
						fmt.Fprint(w, `{"posts":[{"id":100}],"query":{"general":{"sitename":"An ordinary site"}}}`)
						return
					}
					fmt.Fprint(w, row.response)
				}))
				defer srv.Close()
				client := testClient()
				defer client.CloseIdleConnections()
				var ex Extractor = NewFoolFuuka(client)
				if row.wiki {
					ex = NewMediaWiki(client)
				}
				page := htmlPage(t, srv.URL+row.path, fmt.Sprintf(`<html><meta name="generator" content="%s"><title>Custom frontend</title></html>`, row.generator))
				if got := ex.Recognise(t.Context(), page); got != (answer == "platform") {
					t.Errorf("recognised=%v after %d API requests; answer=%s", got, calls.Load(), answer)
				}
				first := calls.Load()
				bound := 1
				if row.wiki {
					bound = len(mediaWikiCandidates(page.Final, ""))
				}
				if first < 1 || first > int32(bound) {
					t.Errorf("API requests=%d; want 1..%d", first, bound)
				}
				if got := ex.Recognise(t.Context(), page); got != (answer == "platform") || calls.Load() != first {
					t.Errorf("repeat recognised=%v, requests=%d; want the memoised answer after %d requests", got, calls.Load(), first)
				}
				if ex.Shape(page.Final) {
					t.Error("a broad page route became an automatic discovery form")
				}
			})
		}
	}
}

func TestPlatformFrontendDiscoveryKeepsBoundaries(t *testing.T) {
	for _, row := range []struct {
		name, path, kind string
		status           int
		wiki             bool
	}{
		{"wiki ordinary slug", "/An_article", "text/html", 200, true},
		{"wiki media", "/wiki/Example_article", "video/mp4", 200, true},
		{"wiki missing article", "/wiki/Example_article", "text/html", 404, true},
		{"wiki removed article", "/wiki/Example_article", "text/html", 410, true},
		{"archive no trailing slash", "/b", "text/html", 200, false},
		{"archive site root", "/", "text/html", 200, false},
		{"archive long section", "/account/", "text/html", 200, false},
		{"archive media", "/b/", "video/mp4", 200, false},
		{"archive missing board", "/b/", "text/html", 404, false},
		{"archive removed board", "/b/", "text/html", 410, false},
	} {
		t.Run(row.name, func(t *testing.T) {
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				w.Header().Set(httpx.HeaderContentType, httpx.ContentTypeJSON)
				if row.wiki {
					fmt.Fprint(w, mediaWikiSiteInfo)
				} else {
					fmt.Fprint(w, foolFuukaIndexJSON)
				}
			}))
			defer srv.Close()
			client := testClient()
			defer client.CloseIdleConnections()
			var ex Extractor = NewFoolFuuka(client)
			if row.wiki {
				ex = NewMediaWiki(client)
			}
			page := fixturePage(t, srv.URL+row.path, row.status, row.kind, "<html>A different site</html>")
			if ex.Recognise(t.Context(), page) || calls.Load() != 0 {
				t.Errorf("ineligible route asked its API %d times", calls.Load())
			}
		})
	}
}
