// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

// A page shaped like the software's. The track anchor carries the whole
// record — title, artist and the path the audio comes from — which is what
// makes one fetch enough; the surrounding markup is a theme's business and
// deliberately not read.
const bandzooglePage = `<!DOCTYPE html><html><head>
<title>A Band Name - Official Site</title>
</head><body>
<h2>Discography</h2>

<section class="album">
<h3>An Album Name</h3>
<div class="zoogle-music-player" data-controller="zoogle-media-player">
<ol class="track-list" data-offset="20" data-load-more="false">
  <li class="track-list-item">
    <div class="track-number-play">
      <a type="audio/mp3" data-id="111" data-artist="A Band Name" data-duration="2:01"
         data-title="First Song" data-dest="/player/900/tracks/111.mp3"
         data-zoogle-track="true" class="track-icon play" href="#"></a>
      <span class="track-number"> 1 </span>
    </div>
  </li>
  <li class="track-list-item">
    <div class="track-number-play">
      <a type="audio/mp3" data-id="112" data-artist="A Band Name" data-duration="1:12"
         data-title="Second Song" data-dest="/player/900/tracks/112.mp3"
         data-zoogle-track="true" class="track-icon play" href="#"></a>
      <span class="track-number"> 12 </span>
    </div>
  </li>
  <li class="track-list-item">
    <!-- No number printed beside it, and no title either: the path names it. -->
    <a type="audio/mp3" data-id="113" data-dest="/player/900/tracks/untitled-113.mp3"
       data-zoogle-track="true" class="track-icon play" href="#"></a>
  </li>
</ol>
</div>
</section>

<section class="album">
<!-- The same album under a second player, which is how these pages present
     one release in two places: the ids repeat and only the path differs, so
     nothing keyed on the path would notice. -->
<h3>An Album Name</h3>
<div class="zoogle-music-player" data-controller="zoogle-media-player">
<ol class="track-list" data-offset="20" data-load-more="false">
  <li class="track-list-item">
    <div class="track-number-play">
      <a type="audio/mp3" data-id="111" data-artist="A Band Name"
         data-title="First Song" data-dest="/player/901/tracks/111.mp3"
         data-zoogle-track="true" class="track-icon play" href="#"></a>
      <span class="track-number"> 1 </span>
    </div>
  </li>
</ol>
</div>
</section>

<section class="album">
<h3>A Second Album</h3>
<div class="zoogle-music-player" data-controller="zoogle-media-player">
<ol class="track-list" data-offset="20" data-load-more="false">
  <li class="track-list-item">
    <div class="track-number-play">
      <a type="audio/mp3" data-id="221" data-artist="A Band Name"
         data-title="Another First Song" data-dest="/player/902/tracks/221.mp3"
         data-zoogle-track="true" class="track-icon play" href="#"></a>
      <span class="track-number"> 1 </span>
    </div>
  </li>
</ol>
</div>
</section>

<a href="/merch">Not a track</a>
<a data-zoogle-track="true" href="#">A track anchor with nowhere to fetch from</a>
</body></html>`

func bandzoogleServer(t *testing.T, body string) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(s.Close)
	return s
}

func TestBandzoogleReadsEveryTrackOnce(t *testing.T) {
	base := mustParse(t, "https://aband.example.test/")
	root := mustParseDoc(t, bandzooglePage)

	files, artist := bandzoogleTracks(root, base)
	if artist != "A Band Name" {
		t.Errorf("artist = %q", artist)
	}
	if len(files) != 4 {
		t.Fatalf("got %d tracks, want 4 — the recording listed under two "+
			"players is one track, and the anchor with no destination is none",
			len(files))
	}

	// Numbered as the page prints them, padded so a directory listing sorts
	// the way the album plays.
	want := []struct{ name, link string }{
		{"01 First Song.mp3", "https://aband.example.test/player/900/tracks/111.mp3"},
		{"12 Second Song.mp3", "https://aband.example.test/player/900/tracks/112.mp3"},
		{"untitled-113.mp3", "https://aband.example.test/player/900/tracks/untitled-113.mp3"},
	}
	for i, w := range want {
		if files[i].Name != w.name {
			t.Errorf("track %d name = %q, want %q", i, files[i].Name, w.name)
		}
		// The player link, not a signed one: the signature is minted per
		// request, so storing one would go stale in the queue.
		if files[i].URL != w.link {
			t.Errorf("track %d url = %q, want %q", i, files[i].URL, w.link)
		}
		if files[i].Headers[httpx.HeaderReferer] == "" {
			t.Errorf("track %d carries no referer", i)
		}
		if files[i].Resolve != nil {
			t.Errorf("track %d has a resolver it does not need", i)
		}
	}
}

func TestBandzoogleRecognise(t *testing.T) {
	b := NewBandzoogle(nil)
	expectRecognised(t, b, htmlPage(t, "https://aband.example.test/", bandzooglePage), true)
	expectRecognised(t, b, htmlPage(t, "https://aband.example.test/music/albums", bandzooglePage), true)
	// A page that names the platform but carries no player.
	expectRecognised(t, b, htmlPage(t, "https://aband.example.test/",
		`<html><head><title>A Band Name</title><meta name="generator" content="Bandzoogle"></head>
		<body><p>Built with data-zoogle-track support</p></body></html>`), false)
	// A page that is not one of these at all.
	expectRecognised(t, b, htmlPage(t, "https://aband.example.test/",
		`<!DOCTYPE html><html><head><title>Something else</title></head>
		<body><a href="/x.mp3">a plain link</a><audio src="/y.mp3"></audio></body></html>`), false)
	expectShape(t, b, "https://aband.example.test/music", false)
}

func TestBandzoogleExt(t *testing.T) {
	cases := map[string]string{
		"https://x.example.test/player/1/tracks/2.mp3": ".mp3",
		"https://x.example.test/player/1/tracks/2.m4a": ".m4a",
		// Nothing to read: these serve mp3, and a file with no extension at
		// all is worse than the likely one.
		"https://x.example.test/player/1/tracks/2": ".mp3",
		"": ".mp3",
	}
	for in, want := range cases {
		if got := linkExtAsIs(in, bandzoogleDefaultExt); got != want {
			t.Errorf("linkExtAsIs(%q) = %q, want %q", in, got, want)
		}
	}
}

// A band's own domain is in no list: the page is recognised by its player,
// and read once for recognition and extraction both.
func TestBandzoogleRecognisedThroughTheRegistry(t *testing.T) {
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			hits.Add(1)
		}
		w.Header().Set(httpx.HeaderContentType, "text/html")
		_, _ = w.Write([]byte(bandzooglePage))
	}))
	t.Cleanup(server.Close)
	cfg := &config.Config{UserAgent: config.DefaultUserAgent, Language: config.DefaultLanguage}
	client := httpx.New(cfg.UserAgent, cfg.AcceptLanguage(), 0, 10*time.Second)

	res, ex, err := NewRegistry(cfg, client).Extract(context.Background(), server.URL+"/", Options{})
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if ex.Name() != "bandzoogle" {
		t.Errorf("routed via %s", ex.Name())
	}
	if len(res.Files) != 4 {
		t.Fatalf("got %d tracks through the registry, want 4", len(res.Files))
	}
	if res.Files[0].Name != "01 First Song.mp3" {
		t.Errorf("first track named %q", res.Files[0].Name)
	}
	if res.Title != "A Band Name" {
		t.Errorf("title = %q, want the artist the tracks name", res.Title)
	}
	if n := hits.Load(); n != 1 {
		t.Errorf("the page was fetched %d times, want once", n)
	}
}

// Every page on the web is offered to the recogniser, so what happens when
// the answer is "not mine" matters more than what happens when it is.
func TestBandzoogleKeepsOutOfTheWay(t *testing.T) {
	cfg := &config.Config{}
	t.Run("a URL naming a file is never fetched", func(t *testing.T) {
		var hits int
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			hits++
			_, _ = w.Write([]byte(bandzooglePage))
		}))
		defer server.Close()
		reg := NewRegistry(cfg, testClient())

		if _, ex, _ := reg.Extract(context.Background(), server.URL+"/files/archive.zip", Options{}); ex == nil || ex.Name() == "bandzoogle" {
			t.Errorf("a file link went to %v", ex)
		}
		if hits != 0 {
			t.Errorf("spent %d requests on a URL that already names a file", hits)
		}
	})

	t.Run("a fetch that fails is not a claim", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "gone", http.StatusNotFound)
		}))
		defer server.Close()
		reg := NewRegistry(cfg, testClient())

		// Not having managed to look is not the same as having looked and
		// recognised it: this platform's name must not go on somebody
		// else's 404.
		if _, ex, _ := reg.Extract(context.Background(), server.URL+"/nothing", Options{}); ex == nil || ex.Name() == "bandzoogle" {
			t.Errorf("a page that would not load went to %v", ex)
		}
	})

	t.Run("a page carrying the player but no tracks is reported", func(t *testing.T) {
		// Committed: falling through here would hand the page to the direct
		// fallback, which would save the HTML and call it a download.
		server := bandzoogleServer(t, `<html><body>
			<a data-zoogle-track="true" href="#">no destination</a></body></html>`)
		reg := NewRegistry(cfg, testClient())

		res, ex, err := reg.Extract(context.Background(), server.URL+"/", Options{})
		if res != nil {
			t.Errorf("returned %+v for a page with nothing to fetch", res)
		}
		if err == nil || ex == nil || ex.Name() != "bandzoogle" {
			t.Errorf("a page that named a player and offered nothing was passed over: %v, %v", ex, err)
		}
	})
}

func TestBandzoogleNumber(t *testing.T) {
	cases := []struct{ name, markup, want string }{
		{
			"the number beside the track",
			`<li><div class="track-number-play">
				<a data-zoogle-track="true" data-dest="/x.mp3"></a>
				<span class="track-number"> 7 </span></div></li>`,
			"07 ",
		},
		{
			"two digits are not padded further",
			`<li><div class="track-number-play">
				<a data-zoogle-track="true" data-dest="/x.mp3"></a>
				<span class="track-number">14</span></div></li>`,
			"14 ",
		},
		{
			"no number printed",
			`<li><div class="track-number-play">
				<a data-zoogle-track="true" data-dest="/x.mp3"></a></div></li>`,
			"",
		},
		{
			"something that is not a number",
			`<li><div class="track-number-play">
				<a data-zoogle-track="true" data-dest="/x.mp3"></a>
				<span class="track-number">bonus</span></div></li>`,
			"",
		},
		{
			// The list holding every track also holds every number. Climbing
			// into it would hand this track its neighbour's position.
			"a neighbour's number is never borrowed",
			`<ul>
				<li><a data-zoogle-track="true" data-dest="/a.mp3"></a></li>
				<li><a data-zoogle-track="true" data-dest="/b.mp3"></a>
				    <span class="track-number">9</span></li>
			</ul>`,
			"",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := mustParseDoc(t, "<html><body>"+tc.markup+"</body></html>")
			a := findFirst(root, isBandzoogleTrack)
			if a == nil {
				t.Fatal("no track anchor in the fixture")
			}
			if got := bandzoogleNumber(a); got != tc.want {
				t.Errorf("bandzoogleNumber = %q, want %q", got, tc.want)
			}
		})
	}
}

// Numbering restarts with each album, so two albums on one page put two
// track 1s in the same directory unless they are foldered apart. That is
// what this page does, and what the destination directory showed when it
// was not done: an "01" from each album sitting side by side.
func TestBandzoogleFoldersTracksByAlbum(t *testing.T) {
	base := mustParse(t, "https://aband.example.test/")
	root := mustParseDoc(t, bandzooglePage)
	files, _ := bandzoogleTracks(root, base)

	byDir := map[string][]string{}
	for _, f := range files {
		byDir[f.Dir] = append(byDir[f.Dir], f.Name)
	}
	if len(byDir) != 2 {
		t.Fatalf("tracks landed in %d folders, want one per album: %v", len(byDir), byDir)
	}
	if got := byDir["An Album Name"]; len(got) != 3 {
		t.Errorf("first album holds %v", got)
	}
	if got := byDir["A Second Album"]; len(got) != 1 || got[0] != "01 Another First Song.mp3" {
		t.Errorf("second album holds %v", got)
	}
	// The section heading above both albums must never become a folder: the
	// walk has to stop before it reaches something holding two players.
	if _, wrong := byDir["Discography"]; wrong {
		t.Error("a track was filed under the heading that encloses every album")
	}
}

func TestBandzoogleAlbumName(t *testing.T) {
	cases := map[string]string{
		// The decoration a heading wraps a release in says nothing a folder
		// needs, and the spacing closes up behind it. German quotes are the
		// pair that prompted this; the others are the same idea spelled the
		// ways the web spells it.
		`EP „Ein Erfundener Titel“`: "EP Ein Erfundener Titel",
		`"Quoted Title"`:            "Quoted Title",
		`Album «Name»`:              "Album Name",
		`  Spaced   Out  `:          "Spaced Out",
		// Nothing to strip.
		"A Plain Release Name": "A Plain Release Name",
		// An apostrophe sits inside a word rather than around a title, so
		// taking it would leave the name misspelt.
		"Don't Stop (extended)": "Don't Stop (extended)",
		"Rock ’n’ Roll":         "Rock ’n’ Roll",
	}
	for in, want := range cases {
		if got := bandzoogleAlbumName(in); got != want {
			t.Errorf("bandzoogleAlbumName(%q) = %q, want %q", in, got, want)
		}
	}
}
