// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

// markedSite is a site whose links have the form /v/<id> and whose pages
// carry a marker its extractor reads, as every recognised site's do.
type markedSite struct{ unshaped bool }

const siteMarker = `data-invented-player`

func (m markedSite) Name() string       { return "marked" }
func (m markedSite) Recognises() string { return "an invented test site" }
func (m markedSite) Shape(u *url.URL) bool {
	return !m.unshaped && strings.HasPrefix(u.Path, "/v/")
}
func (m markedSite) Recognise(_ context.Context, p *Page) bool {
	return p.OK() && strings.Contains(p.Body, siteMarker) && (m.unshaped || m.Shape(p.Final))
}
func (m markedSite) Extract(_ context.Context, u *url.URL, _ Options) (*Result, error) {
	return &Result{Title: "Invented Clip", Files: []File{{Name: "Invented Clip.mp4", URL: "https://cdn.example.test/media/clip.mp4", Size: -1}}}, nil
}

func markedRegistry(client *httpx.Client, site markedSite) *Registry {
	direct := NewDirect(client)
	reg := &Registry{client: client, fallback: direct, extractors: []Extractor{site}}
	direct.registry = reg
	return reg
}

// A submitted link of a supported form whose page would not load is that
// site's failure, reported with the answer it got, never the fallback's
// copy of the refusal queued as the file (R-001). A page that loaded
// without what the site's extractor reads, and holding nothing else, is
// refused too, by name.
func TestSubmittedClaimedLinkKeepsTheProbesFailure(t *testing.T) {
	for _, status := range []int{http.StatusServiceUnavailable, http.StatusForbidden, http.StatusNotFound, http.StatusGone} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set(httpx.HeaderContentType, "text/html")
				w.WriteHeader(status)
				fmt.Fprint(w, `<html><title>Unavailable</title></html>`)
			}))
			defer srv.Close()
			client := testClient()
			defer client.CloseIdleConnections()
			res, _, err := markedRegistry(client, markedSite{}).Extract(t.Context(), srv.URL+"/v/abc123", Options{})
			if res != nil || !httpx.HasStatus(err, status) {
				t.Fatalf("got %+v, %v; want the %d", res, err, status)
			}
		})
	}
	t.Run("removed page", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set(httpx.HeaderContentType, "text/html")
			fmt.Fprint(w, `<html><title>This video was removed</title><a href="/about">About</a></html>`)
		}))
		defer srv.Close()
		client := testClient()
		defer client.CloseIdleConnections()
		res, ex, err := markedRegistry(client, markedSite{}).Extract(t.Context(), srv.URL+"/v/abc123", Options{})
		if res != nil || err == nil || !strings.HasPrefix(err.Error(), "marked: ") || !strings.Contains(err.Error(), "removed") {
			t.Fatalf("got %+v via %v, %v; want the site's page refused", res, ex, err)
		}
	})
}

// A listing whose address has the form of the items it lists is harvested
// like any other page: the links of its own form are what it holds.
func TestDirectHarvestsAListingOfItsOwnForm(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v/a-category" {
			w.Header().Set(httpx.HeaderContentType, "text/html")
			fmt.Fprint(w, `<html><title>A category</title><a href="/v/first">First</a><a href="/v/second">Second</a></html>`)
			return
		}
		markedPage(w, r)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	client := testClient()
	defer client.CloseIdleConnections()
	res, ex, err := markedRegistry(client, markedSite{}).Extract(t.Context(), srv.URL+"/v/a-category", Options{})
	if err != nil || ex.Name() != "direct" || len(res.Files) != 2 {
		t.Fatalf("got %+v via %v, %v; want both listed items", res, ex, err)
	}
}

// A page nobody recognises that holds nothing is refused where it is
// submitted, rather than queued as a file the downloader can only reject
// as a web page after its retries. A link that names a file is still the
// file, with no fetch.
func TestDirectRefusesAnEmptyPage(t *testing.T) {
	var asked atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		asked.Add(1)
		w.Header().Set(httpx.HeaderContentType, "text/html")
		fmt.Fprint(w, `<html><title>An ordinary page</title><a href="/about">About</a></html>`)
	}))
	defer srv.Close()
	client := testClient()
	defer client.CloseIdleConnections()
	reg := markedRegistry(client, markedSite{})
	res, ex, err := reg.Extract(t.Context(), srv.URL+"/blog/a-post", Options{})
	if res != nil || ex != reg.fallback || err == nil || !strings.Contains(err.Error(), "is a web page, not a file") {
		t.Fatalf("got %+v via %v, %v", res, ex, err)
	}
	if asked.Load() != 2 {
		t.Errorf("page fetched %d times; want the probe and one origin-Referer retry", asked.Load())
	}
	asked.Store(0)
	res, _, err = reg.Extract(t.Context(), srv.URL+"/files/archive.zip", Options{})
	if err != nil || len(res.Files) != 1 || res.Files[0].URL != srv.URL+"/files/archive.zip" || asked.Load() != 0 {
		t.Fatalf("file link: %+v, %v after %d requests", res, err, asked.Load())
	}
}

// An article's own clip is found even when a supported link beside it
// could not be read, failed or challenged: that link's answer is its own,
// not the article's (R-010, R-011).
func TestDirectFallsBackToThePagesMediaWhenItsLinksFail(t *testing.T) {
	for name, side := range map[string]http.HandlerFunc{
		"unavailable": func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
		},
		"challenged": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set(httpx.HeaderCFMitigated, "challenge")
			w.Header().Set(httpx.HeaderContentType, "text/html")
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprint(w, `<html>Just a moment...</html>`)
		},
	} {
		t.Run(name, func(t *testing.T) {
			mux := http.NewServeMux()
			mux.HandleFunc("/v/", side)
			mux.HandleFunc("/news/a-story", func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set(httpx.HeaderContentType, "text/html")
				fmt.Fprint(w, `<html><head><title>A story</title></head><body>
<video src="/media/story-clip.mp4"></video>
<a href="/v/elsewhere1">Watch next</a></body></html>`)
			})
			srv := httptest.NewServer(mux)
			defer srv.Close()
			client := testClient()
			defer client.CloseIdleConnections()
			res, ex, err := markedRegistry(client, markedSite{}).Extract(t.Context(), srv.URL+"/news/a-story", Options{})
			if err != nil || len(res.Files) != 1 || !strings.HasSuffix(res.Files[0].URL, "/media/story-clip.mp4") {
				t.Fatalf("got %+v via %v, %v; want the story's clip", res, ex, err)
			}
		})
	}
}

// A link printed in a script's JSON string keeps no backslash from the
// escaped quote that closes it (R-011).
func TestLinksCandidatesDropAScriptsEscapedQuote(t *testing.T) {
	root, err := parseHTML(`<script>var page = "{\"next\":\"https://other.example.test/v/abc\"}";</script>`)
	if err != nil {
		t.Fatal(err)
	}
	got := linksCandidates(root, mustParse(t, "https://page.example.test/"))
	if len(got) != 1 || got[0] != "https://other.example.test/v/abc" {
		t.Fatalf("candidates %q", got)
	}
}

// Dead links among a page's supported links are counted as what could not
// be read, with the reason, rather than dropped as navigation (R-012).
func TestDeadLinksAreReportedNotDropped(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v/", func(w http.ResponseWriter, _ *http.Request) { http.NotFound(w, nil) })
	mux.HandleFunc("/thread", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set(httpx.HeaderContentType, "text/html")
		fmt.Fprint(w, `<html><title>A thread</title><a href="/v/gone1">one</a><a href="/v/gone2">two</a></html>`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	client := testClient()
	defer client.CloseIdleConnections()
	reg := markedRegistry(client, markedSite{})
	reg.extractors = append([]Extractor{NewLinks(client, reg)}, reg.extractors...)
	_, _, err := reg.Extract(t.Context(), "links:"+srv.URL+"/thread", Options{})
	if err == nil || !strings.Contains(err.Error(), "none of the 2 supported links") || !strings.Contains(err.Error(), "404") {
		t.Fatalf("got %v; want both dead links reported", err)
	}
}

// routedRegistry serves origin through two proxy routes and a direct
// connection, each answered by its own handler.
func routedContext(t *testing.T, client *httpx.Client, id string, handler http.HandlerFunc) context.Context {
	t.Helper()
	proxy := httptest.NewServer(handler)
	t.Cleanup(proxy.Close)
	routed, err := client.ThroughProxy(proxy.URL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(routed.CloseIdleConnections)
	return httpx.WithRoute(t.Context(), id, routed, nil)
}

func markedPage(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set(httpx.HeaderContentType, "text/html")
	fmt.Fprint(w, `<html><title>Invented Clip</title><div `+siteMarker+`></div></html>`)
}

// Recovery from a challenge does not stop at a route that refused the
// page: the fallback's copy of the refusal is no recovery, and the next
// route, which serves the page, is tried (R-003).
func TestChallengeRecoveryMovesPastARefusedRoute(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set(httpx.HeaderCFMitigated, "challenge")
		w.WriteHeader(http.StatusForbidden)
	}))
	defer origin.Close()
	client := testClient()
	defer client.CloseIdleConnections()
	refused := routedContext(t, client, "refused", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "forbidden", http.StatusForbidden)
	})
	working := routedContext(t, client, "working", markedPage)
	reg := markedRegistry(client, markedSite{})
	var tried []string
	ctx := WithChallengeRecovery(t.Context(), func(_ context.Context, read func(context.Context) (*Result, error), _ error) (*Result, error) {
		for _, route := range []context.Context{refused, working} {
			res, err := read(route)
			tried = append(tried, fmt.Sprintf("%s:%v", httpx.RouteID(route), err == nil))
			if err == nil {
				return res, nil
			}
		}
		return nil, errors.New("no route recovered")
	})
	res, ex, err := reg.Extract(ctx, origin.URL+"/v/abc123", Options{})
	if err != nil || ex.Name() != "marked" || len(res.Files) != 1 || res.Files[0].URL != "https://cdn.example.test/media/clip.mp4" {
		t.Fatalf("got %+v via %v, %v after %v", res, ex, err, tried)
	}
	if strings.Join(tried, " ") != "refused:false working:true" {
		t.Errorf("routes tried: %v", tried)
	}
}

// A refresh that is served something else than the page first read — a
// refusal, or a page its extractor no longer recognises — fails, rather
// than swapping the item for the fallback's reading of that page under a
// new name (R-004).
func TestRefreshKeepsTheExtractorItWasReadBy(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(markedPage))
	defer origin.Close()
	client := testClient()
	defer client.CloseIdleConnections()
	for name, tc := range map[string]struct {
		site  markedSite
		route http.HandlerFunc
	}{
		"refused": {markedSite{}, func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
		}},
		"other page": {markedSite{unshaped: true}, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set(httpx.HeaderContentType, "text/html")
			fmt.Fprint(w, `<html><title>Sign in</title><video src="/media/advert.mp4"></video></html>`)
		}},
	} {
		t.Run(name, func(t *testing.T) {
			reg := markedRegistry(client, tc.site)
			res, _, err := reg.Extract(t.Context(), origin.URL+"/v/abc123", Options{})
			if err != nil || len(res.Files) != 1 {
				t.Fatalf("first read: %+v, %v", res, err)
			}
			target, err := res.Files[0].Refresh(routedContext(t, client, "elsewhere", tc.route))
			if err == nil {
				t.Fatalf("refresh became %+v", target)
			}
		})
	}
}

// A page too large to read whole is sniffed by the prefix the probe kept,
// and not fetched again only to be refused for its size (R-070).
func TestDirectSniffsAnOversizedPagesPrefix(t *testing.T) {
	var asked atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		asked.Add(1)
		w.Header().Set(httpx.HeaderContentType, "text/html")
		fmt.Fprint(w, `<html><head><title>A long page</title></head><body><video src="/media/long-clip.mp4"></video>`)
		fmt.Fprint(w, strings.Repeat("<p>filler</p>", config.MaxResponseBytes/12))
	}))
	defer srv.Close()
	client := testClient()
	defer client.CloseIdleConnections()
	res, _, err := markedRegistry(client, markedSite{}).Extract(t.Context(), srv.URL+"/watch/a-long-page", Options{})
	if err != nil || len(res.Files) != 1 || !strings.HasSuffix(res.Files[0].URL, "/media/long-clip.mp4") {
		t.Fatalf("got %+v, %v", res, err)
	}
	if asked.Load() != 1 {
		t.Errorf("page fetched %d times", asked.Load())
	}
}

// A job is labelled by its link's form only when one form claims it; links
// several forms share are labelled by their host until read (R-009).
func TestLabelIsTheHostWhenFormsOverlap(t *testing.T) {
	reg := NewRegistry(&config.Config{}, nil)
	for raw, want := range map[string]string{
		"https://share.example.test/folder/BBBBBBBB#key": "mega",
		"https://player.example.test/e/abcdef123456":     "player.example.test",
		"https://nobody.example.test/some/file.bin":      "nobody.example.test",
	} {
		u := mustParse(t, raw)
		var claims []string
		for _, e := range reg.extractors {
			if e.Shape(u) {
				claims = append(claims, e.Name())
			}
		}
		if got := reg.Label(u); got != want {
			t.Errorf("Label(%s) = %q (forms %v), want %q", raw, got, claims, want)
		}
	}
}
