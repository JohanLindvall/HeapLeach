// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/JohanLindvall/HeapLeach/internal/httpx"
	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// A viewer page shaped like the site's, holding the two traps this extractor
// exists to avoid: the page's own furniture is images too, and the metadata
// offers the poster frame rather than the file.
const imagePondVideoPage = `<!DOCTYPE html><html><head>
<meta property="og:title" content="Some+Clip+Name.mov" />
<meta property="og:type" content="video.other" />
<meta property="og:image" content="https://media.imagepond.net/media/videos/SomeClipName_abc123_thumb.jpg" />
<meta property="og:video" content="https://media.imagepond.net/media/videos/SomeClipName_abc123.mov" />
<meta property="og:video:type" content="video/mp4" />
</head><body>
<img src="https://media.imagepond.net/media/images/site-logo_aaa000.png" alt="ImagePond logo">
<img src="https://ads.example.test/banner.jpg" alt="advert">
<div class="video-player-wrapper">
  <video id="videoPlayer" controls
         poster="https://media.imagepond.net/media/videos/SomeClipName_abc123_thumb.jpg"
         data-src="https://media.imagepond.net/media/videos/SomeClipName_abc123.mov"
         data-type="video/quicktime"></video>
</div></body></html>`

const imagePondImagePage = `<!DOCTYPE html><html><head>
<meta property="og:title" content="A+Still+Picture.jpg" />
<meta property="og:image" content="https://media.imagepond.net/media/images/AStillPicture_xyz789.jpg" />
</head><body>
<img src="https://media.imagepond.net/media/images/site-logo_aaa000.png" alt="ImagePond logo">
<img src="https://media.imagepond.net/media/images/AStillPicture_xyz789.jpg" alt="A Still Picture">
</body></html>`

// The newer shape of a video page: the player is rendered client-side, so
// the delivered markup carries no <video> element at all. og:video names the
// site's own /direct route instead, and og:image beside it is the poster
// frame — the trap this page exists to pin.
const imagePondDirectVideoPage = `<!DOCTYPE html><html><head>
<meta property="og:title" content="Another+Clip.mp4" />
<meta property="og:type" content="video.other" />
<meta property="og:url" content="https://www.imagepond.net/i/abc123" />
<meta property="og:video" content="https://www.imagepond.net/i/abc123/direct" />
<meta property="og:video:type" content="video/mp4" />
<meta property="og:image" content="https://media.imagepond.net/media/videos/AnotherClip_abc123_thumb.jpg" />
</head><body>
<img src="https://media.imagepond.net/media/images/site-logo_aaa000.png" alt="ImagePond logo">
<div class="video-player-wrapper" data-video="abc123"></div>
</body></html>`

// A page whose player is client-side resolves through the site's own
// download route, rather than failing for want of a <video> element or
// settling for the poster frame the metadata offers beside it.
func TestImagePondMediaLinkTakesTheDirectRoute(t *testing.T) {
	base, _ := url.Parse("https://www.imagepond.net/i/abc123")
	root := mustParseDoc(t, imagePondDirectVideoPage)

	got := imagePondMediaLink(root, base)
	want := "https://www.imagepond.net/i/abc123/direct"
	if got != want {
		t.Errorf("media link = %q, want the site's own route %q", got, want)
	}
}

// The two shapes coexist, and the older one must keep resolving to the
// stored file rather than being rerouted through /direct.
func TestImagePondMediaLinkStillPrefersAPlayerElement(t *testing.T) {
	base, _ := url.Parse("https://www.imagepond.net/i/abc123")
	root := mustParseDoc(t, `<html><head>
		<meta property="og:video" content="https://www.imagepond.net/i/abc123/direct" />
		</head><body>
		<video data-src="https://media.imagepond.net/media/videos/SomeClipName_abc123.mov"></video>
		</body></html>`)

	got := imagePondMediaLink(root, base)
	want := "https://media.imagepond.net/media/videos/SomeClipName_abc123.mov"
	if got != want {
		t.Errorf("media link = %q, want the player's own stored file %q", got, want)
	}
}

// An image page carries no og:video, so widening what counts as a usable
// link must not divert it away from the file its metadata names.
func TestImagePondMediaLinkLeavesAnImagePageAlone(t *testing.T) {
	base, _ := url.Parse("https://www.imagepond.net/i/xyz789")
	root := mustParseDoc(t, imagePondImagePage)

	got := imagePondMediaLink(root, base)
	want := "https://media.imagepond.net/media/images/AStillPicture_xyz789.jpg"
	if got != want {
		t.Errorf("media link = %q, want the stored image %q", got, want)
	}
}

// imagePondDirect is the whole of what widened, so what it refuses matters
// as much as what it takes: only this site's own item route, and only in the
// shape that actually redirects to a file.
func TestImagePondDirect(t *testing.T) {
	cases := map[string]bool{
		"https://www.imagepond.net/i/abc123/direct":      true,
		"https://imagepond.net/i/abc123/direct":          true,  // the bare domain serves it too
		"https://www.imagepond.net/i/abc123":             false, // the viewer page, not the file
		"https://www.imagepond.net/i/abc123/direct/more": false,
		"https://www.imagepond.net/videos/abc123":        false, // the embed player
		"https://www.imagepond.net/direct":               false,
		"https://elsewhere.example.test/i/abc123/direct": false, // another site's route
		"":            false,
		"::not a url": false,
	}
	for link, want := range cases {
		if got := imagePondDirect(link) != ""; got != want {
			t.Errorf("imagePondDirect(%q) accepted=%v, want %v", link, got, want)
		}
	}
}

// The two halves of the filter, together: either shape is usable, anything
// else is not.
func TestImagePondUsable(t *testing.T) {
	cases := map[string]bool{
		"https://media.imagepond.net/media/videos/SomeClipName_abc123.mov":       true,
		"https://www.imagepond.net/i/abc123/direct":                              true,
		"https://media.imagepond.net/media/videos/SomeClipName_abc123_thumb.jpg": false,
		"https://ads.example.test/preroll.mp4":                                   false,
	}
	for link, want := range cases {
		if got := imagePondUsable(link) != ""; got != want {
			t.Errorf("imagePondUsable(%q) accepted=%v, want %v", link, got, want)
		}
	}
}

func TestImagePondMediaLinkPrefersThePlayerOverTheMetadata(t *testing.T) {
	base, err := url.Parse("https://www.imagepond.net/i/abc123")
	if err != nil {
		t.Fatal(err)
	}
	root := mustParseDoc(t, imagePondVideoPage)

	got := imagePondMediaLink(root, base)
	want := "https://media.imagepond.net/media/videos/SomeClipName_abc123.mov"
	if got != want {
		t.Errorf("media link = %q, want the player's own data-src %q", got, want)
	}
}

// The site's own logo is served from the media host and appears in the
// markup before the item does, so an image page has to be resolved from the
// metadata that names the item rather than by scanning for a hosted image.
func TestImagePondMediaLinkFindsTheImageNotTheLogo(t *testing.T) {
	base, _ := url.Parse("https://www.imagepond.net/i/xyz789")
	root := mustParseDoc(t, imagePondImagePage)

	got := imagePondMediaLink(root, base)
	want := "https://media.imagepond.net/media/images/AStillPicture_xyz789.jpg"
	if got != want {
		t.Errorf("media link = %q, want the item rather than the site's logo (%q)", got, want)
	}
}

func TestImagePondMediaLinkIgnoresEverythingElse(t *testing.T) {
	base, _ := url.Parse("https://www.imagepond.net/i/abc123")
	root := mustParseDoc(t, `<html><body>
		<img src="https://ads.example.test/banner.jpg">
		<video data-src="https://ads.example.test/preroll.mp4"></video>
	</body></html>`)
	if got := imagePondMediaLink(root, base); got != "" {
		t.Errorf("media link = %q, want none: nothing on the page is hosted media", got)
	}
}

func TestImagePondHostedRejectsThePosterFrame(t *testing.T) {
	poster := "https://media.imagepond.net/media/videos/SomeClipName_abc123_thumb.jpg"
	if got := imagePondHosted(poster); got != "" {
		t.Errorf("imagePondHosted(poster) = %q, want none", got)
	}
}

func TestImagePondName(t *testing.T) {
	cases := map[string]string{
		"Some+Clip+Name.mov":   "Some Clip Name.mov",
		"already%20fine.jpg":   "already fine.jpg",
		"NoEncodingNeeded.mp4": "NoEncodingNeeded.mp4",
		"":                     "",
	}
	for in, want := range cases {
		if got := imagePondName(in); got != want {
			t.Errorf("imagePondName(%q) = %q, want %q", in, got, want)
		}
	}
}

// imagePondServer serves one page as the viewer would, so Extract can be
// exercised end to end without the site.
func imagePondServer(t *testing.T, page string) (*Share, string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(page))
	}))
	t.Cleanup(srv.Close)
	return NewImagePond(testClient()), srv.URL
}

// End to end over the newer page shape: the item resolves to the site's own
// route, and is named by the metadata rather than by that route — whose last
// segment is the same word for every video on the site.
func TestImagePondExtractNamesTheDirectRoute(t *testing.T) {
	ip, base := imagePondServer(t, imagePondDirectVideoPage)
	res := extractOK(t, ip, base+"/i/abc123", Options{})
	if len(res.Files) != 1 {
		t.Fatalf("got %d files, want 1", len(res.Files))
	}
	if want := "https://www.imagepond.net/i/abc123/direct"; res.Files[0].URL != want {
		t.Errorf("url = %q, want %q", res.Files[0].URL, want)
	}
	if want := "Another Clip.mp4"; res.Files[0].Name != want {
		t.Errorf("name = %q, want the decoded og:title %q", res.Files[0].Name, want)
	}
	// The route is unsigned and does not expire, so there is nothing to
	// re-mint per attempt.
	if res.Files[0].Resolve != nil {
		t.Error("the /direct route was given a resolver; it carries no expiry to outrun")
	}
}

// Without an og:title there is nothing in the link to name the file after,
// and the route's own last segment must not become the filename.
func TestImagePondExtractDoesNotNameAFileDirect(t *testing.T) {
	page := `<html><head>
		<meta property="og:video" content="https://www.imagepond.net/i/abc123/direct" />
		</head><body></body></html>`
	ip, base := imagePondServer(t, page)
	res := extractOK(t, ip, base+"/i/a-clip-slug", Options{})
	if res.Files[0].Name == imagePondDirectSegment {
		t.Fatal("the file was named after the route rather than the item")
	}
	if want := "a-clip-slug"; res.Files[0].Name != want {
		t.Errorf("name = %q, want the item's own slug %q", res.Files[0].Name, want)
	}
}

// A page that names nothing usable is still reported as such rather than
// resolving to the poster frame or to a route guessed from the path.
func TestImagePondExtractReportsAPageWithNoMedia(t *testing.T) {
	page := `<html><head>
		<meta property="og:title" content="Gone.mp4" />
		<meta property="og:image" content="https://media.imagepond.net/media/videos/Gone_abc123_thumb.jpg" />
		</head><body></body></html>`
	ip, base := imagePondServer(t, page)
	u, _ := ParseURL(base + "/i/abc123")

	_, err := ip.Extract(context.Background(), u, Options{})
	if err == nil {
		t.Fatal("a page offering only the poster frame resolved to something")
	}
	if !strings.Contains(err.Error(), "no media") {
		t.Errorf("error = %v, want it to say the page names no media", err)
	}
}

func TestImagePondRecognise(t *testing.T) {
	i := NewImagePond(nil)
	head := `<meta property="og:site_name" content="ImagePond">`
	video := `<html><head>` + head + `<meta property="og:video" content="https://media.example.test/media/videos/First_Clip_ab12.mov">` +
		`<meta property="og:image" content="https://media.example.test/media/videos/First_Clip_ab12_thumb.jpg"></head><body></body></html>`
	direct := `<html><head>` + head + `<meta property="og:video" content="/i/abc123/direct"></head><body></body></html>`
	album := `<html><head>` + head + `</head><body><a :href="manageMode ? 'javascript:void(0)' : '/i/abc123'">First</a></body></html>`
	expectRecognised(t, i, htmlPage(t, "https://images.example.test/i/abc123", video), true)
	expectRecognised(t, i, htmlPage(t, "https://images.example.test/i/abc123", direct), true)
	expectRecognised(t, i, htmlPage(t, "https://images.example.test/a/album1", album), true)
	expectRecognised(t, i, htmlPage(t, "https://images.example.test/a/album1", imagePondAlbumPage), true)
	expectRecognised(t, i, htmlPage(t, "https://images.example.test/a/album1",
		`<html><head>`+head+`</head><body><a href="/i/abc123">First</a></body></html>`), true)
	// The form and the name, without the stored file or the cards.
	named := `<html><head>` + head + `<meta property="og:image" content="/logo.png"></head><body><a href="/browse">Browse</a></body></html>`
	expectRecognised(t, i, htmlPage(t, "https://images.example.test/i/abc123", named), false)
	expectRecognised(t, i, htmlPage(t, "https://images.example.test/a/album1", named), false)
	// The stored file under another name, or on a page of another form.
	expectRecognised(t, i, htmlPage(t, "https://images.example.test/i/abc123",
		`<html><head><meta property="og:video" content="/media/videos/First_Clip.mov"></head></html>`), false)
	expectRecognised(t, i, htmlPage(t, "https://images.example.test/trending", video), false)
	expectShape(t, i, "https://images.example.test/i/abc123", true)
	expectShape(t, i, "https://images.example.test/a/album1", true)
	expectShape(t, i, "https://images.example.test/i/abc123/direct", false)
	expectShape(t, i, "https://images.example.test/browse", false)
}

// An album listing shaped like the site's. The traps it holds are both real:
// the card link is an Alpine expression rather than an href, and the video
// card's thumbnail comes from the media host while the image cards' come
// from a /i/<code>/thumb route — so nothing can key on the thumbnail to find
// the item. The name sits in a <p> identified only by utility classes.
const imagePondAlbumPage = `<!DOCTYPE html><html><head>
<title>Holiday pictures - ImagePond</title>
</head><body>
<a href="https://www.imagepond.net/">home</a>
<img src="https://media.imagepond.net/media/images/site-logo_aaa000.png" alt="logo">
<h1>Holiday pictures</h1>
<div class="group block relative" :class="manageMode ? 'cursor-pointer' : ''">
  <a :href="manageMode ? 'javascript:void(0)' : 'https://www.imagepond.net/i/voCUUYAj'"
     @click="manageMode &amp;&amp; toggleImageSelection(2043960)" class="block">
    <div class="aspect-square">
      <img src="https://media.imagepond.net/media/videos/20260820_221249_TfNxuyl0_thumb.jpg" alt="">
    </div>
    <p class="mt-1.5 text-xs text-gray-400 truncate">20260820_221249.mp4</p>
  </a>
</div>
<div class="group block relative">
  <a :href="manageMode ? 'javascript:void(0)' : 'https://www.imagepond.net/i/TSAehpVs'" class="block">
    <div class="aspect-square">
      <img src="https://www.imagepond.net/i/TSAehpVs/thumb/300" alt="">
    </div>
    <p class="mt-1.5 text-xs text-gray-400 truncate">Screenshot_20260827_093756_Chrome.jpg</p>
  </a>
</div>
<div class="group block relative">
  <a :href="manageMode ? 'javascript:void(0)' : 'https://www.imagepond.net/i/TSAehpVs'" class="block">
    <p class="truncate">Screenshot_20260827_093756_Chrome.jpg</p>
  </a>
</div>
<a href="https://www.imagepond.net/a/OtherAlbum">another album</a>
<a href="https://www.imagepond.net/i/TSAehpVs/direct">download</a>
</body></html>`

func TestImagePondAlbumListsEveryItemOnce(t *testing.T) {
	base := mustParse(t, "https://www.imagepond.net/a/AXxfH2Fp")
	root := mustParseDoc(t, imagePondAlbumPage)

	files := imagePondAlbumFiles(root, base, func(page, code string) func(context.Context) (*Target, error) {
		return func(context.Context) (*Target, error) { return &Target{URL: page + "#" + code}, nil }
	})

	if len(files) != 2 {
		t.Fatalf("got %d files, want 2 — the repeated card is one item, and the "+
			"album link, the /direct link and the site furniture are none", len(files))
	}
	// Names come from the card, so the queue shows them before anything resolves.
	if files[0].Name != "20260820_221249.mp4" {
		t.Errorf("first name = %q", files[0].Name)
	}
	if files[1].Name != "Screenshot_20260827_093756_Chrome.jpg" {
		t.Errorf("second name = %q", files[1].Name)
	}
	// No URL up front: the media address is a fetch further on.
	for _, f := range files {
		if f.URL != "" {
			t.Errorf("%s carries a URL before resolving: %q", f.Name, f.URL)
		}
		if f.Resolve == nil {
			t.Errorf("%s has no resolver", f.Name)
		}
	}
	// And the resolver is pointed at the item page, not the album.
	target, err := files[0].Resolve(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if target.URL != "https://www.imagepond.net/i/voCUUYAj#voCUUYAj" {
		t.Errorf("resolver aimed at %q", target.URL)
	}
}

func TestImagePondCardLinkReadsTheAlpineExpression(t *testing.T) {
	base := mustParse(t, "https://www.imagepond.net/a/AXxfH2Fp")
	cases := []struct {
		name, markup, want string
	}{
		{
			"the ternary's item operand wins over the javascript one",
			`<a :href="manageMode ? 'javascript:void(0)' : 'https://www.imagepond.net/i/abc123'">x</a>`,
			"https://www.imagepond.net/i/abc123",
		},
		{
			"a plain href still works",
			`<a href="https://www.imagepond.net/i/abc123">x</a>`,
			"https://www.imagepond.net/i/abc123",
		},
		{
			"a relative operand resolves against the album",
			`<a :href="m ? 'javascript:void(0)' : '/i/abc123'">x</a>`,
			"https://www.imagepond.net/i/abc123",
		},
		{
			"the download route is not an item page",
			`<a href="https://www.imagepond.net/i/abc123/direct">x</a>`,
			"",
		},
		{
			"another album is not an item",
			`<a href="https://www.imagepond.net/a/Other">x</a>`,
			"",
		},
		{
			"a foreign host is not an item however shaped",
			`<a href="https://elsewhere.test/i/abc123">x</a>`,
			"",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := mustParseDoc(t, "<html><body>"+tc.markup+"</body></html>")
			a := findFirst(root, func(n *html.Node) bool { return isElem(n, atom.A) })
			if a == nil {
				t.Fatal("no anchor in the fixture")
			}
			if got := imagePondCardLink(a, base, imagePondItemPage); got != tc.want {
				t.Errorf("imagePondCardLink = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestImagePondAlbumEndToEnd(t *testing.T) {
	var hits int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		switch {
		case strings.HasPrefix(r.URL.Path, "/a/"):
			_, _ = io.WriteString(w, imagePondAlbumPage)
		case strings.HasPrefix(r.URL.Path, "/i/"):
			_, _ = io.WriteString(w, imagePondVideoPage)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	// The fixture names the real host, so the album is fetched from the test
	// server while its cards still point at imagepond.net; only the listing
	// hop is under test here.
	ip := NewImagePond(testClient())
	res := extractOK(t, ip, server.URL+"/a/AXxfH2Fp", Options{})
	if res.Title != "Holiday pictures" {
		t.Errorf("title = %q, want the album's h1", res.Title)
	}
	if len(res.Files) != 2 {
		t.Fatalf("got %d files, want 2", len(res.Files))
	}
	// One fetch for the whole listing: the per-item hops are deferred.
	if hits != 1 {
		t.Errorf("album cost %d fetches, want 1 — items resolve at download time", hits)
	}
}

func TestImagePondRejectsAProfilePage(t *testing.T) {
	ip := NewImagePond(testClient())
	_, err := extract(t, ip, "https://www.imagepond.net/someone", Options{})
	if err == nil {
		t.Fatal("a profile page resolved")
	}
	if !strings.Contains(err.Error(), "/a/<code>") {
		t.Errorf("the refusal should name the album route it now accepts: %v", err)
	}
}

// A link straight to the media host is the file itself. Match claims that
// host along with the site — one is a subdomain of the other — so if this
// route did not exist nothing else would handle it, and the URL would be
// refused for not being a page.
func TestImagePondTakesAMediaHostLinkAsTheFile(t *testing.T) {
	ip := NewImagePond(testClient())
	raw := "https://media.imagepond.net/media/videos/SomeClip_abc123.mp4"

	res := extractOK(t, ip, raw, Options{})
	if len(res.Files) != 1 {
		t.Fatalf("got %d files, want 1", len(res.Files))
	}
	f := res.Files[0]
	if f.URL != raw {
		t.Errorf("url = %q, want the link as given", f.URL)
	}
	if f.Name != "SomeClip_abc123.mp4" {
		t.Errorf("name = %q, want the filename from the path", f.Name)
	}
	// No page to read, so nothing to resolve later.
	if f.Resolve != nil {
		t.Error("a direct file link should need no resolver")
	}
	if f.Headers[httpx.HeaderReferer] == "" {
		t.Error("the referer every other file here carries is missing")
	}
}

func TestImagePondMediaFile(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{
			"a stored video",
			"https://media.imagepond.net/media/videos/clip_abc123.mp4",
			"https://media.imagepond.net/media/videos/clip_abc123.mp4",
		},
		{
			// Asked for by name rather than found on a page, so it is what
			// was wanted — unlike imagePondHosted, which must skip these.
			"a thumbnail asked for by name",
			"https://media.imagepond.net/media/videos/clip_abc123_thumb.jpg",
			"https://media.imagepond.net/media/videos/clip_abc123_thumb.jpg",
		},
		{"the site host is not the media host", "https://www.imagepond.net/i/abc123", ""},
		{"no filename to save under", "https://media.imagepond.net/", ""},
		{"a foreign host", "https://media.elsewhere.test/media/videos/x.mp4", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := imagePondMediaFile(mustParse(t, tc.in)); got != tc.want {
				t.Errorf("imagePondMediaFile(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
