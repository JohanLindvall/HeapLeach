// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
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
