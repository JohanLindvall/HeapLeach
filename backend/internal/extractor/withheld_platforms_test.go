// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

// A band's front page that refuses every client is read from its /music
// listing: recognised by that listing, asked once, and the discography read
// from the same answer.
func TestBandcampFrontPageRefusedToEveryClient(t *testing.T) {
	grid := `<html><head><script src="/client-bundle/1/trackpipe/global_head-0123.js"></script>` +
		`<meta property="og:site_name" content="A Band"></head><body>` +
		`<ol id="music-grid"><li><a href="/album/first">First</a></li></ol></body></html>`
	var music atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(httpx.HeaderContentType, "text/html")
		switch {
		case r.URL.Path == "/music" && r.UserAgent() == bandcampAgent:
			music.Add(1)
			_, _ = w.Write([]byte(grid))
		case r.URL.Path == "/album/first":
			_, _ = w.Write([]byte(bandcampPageFixture(bandcampAlbumData, "")))
		default:
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`<html><title>Forbidden</title></html>`))
		}
	}))
	defer srv.Close()
	client := httpx.New("Mozilla/5.0", "en-US", 0, 5*time.Second)
	reg := &Registry{client: client, fallback: NewDirect(client)}
	reg.extractors = []Extractor{NewBandcamp(client)}
	res, ex, err := reg.Extract(context.Background(), srv.URL+"/", Options{})
	if err != nil || ex.Name() != "bandcamp" {
		t.Fatalf("extractor %v, err %v; want the band's discography", ex, err)
	}
	if len(res.Files) != 3 || res.Title != "A Band" {
		t.Errorf("%d files, title %q", len(res.Files), res.Title)
	}
	if n := music.Load(); n != 1 {
		t.Errorf("/music asked %d times, want once", n)
	}
	// A refused root whose /music is no band's listing is not taken.
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer other.Close()
	if NewBandcamp(client).Recognise(context.Background(), fixturePage(t, other.URL+"/", http.StatusForbidden, "text/html", "")) {
		t.Error("a refused site root with no listing was taken")
	}
}

// A page whose front end fails while the API answers is read through the
// API, as a challenged one is; a page served without the site's marks is not.
func TestCivitaiTakesAFailedPageByItsAPI(t *testing.T) {
	srv, _ := jsonServer(t, map[string]string{
		"/api/v1/images": `{"items":[{"id":12345,"url":"https://media.example.test/a.jpeg"}],"metadata":{}}`,
	})
	c := NewCivitai(testClient())
	for _, status := range []int{http.StatusForbidden, http.StatusTooManyRequests, http.StatusServiceUnavailable} {
		if !c.Recognise(context.Background(), fixturePage(t, srv.URL+"/images/12345", status, "text/html", "")) {
			t.Errorf("a %d image page the API knows was not taken", status)
		}
	}
	if c.Recognise(context.Background(), htmlPage(t, srv.URL+"/images/12345", "<p>a blog</p>")) {
		t.Error("a served page of no such site was asked about")
	}
}
