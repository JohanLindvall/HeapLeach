// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
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

// bigFeed is an RSS document of n episodes, each padded with show notes so
// the whole runs to about size bytes.
func bigFeed(n, size int) string {
	pad := strings.Repeat("notes ", max(size/n/6, 1))
	var b strings.Builder
	b.WriteString(`<?xml version="1.0"?><rss version="2.0"><channel><title>A Long Show</title>`)
	for i := range n {
		fmt.Fprintf(&b, `<item><title>Episode %d</title><description>%s</description>`+
			`<enclosure url="https://cdn.example.test/ep%d.mp3" type="audio/mpeg" length="1"/></item>`, i, pad, i)
	}
	b.WriteString(`</channel></rss>`)
	return b.String()
}

// TestFeedsPastTheResponseBound: a feed longer than the ordinary response
// bound is still a feed, recognised by its head and read whole, rather than
// queued as one XML file.
func TestFeedsPastTheResponseBound(t *testing.T) {
	doc := bigFeed(500, config.MaxResponseBytes+config.MaxResponseBytes/8)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set(httpx.HeaderContentType, "application/rss+xml")
		_, _ = io.WriteString(w, doc)
	}))
	t.Cleanup(srv.Close)
	for _, path := range []string{"/shows/long.xml", "/feed"} {
		res, ex, err := NewRegistry(&config.Config{}, testClient()).
			Extract(context.Background(), srv.URL+path, Options{})
		if err != nil || ex.Name() != "feed" || len(res.Files) != 500 || res.Note != "" {
			t.Fatalf("%s: got %d files (%q) from %v: %v, want the feed's 500", path, fileCount(res), noteOf(res), ex, err)
		}
	}
}

// TestFeedsAskedForByName: a feed-shaped URL whose probe got a generic
// binary type, which the probe does not read, or an HTML subscribe page
// served to its browser-shaped Accept, is asked again for a feed by name.
func TestFeedsAskedForByName(t *testing.T) {
	for _, tc := range []struct{ name, served string }{
		{"octet-stream", "application/octet-stream"},
		{"negotiated", "application/rss+xml"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.name == "negotiated" && !strings.Contains(r.Header.Get(httpx.HeaderAccept), "rss") {
					w.Header().Set(httpx.HeaderContentType, "text/html")
					_, _ = io.WriteString(w, `<html><title>Subscribe</title><body>Subscribe</body></html>`)
					return
				}
				w.Header().Set(httpx.HeaderContentType, tc.served)
				_, _ = io.WriteString(w, feedRSSFixture)
			}))
			t.Cleanup(srv.Close)
			for _, path := range []string{"/feed", "/shows/one.rss", "/podcast/?feed=podcast"} {
				res, ex, err := NewRegistry(&config.Config{}, testClient()).
					Extract(context.Background(), srv.URL+path, Options{})
				if err != nil || ex.Name() != "feed" || len(res.Files) != 5 {
					t.Fatalf("%s: got %d files from %v: %v, want the feed's 5", path, fileCount(res), ex, err)
				}
			}
		})
	}
}

// TestFeedsNeverAskAgainForMedia: a media file under a feed-shaped path is
// not asked for again as a feed.
func TestFeedsNeverAskAgainForMedia(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(httpx.HeaderAccept) == feedAccept {
			hits.Add(1)
		}
		w.Header().Set(httpx.HeaderContentType, "audio/mpeg")
		_, _ = io.WriteString(w, "ID3")
	}))
	t.Cleanup(srv.Close)
	_, ex, err := NewRegistry(&config.Config{}, testClient()).
		Extract(context.Background(), srv.URL+"/feed/episode", Options{})
	if err != nil || ex.Name() == "feed" {
		t.Fatalf("got %v: %v, want the fallback's file", ex, err)
	}
	if n := hits.Load(); n != 0 {
		t.Errorf("asked for it as a feed %d times, want never", n)
	}
}

func fileCount(res *Result) int {
	if res == nil {
		return 0
	}
	return len(res.Files)
}

func noteOf(res *Result) string {
	if res == nil {
		return ""
	}
	return res.Note
}

// feedsHostClient reaches, through a forward proxy, a stand-in for
// publishers' feeds hosts: it serves an HTML subscribe page to a browser's
// Accept and the feed to one that asks for it by name, at any path on any
// host, and a show page on page.example.test that links its feed.
func feedsHostClient(t *testing.T) *httpx.Client {
	t.Helper()
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.Host, "page."):
			w.Header().Set(httpx.HeaderContentType, "text/html")
			_, _ = io.WriteString(w, `<html><title>A Show</title><body>`+
				`<a href="http://feeds.example.test/the-example-show">Subscribe</a></body></html>`)
		case r.Header.Get(httpx.HeaderAccept) == feedAccept:
			w.Header().Set(httpx.HeaderContentType, "application/rss+xml")
			_, _ = io.WriteString(w, feedRSSFixture)
		default:
			w.Header().Set(httpx.HeaderContentType, "text/html")
			_, _ = io.WriteString(w, `<html><title>Subscribe</title><body>Subscribe</body></html>`)
		}
	}))
	t.Cleanup(proxy.Close)
	client, err := testClient().ThroughProxy(proxy.URL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.CloseIdleConnections)
	return client
}

// TestFeedsOnAFeedsHost: a URL on a feeds., feed. or rss. host, whatever
// its path, is asked for as a feed by name when the probe was served a
// subscribe page, and a link to one on another site's page is followed.
func TestFeedsOnAFeedsHost(t *testing.T) {
	client := feedsHostClient(t)
	reg := NewRegistry(&config.Config{}, client)
	for _, raw := range []string{
		"http://feeds.example.test/the-example-show",
		"http://feed.example.test/shows/1",
		"http://rss.example.test/shows/one",
	} {
		res, ex, err := reg.Extract(context.Background(), raw, Options{})
		if err != nil || ex.Name() != "feed" || len(res.Files) != 5 {
			t.Fatalf("%s: got %d files from %v: %v, want the feed's 5", raw, fileCount(res), ex, err)
		}
	}
	from := mustParse(t, "http://page.example.test/a-show")
	if ex, ok := reg.Follows(mustParse(t, "http://feeds.example.test/the-example-show"), from); !ok || ex.Name() != "feed" {
		t.Errorf("a feeds host's link is not followed as a feed: %v %v", ex, ok)
	}
	if ex, ok := reg.Follows(mustParse(t, "http://feedback.example.test/a-story"), from); ok && ex.Name() == "feed" {
		t.Error("a host that merely begins with feed was followed as a feed")
	}
	res, _, err := reg.Extract(context.Background(), "links:http://page.example.test/a-show", Options{})
	if err != nil || len(res.Files) != 5 {
		t.Fatalf("harvest: got %d files: %v, want the linked feed's 5", fileCount(res), err)
	}
}
