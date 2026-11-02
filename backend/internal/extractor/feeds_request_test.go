// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

// The submitted feed shape survives a redirect to an ordinary path. The
// feed request must start at that submitted URL: its Accept can select a
// different redirect destination than the browser's subscribe page.
func TestFeedsNegotiateTheSubmittedURL(t *testing.T) {
	for _, source := range []string{"http://publisher.example.test/feed", "http://feeds.example.test/example-show"} {
		t.Run(source, func(t *testing.T) {
			var sourceReads, feedReads atomic.Int32
			proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.String() {
				case source:
					where := "http://publisher.example.test/subscribe"
					if r.Header.Get(httpx.HeaderAccept) == feedAccept {
						sourceReads.Add(1)
						where = "http://publisher.example.test/delivery"
					}
					http.Redirect(w, r, where, http.StatusFound)
				case "http://publisher.example.test/subscribe":
					w.Header().Set(httpx.HeaderContentType, "text/html")
					_, _ = io.WriteString(w, "<html>Subscribe with a feed reader.</html>")
				case "http://publisher.example.test/delivery":
					feedReads.Add(1)
					w.Header().Set(httpx.HeaderContentType, "application/rss+xml")
					_, _ = io.WriteString(w, feedRSSFixture)
				default:
					http.NotFound(w, r)
				}
			}))
			t.Cleanup(proxy.Close)
			base := testClient()
			t.Cleanup(base.CloseIdleConnections)
			client, err := base.ThroughProxy(proxy.URL)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(client.CloseIdleConnections)
			res, ex, err := NewRegistry(&config.Config{}, client).Extract(t.Context(), source, Options{})
			if err != nil || ex == nil || ex.Name() != "feed" || fileCount(res) != 5 {
				t.Fatalf("redirected feed: %d files from %v, error %v", fileCount(res), ex, err)
			}
			if sourceReads.Load() != 1 || feedReads.Load() != 1 {
				t.Errorf("feed source/destination requests = %d/%d, want 1/1", sourceReads.Load(), feedReads.Load())
			}
		})
	}
}

func TestFeedsNegotiationRetriesAndPreservesTransientFailures(t *testing.T) {
	for _, status := range []int{http.StatusServiceUnavailable, http.StatusTooManyRequests} {
		for _, recovers := range []bool{true, false} {
			t.Run(fmt.Sprintf("status=%d/recovers=%t", status, recovers), func(t *testing.T) {
				var reads atomic.Int32
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Header.Get(httpx.HeaderAccept) != feedAccept {
						w.Header().Set(httpx.HeaderContentType, "text/html")
						_, _ = io.WriteString(w, "<html>Subscribe with a feed reader.</html>")
						return
					}
					if n := reads.Add(1); n == 1 || !recovers {
						w.Header().Set(httpx.HeaderRetryAfter, "0")
						http.Error(w, "Please try again.", status)
						return
					}
					w.Header().Set(httpx.HeaderContentType, "application/rss+xml")
					_, _ = io.WriteString(w, feedRSSFixture)
				}))
				t.Cleanup(srv.Close)
				client := testClient()
				t.Cleanup(client.CloseIdleConnections)
				res, ex, err := NewRegistry(&config.Config{}, client).Extract(t.Context(), srv.URL+"/feed", Options{})
				if ex == nil || ex.Name() != "feed" {
					t.Fatalf("transient failure became non-recognition: %v, %v", ex, err)
				}
				if recovers {
					if err != nil || fileCount(res) != 5 {
						t.Fatalf("recovered feed: %d files, error %v", fileCount(res), err)
					}
				} else if !httpx.HasStatus(err, status) || res != nil {
					t.Fatalf("wanted preserved HTTP %d, got result %+v, error %v", status, res, err)
				}
				if n := reads.Load(); n != config.PlatformProbeAttempts {
					t.Errorf("feed requests = %d, want %d bounded attempts without another reader fetch", n, config.PlatformProbeAttempts)
				}
			})
		}
	}
}

func TestFeedsNegotiationHonorsRetryAfter(t *testing.T) {
	for _, status := range []int{http.StatusServiceUnavailable, http.StatusTooManyRequests} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var reads, early atomic.Int32
			var allowedAt atomic.Int64
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get(httpx.HeaderAccept) != feedAccept {
					w.Header().Set(httpx.HeaderContentType, "text/html")
					_, _ = io.WriteString(w, "<html>Subscribe with a feed reader.</html>")
					return
				}
				if reads.Add(1) == 1 {
					allowedAt.Store(time.Now().Add(time.Second).UnixNano())
					w.Header().Set(httpx.HeaderRetryAfter, "1")
					http.Error(w, "The feed will be ready in one second.", status)
					return
				}
				if time.Now().UnixNano() < allowedAt.Load() {
					early.Add(1)
					http.Error(w, "The feed is not ready yet.", status)
					return
				}
				w.Header().Set(httpx.HeaderContentType, "application/rss+xml")
				_, _ = io.WriteString(w, feedRSSFixture)
			}))
			t.Cleanup(srv.Close)
			client := testClient()
			t.Cleanup(client.CloseIdleConnections)
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			res, ex, err := NewRegistry(&config.Config{}, client).Extract(ctx, srv.URL+"/feed", Options{})
			if err != nil || ex == nil || ex.Name() != "feed" || fileCount(res) != 5 {
				t.Fatalf("feed after Retry-After: %d files from %v, error %v", fileCount(res), ex, err)
			}
			if reads.Load() != 2 || early.Load() != 0 {
				t.Errorf("feed requests = %d, early retries = %d; want 2/0", reads.Load(), early.Load())
			}
		})
	}
}

func TestFeedsNegotiationRetryAfterReleasesAdmissionAndCancels(t *testing.T) {
	var reads atomic.Int32
	refused := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if reads.Add(1) == 1 {
			defer close(refused)
		}
		w.Header().Set(httpx.HeaderRetryAfter, "3600")
		http.Error(w, "The feed is temporarily limited.", http.StatusTooManyRequests)
	}))
	t.Cleanup(srv.Close)
	client := testClient()
	t.Cleanup(client.CloseIdleConnections)
	u, err := url.Parse(srv.URL + "/feed")
	if err != nil {
		t.Fatal(err)
	}
	admission := httpx.NewAdmission(1)
	ctx, cancel := context.WithCancel(httpx.WithAdmission(t.Context(), admission))
	defer cancel()
	done := make(chan feedAnswer, 1)
	go func() { done <- (&Feeds{client: client}).askFeed(ctx, u) }()
	select {
	case <-refused:
	case <-time.After(time.Second):
		t.Fatal("feed request did not reach the server")
	}
	// Acquiring the only slot proves the failed attempt released it before
	// the long server-requested wait, so other metadata can proceed.
	check, stop := context.WithTimeout(ctx, time.Second)
	defer stop()
	release, err := httpx.Admit(check)
	if err != nil {
		t.Fatalf("Retry-After held the metadata admission: %v", err)
	}
	release()
	select {
	case answer := <-done:
		t.Fatalf("Retry-After finished before cancellation: %+v", answer)
	default:
	}
	cancel()
	select {
	case answer := <-done:
		if !errors.Is(answer.err, context.Canceled) || reads.Load() != 1 {
			t.Fatalf("canceled Retry-After: error %v, requests %d", answer.err, reads.Load())
		}
	case <-time.After(time.Second):
		t.Fatal("Retry-After did not stop on cancellation")
	}
}

// The feed-specific response is consumed once, including enclosures beyond
// the identification prefix. Its own cookie update and no-store policy do
// not force its originating reader to consume the signed URL twice.
func TestFeedsConsumeSingleUseNegotiation(t *testing.T) {
	var reads atomic.Int32
	doc := strings.Replace(feedRSSFixture, "<channel>", "<channel><!--"+strings.Repeat("x", config.FeedSniffBytes)+"-->", 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(httpx.HeaderAccept) != feedAccept {
			w.Header().Set(httpx.HeaderContentType, "text/html")
			_, _ = io.WriteString(w, "<html>Subscribe with a feed reader.</html>")
			return
		}
		if reads.Add(1) != 1 {
			http.Error(w, "The feed was already delivered.", http.StatusGone)
			return
		}
		http.SetCookie(w, &http.Cookie{Name: "delivery", Value: "complete", Path: "/"})
		w.Header().Set(httpx.HeaderCacheControl, httpx.CacheControlNoStore)
		w.Header().Set(httpx.HeaderContentType, "application/rss+xml")
		_, _ = io.WriteString(w, doc)
	}))
	t.Cleanup(srv.Close)
	client := testClient()
	t.Cleanup(client.CloseIdleConnections)
	res, ex, err := NewRegistry(&config.Config{}, client).Extract(t.Context(), srv.URL+"/feed", Options{})
	if err != nil || ex == nil || ex.Name() != "feed" || fileCount(res) != 5 || res.Note != "" {
		t.Fatalf("single-use feed: %d files from %v, note %q, error %v", fileCount(res), ex, noteOf(res), err)
	}
	if n := reads.Load(); n != 1 {
		t.Errorf("feed requests = %d, want one complete acquisition", n)
	}
}

func TestFeedNegotiatedAnswerBelongsToItsAcquisition(t *testing.T) {
	var reads atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(httpx.HeaderAccept) != feedAccept {
			w.Header().Set(httpx.HeaderContentType, "text/html")
			_, _ = io.WriteString(w, "<html>Subscribe with a feed reader.</html>")
			return
		}
		n := reads.Add(1)
		w.Header().Set(httpx.HeaderCacheControl, httpx.CacheControlNoStore)
		w.Header().Set(httpx.HeaderContentType, "application/rss+xml")
		fmt.Fprintf(w, `<rss><channel><title>Example Show</title><item><title>Episode</title><enclosure url="https://media.example.test/episode-%d.mp3"/></item></channel></rss>`, n)
	}))
	t.Cleanup(srv.Close)
	client := testClient()
	t.Cleanup(client.CloseIdleConnections)
	reg := NewRegistry(&config.Config{}, client)
	for n := 1; n <= 2; n++ {
		res, _, err := reg.Extract(t.Context(), srv.URL+"/feed", Options{})
		want := fmt.Sprintf("https://media.example.test/episode-%d.mp3", n)
		if err != nil || fileCount(res) != 1 || res.Files[0].URL != want {
			t.Fatalf("acquisition %d reused a prior feed: %+v, %v", n, res, err)
		}
	}
	if n := reads.Load(); n != 2 {
		t.Errorf("feed requests = %d, want one per acquisition", n)
	}
}

func TestFeedNegotiatedAnswerChecksReaderIdentity(t *testing.T) {
	for _, change := range []string{"unchanged", "cookie", "redirect-cookie", "client", "route", "agent"} {
		t.Run(change, func(t *testing.T) {
			var reads atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get(httpx.HeaderAccept) != feedAccept {
					w.Header().Set(httpx.HeaderContentType, "text/html")
					_, _ = io.WriteString(w, "<html>Subscribe with a feed reader.</html>")
					return
				}
				if r.URL.Path == "/feed" {
					http.Redirect(w, r, "/delivery/feed", http.StatusFound)
					return
				}
				n := reads.Add(1)
				w.Header().Set(httpx.HeaderContentType, "application/rss+xml")
				fmt.Fprintf(w, `<rss><channel><title>Example Show</title><item><title>Episode</title><enclosure url="https://media.example.test/episode-%d.mp3"/></item></channel></rss>`, n)
			}))
			t.Cleanup(srv.Close)
			client := testClient()
			t.Cleanup(client.CloseIdleConnections)
			jar, err := cookiejar.New(nil)
			if err != nil {
				t.Fatal(err)
			}
			ctx := httpx.WithCookieJar(t.Context(), jar)
			u := mustParse(t, srv.URL+"/feed")
			page := probePage(ctx, client, u).acquisition()
			feed := NewFeeds(client)
			if !feed.Recognise(ctx, page) {
				t.Fatal("feed was not recognised")
			}
			switch change {
			case "cookie", "redirect-cookie":
				path := "/"
				if change == "redirect-cookie" {
					path = "/delivery"
				}
				jar.SetCookies(mustParse(t, srv.URL+path), []*http.Cookie{{Name: "session", Value: "new", Path: path}})
			case "client":
				other := httpx.New("another-agent", "en-US", 0, time.Second)
				t.Cleanup(other.CloseIdleConnections)
				feed = NewFeeds(other)
			case "route":
				ctx = httpx.WithRoute(ctx, "new-route", client, nil)
			case "agent":
				ctx = httpx.WithRouteAgent(ctx, "", client, nil, "another-agent")
			}
			res, err := feed.Extract(withPage(ctx, page), u, Options{})
			wantReads := int32(2)
			if change == "unchanged" {
				wantReads = 1
			}
			want := fmt.Sprintf("https://media.example.test/episode-%d.mp3", wantReads)
			if err != nil || fileCount(res) != 1 || res.Files[0].URL != want || reads.Load() != wantReads {
				t.Fatalf("reader %s: result %+v, error %v, requests %d; want %s", change, res, err, reads.Load(), want)
			}
		})
	}
}

func TestFeedNegotiatedBodyKeepsReaderTimeout(t *testing.T) {
	old := platformProbeTimeout
	platformProbeTimeout = 100 * time.Millisecond
	t.Cleanup(func() { platformProbeTimeout = old })
	prefix := `<rss><channel><title>Example Show</title><!--` + strings.Repeat("x", config.FeedSniffBytes) + `-->`
	for _, kind := range []string{"application/rss+xml", "application/xml"} {
		t.Run(kind, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get(httpx.HeaderAccept) != feedAccept {
					w.Header().Set(httpx.HeaderContentType, "text/html")
					_, _ = io.WriteString(w, "<html>Subscribe with a feed reader.</html>")
					return
				}
				w.Header().Set(httpx.HeaderContentType, kind)
				if kind == "application/xml" {
					_, _ = io.WriteString(w, prefix)
				}
				w.(http.Flusher).Flush()
				time.Sleep(3 * platformProbeTimeout)
				if kind == "application/rss+xml" {
					_, _ = io.WriteString(w, prefix)
				}
				_, _ = io.WriteString(w, `<item><title>Episode</title><enclosure url="https://media.example.test/episode.mp3"/></item></channel></rss>`)
			}))
			t.Cleanup(srv.Close)
			client := testClient()
			t.Cleanup(client.CloseIdleConnections)
			res, _, err := NewRegistry(&config.Config{}, client).Extract(t.Context(), srv.URL+"/feed", Options{})
			if err != nil || fileCount(res) != 1 {
				t.Fatalf("an identified feed inherited the short probe timeout: %+v, %v", res, err)
			}
		})
	}
}

func TestFeedNegotiatedBodyHonorsCancellation(t *testing.T) {
	var reads atomic.Int32
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(httpx.HeaderAccept) != feedAccept {
			w.Header().Set(httpx.HeaderContentType, "text/html")
			_, _ = io.WriteString(w, "<html>Subscribe with a feed reader.</html>")
			return
		}
		reads.Add(1)
		w.Header().Set(httpx.HeaderContentType, "application/rss+xml")
		_, _ = io.WriteString(w, `<rss><channel><!--`+strings.Repeat("x", config.FeedSniffBytes))
		w.(http.Flusher).Flush()
		cancel()
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)
	client := testClient()
	t.Cleanup(client.CloseIdleConnections)
	res, _, err := NewRegistry(&config.Config{}, client).Extract(ctx, srv.URL+"/feed", Options{})
	if !errors.Is(err, context.Canceled) || res != nil || reads.Load() != 1 {
		t.Fatalf("cancelled negotiation: result %+v, error %v, requests %d", res, err, reads.Load())
	}
}
