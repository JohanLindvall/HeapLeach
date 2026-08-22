package extractor

import (
	"net/url"
	"testing"
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
