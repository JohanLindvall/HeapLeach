// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

// Cookies with one name but different paths reach the server in path order.
// The same values in another order can select another session, including at
// a redirect destination whose cookies were absent from the initial request.
func TestRegistryPageCachePreservesCookieOrder(t *testing.T) {
	// Keep the redirect outside player item forms: their additional API or
	// canonical-page questions belong to each acquisition, not this cache.
	for _, path := range []string{"/clip/watch", "/generic/start"} {
		t.Run(path, func(t *testing.T) {
			var reads atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/prepare/alpha", "/prepare/beta":
					specific, broad := "alpha", "beta"
					if r.URL.Path == "/prepare/beta" {
						specific, broad = broad, specific
					}
					http.SetCookie(w, &http.Cookie{Name: "session", Value: specific, Path: "/clip/watch"})
					http.SetCookie(w, &http.Cookie{Name: "session", Value: broad, Path: "/clip"})
					_, _ = fmt.Fprint(w, "ready")
				case "/generic/start":
					http.Redirect(w, r, "/clip/watch", http.StatusFound)
				case "/clip/watch":
					reads.Add(1)
					cookie, err := r.Cookie("session")
					if err != nil {
						http.Error(w, "no session", http.StatusForbidden)
						return
					}
					w.Header().Set(httpx.HeaderContentType, "text/html")
					w.Header().Set(httpx.HeaderVary, httpx.HeaderCookie)
					fmt.Fprintf(w, `<html><video src="https://media.example.test/%s.mp4"></video></html>`, cookie.Value)
				default:
					http.NotFound(w, r)
				}
			}))
			t.Cleanup(srv.Close)
			client := testClient()
			t.Cleanup(client.CloseIdleConnections)
			reg := NewRegistry(&config.Config{}, client)
			contexts := make(map[string]context.Context)
			for _, session := range []string{"alpha", "beta"} {
				route := testClient()
				t.Cleanup(route.CloseIdleConnections)
				if _, err := route.GetString(t.Context(), srv.URL+"/prepare/"+session, nil); err != nil {
					t.Fatal(err)
				}
				contexts[session] = httpx.WithRoute(t.Context(), session, route, nil)
			}
			for _, session := range []string{"alpha", "beta"} {
				registryCookieMedia(t, reg, contexts[session], srv.URL+path, Options{}, "https://media.example.test/"+session+".mp4")
				firstReads := reads.Load()
				registryCookieMedia(t, reg, contexts[session], srv.URL+path, Options{}, "https://media.example.test/"+session+".mp4")
				if got := reads.Load(); got != firstReads {
					t.Errorf("repeated %s extraction fetched the page again: reads %d -> %d", session, firstReads, got)
				}
			}
		})
	}
}
