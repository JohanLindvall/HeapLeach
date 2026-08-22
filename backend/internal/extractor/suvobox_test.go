package extractor

import (
	"net/url"
	"testing"
)

// A listing shaped like the site's, cut down to the parts the extractor
// reads: the tile link, the full name, the size, and the metadata title the
// heading above it truncates.
const suvoboxAlbumPage = `<!DOCTYPE html><html><head>
<title>SuvoBox</title>
<meta property="og:title" content="a whole album (p123)" />
</head><body>
<div class="album-header"><div class="album-box album-title-box">a whole album…</div></div>
<div class="thumb-grid">
  <a href="/f/aaaa1111?album=abcd" class="thumb-item">
    <div class="thumb-preview"><img src="//media.suvobox.example/t/aaaa1111-thumb.jpg" alt="first_AbCdEf"></div>
    <span class="thumb-name">first_AbCdEf.jpg</span>
    <span class="thumb-size">163.9 KB</span>
  </a>
  <a href="/f/bbbb2222?album=abcd" class="thumb-item">
    <div class="thumb-preview"><img src="//media.suvobox.example/t/bbbb2222-thumb.jpg" alt="second_GhIjKl"></div>
    <span class="thumb-name">second_GhIjKl.mp4</span>
    <span class="thumb-size">2.7 MB</span>
  </a>
  <a href="/takedowns" class="nametags">Takedowns</a>
</div></body></html>`

func TestSuvoboxAlbum(t *testing.T) {
	u, err := url.Parse("https://www.suvobox.com/a/abcd")
	if err != nil {
		t.Fatal(err)
	}
	root, err := parseHTML(suvoboxAlbumPage)
	if err != nil {
		t.Fatal(err)
	}

	res, err := suvoboxAlbum(root, u)
	if err != nil {
		t.Fatalf("suvoboxAlbum: %v", err)
	}
	// The heading is ellipsised to fit its box; the metadata is not.
	if res.Title != "a whole album (p123)" {
		t.Errorf("title = %q, want the untruncated metadata title", res.Title)
	}
	if len(res.Files) != 2 {
		t.Fatalf("found %d files, want 2 (the takedowns link is not one)", len(res.Files))
	}

	first := res.Files[0]
	if first.Name != "first_AbCdEf.jpg" {
		t.Errorf("name = %q, want the tile's own name, extension included", first.Name)
	}
	if want := "https://media.suvobox.com/f/aaaa1111?raw=1"; first.URL != want {
		t.Errorf("url = %q, want %q", first.URL, want)
	}
	// A listing rounds its sizes, so they must never settle whether a file
	// already on disk is this one.
	if first.Size <= 0 || !first.SizeApprox {
		t.Errorf("size = %d approx = %v, want a positive rounded size", first.Size, first.SizeApprox)
	}
	if res.Files[1].Name != "second_GhIjKl.mp4" {
		t.Errorf("second name = %q, want the video's name", res.Files[1].Name)
	}
}

func TestSuvoboxAlbumWithoutTilesIsAnError(t *testing.T) {
	u, _ := url.Parse("https://www.suvobox.com/a/abcd")
	root, err := parseHTML(`<html><body><p>gone</p></body></html>`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := suvoboxAlbum(root, u); err == nil {
		t.Fatal("an album with no tiles resolved to something")
	}
}

func TestSuvoboxFileID(t *testing.T) {
	cases := map[string]string{
		"https://www.suvobox.com/f/aaaa1111?album=abcd": "aaaa1111",
		"https://www.suvobox.com/f/aaaa1111":            "aaaa1111",
		"https://www.suvobox.com/a/abcd":                "",
		"https://www.suvobox.com/takedowns":             "",
		"://not a url":                                  "",
	}
	for link, want := range cases {
		if got := suvoboxFileID(link); got != want {
			t.Errorf("suvoboxFileID(%q) = %q, want %q", link, got, want)
		}
	}
}

func TestSuvoboxMatch(t *testing.T) {
	s := NewSuvobox(nil)
	for _, raw := range []string{"https://www.suvobox.com/a/abcd", "https://suvobox.com/f/aaaa1111"} {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		if !s.Match(u) {
			t.Errorf("Match(%q) = false", raw)
		}
	}
	other, _ := url.Parse("https://example.test/a/abcd")
	if s.Match(other) {
		t.Error("matched an unrelated host")
	}
}
