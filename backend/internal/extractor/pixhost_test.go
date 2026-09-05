package extractor

import (
	"strings"
	"testing"
)

// Synthetic ids and filenames throughout: what is under test is the mapping
// between two URL shapes, which does not care what is behind them.
const pixhostGalleryPage = `<html><head><title>A Synthetic Gallery - PiXhost Gallery</title></head><body>
<h1>Adult content</h1>
<h1>A Synthetic Gallery</h1>
<div class="images">
  <a href="https://pixhost.to/show/9999/1000001_first.jpg">
    <img class="js-user-action" src="https://t2.pixhost.to/thumbs/9999/1000001_first.jpg" alt="first.jpg">
  </a>
  <a href="https://pixhost.to/show/9999/1000002_second.jpg">
    <img class="js-user-action" src="https://t2.pixhost.to/thumbs/9999/1000002_second.jpg" alt="second.jpg">
  </a>
</div>
<img src="https://pixhost.to/static/logo.png" alt="site logo">
</body></html>`

func TestPixhostGallery(t *testing.T) {
	u, err := ParseURL("https://pixhost.to/gallery/synth1")
	if err != nil {
		t.Fatal(err)
	}
	res, err := pixhostGallery(mustParseDoc(t, pixhostGalleryPage), u)
	if err != nil {
		t.Fatalf("pixhostGallery: %v", err)
	}

	// The first heading on the page is a content warning, not the gallery.
	if res.Title != "A Synthetic Gallery" {
		t.Errorf("title = %q, want the gallery's own name", res.Title)
	}
	if len(res.Files) != 2 {
		t.Fatalf("found %d images, want 2 (the site's own logo is not one)", len(res.Files))
	}
	want := []string{
		"https://img2.pixhost.to/images/9999/1000001_first.jpg",
		"https://img2.pixhost.to/images/9999/1000002_second.jpg",
	}
	for i, file := range res.Files {
		if file.URL != want[i] {
			t.Errorf("image %d = %q, want %q", i, file.URL, want[i])
		}
	}
	// The id prefix is kept: two images in one gallery can share an
	// original name, and only the id tells them apart.
	if res.Files[0].Name != "1000001_first.jpg" {
		t.Errorf("name = %q, want the id-prefixed filename", res.Files[0].Name)
	}
}

func TestPixhostGalleryReportsAnEmptyPage(t *testing.T) {
	u, err := ParseURL("https://pixhost.to/gallery/gone")
	if err != nil {
		t.Fatal(err)
	}
	_, err = pixhostGallery(mustParseDoc(t, `<html><body><p>Gallery not found</p></body></html>`), u)
	if err == nil {
		t.Fatal("a gallery with no images was accepted")
	}
	if !strings.Contains(err.Error(), "pixhost") {
		t.Errorf("error %q does not name the host", err)
	}
}

func TestPixhostImage(t *testing.T) {
	const page = `<html><head><title>first.jpg - PiXhost</title></head><body>
<img id="image" class="image-img" src="https://img2.pixhost.to/images/9999/1000001_first.jpg" alt="first.jpg">
</body></html>`

	u, err := ParseURL("https://pixhost.to/show/9999/1000001_first.jpg")
	if err != nil {
		t.Fatal(err)
	}
	res, err := pixhostImage(mustParseDoc(t, page), u)
	if err != nil {
		t.Fatalf("pixhostImage: %v", err)
	}
	if len(res.Files) != 1 {
		t.Fatalf("found %d files, want 1", len(res.Files))
	}
	if res.Files[0].URL != "https://img2.pixhost.to/images/9999/1000001_first.jpg" {
		t.Errorf("url = %q", res.Files[0].URL)
	}
}

// TestPixhostFullImage covers the mapping a gallery is resolved through, and
// the cases it has to refuse: getting this wrong would turn every download
// into a 404, or worse, save thumbnails in place of the images.
func TestPixhostFullImage(t *testing.T) {
	tests := map[string]string{
		"https://t2.pixhost.to/thumbs/9999/1000001_first.jpg": "https://img2.pixhost.to/images/9999/1000001_first.jpg",
		"https://t51.pixhost.to/thumbs/1/2_a.png":             "https://img51.pixhost.to/images/1/2_a.png",
		// Not a thumbnail, so not an image of the gallery's.
		"https://pixhost.to/static/logo.png":                    "",
		"https://img2.pixhost.to/images/9999/1000001_first.jpg": "",
		"https://t2.elsewhere.example.test/thumbs/1/2.jpg":      "",
		"not a url": "",
	}
	for thumb, want := range tests {
		if got := pixhostFullImage(thumb); got != want {
			t.Errorf("pixhostFullImage(%q) = %q, want %q", thumb, got, want)
		}
	}
}
