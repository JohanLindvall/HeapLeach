package extractor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/httpx"
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
	root, err := parseHTML(imagePondDirectVideoPage)
	if err != nil {
		t.Fatal(err)
	}

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
	root, err := parseHTML(`<html><head>
		<meta property="og:video" content="https://www.imagepond.net/i/abc123/direct" />
		</head><body>
		<video data-src="https://media.imagepond.net/media/videos/SomeClipName_abc123.mov"></video>
		</body></html>`)
	if err != nil {
		t.Fatal(err)
	}

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
	root, err := parseHTML(imagePondImagePage)
	if err != nil {
		t.Fatal(err)
	}

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
	root, err := parseHTML(imagePondVideoPage)
	if err != nil {
		t.Fatal(err)
	}

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
	root, err := parseHTML(imagePondImagePage)
	if err != nil {
		t.Fatal(err)
	}

	got := imagePondMediaLink(root, base)
	want := "https://media.imagepond.net/media/images/AStillPicture_xyz789.jpg"
	if got != want {
		t.Errorf("media link = %q, want the item rather than the site's logo (%q)", got, want)
	}
}

func TestImagePondMediaLinkIgnoresEverythingElse(t *testing.T) {
	base, _ := url.Parse("https://www.imagepond.net/i/abc123")
	root, err := parseHTML(`<html><body>
		<img src="https://ads.example.test/banner.jpg">
		<video data-src="https://ads.example.test/preroll.mp4"></video>
	</body></html>`)
	if err != nil {
		t.Fatal(err)
	}
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
func imagePondServer(t *testing.T, page string) (*ImagePond, string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(page))
	}))
	t.Cleanup(srv.Close)
	return NewImagePond(httpx.New("test-agent", "en-US", 0, 5*time.Second)), srv.URL
}

// End to end over the newer page shape: the item resolves to the site's own
// route, and is named by the metadata rather than by that route — whose last
// segment is the same word for every video on the site.
func TestImagePondExtractNamesTheDirectRoute(t *testing.T) {
	ip, base := imagePondServer(t, imagePondDirectVideoPage)
	u, _ := ParseURL(base + "/i/abc123")

	res, err := ip.Extract(context.Background(), u, Options{})
	if err != nil {
		t.Fatal(err)
	}
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
	u, _ := ParseURL(base + "/i/a-clip-slug")

	res, err := ip.Extract(context.Background(), u, Options{})
	if err != nil {
		t.Fatal(err)
	}
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

func TestImagePondMatch(t *testing.T) {
	i := NewImagePond(nil)
	for _, raw := range []string{"https://www.imagepond.net/i/abc123", "https://imagepond.net/i/abc123"} {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		if !i.Match(u) {
			t.Errorf("Match(%q) = false", raw)
		}
	}
	other, _ := url.Parse("https://example.test/i/abc123")
	if i.Match(other) {
		t.Error("matched an unrelated host")
	}
}
