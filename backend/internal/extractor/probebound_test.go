// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

// A probe whose body is cut short — past the response bound, or by a
// dropped connection — is a failure: never cached, never offered as the
// document, and asked again by the next caller, which a complete answer
// then satisfies. A body exactly at the bound is complete.
func TestProbeRefusesIncompleteDocuments(t *testing.T) {
	page := func(n int) string {
		head := "<html><title>Page</title>"
		return head + strings.Repeat("x", n-len(head))
	}
	for _, tc := range []struct {
		name  string
		serve func(w http.ResponseWriter)
		ok    bool
	}{
		{"under", func(w http.ResponseWriter) { io.WriteString(w, page(config.MaxResponseBytes-1)) }, true},
		{"exact", func(w http.ResponseWriter) { io.WriteString(w, page(config.MaxResponseBytes)) }, true},
		{"over", func(w http.ResponseWriter) { io.WriteString(w, page(config.MaxResponseBytes+64)) }, false},
		{"interrupted", func(w http.ResponseWriter) {
			// A length promised and not delivered: the client sees an
			// unexpected EOF after the bytes that did arrive.
			w.Header().Set("Content-Length", strconv.Itoa(4096))
			io.WriteString(w, page(1024))
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var hits atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set(httpx.HeaderContentType, "text/html")
				if hits.Add(1) > 1 {
					io.WriteString(w, "<html><title>Complete</title></html>")
					return
				}
				tc.serve(w)
			}))
			defer srv.Close()
			client := testClient()
			u, _ := url.Parse(srv.URL + "/a")
			var cache pageCache
			p := cache.get(context.Background(), client, u)
			if _, ok := p.Document(); ok != tc.ok || (p.Err == nil) != tc.ok {
				t.Fatalf("document ok=%v err=%v, want ok=%v", ok, p.Err, tc.ok)
			}
			if !tc.ok && !strings.HasPrefix(p.Body, "<html><title>Page") {
				t.Errorf("the prefix was not kept for recognisers: %q", p.Body[:min(len(p.Body), 32)])
			}
			again := cache.get(context.Background(), client, u)
			want, hitsWant := "Page", int32(1)
			if !tc.ok {
				want, hitsWant = "Complete", 2
			}
			if again.Title() != want || hits.Load() != hitsWant {
				t.Errorf("second read title=%q hits=%d, want %q after %d", again.Title(), hits.Load(), want, hitsWant)
			}
		})
	}
}

// A probe that followed a redirect keeps the final URL, and a body that
// was not text is never read.
func TestProbeKeepsRedirectAndSkipsMedia(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/old":
			http.Redirect(w, r, "/new", http.StatusFound)
		case "/new":
			w.Header().Set(httpx.HeaderContentType, "text/html")
			io.WriteString(w, "<title>Moved</title>")
		default:
			w.Header().Set(httpx.HeaderContentType, "video/mp4")
			w.Header().Set("Content-Length", strconv.Itoa(config.MaxResponseBytes*2))
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(make([]byte, 1<<20))
		}
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL + "/old")
	p := probePage(context.Background(), testClient(), u)
	if doc, ok := p.Document(); !ok || doc != "<title>Moved</title>" || p.Final.Path != "/new" {
		t.Fatalf("redirected probe: ok=%v final=%v err=%v", ok, p.Final, p.Err)
	}
	m, _ := url.Parse(srv.URL + "/clip.mp4")
	p = probePage(context.Background(), testClient(), m)
	if p.Err != nil || p.Body != "" || p.Status != http.StatusOK {
		t.Fatalf("media probe: status=%d body=%d err=%v", p.Status, len(p.Body), p.Err)
	}
}
