// SPDX-License-Identifier: MIT

package extractor

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

func TestLinksHarvestsKnownPlatformItems(t *testing.T) {
	for _, tc := range []struct {
		name       string
		known      bool
		maxSources int
		want       int
	}{
		{name: "known install", known: true, want: 2},
		{name: "source cap", known: true, maxSources: 1, want: 1},
		{name: "unrelated host"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			reads := map[string]int{}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				reads[r.URL.RequestURI()]++
				mu.Unlock()
				w.Header().Set(httpx.HeaderContentType, "text/html")
				switch r.URL.Path {
				case "/entry":
					http.Redirect(w, r, "/videos/collection/", http.StatusFound)
				case "/videos/collection/":
					fmt.Fprint(w, `<html><title>Selection</title>
<a href="/">Home</a><a href="/login/">Login</a><a href="/account/">Account</a>
<a href="/members/42/videos/">Member</a><a href="/search/example/">Search</a>
<a href="/categories/example/">Category</a><a href="/videos/">Videos</a>
<a href="#items">Self</a><a href="/entry#items">Original URL</a>
<a href="/videos/collection/">Final URL</a>
<a href="../first/?token=synthetic%2Fvalue+raw">First</a>
<a href="../first/?token=synthetic%2Fvalue+raw">Duplicate</a>
<a href="../second/">Second</a></html>`)
				case "/videos/first/", "/videos/second/":
					name := strings.Split(r.URL.Path, "/")[2]
					fmt.Fprintf(w, `<html><title>%[1]s</title><script>var flashvars = {
video_title: '%[1]s', video_url: 'http://%[2]s/media/%[1]s.mp4?signature=synthetic%%2Fvalue+raw'};</script></html>`, name, r.Host)
				default:
					t.Errorf("unexpected navigation or media request: %s", r.URL)
					http.NotFound(w, r)
				}
			}))
			defer srv.Close()
			client := testClient()
			defer client.CloseIdleConnections()
			cfg := &config.Config{MaxSources: tc.maxSources}
			if tc.known {
				cfg.ExtraHosts = map[string][]string{config.FamilyKVS: {mustParse(t, srv.URL).Hostname()}}
			}
			reg := NewRegistry(cfg, client)
			res, _, err := reg.Extract(t.Context(), "links:"+srv.URL+"/entry", Options{})
			if tc.want == 0 {
				if err == nil || !strings.Contains(err.Error(), "none of the") {
					t.Fatalf("unrelated host: result=%+v, err=%v", res, err)
				}
			} else {
				if err != nil || len(res.Files) != tc.want {
					t.Fatalf("result=%+v, err=%v; want %d files", res, err, tc.want)
				}
				for i, f := range res.Files {
					name := []string{"first", "second"}[i]
					want := srv.URL + "/media/" + name + ".mp4?signature=synthetic%2Fvalue+raw"
					referer := srv.URL + "/videos/" + name + "/"
					if i == 0 {
						referer += "?token=synthetic%2Fvalue+raw"
					}
					if f.URL != want || f.Dir != name || f.Headers[httpx.HeaderReferer] != referer {
						t.Errorf("file %d=%+v; want %s in folder %s with item Referer %s", i, f, want, name, referer)
					}
				}
				wantNote := ""
				if tc.maxSources == 1 {
					wantNote = "1 of 2 links"
				}
				if res.Title != "Selection" || res.Note != wantNote {
					t.Errorf("title=%q, note=%q; want Selection, %q", res.Title, res.Note, wantNote)
				}
			}
			mu.Lock()
			defer mu.Unlock()
			if reads["/entry"] != 1 || reads["/videos/collection/"] != 1 {
				t.Errorf("harvest or self-link was fetched again: %v", reads)
			}
			if (reads["/videos/first/?token=synthetic%2Fvalue+raw"] > 0) != (tc.want > 0) ||
				(reads["/videos/second/"] > 0) != (tc.want > 1) {
				t.Errorf("item requests disagree with admission: %v", reads)
			}
		})
	}
}

func TestLinksPlatformItemsKeepTheirFailures(t *testing.T) {
	for _, mixed := range []bool{false, true} {
		t.Run(fmt.Sprintf("mixed=%v", mixed), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set(httpx.HeaderContentType, "text/html")
				switch r.URL.Path {
				case "/selection":
					fmt.Fprint(w, `<a href="/videos/removed/">Removed</a>`)
					if mixed {
						fmt.Fprint(w, `<a href="/videos/available/">Available</a>`)
					}
				case "/videos/removed/":
					w.WriteHeader(http.StatusNotFound)
					fmt.Fprint(w, `<html>Removed<a href="/videos/unrelated/">Related clip</a></html>`)
				case "/videos/available/":
					fmt.Fprintf(w, `<script>var flashvars = {video_title: 'Available',
video_url: 'http://%s/media/available.mp4'};</script>`, r.Host)
				default:
					t.Errorf("unexpected related-page or media request: %s", r.URL)
					http.NotFound(w, r)
				}
			}))
			defer srv.Close()
			client := testClient()
			defer client.CloseIdleConnections()
			reg := NewRegistry(&config.Config{ExtraHosts: map[string][]string{
				config.FamilyKVS: {mustParse(t, srv.URL).Hostname()},
			}}, client)
			res, _, err := reg.Extract(t.Context(), "links:"+srv.URL+"/selection", Options{})
			if !mixed {
				if err == nil || !strings.Contains(err.Error(), "404") {
					t.Fatalf("removed item: result=%+v, err=%v; want its HTTP failure", res, err)
				}
				return
			}
			if err != nil || len(res.Files) != 1 || res.Files[0].URL != srv.URL+"/media/available.mp4" {
				t.Fatalf("result=%+v, err=%v; want only the available item", res, err)
			}
			if !strings.Contains(res.Note, "1 of 2 links resolved") {
				t.Errorf("note=%q; want the failed item counted", res.Note)
			}
		})
	}
}
