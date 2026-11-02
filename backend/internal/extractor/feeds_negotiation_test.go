// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

// The browser's refusal or subscribe notice is not the feed reader's
// representation. Both established feed paths and feeds hosts must get the
// bounded Accept negotiation before the registry gives up on the feed.
func TestFeedsNegotiateBrowserRepresentations(t *testing.T) {
	for _, source := range []struct{ name, raw string }{
		{"feed_path", "http://publisher.example.test/feed"},
		{"feed_host", "http://feeds.example.test/example-show"},
	} {
		for _, browser := range []struct {
			name, kind string
			status     int
		}{
			{"forbidden", "text/html", http.StatusForbidden},
			{"not_acceptable", "text/html", http.StatusNotAcceptable},
			{"forbidden_rss", "application/rss+xml", http.StatusForbidden},
			{"not_acceptable_json", "application/json", http.StatusNotAcceptable},
			{"refused_media", "audio/mpeg", http.StatusForbidden},
			{"plain_notice", "text/plain", http.StatusOK},
			{"untyped_notice", "", http.StatusOK},
			{"html_control", "text/html", http.StatusOK},
			{"binary_control", "application/octet-stream", http.StatusOK},
		} {
			t.Run(source.name+"/"+browser.name, func(t *testing.T) {
				var feedReads atomic.Int32
				proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.String() != source.raw {
						http.NotFound(w, r)
						return
					}
					if r.Header.Get(httpx.HeaderAccept) == feedAccept {
						feedReads.Add(1)
						w.Header().Set(httpx.HeaderContentType, "application/rss+xml")
						_, _ = io.WriteString(w, feedRSSFixture)
						return
					}
					if browser.kind == "" {
						w.Header()[httpx.HeaderContentType] = nil
					} else {
						w.Header().Set(httpx.HeaderContentType, browser.kind)
					}
					w.WriteHeader(browser.status)
					_, _ = io.WriteString(w, "Subscribe using a feed reader.")
				}))
				t.Cleanup(proxy.Close)
				base := testClient()
				t.Cleanup(base.CloseIdleConnections)
				client, err := base.ThroughProxy(proxy.URL)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(client.CloseIdleConnections)
				res, ex, err := NewRegistry(&config.Config{}, client).
					Extract(context.Background(), source.raw, Options{})
				if err != nil || ex == nil || ex.Name() != "feed" || fileCount(res) != 5 {
					t.Errorf("got %d files from %v: %v, want the feed's five enclosures", fileCount(res), ex, err)
				}
				if got := feedReads.Load(); got != 1 {
					t.Errorf("feed requests = %d, want negotiation and extraction to consume one response", got)
				}
			})
		}
	}
}

func TestFeedsNegotiationRejectsNonFeeds(t *testing.T) {
	for _, tc := range []struct {
		name, kind, body string
		status           int
	}{
		{"forbidden_feed", "application/rss+xml", feedRSSFixture, http.StatusForbidden},
		{"unacceptable_feed", "application/rss+xml", feedRSSFixture, http.StatusNotAcceptable},
		{"sitemap", "application/xml", `<urlset><url><loc>https://example.test/</loc></url></urlset>`, http.StatusOK},
		{"html_mentions_feed", "text/html", `<html><pre>&lt;rss&gt;</pre></html>`, http.StatusOK},
		{"plain_notice", "text/plain", "A feed reader is required.", http.StatusOK},
		{"past_sniff_bound", "application/xml", strings.Repeat(" ", config.FeedSniffBytes) + feedRSSFixture, http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var reads atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				reads.Add(1)
				if r.Header.Get(httpx.HeaderAccept) != feedAccept {
					t.Errorf("Accept = %q, want the feed's own request", r.Header.Get(httpx.HeaderAccept))
				}
				w.Header().Set(httpx.HeaderContentType, tc.kind)
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			t.Cleanup(srv.Close)
			client := testClient()
			t.Cleanup(client.CloseIdleConnections)
			feed := NewFeeds(client)
			page := fixturePage(t, srv.URL+"/feed", http.StatusForbidden, "text/html", "Use a feed reader.")
			for range 2 {
				if feed.Recognise(context.Background(), page) {
					t.Error("a refused or non-feed response was recognised as a feed")
				}
			}
			if got := reads.Load(); got != 1 {
				t.Errorf("recognition requests = %d, want one bounded, memoised request", got)
			}
		})
	}
}

func TestFeedsNegotiationRespectsRequestBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name, path, kind string
		status           int
		readError        error
		cancelled        bool
	}{
		{name: "unrelated_refusal", path: "/article", kind: "text/html", status: http.StatusForbidden},
		{name: "unrelated_notice", path: "/article", kind: "text/plain", status: http.StatusOK},
		{name: "media_file", path: "/feed/episode", kind: "audio/mpeg", status: http.StatusOK},
		{name: "read_failure", path: "/feed", kind: "text/html", status: http.StatusForbidden, readError: io.ErrUnexpectedEOF},
		{name: "no_response", path: "/feed", kind: "text/html"},
		{name: "cancelled", path: "/feed", kind: "text/html", status: http.StatusForbidden, cancelled: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var reads atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				reads.Add(1)
				w.Header().Set(httpx.HeaderContentType, "application/rss+xml")
				_, _ = io.WriteString(w, feedRSSFixture)
			}))
			t.Cleanup(srv.Close)
			client := testClient()
			t.Cleanup(client.CloseIdleConnections)
			page := fixturePage(t, srv.URL+tc.path, tc.status, tc.kind, "Use a feed reader.")
			page.Err = tc.readError
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.cancelled {
				cancel()
			}
			if NewFeeds(client).Recognise(ctx, page) {
				t.Error("recognised a feed outside its negotiation boundaries")
			}
			if got := reads.Load(); got != 0 {
				t.Errorf("requests = %d, want none", got)
			}
		})
	}
}
