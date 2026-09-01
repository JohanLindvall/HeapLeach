package extractor

import (
	"context"
	"net/url"
	"testing"
)

// A view page in the shape the index serves: the player frame, the site's
// own furniture framed the same way, and the advertising that sits beside
// it. Only the first is the video.
const alohaViewPage = `<html><head><title>Aloha Tube</title></head><body>
<div id="play-div">
  <iframe src="https://tube.example.test/embed/1234567" frameborder=0 scrolling=no
          id="frametube" allowfullscreen
          sandbox="allow-forms allow-same-origin allow-scripts"></iframe>
</div>
<iframe width="300" height="268" src="https://ads.example.test/300x250.html"
        sandbox="allow-same-origin allow-scripts"></iframe>
<iframe src="/menu/categories.html"></iframe>
<iframe src="https://www.alohatube.com/promo/frame.html"></iframe>
</body></html>`

func alohaPage(t *testing.T) *url.URL {
	t.Helper()
	u, err := ParseURL("https://www.alohatube.com/ktm/view.cgi?gid=1234567&u=https://wrap.example.test/v/AAAA")
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// The player is the frame offering fullscreen; an advert has no use for it.
// Both are off-site, so nothing else on the page separates them.
func TestAlohaFramesPutThePlayerFirst(t *testing.T) {
	got := alohaFrames(alohaViewPage, alohaPage(t))
	if len(got) != 2 {
		t.Fatalf("read %d frames, want 2: %v", len(got), got)
	}
	if want := "https://tube.example.test/embed/1234567"; got[0] != want {
		t.Errorf("first frame is %q, want the player %q", got[0], want)
	}
	if want := "https://ads.example.test/300x250.html"; got[1] != want {
		t.Errorf("second frame is %q, want the advert %q", got[1], want)
	}
}

func TestAlohaFramesOnAPageWithNoPlayer(t *testing.T) {
	if got := alohaFrames(`<html><body><p>This video has been removed.</p></body></html>`,
		alohaPage(t)); len(got) != 0 {
		t.Errorf("found %d frames on a page with no player: %v", len(got), got)
	}
}

// TestAlohaOffsite guards the one thing separating the video from the
// index's own furniture, which is framed exactly the same way.
func TestAlohaOffsite(t *testing.T) {
	tests := map[string]string{
		"https://tube.example.test/embed/1":      "https://tube.example.test/embed/1",
		"http://tube.example.test/embed/1":       "http://tube.example.test/embed/1",
		"https://www.alohatube.com/promo/f.html": "",
		"https://alohatube.com/promo/f.html":     "",
		"/menu/categories.html":                  "",
		"javascript:void(0)":                     "",
		"":                                       "",
	}
	for link, want := range tests {
		if got := alohaOffsite(link); got != want {
			t.Errorf("alohaOffsite(%q) = %q, want %q", link, got, want)
		}
	}
}

func TestAlohaTubeMatch(t *testing.T) {
	a := NewAlohaTube(nil, nil)
	for _, host := range []string{"alohatube.com", "www.alohatube.com", "m.alohatube.com"} {
		u := &url.URL{Scheme: "https", Host: host, Path: "/ktm/view.cgi"}
		if !a.Match(u) {
			t.Errorf("%s was not matched", host)
		}
	}
	if a.Match(&url.URL{Scheme: "https", Host: "notalohatube.com", Path: "/ktm/view.cgi"}) {
		t.Error("notalohatube.com was matched")
	}
}

// The frame is resolved by the extractor that claims its host, which for the
// host this was written against is the only thing that works — the external
// downloader fails on precisely that link.
func TestAlohaTubeResolvesAKnownFrameThroughTheRegistry(t *testing.T) {
	frame := "https://tube.example.test/embed/1234567"
	parsed, err := url.Parse(frame)
	if err != nil {
		t.Fatal(err)
	}
	reg := &Registry{fallback: NewDirect(nil)}
	reg.extractors = []Extractor{stubExtractor{host: "tube.example.test", title: "a synthetic clip"}}

	if ex, ok := reg.Known(parsed); !ok {
		t.Fatal("the stub host was not claimed")
	} else if got, err := ex.Extract(t.Context(), parsed, Options{}); err != nil {
		t.Fatalf("extract: %v", err)
	} else if got.Title != "a synthetic clip" {
		t.Errorf("title %q, want the frame host's own", got.Title)
	}
}

// stubExtractor stands in for whichever host a frame points at.
type stubExtractor struct{ host, title string }

func (s stubExtractor) Name() string { return "stub" }

func (s stubExtractor) Match(u *url.URL) bool { return u.Hostname() == s.host }

func (s stubExtractor) Extract(_ context.Context, u *url.URL, _ Options) (*Result, error) {
	return &Result{Title: s.title, Files: []File{{Name: s.title + ".mp4", URL: u.String(), Size: -1}}}, nil
}
