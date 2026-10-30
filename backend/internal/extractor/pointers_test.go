// SPDX-License-Identifier: MIT

package extractor

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/JohanLindvall/HeapLeach/internal/config"
)

// pointerNamed is the pointer site of that name, with no client or registry
// behind it.
func pointerNamed(t *testing.T, name string) *Pointer {
	t.Helper()
	p := NewPointer(nil, name)
	if p == nil {
		t.Fatalf("no pointer site %s", name)
	}
	return p
}

// TestPointerSitesAreUsable guards the table: every field ends up in front
// of the user or decides what is downloaded, and every site is registered
// and handed the registry it resolves through.
func TestPointerSitesAreUsable(t *testing.T) {
	reg := NewRegistry(&config.Config{}, nil)
	registered := map[string]bool{}
	for _, e := range reg.extractors {
		if p, ok := e.(*Pointer); ok {
			registered[p.Name()] = p.registry == reg
		}
	}
	seen := map[string]bool{}
	for _, site := range pointerSites {
		if site.name == "" || site.shape == nil || site.recognise == nil || site.recognises == "" ||
			site.links == nil || site.missing == "" {
			t.Errorf("%q is missing a field", site.name)
		}
		if seen[site.name] {
			t.Errorf("two pointer sites are both named %q", site.name)
		}
		seen[site.name] = true
		if !registered[site.name] {
			t.Errorf("%s is not registered, or not handed its registry", site.name)
		}
	}
}

// A preview page in the shape the search serves: a viewer, and beside it the
// link naming the site the video actually lives on.
const yandexPreviewPage = `<html><head><title>Video search</title></head><body>
<div class="VideoViewer-Details">
  <div class="VideoViewer-Source VideoViewer-DetailsSource">
    <a class="Link VideoViewer-SourceIconLink" target="_blank"
       href="https://tube.example.test/video/synthetic-id/">source</a>
    <a class="Link VideoViewer-SourceLink" href="https://tube.example.test/video/synthetic-id/">again</a>
  </div>
  <a class="Link" href="https://yandex.kz/support/video/">help</a>
</div>
</body></html>`

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

// TestPointerLinks reads each site's links off a page in its shape: the
// video's home first, the site's own furniture never.
func TestPointerLinks(t *testing.T) {
	for _, tc := range []struct {
		site, page, doc string
		want            []string
	}{
		{
			site: "yandex", page: "https://yandex.kz/video/preview/1234567890", doc: yandexPreviewPage,
			want: []string{"https://tube.example.test/video/synthetic-id/"},
		},
		{
			site: "yandex", page: "https://yandex.kz/video/preview/1234567890",
			doc: `<html><body><p>Nothing here</p></body></html>`,
		},
		{
			// The player is the frame offering fullscreen; an advert has no
			// use for it. Both are off-site, so nothing else separates them.
			site: "alohatube", page: "https://www.alohatube.com/ktm/view.cgi?gid=1234567&u=https://wrap.example.test/v/AAAA",
			doc:  alohaViewPage,
			want: []string{"https://tube.example.test/embed/1234567", "https://ads.example.test/300x250.html"},
		},
		{
			site: "alohatube", page: "https://www.alohatube.com/ktm/view.cgi?gid=1234567",
			doc: `<html><body><p>This video has been removed.</p></body></html>`,
		},
	} {
		root, err := parseHTML(tc.doc)
		if err != nil {
			t.Fatal(err)
		}
		page, err := ParseURL(tc.page)
		if err != nil {
			t.Fatal(err)
		}
		if got := pointerNamed(t, tc.site).site.links(root, page); !slices.Equal(got, tc.want) {
			t.Errorf("%s links = %v, want %v", tc.site, got, tc.want)
		}
	}
}

// TestPointerOffsite guards the one thing separating the video's real home
// from each site's own furniture, which is linked exactly the same way.
func TestPointerOffsite(t *testing.T) {
	for name, tc := range map[string]struct {
		offsite func(string) string
		cases   map[string]string
	}{
		"yandex": {yandexOffsite, map[string]string{
			"https://tube.example.test/video/1/": "https://tube.example.test/video/1/",
			"http://tube.example.test/video/1/":  "http://tube.example.test/video/1/",
			"https://yandex.kz/support/video/":   "",
			"https://yandex.ru/video/":           "",
			"https://an.yandex.ru/count/x":       "",
			"https://yastatic.net/s/x.js":        "",
			"/video/preview/123":                 "",
			"javascript:void(0)":                 "",
		}},
		"alohatube": {alohaOffsite, map[string]string{
			"https://tube.example.test/embed/1":      "https://tube.example.test/embed/1",
			"http://tube.example.test/embed/1":       "http://tube.example.test/embed/1",
			"https://www.alohatube.com/promo/f.html": "",
			"https://alohatube.com/promo/f.html":     "",
			"/menu/categories.html":                  "",
			"javascript:void(0)":                     "",
			"":                                       "",
		}},
	} {
		for link, want := range tc.cases {
			if got := tc.offsite(link); got != want {
				t.Errorf("%s offsite(%q) = %q, want %q", name, link, got, want)
			}
		}
	}
}

func TestAlohaTubeRecognise(t *testing.T) {
	a := pointerNamed(t, "alohatube")
	const view = "https://index.example.test/ktm/view.cgi?gid=1234567&u=https://wrap.example.test/v/AAAA"
	expectRecognised(t, a, htmlPage(t, view, alohaViewPage), true)
	// The right form and the name, but no player frame.
	expectRecognised(t, a, htmlPage(t, view,
		`<html><head><title>Aloha Tube</title></head><body><p>This video has been removed.</p></body></html>`), false)
	// A player frame under the name, on a page of another form.
	expectRecognised(t, a, htmlPage(t, "https://index.example.test/amateur.html", alohaViewPage), false)
	expectShape(t, a, view, true)
	expectShape(t, a, "https://index.example.test/ktm/view.cgi", false)
	expectShape(t, a, "https://index.example.test/top/amateur", false)
}

// The link is resolved by the extractor that recognises it, which for the
// host this was written against is the only thing that works — the external
// downloader fails on precisely that link. An advert nobody recognises is
// passed over on the way, wherever it sits.
func TestPointerResolvesThroughTheRegistry(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`<html><head><title>Aloha Tube</title></head><body>
<iframe src="https://ads.example.test/300x250.html" allowfullscreen></iframe>
<iframe src="https://tube.example.test/embed/1234567" allowfullscreen></iframe>
</body></html>`))
	}))
	t.Cleanup(srv.Close)

	client := testClient()
	reg := &Registry{fallback: NewDirect(nil)}
	reg.extractors = []Extractor{stubExtractor{hostClaim: "tube.example.test", title: "a synthetic clip"}}
	p := &Pointer{client: client, registry: reg, site: pointerNamed(t, "alohatube").site}

	u, err := url.Parse(srv.URL + "/ktm/view.cgi?gid=1234567")
	if err != nil {
		t.Fatal(err)
	}
	res, err := p.Extract(t.Context(), u, Options{})
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if res.Title != "a synthetic clip" {
		t.Errorf("title %q, want the frame host's own", res.Title)
	}
}

// A page pointing nowhere says so in the site's name.
func TestPointerReportsAPageThatPointsNowhere(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`<html><body><p>Nothing here</p></body></html>`))
	}))
	t.Cleanup(srv.Close)

	p := &Pointer{client: testClient(), site: pointerNamed(t, "yandex").site}
	u, err := url.Parse(srv.URL + "/video/preview/1234567890")
	if err != nil {
		t.Fatal(err)
	}
	_, err = p.Extract(t.Context(), u, Options{})
	if err == nil || !strings.HasPrefix(err.Error(), "yandex: ") || !strings.Contains(err.Error(), "names no source video") {
		t.Fatalf("error = %v, want the site's own report of a preview naming no source", err)
	}
}
