package extractor

import (
	"context"
	"net/url"
	"testing"
)

// A listing shaped like the site's, cut down to what the extractor reads and
// keeping every trap the real page carries:
//
//   - id="file" repeated on every anchor, so nothing may assume it is unique;
//   - a second anchor to a file already listed, which must not add an entry
//     and must not cost that entry its size;
//   - an advert wearing the same id between an entry and its size;
//   - an entry with no printed size, followed by one that has its own;
//   - a size already rounded to "2.7 MB", the form the page's own script
//     rewrites these into, which must never be taken for an exact length;
//   - a heading whose text is padded and whose title attribute is not.
const cyberdropAlbumPage = `<!DOCTYPE html><html><head>
<title>a holiday album | Cyberdrop</title>
</head><body>
<section class="hero">
  <h1 class="title" id="title" title="a holiday album">
      a holiday album
  </h1>
  <p class="subtitle">4 files</p>
</section>
<div class="columns is-multiline">

  <div class="image-container column">
    <a class="image" href="/f/aaaa1111" id="file" title="first_clip.mp4" target="_blank">
      <img class="lazyload" data-src="//img.example.test/t/aaaa1111.jpg" alt="first_clip.mp4">
    </a>
    <a class="is-hidden" href="/f/aaaa1111" id="file" title="first_clip.mp4">first_clip.mp4</a>
    <p class="file-size">798157 B</p>
  </div>

  <div class="image-container column">
    <a class="image" href="/f/bbbb2222" id="file" title="second photo.jpg">
      <img class="lazyload" data-src="//img.example.test/t/bbbb2222.jpg" alt="second photo.jpg">
    </a>
    <p class="file-size">2.7 MB</p>
  </div>

  <div class="image-container column">
    <a class="image" href="/f/cccc3333" id="file" title="third.zip"></a>
  </div>

  <div class="image-container column">
    <a class="image" href="/f/dddd4444" id="file" title="fourth.bin"></a>
    <a href="https://ads.example.test/click" id="file" title="Download our app!">advert</a>
    <p class="file-size">10485760 B</p>
  </div>

</div>
<footer><a href="/faq">FAQ</a></footer>
</body></html>`

// TestCyberdropAlbumListsEveryFileOnce pins the listing scrape: names off the
// anchor's attribute, one entry per file however many anchors point at it, and
// nothing minted up front.
func TestCyberdropAlbumListsEveryFileOnce(t *testing.T) {
	u, err := url.Parse("https://cyberdrop.cr/a/abcd1234")
	if err != nil {
		t.Fatal(err)
	}
	root, err := parseHTML(cyberdropAlbumPage)
	if err != nil {
		t.Fatal(err)
	}

	res, err := NewCyberdrop(nil).albumFrom(root, u)
	if err != nil {
		t.Fatalf("albumFrom: %v", err)
	}
	// The heading's text is padded; its attribute is the name verbatim.
	if res.Title != "a holiday album" {
		t.Errorf("title = %q, want the heading's title attribute", res.Title)
	}
	if len(res.Files) != 4 {
		t.Fatalf("found %d files, want 4 (the duplicate link and the advert are not files)",
			len(res.Files))
	}

	want := []string{"first_clip.mp4", "second photo.jpg", "third.zip", "fourth.bin"}
	for i, name := range want {
		if res.Files[i].Name != name {
			t.Errorf("file %d named %q, want %q", i, res.Files[i].Name, name)
		}
		if res.Files[i].URL != "" {
			t.Errorf("file %d carries a URL %q, but the link is minted per attempt",
				i, res.Files[i].URL)
		}
		if res.Files[i].Resolve == nil {
			t.Errorf("file %d has no resolver, so nothing would ever mint its link", i)
		}
		if res.Files[i].SizeApprox {
			t.Errorf("file %d is marked approximate, but a listing size here is exact", i)
		}
	}
}

// TestCyberdropAlbumSizesStayWithTheirOwnFile is the pairing rule: the size
// node sits beside the anchor rather than inside it, so a listing that skips
// one, or puts something else between the two, must not shift the columns.
func TestCyberdropAlbumSizesStayWithTheirOwnFile(t *testing.T) {
	u, _ := url.Parse("https://cyberdrop.cr/a/abcd1234")
	root, err := parseHTML(cyberdropAlbumPage)
	if err != nil {
		t.Fatal(err)
	}

	entries := cyberdropEntries(root, u)
	want := []cyberdropEntry{
		// The duplicate anchor comes before the size and must not steal it.
		{slug: "aaaa1111", name: "first_clip.mp4", size: 798157},
		// Already rounded for display, so refused rather than believed.
		{slug: "bbbb2222", name: "second photo.jpg", size: -1},
		// Prints no size, and must not borrow the next entry's.
		{slug: "cccc3333", name: "third.zip", size: -1},
		// The advert between the anchor and the size changes nothing.
		{slug: "dddd4444", name: "fourth.bin", size: 10485760},
	}
	if len(entries) != len(want) {
		t.Fatalf("read %d entries, want %d", len(entries), len(want))
	}
	for i, w := range want {
		if entries[i] != w {
			t.Errorf("entry %d = %+v, want %+v", i, entries[i], w)
		}
	}
}

func TestCyberdropAlbumWithoutEntriesIsAnError(t *testing.T) {
	u, _ := url.Parse("https://cyberdrop.cr/a/abcd1234")
	root, err := parseHTML(`<html><head><title>Cyberdrop</title></head>
<body><h1 class="title" id="title" title="gone">gone</h1></body></html>`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewCyberdrop(nil).albumFrom(root, u); err == nil {
		t.Fatal("an album naming no files resolved to something")
	}
}

// TestCyberdropBytesAcceptsOnlyTheExactCount guards the one number in this
// extractor that the downloader is allowed to treat as exact.
func TestCyberdropBytesAcceptsOnlyTheExactCount(t *testing.T) {
	cases := map[string]int64{
		"798157 B":    798157,
		" 1234 B ":    1234,
		"0 B":         0,
		"10485760 B":  10485760,
		"2.7 MB":      -1, // what the page's own script rewrites these into
		"779.45 KB":   -1,
		"798157":      -1,
		"B":           -1,
		"":            -1,
		"about 12 B":  -1,
		"-5 B":        -1,
		"9999999999B": 9999999999,
	}
	for text, want := range cases {
		if got := cyberdropBytes(text); got != want {
			t.Errorf("cyberdropBytes(%q) = %d, want %d", text, got, want)
		}
	}
}

func TestCyberdropSlug(t *testing.T) {
	cases := map[string]string{
		"https://cyberdrop.cr/f/abcd1234":     "abcd1234",
		"https://cyberdrop.cr/e/abcd1234":     "abcd1234",
		"https://cyberdrop.cr/f/abcd1234?x=1": "abcd1234",
		"https://cyberdrop.cr/a/abcd1234":     "",
		"https://cyberdrop.cr/faq":            "",
		"https://cyberdrop.cr/":               "",
		"https://ads.example.test/click":      "",
		"://not a url":                        "",
	}
	for link, want := range cases {
		if got := cyberdropSlug(link); got != want {
			t.Errorf("cyberdropSlug(%q) = %q, want %q", link, got, want)
		}
	}
}

// TestCyberdropAPIURLsAreSlugSubstitution pins the property the album path
// relies on: nothing but the slug is needed to sign an entry, which is why the
// listing never asks the metadata endpoint about its own files.
func TestCyberdropAPIURLsAreSlugSubstitution(t *testing.T) {
	if got, want := cyberdropInfoURL("abcd1234"), "https://api.cyberdrop.cr/api/file/info/abcd1234"; got != want {
		t.Errorf("cyberdropInfoURL = %q, want %q", got, want)
	}
	if got, want := cyberdropAuthURL("abcd1234"), "https://api.cyberdrop.cr/api/file/auth/abcd1234"; got != want {
		t.Errorf("cyberdropAuthURL = %q, want %q", got, want)
	}
	// A slug is site-generated, but it is still remote input: it must not be
	// able to reach a different endpoint.
	if got, want := cyberdropAuthURL("../info/x"), "https://api.cyberdrop.cr/api/file/auth/..%2Finfo%2Fx"; got != want {
		t.Errorf("cyberdropAuthURL escaped to %q, want %q", got, want)
	}
}

// TestCyberdropMatchIsTheOneDomain is the correction worth pinning: the two
// domains listed alongside this host are not aliases of it. One does not
// resolve, and the other is parked and would answer with advertising.
func TestCyberdropMatchIsTheOneDomain(t *testing.T) {
	c := NewCyberdrop(nil)
	for _, raw := range []string{
		"https://cyberdrop.cr/a/abcd1234",
		"https://www.cyberdrop.cr/f/abcd1234",
	} {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		if !c.Match(u) {
			t.Errorf("Match(%q) = false", raw)
		}
	}
	for _, raw := range []string{
		"https://cyberdrop.me/a/abcd1234",
		"https://cyberdrop.to/a/abcd1234",
		"https://cyberdrop.example.test/a/abcd1234",
	} {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		if c.Match(u) {
			t.Errorf("Match(%q) = true, but that host is not this service", raw)
		}
	}
}

// TestCyberdropRejectsUnknownPathsWithoutFetching keeps a URL that names
// neither an album nor a file from costing a request to find out.
func TestCyberdropRejectsUnknownPathsWithoutFetching(t *testing.T) {
	// A nil client is the assertion: reaching the network would panic.
	c := NewCyberdrop(nil)
	for _, raw := range []string{
		"https://cyberdrop.cr/",
		"https://cyberdrop.cr/faq",
		"https://cyberdrop.cr/u/someone",
	} {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := c.Extract(context.Background(), u, Options{}); err == nil {
			t.Errorf("Extract(%q) resolved to something", raw)
		}
	}
}
