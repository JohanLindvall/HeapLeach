package extractor

import (
	"net/url"
	"strings"
	"testing"
)

// A preview page in the shape the search serves: a viewer, and beside it the
// link naming the site the video actually lives on.
const yandexPreviewPage = `<html><head><title>Yandex video search</title></head><body>
<div class="VideoViewer-Details">
  <div class="VideoViewer-Source VideoViewer-DetailsSource">
    <a class="Link VideoViewer-SourceIconLink" target="_blank"
       href="https://tube.example.test/video/synthetic-id/">source</a>
  </div>
  <a class="Link" href="https://yandex.kz/support/video/">help</a>
</div>
</body></html>`

func TestYandexSource(t *testing.T) {
	u, err := ParseURL("https://yandex.kz/video/preview/1234567890")
	if err != nil {
		t.Fatal(err)
	}
	got, err := yandexSource(yandexPreviewPage, u)
	if err != nil {
		t.Fatalf("yandexSource: %v", err)
	}
	if want := "https://tube.example.test/video/synthetic-id/"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestYandexSourceReportsAPageWithNoSource(t *testing.T) {
	u, err := ParseURL("https://yandex.kz/video/preview/1234567890")
	if err != nil {
		t.Fatal(err)
	}
	_, err = yandexSource(`<html><body><p>Nothing here</p></body></html>`, u)
	if err == nil {
		t.Fatal("a preview naming no source was accepted")
	}
	if !strings.Contains(err.Error(), "yandex") {
		t.Errorf("error %q does not name the host", err)
	}
}

// TestYandexOffsite guards the one thing that separates the video's real home
// from the viewer's own furniture, which is linked exactly the same way.
func TestYandexOffsite(t *testing.T) {
	tests := map[string]string{
		"https://tube.example.test/video/1/": "https://tube.example.test/video/1/",
		"http://tube.example.test/video/1/":  "http://tube.example.test/video/1/",
		"https://yandex.kz/support/video/":   "",
		"https://yandex.ru/video/":           "",
		"https://an.yandex.ru/count/x":       "",
		"https://yastatic.net/s/x.js":        "",
		"/video/preview/123":                 "",
		"javascript:void(0)":                 "",
	}
	for link, want := range tests {
		if got := yandexOffsite(link); got != want {
			t.Errorf("yandexOffsite(%q) = %q, want %q", link, got, want)
		}
	}
}

// TestYandexMatch covers the country domains the same search runs on, and
// keeps the extractor off the rest of the site.
func TestYandexMatch(t *testing.T) {
	y := NewYandex(nil)
	for _, host := range []string{"yandex.kz", "yandex.ru", "yandex.com", "www.yandex.by"} {
		u := &url.URL{Scheme: "https", Host: host, Path: "/video/preview/1"}
		if !y.Match(u) {
			t.Errorf("%s was not matched", host)
		}
	}
	for _, u := range []*url.URL{
		{Scheme: "https", Host: "yandex.ru", Path: "/search/"},
		{Scheme: "https", Host: "yandex.ru", Path: "/"},
		{Scheme: "https", Host: "notyandex.com", Path: "/video/preview/1"},
	} {
		if y.Match(u) {
			t.Errorf("%s%s was matched", u.Host, u.Path)
		}
	}
}
