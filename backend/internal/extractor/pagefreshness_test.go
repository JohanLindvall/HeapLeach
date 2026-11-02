// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

func TestPageResponseFreshness(t *testing.T) {
	started := time.Date(2026, time.January, 1, 12, 0, 0, 0, time.UTC)
	received := started.Add(2 * time.Second)
	date := func(offset time.Duration) string { return started.Add(offset).Format(http.TimeFormat) }
	for _, tc := range []struct {
		name    string
		headers http.Header
		want    time.Time
		reuse   bool
	}{
		{"unstated", nil, time.Time{}, true},
		{"positive max-age", http.Header{httpx.HeaderCacheControl: {"max-age=30"}}, started.Add(30 * time.Second), true},
		{"quoted mixed case", http.Header{httpx.HeaderCacheControl: {`private, MaX-AgE = "30"`}}, started.Add(30 * time.Second), true},
		{"zero", http.Header{httpx.HeaderCacheControl: {"max-age=0"}}, received, false},
		{"response delay", http.Header{httpx.HeaderCacheControl: {"max-age=1"}}, started.Add(time.Second), false},
		{"age", http.Header{httpx.HeaderCacheControl: {"max-age=30"}, httpx.HeaderAge: {"10"}}, started.Add(20 * time.Second), true},
		{"age boundary", http.Header{httpx.HeaderCacheControl: {"max-age=30"}, httpx.HeaderAge: {"30"}}, received, false},
		{"apparent age", http.Header{httpx.HeaderCacheControl: {"max-age=30"}, httpx.HeaderDate: {date(-20 * time.Second)}}, started.Add(10 * time.Second), true},
		{"old date", http.Header{httpx.HeaderCacheControl: {"max-age=30"}, httpx.HeaderDate: {date(-30 * time.Second)}}, started, false},
		{"future date", http.Header{httpx.HeaderCacheControl: {"max-age=30"}, httpx.HeaderDate: {date(time.Hour)}}, started.Add(30 * time.Second), true},
		{"age wins date", http.Header{httpx.HeaderCacheControl: {"max-age=30"}, httpx.HeaderDate: {date(-5 * time.Second)}, httpx.HeaderAge: {"20"}}, started.Add(10 * time.Second), true},
		{"expires", http.Header{httpx.HeaderDate: {date(0)}, httpx.HeaderExpires: {date(30 * time.Second)}}, started.Add(30 * time.Second), true},
		{"expires aged", http.Header{httpx.HeaderDate: {date(0)}, httpx.HeaderExpires: {date(30 * time.Second)}, httpx.HeaderAge: {"10"}}, started.Add(20 * time.Second), true},
		{"expires without date", http.Header{httpx.HeaderExpires: {date(30 * time.Second)}}, started.Add(28 * time.Second), true},
		{"expires old", http.Header{httpx.HeaderDate: {date(-time.Minute)}, httpx.HeaderExpires: {date(-time.Second)}}, started.Add(-time.Second), false},
		{"max-age wins expires", http.Header{httpx.HeaderCacheControl: {"max-age=30"}, httpx.HeaderExpires: {"0"}}, started.Add(30 * time.Second), true},
		{"no-store wins", http.Header{httpx.HeaderCacheControl: {"max-age=30", "no-store"}}, time.Time{}, false},
		{"negative", http.Header{httpx.HeaderCacheControl: {"max-age=-1"}}, time.Time{}, false},
		{"signed", http.Header{httpx.HeaderCacheControl: {"max-age=+30"}}, time.Time{}, false},
		{"fractional", http.Header{httpx.HeaderCacheControl: {"max-age=0.5"}}, time.Time{}, false},
		{"missing value", http.Header{httpx.HeaderCacheControl: {"max-age"}}, time.Time{}, false},
		{"unclosed quote", http.Header{httpx.HeaderCacheControl: {`max-age="30`}}, time.Time{}, false},
		{"conflicting lifetimes", http.Header{httpx.HeaderCacheControl: {"max-age=60", "max-age=30"}}, time.Time{}, false},
		{"duration overflow", http.Header{httpx.HeaderCacheControl: {"max-age=18446744073709551615"}}, started.Add(time.Duration(1<<63 - 1)), true},
		{"integer overflow", http.Header{httpx.HeaderCacheControl: {"max-age=18446744073709551616"}}, time.Time{}, false},
		{"age overflow", http.Header{httpx.HeaderCacheControl: {"max-age=30"}, httpx.HeaderAge: {"18446744073709551615"}}, received, false},
		{"invalid age", http.Header{httpx.HeaderCacheControl: {"max-age=30"}, httpx.HeaderAge: {"-1"}}, time.Time{}, false},
		{"duplicate age", http.Header{httpx.HeaderCacheControl: {"max-age=30"}, httpx.HeaderAge: {"1", "2"}}, time.Time{}, false},
		{"invalid date", http.Header{httpx.HeaderCacheControl: {"max-age=30"}, httpx.HeaderDate: {"yesterday"}}, time.Time{}, false},
		{"duplicate date", http.Header{httpx.HeaderCacheControl: {"max-age=30"}, httpx.HeaderDate: {date(0), date(0)}}, time.Time{}, false},
		{"invalid expires", http.Header{httpx.HeaderExpires: {"0"}}, time.Time{}, false},
		{"duplicate expires", http.Header{httpx.HeaderExpires: {date(time.Minute), date(2 * time.Minute)}}, time.Time{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, reuse := pageFreshUntil(tc.headers, started, received)
			if !got.Equal(tc.want) || reuse != tc.reuse {
				t.Fatalf("deadline=%s reusable=%v, want %s reusable=%v", got, reuse, tc.want, tc.reuse)
			}
		})
	}
}

// A short lifetime on any response in the chain caps the cached Page,
// including a HEAD that led to a GET. Expiry does not invalidate a reader
// that is already consuming this acquisition's body and memoized answers.
func TestPageCacheUsesShortestFreshness(t *testing.T) {
	for _, kind := range []string{"redirect", "final", "HEAD before GET", "local TTL"} {
		t.Run(kind, func(t *testing.T) {
			var hits atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				short := kind == "redirect" && r.URL.Path == "/start" || kind == "final" && r.URL.Path == "/final" ||
					kind == "HEAD before GET" && r.Method == http.MethodHead
				if short {
					w.Header().Set(httpx.HeaderCacheControl, "max-age=20")
				} else {
					w.Header().Set(httpx.HeaderCacheControl, "max-age=600")
				}
				if r.URL.Path == "/start" {
					http.Redirect(w, r, "/final", http.StatusFound)
					return
				}
				w.Header().Set(httpx.HeaderContentType, "text/html")
				fmt.Fprintf(w, "<html><title>Visit %d</title></html>", hits.Add(1))
			}))
			t.Cleanup(srv.Close)
			client := testClient()
			t.Cleanup(client.CloseIdleConnections)
			path := "/start"
			if kind == "HEAD before GET" {
				path = "/page.mp4"
			}
			u := mustParse(t, srv.URL+path)
			var cache pageCache
			before := time.Now()
			first := cache.get(t.Context(), client, u)
			if first.Err != nil || first.Status != http.StatusOK {
				t.Fatalf("first acquisition: status=%d err=%v", first.Status, first.Err)
			}
			if cache.get(t.Context(), client, u) != first {
				t.Fatal("fresh response was not reused")
			}
			otherRoute := httpx.WithRoute(t.Context(), "other", client, nil)
			if cache.get(otherRoute, client, u) != first {
				t.Fatal("fresh response was not shared with another route")
			}
			cache.mu.Lock()
			e := cache.order.Front().Value.(*pageCacheEntry)
			remaining := e.expires().Sub(before)
			want := 20 * time.Second
			if kind == "local TTL" {
				want = config.PageCacheTTL
			}
			if remaining <= want-2*time.Second || remaining > want+time.Second {
				t.Errorf("remaining freshness=%s, want about %s", remaining, want)
			}
			if !cache.sweepAt.Equal(e.expires()) {
				t.Error("expiration sweep does not follow the shortest lifetime")
			}
			// Advance only this entry's clock, under the cache lock, rather
			// than sleeping through an HTTP freshness interval.
			if kind == "local TTL" {
				e.fetched = time.Now().Add(-config.PageCacheTTL)
			} else {
				first.reuseUntil = time.Now().Add(-time.Second)
			}
			cache.mu.Unlock()
			reads := hits.Load()
			body, _, err := fetchDocument(withPage(t.Context(), first), client, u, nil)
			if err != nil || body != first.Body || hits.Load() != reads {
				t.Fatalf("originating reader refetched expired probe: err=%v reads=%d", err, hits.Load())
			}
			body, _, err = fetchDocument(withPage(otherRoute, first), client, u, nil)
			if err != nil || body != first.Body || hits.Load() != reads {
				t.Fatalf("reader that acquired the page on another route refetched after expiry: err=%v reads=%d", err, hits.Load())
			}
			memo(first, "answer", func() string { return "old" })
			second := cache.get(t.Context(), client, u)
			if second == first || hits.Load() <= reads || second.Err != nil {
				t.Fatalf("expired response reused: same=%v reads=%d err=%v", second == first, hits.Load(), second.Err)
			}
			if answer := memo(second, "answer", func() string { return "new" }); answer != "new" {
				t.Fatalf("expired memoized answer reused: %q", answer)
			}
		})
	}
}

func TestPageCacheResponsePolicySeparatesWaitingAcquisitions(t *testing.T) {
	for _, policy := range []struct{ name, header, value string }{
		{"expired", httpx.HeaderCacheControl, "max-age=0"},
		{"no-store", httpx.HeaderCacheControl, "no-store"},
		{"no-cache", httpx.HeaderCacheControl, "no-cache"},
		{"wildcard", httpx.HeaderVary, "*"},
		{"agent", httpx.HeaderVary, httpx.HeaderUserAgent},
	} {
		t.Run(policy.name, func(t *testing.T) {
			var hits atomic.Int32
			entered, hold := make(chan struct{}), make(chan struct{})
			release := sync.OnceFunc(func() { close(hold) })
			defer release()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				visit := hits.Add(1)
				w.Header().Set(httpx.HeaderContentType, "text/html")
				w.Header().Set(policy.header, policy.value)
				if visit == 1 {
					close(entered)
					select {
					case <-hold:
					case <-r.Context().Done():
						return
					}
				}
				fmt.Fprintf(w, "<html><title>Visit %d</title></html>", visit)
			}))
			t.Cleanup(srv.Close)
			client := testClient()
			t.Cleanup(client.CloseIdleConnections)
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			t.Cleanup(cancel)
			u := mustParse(t, srv.URL+"/page")
			var cache pageCache
			owner, waiter := make(chan *Page, 1), make(chan *Page, 1)
			go func() { owner <- cache.get(ctx, client, u) }()
			select {
			case <-entered:
			case <-ctx.Done():
				t.Fatal("probe did not reach the server")
			}
			waiting := &pageCookieWaitContext{Context: ctx, waiting: make(chan struct{})}
			go func() { waiter <- cache.get(waiting, client, u) }()
			select {
			case <-waiting.waiting:
			case <-ctx.Done():
				t.Fatal("second acquisition did not wait for the probe")
			}
			release()
			first, second := <-owner, <-waiter
			if first.Err != nil || second.Err != nil || first.Title() != "Visit 1" || second.Title() != "Visit 2" || hits.Load() != 2 {
				t.Fatalf("owner=%q/%v waiter=%q/%v requests=%d; want separate answers", first.Title(), first.Err, second.Title(), second.Err, hits.Load())
			}
		})
	}
}
