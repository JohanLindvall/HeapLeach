package extractor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

// An album page as the site renders it: each item appears twice — once for
// the inline player, once for the lightbox — images lazy-load through
// data-src with a thumbnail in src, and the page's own furniture shares the
// markup. The parser has to fold the repeats and keep only the media.
const eromeAlbumPage = `<html><head><title>An Album - EroMe</title></head><body>
  <h1 class="album-title-page">An Album</h1>
  <video><source src="https://media.example.test/v/clip1.mp4"></video>
  <div><img class="img-back" data-src="https://media.example.test/i/pic1.jpg"
            src="https://media.example.test/thumbs/thumb_pic1.jpg"></div>
  <video><source src="https://media.example.test/v/clip1.mp4"></video>
  <img src="https://media.example.test/logo.svg">
  <img class="img-front" src="https://media.example.test/i/pic2.png">
</body></html>`

func TestEromeAlbum(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/a/AAAA" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(eromeAlbumPage))
	}))
	t.Cleanup(srv.Close)

	e := NewErome(httpx.New("test-agent", "en-US", 0, 5*time.Second))
	u, _ := ParseURL(srv.URL + "/a/AAAA")
	res, err := e.Extract(context.Background(), u, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Title != "An Album" {
		t.Errorf("title = %q, want the page's own heading", res.Title)
	}
	var names []string
	for _, f := range res.Files {
		names = append(names, f.Name)
		if f.Headers[httpx.HeaderReferer] == "" {
			t.Errorf("%s carries no referer; the CDN checks one", f.Name)
		}
	}
	want := "clip1.mp4 pic1.jpg pic2.png"
	if got := strings.Join(names, " "); got != want {
		t.Errorf("files = %q, want %q (repeats folded, logo and thumbnail dropped)", got, want)
	}
}

func TestIsEromeMedia(t *testing.T) {
	cases := map[string]bool{
		"https://media.example.test/v/clip.mp4":     true,
		"https://media.example.test/i/pic.webp":     true,
		"https://media.example.test/thumb_x.jpg":    false, // a thumbnail
		"https://media.example.test/player.js":      false, // not media at all
		"https://media.example.test/no-extension":   false,
		"relative/path.mp4":                         false, // no host to fetch from
		"https://media.example.test/thumbs/....mp4": true,  // thumb in the path is fine; only the name decides
	}
	for raw, want := range cases {
		if got := isEromeMedia(raw); got != want {
			t.Errorf("isEromeMedia(%q) = %v, want %v", raw, got, want)
		}
	}
}
