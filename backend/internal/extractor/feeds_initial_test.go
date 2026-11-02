// SPDX-License-Identifier: MIT

package extractor

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

// A successful navigation response can consume a feed just as a negotiated
// response can. The first request must already use the feed representation,
// and identification must hand that entire response to its reader once.
func TestFeedsAcquireTheFirstResponse(t *testing.T) {
	for _, source := range []string{"/feed", "/show.xml"} {
		for _, variant := range []string{"varying_xml", "identical_xml", "plain_prefix"} {
			t.Run(source+"/"+variant, func(t *testing.T) {
				var reads, navigation atomic.Int32
				doc := feedRSSFixture
				kind := "application/rss+xml"
				if variant == "plain_prefix" {
					kind = "text/plain"
					doc = strings.Replace(doc, "<channel>", "<channel><!--"+strings.Repeat("x", config.ProbeSniffBytes)+"-->", 1)
				}
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path != source || r.Method != http.MethodGet {
						t.Errorf("unexpected request: %s %s", r.Method, r.URL)
						http.NotFound(w, r)
						return
					}
					if reads.Add(1) != 1 {
						http.Error(w, "Already delivered", http.StatusGone)
						return
					}
					http.SetCookie(w, &http.Cookie{Name: "delivery", Value: "done", Path: "/"})
					w.Header().Set(httpx.HeaderCacheControl, httpx.CacheControlNoStore)
					w.Header().Set(httpx.HeaderContentType, kind)
					if variant == "varying_xml" {
						w.Header().Set(httpx.HeaderVary, httpx.HeaderAccept)
					}
					if r.Header.Get(httpx.HeaderAccept) != feedAccept {
						navigation.Add(1)
						if variant == "varying_xml" {
							_, _ = io.WriteString(w, `<rss><channel><title>Preview</title></channel></rss>`)
							return
						}
					}
					_, _ = io.WriteString(w, doc)
				}))
				t.Cleanup(srv.Close)
				client := testClient()
				t.Cleanup(client.CloseIdleConnections)
				res, ex, err := NewRegistry(&config.Config{}, client).Extract(t.Context(), srv.URL+source, Options{})
				if err != nil || ex == nil || ex.Name() != "feed" || fileCount(res) != 5 || noteOf(res) != "" {
					t.Fatalf("first feed response: %d files from %v, note %q, error %v", fileCount(res), ex, noteOf(res), err)
				}
				if reads.Load() != 1 || navigation.Load() != 0 {
					t.Errorf("requests=%d navigation=%d, want one correctly negotiated first response", reads.Load(), navigation.Load())
				}
			})
		}
	}
}

// A feed-looking URL remains a guess. Its feed Accept can select a different
// HTML document, so the generic reader still receives normal navigation.
func TestFeedFirstPreservesNonFeedNavigation(t *testing.T) {
	for _, kind := range []string{"text/html", "application/octet-stream"} {
		t.Run(kind, func(t *testing.T) {
			var feeds, navigation atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set(httpx.HeaderContentType, "text/html")
				file := "complete"
				if r.Header.Get(httpx.HeaderAccept) == feedAccept {
					feeds.Add(1)
					w.Header().Set(httpx.HeaderContentType, kind)
					file = "preview"
				} else {
					navigation.Add(1)
				}
				_, _ = fmt.Fprintf(w, `<html><video src="https://media.example.test/%s.mp4"></video></html>`, file)
			}))
			t.Cleanup(srv.Close)
			client := testClient()
			t.Cleanup(client.CloseIdleConnections)
			res, ex, err := NewRegistry(&config.Config{}, client).Extract(t.Context(), srv.URL+"/feed", Options{})
			if err != nil || ex == nil || ex.Name() != "direct" || fileCount(res) != 1 || res.Files[0].URL != "https://media.example.test/complete.mp4" {
				t.Fatalf("generic representation: %+v from %v, error %v", res, ex, err)
			}
			if feeds.Load() != 1 || navigation.Load() != 1 {
				t.Errorf("feed/navigation requests=%d/%d, want 1/1", feeds.Load(), navigation.Load())
			}
		})
	}
}

// A navigation response can establish the feed's session. A negative answer
// obtained before those cookies must not suppress the new feed request.
func TestFeedFirstReconsidersAfterNavigationCookies(t *testing.T) {
	var feeds, navigation atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(httpx.HeaderAccept) == feedAccept {
			feeds.Add(1)
			if cookie, err := r.Cookie("ready"); err == nil && cookie.Value == "yes" {
				w.Header().Set(httpx.HeaderContentType, "application/rss+xml")
				_, _ = io.WriteString(w, feedRSSFixture)
				return
			}
		} else {
			navigation.Add(1)
			http.SetCookie(w, &http.Cookie{Name: "ready", Value: "yes", Path: "/"})
		}
		w.Header().Set(httpx.HeaderContentType, "text/html")
		_, _ = io.WriteString(w, "<html>Subscribe with a feed reader.</html>")
	}))
	t.Cleanup(srv.Close)
	client := testClient()
	t.Cleanup(client.CloseIdleConnections)
	res, ex, err := NewRegistry(&config.Config{}, client).Extract(t.Context(), srv.URL+"/feed", Options{})
	if err != nil || ex == nil || ex.Name() != "feed" || fileCount(res) != 5 {
		t.Fatalf("feed after session setup: %+v from %v, error %v", res, ex, err)
	}
	if feeds.Load() != 2 || navigation.Load() != 1 {
		t.Errorf("feed/navigation requests=%d/%d, want 2/1", feeds.Load(), navigation.Load())
	}
}

func TestFeedFirstPreservesNamedMediaProbes(t *testing.T) {
	var heads, gets, feeds atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(httpx.HeaderAccept) == feedAccept {
			feeds.Add(1)
		}
		if r.Method == http.MethodHead {
			heads.Add(1)
		} else {
			gets.Add(1)
		}
		w.Header().Set(httpx.HeaderContentType, "audio/mpeg")
	}))
	t.Cleanup(srv.Close)
	client := testClient()
	t.Cleanup(client.CloseIdleConnections)
	source := srv.URL + "/feed/episode.mp3"
	res, ex, err := NewRegistry(&config.Config{}, client).Extract(t.Context(), source, Options{})
	if err != nil || ex == nil || ex.Name() != "direct" || fileCount(res) != 1 || res.Files[0].URL != source {
		t.Fatalf("named file: %+v from %v, error %v", res, ex, err)
	}
	if heads.Load() != 1 || gets.Load() != 0 || feeds.Load() != 0 {
		t.Errorf("HEAD/GET/feed requests=%d/%d/%d, want 1/0/0", heads.Load(), gets.Load(), feeds.Load())
	}
}

// Named XML can be an ordinary file rather than a feed. Identification's
// successful GET must not be spent again merely to confirm that same file.
func TestFeedFirstPreservesSingleUseNonFeedXML(t *testing.T) {
	var gets atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && gets.Add(1) != 1 {
			http.Error(w, "Already delivered", http.StatusGone)
			return
		}
		w.Header().Set(httpx.HeaderContentType, "application/xml")
		_, _ = io.WriteString(w, `<document><title>Synthetic Document</title></document>`)
	}))
	t.Cleanup(srv.Close)
	client := testClient()
	t.Cleanup(client.CloseIdleConnections)
	source := srv.URL + "/synthetic.xml"
	res, ex, err := NewRegistry(&config.Config{}, client).Extract(t.Context(), source, Options{})
	if err != nil || ex == nil || ex.Name() != "direct" || fileCount(res) != 1 || res.Files[0].URL != source {
		t.Fatalf("XML file: %+v from %v, error %v", res, ex, err)
	}
	if gets.Load() != 1 {
		t.Errorf("GET requests=%d, want one", gets.Load())
	}
}
