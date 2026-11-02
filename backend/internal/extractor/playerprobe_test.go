// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
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

func TestPlayerCanonicalProbeReusesValidatedPlayer(t *testing.T) {
	var reads atomic.Int32
	canonical := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reads.Add(1)
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<html><title>Invented clip - VidMoly</title><script>player.setup({sources:[{file:"https://cdn.example.test/invented.mp4"}]})</script></html>`))
	}))
	defer canonical.Close()
	alias := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/advert" {
			http.Redirect(w, r, "/advert", http.StatusFound)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<html><title>Loading</title></html>`))
	}))
	defer alias.Close()
	client := httpx.New("test", "en", 0, config.PlatformProbeTimeout)
	defer client.CloseIdleConnections()
	site := vidmolyPlayer
	site.page = func(u *url.URL) (string, error) { return canonical.URL + "/embed-" + vidmolyCode(u) + ".html", nil }
	p := NewPlayer(client, &site)
	reg := &Registry{client: client, extractors: []Extractor{p}, fallback: NewDirect(client)}
	res, ex, err := reg.Extract(context.Background(), alias.URL+"/embed-abcdefghijkl.html", Options{})
	if err != nil {
		t.Fatal(err)
	}
	if ex != p || len(res.Files) != 1 || res.Files[0].URL != "https://cdn.example.test/invented.mp4" || reads.Load() != 1 {
		t.Fatalf("extractor=%v result=%+v canonical requests=%d", ex, res, reads.Load())
	}
}

func TestPlayerCanonicalProbeRequiresShapeAndPlayer(t *testing.T) {
	var reads atomic.Int32
	canonical := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reads.Add(1)
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<html><title>VidMoly</title><p>Nothing here.</p></html>`))
	}))
	defer canonical.Close()
	client := httpx.New("test", "en", 0, config.PlatformProbeTimeout)
	defer client.CloseIdleConnections()
	site := vidmolyPlayer
	site.page = func(u *url.URL) (string, error) { return canonical.URL + "/embed-" + vidmolyCode(u) + ".html", nil }
	p := NewPlayer(client, &site)
	for _, path := range []string{"/help", "/embed-abcdefghijkl.html"} {
		page := htmlPage(t, "https://alias.example.test"+path, `<html>Loading</html>`)
		for range 2 {
			if p.Recognise(context.Background(), page) {
				t.Fatalf("claimed unmarked page %s", path)
			}
		}
	}
	if reads.Load() != 1 {
		t.Fatalf("made %d probes; want one for the matching shape, shared", reads.Load())
	}
	// Canonical probing stays opt-in for every other player row.
	site.probeCanonical = false
	if p.Recognise(context.Background(), htmlPage(t, "https://alias.example.test/embed-abcdefghijkl.html", strings.Repeat("x", 10))) || reads.Load() != 1 {
		t.Fatal("unrequested canonical probe")
	}
}

// A known canonical player chooses the reader's initial headers, while the
// response still determines its extractor. The same form on an unfamiliar
// host, or an unrelated page on the known host, remains ordinary navigation.
func TestPlayerProbeKeepsRecognitionAndHostBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name, path string
		known      bool
		reader     bool
	}{
		{"known canonical", "/v/42", true, true},
		{"unfamiliar canonical", "/v/42", false, false},
		{"known unrelated page", "/help", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var hits atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hits.Add(1)
				want := ""
				if tc.reader {
					want = "http://" + r.Host + tc.path
				}
				if r.Referer() != want {
					t.Errorf("Referer = %q, want %q", r.Referer(), want)
				}
				w.Header().Set(httpx.HeaderContentType, "application/rss+xml")
				fmt.Fprint(w, `<rss version="2.0"><channel><title>Example Feed</title></channel></rss>`)
			}))
			t.Cleanup(srv.Close)
			client := testClient()
			t.Cleanup(client.CloseIdleConnections)
			reg := NewRegistry(&config.Config{}, client)
			if tc.known {
				aliasClaims(reg, vidsPlayer.name, "127.0.0.1")
			}
			ex, page := reg.identify(t.Context(), mustParse(t, srv.URL+tc.path))
			if ex.Name() != "feed" || page.Err != nil || hits.Load() != 1 {
				t.Fatalf("extractor=%s err=%v reads=%d; want the feed after one read", ex.Name(), page.Err, hits.Load())
			}
		})
	}
}

// No-Vary responses still need distinct cache entries when acquisition
// explicitly selected a reader policy. Ordinary navigation's refusal retry
// also differs from an explicitly empty Referer. Both route-local and shared
// lookups must return the representation that their own request selected.
func TestPlayerProbeCacheSeparatesReaderReferers(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set(httpx.HeaderContentType, "text/html")
		fmt.Fprintf(w, "<html><title>%s</title></html>", r.Referer())
	}))
	t.Cleanup(srv.Close)
	client := testClient()
	t.Cleanup(client.CloseIdleConnections)
	u := mustParse(t, srv.URL+"/v/42")
	self, other, empty := u.String(), srv.URL+"/other", ""
	policies := []struct {
		name    string
		referer *string
		page    *Page
	}{
		{name: "navigation"},
		{name: "self", referer: &self},
		{name: "other", referer: &other},
		{name: "explicit empty", referer: &empty},
	}
	var cache pageCache
	for i := range policies {
		policy := &policies[i]
		policy.page = cache.getWithReferer(t.Context(), client, u, policy.referer)
		want := ""
		if policy.referer != nil {
			want = *policy.referer
		}
		if policy.page.Err != nil || policy.page.Body != "<html><title>"+want+"</title></html>" || hits.Load() != int32(i+1) {
			t.Fatalf("%s: body=%q err=%v reads=%d; want its own representation on read %d", policy.name, policy.page.Body, policy.page.Err, hits.Load(), i+1)
		}
	}
	for _, route := range []string{"", "another-route"} {
		ctx := httpx.WithRoute(t.Context(), route, client, nil)
		for _, policy := range policies {
			if page := cache.getWithReferer(ctx, client, u, policy.referer); page != policy.page {
				t.Errorf("%s on route %q did not reuse its representation", policy.name, route)
			}
		}
	}
	if hits.Load() != int32(len(policies)) {
		t.Errorf("reads=%d, want one per policy", hits.Load())
	}
}

// A reader's refusal remains its refusal. The generic origin retry must not
// substitute another representation for an explicitly selected policy.
func TestPlayerProbeRetainsReaderPolicyOnRefusal(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.Referer() != "http://"+r.Host+r.URL.Path {
			t.Errorf("Referer = %q, want the reader's page", r.Referer())
		}
		http.Error(w, "reader refused", http.StatusForbidden)
	}))
	t.Cleanup(srv.Close)
	client := testClient()
	t.Cleanup(client.CloseIdleConnections)
	u := mustParse(t, srv.URL+"/v/42")
	referer := u.String()
	page := probePageWithReferer(t.Context(), client, u, &referer)
	if page.Status != http.StatusForbidden || hits.Load() != 1 || page.referer != referer {
		t.Fatalf("status=%d reads=%d Referer=%q; want one refused reader request", page.Status, hits.Load(), page.referer)
	}
}
