// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/JohanLindvall/HeapLeach/internal/config"
)

// These run through the registry, as a submitted URL is, rather than
// calling one extractor's Extract: what is under test is that a page of a
// site's own — a video whose player is gone, a search that found nothing, a
// folder holding only folders — reaches that site's extractor and its
// words, instead of the fallback downloading the related videos, the
// recommendations or the HTML beside it.

// statusSite serves each path's body with its status (200 when zero), and
// records every request.
type statusSite struct {
	mu    sync.Mutex
	paths []string
}

type statusPage struct {
	status int
	body   string
}

func newStatusSite(t *testing.T, pages map[string]statusPage) (*httptest.Server, *statusSite) {
	t.Helper()
	s := &statusSite{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.paths = append(s.paths, r.URL.RequestURI())
		s.mu.Unlock()
		page, ok := pages[r.URL.RequestURI()]
		if !ok {
			page, ok = pages[r.URL.Path]
		}
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(max(page.status, http.StatusOK))
		fmt.Fprint(w, page.body)
	}))
	t.Cleanup(srv.Close)
	return srv, s
}

func (s *statusSite) asked() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.paths)
}

// askedAny reports whether any request's path starts with prefix.
func (s *statusSite) askedAny(prefix string) bool {
	for _, p := range s.asked() {
		if strings.HasPrefix(p, prefix) {
			return true
		}
	}
	return false
}

func registryExtract(t *testing.T, raw string) (*Result, string, error) {
	t.Helper()
	reg := NewRegistry(&config.Config{}, testClient())
	res, ex, err := reg.Extract(context.Background(), raw, Options{})
	name := ""
	if ex != nil {
		name = ex.Name()
	}
	return res, name, err
}

// kvsRelatedOnly is a KVS video page whose player has been taken down: the
// related block's items list, named the platform's way, but no container
// around it, no player, and no listing controls.
const kvsRelatedOnly = `<html><head><title>A Clip - Example Tube</title></head><body>
<div class="message">This video is unavailable.</div>
<div class="related"><div id="list_videos_related_videos_items">
<a href="/videos/2/second-clip/">Second</a><a href="/videos/3/third-clip/">Third</a>
</div></div></body></html>`

func TestRegistryKVSVideoWithoutPlayerReportsItsError(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusGone} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			srv, site := newStatusSite(t, map[string]statusPage{
				"/videos/1/a-clip/":      {status: status, body: kvsRelatedOnly},
				"/videos/2/second-clip/": {body: kvsPage()},
				"/videos/3/third-clip/":  {body: kvsPage()},
			})
			res, ex, err := registryExtract(t, srv.URL+"/videos/1/a-clip/")
			if err == nil || ex != kvsLabel {
				t.Fatalf("extractor %q, result %+v, error %v; want the install's own error", ex, res, err)
			}
			if site.askedAny("/videos/2/") || site.askedAny("/videos/3/") {
				t.Fatalf("related videos were fetched: %v", site.asked())
			}
		})
	}
}

func TestRegistryKVSFrontPageIsRefused(t *testing.T) {
	front := `<html><body><div id="list_videos_most_recent_videos"><a data-action="ajax"
	data-block-id="list_videos_most_recent_videos" data-parameters="sort_by:post_date">Latest</a>
	<div id="list_videos_most_recent_videos_items"><a href="/videos/2/second-clip/">Second</a></div></div></body></html>`
	srv, site := newStatusSite(t, map[string]statusPage{"/": {body: front}, "/videos/2/second-clip/": {body: kvsPage()}})
	_, ex, err := registryExtract(t, srv.URL+"/")
	if ex != kvsLabel || err == nil || !strings.Contains(err.Error(), "front page") {
		t.Fatalf("extractor %q, error %v; want the front page refused", ex, err)
	}
	if site.askedAny("/videos/") {
		t.Fatalf("promoted videos were fetched: %v", site.asked())
	}
}

const noodleSearchScript = `<script defer src="/static/js/search.min.abc.js"></script>`

func TestRegistryNoodleEmptySearchIsNotRecommendations(t *testing.T) {
	empty := `<html><head><title>First query - found videos</title>` + noodleSearchScript + `</head><body>
	<span data-i18n="nothing_found">Nothing Found</span><h2>Watching now</h2><div class="list_videos">` +
		noodleCard("/watch/-9_9", "Unrelated") + noodleCard("/watch/-8_8", "Also unrelated") + `</div></body></html>`
	srv, site := newStatusSite(t, map[string]statusPage{"/video/first+query": {body: empty}})
	_, ex, err := registryExtract(t, srv.URL+"/video/first+query")
	if ex != "noodlemagazine" || err == nil || !strings.Contains(err.Error(), "no videos found") {
		t.Fatalf("extractor %q, error %v; want the empty search reported", ex, err)
	}
	if site.askedAny("/watch/") {
		t.Fatalf("recommendations were fetched: %v", site.asked())
	}
}

func TestRegistryNoodleWatchPageWithoutPlayer(t *testing.T) {
	watch := head("First watch online", "og:site_name=noodlemagazine") +
		`<script src="/static/js/watch.min.abc.js"></script><div class="list_videos">` +
		noodleCard("/watch/-9_9", "Unrelated") + `</div></body></html>`
	srv, site := newStatusSite(t, map[string]statusPage{"/watch/-123_456": {body: watch}})
	_, ex, err := registryExtract(t, srv.URL+"/watch/-123_456")
	if ex != "noodlemagazine" || err == nil || !strings.Contains(err.Error(), "no player") {
		t.Fatalf("extractor %q, error %v; want the missing player reported", ex, err)
	}
	if site.askedAny("/watch/-9_9") {
		t.Fatalf("related videos were fetched: %v", site.asked())
	}
}

func TestRegistryKnuthingWatchPageWithoutPlayer(t *testing.T) {
	watch := `<html><head><title>First</title><script src="/themes/knuthing/js/jquery.js"></script></head><body>
	<div class="videos-latest-list"><a href="/watch/other-clip.html">Other</a></div></body></html>`
	srv, site := newStatusSite(t, map[string]statusPage{"/watch/first-clip.html": {body: watch}})
	_, ex, err := registryExtract(t, srv.URL+"/watch/first-clip.html")
	if ex != "knuthing" || err == nil || !strings.Contains(err.Error(), "restricted or removed") {
		t.Fatalf("extractor %q, error %v; want the missing player reported", ex, err)
	}
	if site.askedAny("/watch/other-clip.html") {
		t.Fatalf("related videos were fetched: %v", site.asked())
	}
}

// filesterSubfolder is a subfolder card as a folder page renders it.
func filesterSubfolder(id, name string) string {
	return fmt.Sprintf(`<a class="subfolder-item" href="/f/%s"><span class="subfolder-name">%s</span></a>`, id, name)
}

func TestRegistryFilesterFoldersOfEveryKind(t *testing.T) {
	// Only subfolders at the top, and one level further down.
	onlyFolders := `<h1>Top</h1><div id="subfoldersGrid">` + filesterSubfolder("season-a", "Season A") +
		filesterSubfolder("season-b", "Season B") + `</div>`
	seasonA := `<h1>Season A</h1><div id="filesGrid">` + filesterTestCard("a1", "First Clip.mp4", "100") + `</div>`
	seasonB := `<h1>Season B</h1><div id="subfoldersGrid">` + filesterSubfolder("disc-1", "Disc 1") + `</div>`
	disc1 := `<h1>Disc 1</h1><div id="filesGrid">` + filesterTestCard("g1", "Second Clip.mp4", "200") + `</div>`
	// Cards linked by href rather than by a script.
	hrefCards := `<h1>Linked</h1><div id="filesGrid"><a class="file-item" href="/d/h1" data-name="Third Clip.mp4" data-size="300">Third</a></div>`
	empty := `<h1>Nothing</h1><div id="filesGrid"></div>`
	srv, _ := newStatusSite(t, map[string]statusPage{
		"/f/top": {body: onlyFolders}, "/f/season-a": {body: seasonA}, "/f/season-b": {body: seasonB},
		"/f/disc-1": {body: disc1}, "/f/linked": {body: hrefCards}, "/f/nothing": {body: empty},
	})

	res, ex, err := registryExtract(t, srv.URL+"/f/top")
	if err != nil || ex != "filester" {
		t.Fatalf("subfolders only: extractor %q, error %v", ex, err)
	}
	var got []string
	for _, f := range res.Files {
		got = append(got, f.Dir+"/"+f.Name)
	}
	if want := []string{"Season A/First Clip.mp4", "Season B/Disc 1/Second Clip.mp4"}; !slices.Equal(got, want) {
		t.Fatalf("subfolders only: files %v, want %v", got, want)
	}

	res, ex, err = registryExtract(t, srv.URL+"/f/linked")
	if err != nil || ex != "filester" || len(res.Files) != 1 || res.Files[0].Name != "Third Clip.mp4" || res.Files[0].Resolve == nil {
		t.Fatalf("href cards: extractor %q, result %+v, error %v", ex, res, err)
	}

	if _, ex, err = registryExtract(t, srv.URL+"/f/nothing"); ex != "filester" || err == nil || !strings.Contains(err.Error(), "no files") {
		t.Fatalf("empty folder: extractor %q, error %v; want it reported empty", ex, err)
	}
}

func TestFilesterFileShapeKeepsTrailingSegments(t *testing.T) {
	f := NewFilester(nil)
	expectShape(t, f, "https://share.example.test/d/abc123/First%20Clip.mp4", true)
	expectShape(t, f, "https://share.example.test/d/abc123/a/b/", true)
	page := `<html><head><script src="/js/file_dl.js"></script></head><body><button id="downloadButton">Download</button></body></html>`
	expectRecognised(t, f, htmlPage(t, "https://share.example.test/d/abc123/a-name", page), true)
}

const balbumsForm = `<form id="searchForm"><input name="search"></form>`

func TestRegistryBalbumsFrontPageAndOnePageSearch(t *testing.T) {
	front := `<html><body>` + balbumsForm + `<p>Page 1 of 20000</p><a href="/a/first-album">First</a></body></html>`
	// A search of one page that states no page count.
	onePage := `<html><body>` + balbumsForm + `<a href="/a/first-album">First</a></body></html>`
	srv, site := newStatusSite(t, map[string]statusPage{"/": {body: front}})

	if _, ex, err := registryExtract(t, srv.URL+"/"); ex != "balbums" || err == nil || !strings.Contains(err.Error(), "names no search") {
		t.Fatalf("front page: extractor %q, error %v; want it refused", ex, err)
	}
	if site.askedAny("/a/") {
		t.Fatalf("front page albums were followed: %v", site.asked())
	}

	b := NewBalbums(nil, nil)
	expectRecognised(t, b, htmlPage(t, "https://index.example.test/?search=first", onePage), true)
	// Neither the form without a search, nor a page count without the form.
	expectRecognised(t, b, htmlPage(t, "https://index.example.test/", onePage), false)
	expectRecognised(t, b, htmlPage(t, "https://index.example.test/?search=first", `<p>Page 1 of 3</p>`), false)
}
