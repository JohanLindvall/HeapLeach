package extractor

import (
	"net/url"
	"testing"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

func TestBunkrMatchCoversTheDomainRotation(t *testing.T) {
	b := NewBunkr(nil)
	cases := map[string]bool{
		"https://bunkr.cr/a/AAAA":       true,
		"https://bunkr.si/f/BBBB":       true,
		"https://bunkrr.su/f/CCCC":      true,
		"https://cdn.bunkr.example/x":   true,
		"https://example.test/f/DDDD":   false,
		"https://notbunkr.example/f/EE": false,
	}
	for raw, want := range cases {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		if got := b.Match(u); got != want {
			t.Errorf("Match(%s) = %v, want %v", raw, got, want)
		}
	}
}

func TestParseBunkrDownloadHref(t *testing.T) {
	host, id, ok := parseBunkrDownloadHref("https://dl.bunkr.example/file/12345")
	if !ok || host != "https://dl.bunkr.example" || id != "12345" {
		t.Errorf("got (%q, %q, %v), want the dl host and the id", host, id, ok)
	}

	for _, href := range []string{
		"https://bunkr.example/file/12345",  // not the dl. host
		"https://dl.bunkr.example/f/AAAA",   // a file page, not the API route
		"https://dl.bunkr.example/file",     // no id
		"/file/12345",                       // relative: no host to speak to
		"https://dl.bunkr.example/file/1/2", // too deep
	} {
		if _, _, ok := parseBunkrDownloadHref(href); ok {
			t.Errorf("%q was taken for a download href", href)
		}
	}
}

func TestIsBunkrFilePage(t *testing.T) {
	if !isBunkrFilePage("https://bunkr.example/f/AAAA") {
		t.Error("a /f/<slug> link was not recognised")
	}
	for _, raw := range []string{
		"https://bunkr.example/a/AAAA",
		"https://bunkr.example/f/AAAA/extra",
		"https://bunkr.example/",
	} {
		if isBunkrFilePage(raw) {
			t.Errorf("%q was taken for a file page", raw)
		}
	}
}

// The album listing carries the name and the size on the card rather than on
// the link, so the queue can show real rows before a single resolve runs.
const bunkrAlbumPage = `<html><body>
  <div class="theItem" title="clip one.mp4">
    <a href="/f/slug1"><img alt="clip one.mp4"></a>
    <p class="theName">clip one.mp4</p><p class="theSize">1.5 GB</p>
  </div>
  <div class="theItem">
    <a href="/f/slug2"><img alt="picture two.jpg"></a>
    <p class="theName">picture two.jpg</p><p class="theSize">2.31 MB</p>
  </div>
  <div class="theItem">
    <a href="/f/slug1"><img alt="a repeat of the first"></a>
    <p class="theName">clip one.mp4</p><p class="theSize">1.5 GB</p>
  </div>
  <a href="/faq">FAQ</a>
  <a href="?page=2">2</a><a href="?page=3">3</a>
</body></html>`

func TestBunkrAlbumEntries(t *testing.T) {
	root, err := parseHTML(bunkrAlbumPage)
	if err != nil {
		t.Fatal(err)
	}
	base, _ := url.Parse("https://bunkr.example/a/AAAA")

	b := NewBunkr(nil)
	files := b.albumEntries(root, base, make(map[string]bool))
	if len(files) != 2 {
		t.Fatalf("read %d entries, want 2 (the repeat folded, the FAQ ignored)", len(files))
	}
	if files[0].Name != "clip one.mp4" || files[1].Name != "picture two.jpg" {
		t.Errorf("names = %q, %q", files[0].Name, files[1].Name)
	}
	// Listing sizes are rounded for display, so they must arrive approximate:
	// exact would let the skip check measure a file against a number it can
	// never match.
	if files[0].Size != int64(1.5*(1<<30)) || !files[0].SizeApprox {
		t.Errorf("size = %d approx=%v, want the printed 1.5 GB marked approximate",
			files[0].Size, files[0].SizeApprox)
	}
	for _, f := range files {
		if f.Resolve == nil {
			t.Errorf("%s has no resolver; bunkr links are signed and expire", f.Name)
		}
	}
}

func TestLastAlbumPage(t *testing.T) {
	root, err := parseHTML(bunkrAlbumPage)
	if err != nil {
		t.Fatal(err)
	}
	if got := lastAlbumPage(root); got != 3 {
		t.Errorf("last page = %d, want 3", got)
	}
}

func TestBunkrLinkLabel(t *testing.T) {
	root, err := parseHTML(`<a href="/f/x"><img alt="from the alt"></a>` +
		`<a id="second" href="/f/y">from the text</a>`)
	if err != nil {
		t.Fatal(err)
	}
	anchors := findAll(root, func(n *html.Node) bool { return isElem(n, atom.A) })
	if len(anchors) != 2 {
		t.Fatalf("parsed %d anchors, want 2", len(anchors))
	}
	if got := linkLabel(anchors[0]); got != "from the alt" {
		t.Errorf("label = %q, want the image's alt text", got)
	}
	if got := linkLabel(anchors[1]); got != "from the text" {
		t.Errorf("label = %q, want the anchor's text", got)
	}
}
