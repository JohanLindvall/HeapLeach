// SPDX-License-Identifier: MIT

package extractor

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

// RequestKey models cookies and language, but deliberately not route agents
// or arbitrary request headers. Only representations whose Vary fits that
// identity may be reused by another acquisition, even on the same route.
func TestRegistryPageCacheHonorsVary(t *testing.T) {
	for _, tc := range []struct {
		name   string
		vary   []string
		shared bool
	}{
		{"unstated", nil, true},
		{"cookie and language", []string{"cOoKiE, ACCEPT-language"}, true},
		{"agent", []string{httpx.HeaderUserAgent}, false},
		{"wildcard", []string{"*"}, false},
		{"accept", []string{httpx.HeaderAccept}, false},
		{"referer", []string{httpx.HeaderReferer}, false},
		{"multiple fields", []string{"Cookie", "User-Agent, Accept-Language"}, false},
	} {
		for _, route := range []string{"same route", "different route"} {
			t.Run(tc.name+"/"+route, func(t *testing.T) {
				var hits atomic.Int32
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					hits.Add(1)
					w.Header().Set(httpx.HeaderContentType, "text/html")
					for _, value := range tc.vary {
						w.Header().Add(httpx.HeaderVary, value)
					}
					fmt.Fprintf(w, `<html><video src="https://media.example.test/%s.mp4"></video></html>`, r.UserAgent())
				}))
				t.Cleanup(srv.Close)
				client := testClient()
				t.Cleanup(client.CloseIdleConnections)
				reg := NewRegistry(&config.Config{}, client)
				for i, agent := range []string{"browser-a", "browser-b", "browser-b"} {
					id := "first"
					if i > 0 && route == "different route" {
						id = "second"
					}
					ctx := httpx.WithRouteAgent(t.Context(), id, client, nil, agent)
					want := agent
					if tc.shared {
						want = "browser-a"
					}
					registryCookieMedia(t, reg, ctx, srv.URL+"/page", Options{}, "https://media.example.test/"+want+".mp4")
					reads := int32(i + 1)
					if tc.shared {
						reads = 1
					} else if tc.name == "referer" || tc.name == "wildcard" {
						// The generic reader must acquire its origin-Referer
						// representation before accepting the navigation's media.
						reads *= 2
					}
					if hits.Load() != reads {
						t.Fatalf("reads=%d, want %d", hits.Load(), reads)
					}
				}
			})
		}
	}
}

func TestNoncacheablePageKeepsItsAcquisitionAgent(t *testing.T) {
	for _, vary := range []string{httpx.HeaderUserAgent, "*"} {
		t.Run(vary, func(t *testing.T) {
			var hits atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hits.Add(1)
				w.Header().Set(httpx.HeaderContentType, "text/html")
				w.Header().Set(httpx.HeaderVary, vary)
				fmt.Fprintf(w, "<html>%s</html>", r.UserAgent())
			}))
			t.Cleanup(srv.Close)
			client := testClient()
			t.Cleanup(client.CloseIdleConnections)
			firstRoute := httpx.WithRouteAgent(t.Context(), "same", client, nil, "browser-a")
			otherRoute := httpx.WithRouteAgent(t.Context(), "same", client, nil, "browser-b")
			u := mustParse(t, srv.URL+"/page")
			var cache pageCache
			p := cache.get(firstRoute, client, u)
			body, _, err := fetchDocument(withPage(firstRoute, p), client, u, nil)
			if err != nil || body != "<html>browser-a</html>" || hits.Load() != 1 {
				t.Fatalf("original reader: body=%q err=%v hits=%d", body, err, hits.Load())
			}
			body, _, err = fetchDocument(withPage(otherRoute, p), client, u, nil)
			if err != nil || body != "<html>browser-b</html>" || hits.Load() != 2 {
				t.Fatalf("changed effective agent: body=%q err=%v hits=%d", body, err, hits.Load())
			}
		})
	}
}
