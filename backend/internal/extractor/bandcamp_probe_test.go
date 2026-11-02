// SPDX-License-Identifier: MIT

package extractor

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

// A usable browser page can still differ from the extractor's representation.
// Preserve the ordinary single-request path, but respect variation declared
// by the final response, a redirect, or the initial refusal before a retry.
func TestBandcampProbeHonorsAgentVariation(t *testing.T) {
	for _, tc := range []struct {
		name, where, cacheControl string
		vary                      []string
		different, challenge      bool
		browser, agent            int32
	}{
		{name: "unstated", browser: 1},
		{name: "unrelated variation", vary: []string{"Cookie", "Accept-Language"}, browser: 1},
		{name: "no-store still serves its reader", cacheControl: "no-store", browser: 1},
		{name: "final agent", vary: []string{"User-Agent"}, different: true, browser: 1, agent: 1},
		{name: "final wildcard", vary: []string{"*"}, different: true, browser: 1, agent: 1},
		{name: "mixed fields", vary: []string{"Cookie", " Accept-Language, uSeR-aGeNt "}, different: true, browser: 1, agent: 1},
		{name: "redirect agent", where: "redirect", vary: []string{"User-Agent"}, different: true, browser: 2, agent: 2},
		{name: "redirect wildcard", where: "redirect", vary: []string{"*"}, different: true, browser: 2, agent: 2},
		{name: "refusal agent", where: "refusal", vary: []string{"User-Agent"}, different: true, browser: 2, agent: 1},
		{name: "refusal wildcard", where: "refusal", vary: []string{"*"}, different: true, browser: 2, agent: 1},
		{name: "client challenge", challenge: true, vary: []string{"*"}, browser: 1, agent: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var browser, agent atomic.Int32
			// This forward proxy only serves fixture pages, never forwards.
			// The ordinary client and registry still see a supported host.
			proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Host != "artist.bandcamp.com" ||
					(r.URL.Path != "/album/example" && r.URL.Path != "/album/final") {
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Redacted())
					http.NotFound(w, r)
					return
				}
				ownAgent := r.UserAgent() == bandcampAgent
				if ownAgent {
					agent.Add(1)
				} else {
					browser.Add(1)
				}
				redirect := tc.where == "redirect" && r.URL.Path == "/album/example"
				refused := tc.where == "refusal" && !ownAgent && r.Referer() == ""
				if tc.where == "" || redirect || refused {
					for _, vary := range tc.vary {
						w.Header().Add(httpx.HeaderVary, vary)
					}
				}
				if redirect {
					http.Redirect(w, r, "/album/final", http.StatusFound)
					return
				}
				if refused {
					http.Error(w, "origin referer required", http.StatusForbidden)
					return
				}
				w.Header().Set(httpx.HeaderContentType, "text/html")
				if tc.cacheControl != "" {
					w.Header().Set(httpx.HeaderCacheControl, tc.cacheControl)
				}
				if tc.challenge && !ownAgent {
					fmt.Fprint(w, `<html><head><title>Client Challenge</title><link href="/_fs-ch-1234/assets/styles.css" rel="stylesheet"></head></html>`)
					return
				}
				tracks := `{"track_num":1,"title":"First Song","track_id":11,"file":{"mp3-128":"https://cdn.example.test/first"}}`
				if !tc.different || ownAgent {
					tracks += `,{"track_num":2,"title":"Second Song","track_id":12,"file":{"mp3-128":"https://cdn.example.test/second"}}`
				}
				data := `{"artist":"Synthetic Band","item_type":"album","current":{"title":"Synthetic Album"},"trackinfo":[` + tracks + `]}`
				body := strings.Replace(bandcampPageFixture(data, ""), "</head>",
					`<script src="/client-bundle/1/trackpipe/global_head-0123.js"></script></head>`, 1)
				fmt.Fprint(w, body)
			}))
			t.Cleanup(proxy.Close)
			client, err := httpx.New("Mozilla/5.0 fixture browser", "en-US", 0, 5*time.Second).ThroughProxy(proxy.URL)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(client.CloseIdleConnections)
			reg := NewRegistry(&config.Config{}, client)
			source := (&url.URL{Scheme: "http", Host: "artist.bandcamp.com", Path: "/album/" + "example"}).String()
			res, ex, err := reg.Extract(t.Context(), source, Options{})
			if err != nil {
				t.Fatal(err)
			}
			want := []string{"01 - First Song.mp3", "02 - Second Song.mp3"}
			if ex.Name() != "bandcamp" || !slices.Equal(fileNames(res.Files), want) || res.Note != "" {
				t.Fatalf("extractor=%s files=%v note=%q; want both tracks and no note", ex.Name(), fileNames(res.Files), res.Note)
			}
			if browser.Load() != tc.browser || agent.Load() != tc.agent {
				t.Errorf("browser requests=%d, extractor requests=%d; want %d, %d", browser.Load(), agent.Load(), tc.browser, tc.agent)
			}
		})
	}
}
