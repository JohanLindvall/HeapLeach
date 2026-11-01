// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

const invitedMediaPlaylist = "#EXTM3U\n#EXT-X-TARGETDURATION:4\n#EXTINF:4,\nseg0.ts\n#EXTINF:4,\nseg1.ts\n#EXT-X-ENDLIST\n"

// hotlinkProtected answers 403 with an error page to a request whose
// Referer is not the origin's own, as hotlink protection does, and serves
// to the rest.
func hotlinkProtected(serve http.HandlerFunc) (*httptest.Server, *atomic.Int32) {
	var refused atomic.Int32
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(httpx.HeaderReferer) != srv.URL+"/" {
			refused.Add(1)
			w.Header().Set(httpx.HeaderContentType, "text/html")
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprint(w, "<html><title>Hotlinking is not allowed</title></html>")
			return
		}
		serve(w, r)
	}))
	return srv, &refused
}

// A probe refused with 401 or 403 asks once more with the origin's root as
// its Referer, and that answer is the page: a hotlink-protected file link
// no form claims is queued as the file, and an extensionless manifest on
// such an origin is recognised by what it serves (E01-R1, E05-R3).
func TestProbeAsksARefusalAgainWithTheOriginsReferer(t *testing.T) {
	t.Run("page", func(t *testing.T) {
		srv, refused := hotlinkProtected(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set(httpx.HeaderContentType, "text/html")
			fmt.Fprint(w, "<html><title>Served</title></html>")
		})
		defer srv.Close()
		u, _ := url.Parse(srv.URL + "/page")
		p := probePage(t.Context(), testClient(), u)
		if p.Status != http.StatusOK || p.Title() != "Served" || refused.Load() != 1 {
			t.Fatalf("status=%d title=%q refusals=%d err=%v", p.Status, p.Title(), refused.Load(), p.Err)
		}
	})
	t.Run("file", func(t *testing.T) {
		srv, _ := hotlinkProtected(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set(httpx.HeaderContentType, "application/zip")
			fmt.Fprint(w, "PK")
		})
		defer srv.Close()
		client := testClient()
		defer client.CloseIdleConnections()
		res, ex, err := markedRegistry(client, markedSite{}).Extract(t.Context(), srv.URL+"/get/abc123", Options{})
		if err != nil || ex.Name() != "direct" || len(res.Files) != 1 || res.Files[0].URL != srv.URL+"/get/abc123" {
			t.Fatalf("got %+v via %v, %v; want the file queued", res, ex, err)
		}
	})
	t.Run("manifest", func(t *testing.T) {
		srv, _ := hotlinkProtected(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set(httpx.HeaderContentType, "application/vnd.apple.mpegurl")
			fmt.Fprint(w, invitedMediaPlaylist)
		})
		defer srv.Close()
		client := testClient()
		defer client.CloseIdleConnections()
		direct := NewDirect(client)
		reg := &Registry{client: client, fallback: direct, extractors: []Extractor{NewHLSDirect(client)}}
		direct.registry = reg
		res, ex, err := reg.Extract(t.Context(), srv.URL+"/stream/abc123", Options{})
		if err != nil || ex.Name() != "hls" || len(res.Files) != 1 || len(res.Files[0].Segments) != 2 {
			t.Fatalf("got %+v via %v, %v; want the manifest's segments", res, ex, err)
		}
	})
	t.Run("still refused", func(t *testing.T) {
		var asked atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			asked.Add(1)
			w.Header().Set(httpx.HeaderContentType, "text/html")
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprint(w, "<html><title>Members only</title></html>")
		}))
		defer srv.Close()
		client := testClient()
		defer client.CloseIdleConnections()
		_, _, err := markedRegistry(client, markedSite{}).Extract(t.Context(), srv.URL+"/get/abc123", Options{})
		if !httpx.HasStatus(err, http.StatusForbidden) || asked.Load() != 2 {
			t.Fatalf("got %v after %d requests; want the 403 after two", err, asked.Load())
		}
	})
}

// A probe reads a document whole, within the response bound, but an answer
// that names no type, or a text type that is no document, only as far as
// recognition looks: it may be a large file served untyped. That prefix is
// kept for sniffing and is never the document (E01-R6, X03-R4).
func TestProbeSniffsOnlyAPrefixOfAnUndeclaredAnswer(t *testing.T) {
	const size = 1 << 20
	for _, tc := range []struct {
		kind  string
		whole bool
	}{
		{"", false},
		{"text/plain", false},
		{"text/csv", false},
		{"application/json", true},
		{"text/html", true},
		{"application/xml", true},
	} {
		t.Run(cmpOr(tc.kind, "untyped"), func(t *testing.T) {
			var sent atomic.Int64
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if tc.kind == "" {
					w.Header()[httpx.HeaderContentType] = nil
				} else {
					w.Header().Set(httpx.HeaderContentType, tc.kind)
				}
				chunk := []byte(strings.Repeat("a", 4096))
				for written := 0; written < size; written += len(chunk) {
					n, err := w.Write(chunk)
					sent.Add(int64(n))
					if err != nil {
						return
					}
				}
			}))
			defer srv.Close()
			client := testClient()
			defer client.CloseIdleConnections()
			u, _ := url.Parse(srv.URL + "/dl/abc123")
			var cache pageCache
			p := cache.get(context.Background(), client, u)
			if p.Err != nil || p.Status != http.StatusOK {
				t.Fatalf("status=%d err=%v", p.Status, p.Err)
			}
			_, document := p.Document()
			want := size
			if !tc.whole {
				want = config.ProbeSniffBytes
			}
			if len(p.Body) != want || document != tc.whole || p.servesAnyRoute() != tc.whole {
				t.Fatalf("read %d bytes, document=%v shared=%v; want %d, %v", len(p.Body), document, p.servesAnyRoute(), want, tc.whole)
			}
			if _, _, reused, err := acquire(withPage(context.Background(), p), client, u, nil); err != nil || (reused != nil) != tc.whole {
				t.Errorf("reader reused the probe: %v (%v), want %v", reused != nil, err, tc.whole)
			}
		})
	}
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// The probe asks a transient failure once more, after a short wait bounded
// whatever a rate limit asks, rather than spending the client's whole
// budget before an extractor that has its own (X03-R5). A definite answer
// is asked once.
func TestProbeRetriesAFailureOnceAndBriefly(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		asks   int32
	}{
		{"server error", http.StatusBadGateway, 2},
		{"rate limit", http.StatusTooManyRequests, 2},
		{"not found", http.StatusNotFound, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var asked atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				asked.Add(1)
				w.Header().Set(httpx.HeaderRetryAfter, "60")
				w.Header().Set(httpx.HeaderContentType, "text/html")
				w.WriteHeader(tc.status)
				fmt.Fprint(w, "<html><title>No</title></html>")
			}))
			defer srv.Close()
			// A client with the full budget, as the registry's has.
			client := httpx.New("test-agent", "en-US", config.DefaultMaxRetries, 5*time.Second)
			defer client.CloseIdleConnections()
			u, _ := url.Parse(srv.URL + "/page")
			start := time.Now()
			probePage(t.Context(), client, u)
			if asked.Load() != tc.asks {
				t.Errorf("asked %d times, want %d", asked.Load(), tc.asks)
			}
			if took := time.Since(start); took > config.PlatformProbeRetryWait+time.Second {
				t.Errorf("the probe took %v", took)
			}
		})
	}
}

// A submitted link no form claims, whose probe failed in passing — a server
// error, a rate limit, no answer at all — is queued as the file for the
// transfer's retries, as before the probe existed, and without fetching
// the page again. One refused with an error page still fails with that
// refusal, and so does a followed link or one a form claims (E01-R2).
func TestSubmittedUnclaimedLinkOutlastsAFailedProbe(t *testing.T) {
	serving := func(status int, asked *atomic.Int32) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			asked.Add(1)
			w.Header().Set(httpx.HeaderContentType, "text/html")
			w.WriteHeader(status)
			fmt.Fprint(w, "<html><title>Unavailable</title></html>")
		}))
	}
	for _, status := range []int{http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusTooManyRequests, http.StatusNotImplemented} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var asked atomic.Int32
			srv := serving(status, &asked)
			defer srv.Close()
			client := testClient()
			defer client.CloseIdleConnections()
			reg := markedRegistry(client, markedSite{})
			link := srv.URL + "/dl/abc123"
			res, ex, err := reg.Extract(t.Context(), link, Options{})
			if err != nil || ex.Name() != "direct" || len(res.Files) != 1 || res.Files[0].URL != link {
				t.Fatalf("got %+v via %v, %v; want the file queued", res, ex, err)
			}
			if asked.Load() > 2 {
				t.Errorf("asked %d times before queuing", asked.Load())
			}
			if status == http.StatusTooManyRequests {
				return // each further probe would wait out the limit
			}
			if _, _, err := reg.follow(t.Context(), link, Options{}); err == nil {
				t.Error("a followed link took the file")
			}
			if _, _, err := reg.Extract(t.Context(), srv.URL+"/v/abc123", Options{}); err == nil {
				t.Error("a claimed link took the file")
			}
		})
	}
	t.Run("no answer", func(t *testing.T) {
		srv := httptest.NewServer(http.NotFoundHandler())
		link := srv.URL + "/dl/abc123"
		srv.Close()
		res, _, err := markedRegistry(testClient(), markedSite{}).Extract(t.Context(), link, Options{})
		if err != nil || len(res.Files) != 1 || res.Files[0].URL != link {
			t.Fatalf("got %+v, %v; want the file queued", res, err)
		}
	})
	t.Run("timeout", func(t *testing.T) {
		release := make(chan struct{})
		srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-release }))
		defer srv.Close()
		defer close(release)
		client := httpx.New("test-agent", "en-US", 0, 100*time.Millisecond)
		defer client.CloseIdleConnections()
		link := srv.URL + "/dl/abc123"
		res, _, err := markedRegistry(client, markedSite{}).Extract(t.Context(), link, Options{})
		if err != nil || len(res.Files) != 1 || res.Files[0].URL != link {
			t.Fatalf("got %+v, %v; want the file queued", res, err)
		}
	})
	for _, status := range []int{http.StatusNotFound, http.StatusGone, http.StatusForbidden} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var asked atomic.Int32
			srv := serving(status, &asked)
			defer srv.Close()
			client := testClient()
			defer client.CloseIdleConnections()
			res, _, err := markedRegistry(client, markedSite{}).Extract(t.Context(), srv.URL+"/dl/abc123", Options{})
			if res != nil || !httpx.HasStatus(err, status) {
				t.Fatalf("got %+v, %v; want the %d", res, err, status)
			}
		})
	}
}

// A pasted manifest is asked for once: the probe GETs it, and the reader
// takes the probe's copy rather than spend a single-use link a second time
// (E05-R2, X03-R3).
func TestPastedManifestIsFetchedOnce(t *testing.T) {
	var gets, heads atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			heads.Add(1)
		} else {
			gets.Add(1)
		}
		if gets.Load() > 1 {
			// Signed for one use.
			w.WriteHeader(http.StatusGone)
			return
		}
		w.Header().Set(httpx.HeaderContentType, "application/vnd.apple.mpegurl")
		fmt.Fprint(w, invitedMediaPlaylist)
	}))
	defer srv.Close()
	client := testClient()
	defer client.CloseIdleConnections()
	direct := NewDirect(client)
	reg := &Registry{client: client, fallback: direct, extractors: []Extractor{NewHLSDirect(client)}}
	direct.registry = reg
	res, ex, err := reg.Extract(t.Context(), srv.URL+"/hls/token/index.m3u8", Options{})
	if err != nil || ex.Name() != "hls" || len(res.Files) != 1 || len(res.Files[0].Segments) != 2 {
		t.Fatalf("got %+v via %v, %v", res, ex, err)
	}
	if gets.Load() != 1 || heads.Load() != 0 {
		t.Errorf("asked GET %d, HEAD %d times; want one GET", gets.Load(), heads.Load())
	}
}

// The media-page sniff reads a page past the response bound by its prefix,
// where its player sits, rather than refusing it (E03-R4, H01-R2).
func TestMediaPageFetchSniffsAnOversizedPrefix(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set(httpx.HeaderContentType, "text/html")
		fmt.Fprint(w, `<html><head><title>A long page</title></head><body><video src="/media/long-clip.mp4"></video>`)
		_, _ = io.WriteString(w, strings.Repeat("<p>filler</p>", config.MaxResponseBytes/12))
	}))
	defer srv.Close()
	client := testClient()
	defer client.CloseIdleConnections()
	u, _ := url.Parse(srv.URL + "/watch/a-long-page")
	doc, final, ok := mediaPageFetchURL(t.Context(), client, u)
	if !ok || len(doc) != config.MaxResponseBytes || final.Path != "/watch/a-long-page" ||
		!strings.Contains(doc, "/media/long-clip.mp4") {
		t.Fatalf("ok=%v len=%d final=%v", ok, len(doc), final)
	}
}
