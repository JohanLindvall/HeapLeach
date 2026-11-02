// SPDX-License-Identifier: MIT

package extractor

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

// A no-store/no-cache response belongs to its acquisition, even when only
// a redirect or HEAD carries the restriction. Its reader can consume the
// probe, but a new submission must ask again and retain none of its answers.
func TestPageCacheHonorsResponsePolicy(t *testing.T) {
	for _, policy := range []struct {
		name      string
		headers   http.Header
		cacheable bool
	}{
		{"default", nil, true},
		{"private", http.Header{httpx.HeaderCacheControl: {"private, max-age=60"}}, true},
		{"no-store", http.Header{httpx.HeaderCacheControl: {httpx.CacheControlNoStore}}, false},
		{"multiple headers", http.Header{httpx.HeaderCacheControl: {"private", "max-age=60, No-StOrE"}}, false},
		{"no-cache fields", http.Header{httpx.HeaderCacheControl: {`no-cache="Set-Cookie"`}}, false},
		{"expired", http.Header{httpx.HeaderCacheControl: {"max-age=0"}}, false},
		{"aged", http.Header{httpx.HeaderCacheControl: {"max-age=60"}, httpx.HeaderAge: {"60"}}, false},
		{"expires", http.Header{httpx.HeaderExpires: {time.Unix(1, 0).UTC().Format(http.TimeFormat)}}, false},
		{"wildcard", http.Header{httpx.HeaderVary: {"*"}}, false},
		{"agent", http.Header{httpx.HeaderVary: {httpx.HeaderUserAgent}}, false},
		{"supported vary", http.Header{httpx.HeaderVary: {"Cookie, Accept-Language"}}, true},
	} {
		for _, kind := range []string{"document", "redirect", "HEAD"} {
			t.Run(policy.name+"/"+kind, func(t *testing.T) {
				var hits atomic.Int32
				path := "/page"
				if kind == "HEAD" {
					path = "/first.mp4"
				}
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == path {
						hits.Add(1)
						for name, values := range policy.headers {
							for _, value := range values {
								w.Header().Add(name, value)
							}
						}
						if kind == "redirect" {
							http.Redirect(w, r, "/final", http.StatusFound)
							return
						}
					}
					if kind == "HEAD" {
						if r.Method != http.MethodHead {
							t.Error("media body requested")
						}
						w.Header().Set(httpx.HeaderContentType, "video/mp4")
						return
					}
					w.Header().Set(httpx.HeaderContentType, "text/html")
					fmt.Fprintf(w, "<html><title>Visit %d</title></html>", hits.Load())
				}))
				t.Cleanup(srv.Close)
				client := testClient()
				t.Cleanup(client.CloseIdleConnections)
				u := mustParse(t, srv.URL+path)
				var cache pageCache
				first := cache.get(t.Context(), client, u)
				if first.Err != nil || first.Status != http.StatusOK {
					t.Fatalf("probe: %v, status %d", first.Err, first.Status)
				}
				if kind != "HEAD" {
					body, _, err := fetchDocument(withPage(t.Context(), first), client, u, nil)
					if err != nil || body != first.Body || hits.Load() != 1 {
						t.Fatalf("originating reader did not reuse its probe: body=%q err=%v reads=%d", body, err, hits.Load())
					}
				}
				second := cache.get(t.Context(), client, u)
				want := int32(2)
				if policy.cacheable {
					want = 1
				}
				if (first == second) != policy.cacheable || hits.Load() != want {
					t.Fatalf("same page=%v reads=%d, want cacheable=%v reads=%d", first == second, hits.Load(), policy.cacheable, want)
				}
				if !policy.cacheable {
					memo(first, "answer", func() string { return "not retained" })
					cache.mu.Lock()
					defer cache.mu.Unlock()
					if len(cache.entries) != 0 || len(cache.shared) != 0 || cache.bytes != 0 {
						t.Fatalf("noncacheable response retained: entries=%d shared=%d bytes=%d", len(cache.entries), len(cache.shared), cache.bytes)
					}
				}
			})
		}
	}
}
