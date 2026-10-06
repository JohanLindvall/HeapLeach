// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

// bandcampPageFixture renders a release page the way the site does: the whole
// release as JSON, HTML-escaped into one attribute of a script tag.
func bandcampPageFixture(tralbum, embed string) string {
	var b strings.Builder
	b.WriteString(`<!DOCTYPE html><html><head><meta property="og:site_name" content="A Band">`)
	if embed != "" {
		b.WriteString(`<script data-embed="` + html.EscapeString(embed) + `"></script>`)
	}
	b.WriteString(`<script src="x.js" data-tralbum="` + html.EscapeString(tralbum) + `"></script>`)
	b.WriteString(`</head><body></body></html>`)
	return b.String()
}

const bandcampAlbumData = `{"artist":"A Band","item_type":"album","current":{"title":"First Album"},
"trackinfo":[
 {"track_num":1,"title":"First Song","artist":null,"track_id":11,"file":{"mp3-128":"https://cdn.example.test/stream/11?token=a"}},
 {"track_num":2,"title":"Either / Or","artist":null,"track_id":12,"file":{"mp3-128":"https://cdn.example.test/stream/12?token=a"}},
 {"track_num":3,"title":"Guest Turn","artist":"Another Band","track_id":13,"file":{"mp3-128":"https://cdn.example.test/stream/13?token=a"}},
 {"track_num":4,"title":"Bought Only","artist":null,"track_id":14,"file":null}
]}`

func TestBandcampAlbumIsFiledByBandAndAlbum(t *testing.T) {
	root, err := parseHTML(bandcampPageFixture(bandcampAlbumData, ""))
	if err != nil {
		t.Fatal(err)
	}
	p, err := bandcampRelease(root, "https://a-band.bandcamp.com/album/first-album")
	if err != nil {
		t.Fatal(err)
	}
	if got := p.title(); got != "A Band - First Album" {
		t.Errorf("title = %q", got)
	}

	files := p.files(NewBandcamp(nil))
	var names []string
	for _, f := range files {
		names = append(names, f.Name)
		if f.Dir != "A Band/First Album" {
			t.Errorf("%s filed under %q, want the band then the album", f.Name, f.Dir)
		}
		if f.Resolve == nil {
			t.Errorf("%s has no resolver: its link is signed for a day", f.Name)
		}
		if f.Headers[httpx.HeaderUserAgent] != bandcampAgent {
			t.Errorf("%s would be fetched as a browser, which is what gets the challenge", f.Name)
		}
	}
	want := []string{
		"01 - First Song.mp3",
		"02 - Either / Or.mp3", // the downloader makes the slash safe; the folder is what must not split
		"03 - Another Band - Guest Turn.mp3",
	}
	if strings.Join(names, "|") != strings.Join(want, "|") {
		t.Errorf("names = %q, want %q (a track with no stream is left out)", names, want)
	}
}

func TestBandcampTrackPageNamesItsAlbum(t *testing.T) {
	data := `{"artist":"A Band","item_type":"track","current":{"title":"First Song"},
"trackinfo":[{"track_num":1,"title":"First Song","track_id":11,"file":{"mp3-128":"https://cdn.example.test/s/11"}}]}`
	root, _ := parseHTML(bandcampPageFixture(data, `{"album_title":"A / B Sides"}`))
	p, err := bandcampRelease(root, "https://a-band.bandcamp.com/track/first-song")
	if err != nil {
		t.Fatal(err)
	}
	files := p.files(NewBandcamp(nil))
	if len(files) != 1 || files[0].Dir != "A Band/A _ B Sides" || files[0].Name != "01 - First Song.mp3" {
		t.Fatalf("files = %+v; want it filed under its album, the title's slash kept out of the path", files)
	}

	// A single that belongs to no album goes in the band's folder, unnumbered.
	root, _ = parseHTML(bandcampPageFixture(data, `{}`))
	p, _ = bandcampRelease(root, "https://a-band.bandcamp.com/track/first-song")
	files = p.files(NewBandcamp(nil))
	if files[0].Dir != "A Band" || files[0].Name != "First Song.mp3" {
		t.Errorf("single = %q in %q", files[0].Name, files[0].Dir)
	}
}

func TestBandcampResolveMintsAFreshLinkAsNotABrowser(t *testing.T) {
	var agent string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		agent = r.UserAgent()
		_, _ = w.Write([]byte(bandcampPageFixture(strings.ReplaceAll(bandcampAlbumData, "token=a", "token=b"), "")))
	}))
	defer srv.Close()

	b := NewBandcamp(httpx.New("Mozilla/5.0 a browser", "en-US", 0, 5*time.Second))
	tg, err := b.stream(context.Background(), srv.URL+"/album/first-album", 12)
	if err != nil {
		t.Fatal(err)
	}
	if tg.URL != "https://cdn.example.test/stream/12?token=b" {
		t.Errorf("url = %q, want the fresh link for that track", tg.URL)
	}
	if agent != bandcampAgent {
		t.Errorf("page requested as %q; a browser agent is what gets the challenge", agent)
	}
	if _, err := b.stream(context.Background(), srv.URL+"/album/first-album", 14); err == nil {
		t.Error("a track with no stream resolved")
	}
}

func TestBandcampReleaseLinks(t *testing.T) {
	doc := `<html><body>
<div data-tralbum="` + html.EscapeString(`{"current":{},"trackinfo":null}`) + `"></div>
<a href="/album/not-in-grid">elsewhere</a>
<ol id="music-grid" data-client-items="` + html.EscapeString(`[{"page_url":"/album/third"},{"page_url":"/album/first"}]`) + `">
 <li><a href="/album/first">First</a></li>
 <li><a href="/track/second?from=grid">Second</a></li>
 <li><a href="https://other.example.test/album/x">off-site</a></li>
</ol></body></html>`
	root, _ := parseHTML(doc)
	// The listing carries release data of its own, empty and of no kind; it
	// must not be read as a band with a single release.
	if p, err := bandcampRelease(root, "x"); err == nil && p.kind != "" {
		t.Errorf("a listing read as a %q", p.kind)
	}
	u, _ := url.Parse("https://a-band.bandcamp.com/music")
	got := bandcampReleaseLinks(root, u)
	want := []string{
		"https://a-band.bandcamp.com/album/first",
		"https://a-band.bandcamp.com/track/second",
		"https://a-band.bandcamp.com/album/third",
	}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("links = %q, want %q", got, want)
	}
}

func TestBandcampMatch(t *testing.T) {
	b := NewBandcamp(nil)
	for raw, want := range map[string]bool{
		"https://a-band.bandcamp.com/album/first-album":    true,
		"https://a-band.bandcamp.com/track/first-song":     true,
		"https://a-band.bandcamp.com/":                     true,
		"https://a-band.bandcamp.com/music":                true,
		"https://bandcamp.com/a-listener":                  false,
		"https://a-band.bandcamp.com/merch":                false,
		"https://notbandcamp.com/album/x":                  false,
		"https://a-band.bandcamp.com.example.test/album/x": false,
	} {
		u, _ := url.Parse(raw)
		if got := b.Match(u); got != want {
			t.Errorf("Match(%s) = %v, want %v", raw, got, want)
		}
	}
}
