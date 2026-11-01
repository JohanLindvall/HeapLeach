// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

// A Moebooru post is linked with its tags after the id as a slug. The
// registry takes that form to the boards, and the first page of the API
// answer recognition asked for is the one the extraction reads, not asked
// for again.
func TestBooruRegistryReadsAMoeburuPostWithASlug(t *testing.T) {
	var api atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/post.json":
			api.Add(1)
			w.Header().Set(httpx.HeaderContentType, httpx.ContentTypeJSON)
			if r.URL.Query().Get("tags") == "id:123" {
				_, _ = io.WriteString(w, `[{"id":123,"file_url":"/data/first-picture.jpg","file_size":5}]`)
				return
			}
			_, _ = io.WriteString(w, `[]`)
		case "/post/show/123/a-tag-another-tag":
			w.Header().Set(httpx.HeaderContentType, "text/html")
			_, _ = io.WriteString(w, `<html><head><title>A Board</title></head><body>`+
				`<script>Post.register({"id":123})</script></body></html>`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	expectShape(t, NewBooru(nil), "https://board.example.test/post/show/123/a-tag-slug", true)
	res, ex, err := NewRegistry(&config.Config{}, testClient()).
		Extract(t.Context(), srv.URL+"/post/show/123/a-tag-another-tag", Options{})
	if err != nil {
		t.Fatal(err)
	}
	if ex.Name() != "booru" || len(res.Files) != 1 || res.Files[0].URL != srv.URL+"/data/first-picture.jpg" {
		t.Fatalf("got %s with %+v, want the board's one post", ex.Name(), res.Files)
	}
	if n := api.Load(); n != 1 {
		t.Errorf("the API was asked %d times, want once between recognition and extraction", n)
	}
}

// philomenaServer is a philomena board whose default filter hides every
// image, and whose system "everything" filter shows them.
func philomenaServer(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var filters atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/json/filters/system":
			filters.Add(1)
			w.Header().Set(httpx.HeaderContentType, httpx.ContentTypeJSON)
			_, _ = io.WriteString(w, `{"filters":[{"id":7,"name":"Default"},{"id":99,"name":"Everything"}]}`)
		case "/api/v1/json/search/images":
			w.Header().Set(httpx.HeaderContentType, httpx.ContentTypeJSON)
			if r.URL.Query().Get("filter_id") != "99" {
				_, _ = io.WriteString(w, `{"images":[],"total":0}`)
				return
			}
			_, _ = io.WriteString(w, `{"images":[{"id":42,"view_url":"/img/view/42.png"}],"total":1}`)
		case "/images/42", "/search":
			w.Header().Set(httpx.HeaderContentType, "text/html")
			_, _ = io.WriteString(w, `<html><head><title>A Board</title></head><body>`+
				`<div class="image-show-container" data-uris="{}"></div></body></html>`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &filters
}

// A post or search the default filter hides answers empty, so the board is
// asked again under its own "everything" filter before it is taken for no
// board at all, and the filter is looked up once.
func TestBooruRegistryReadsPostsTheDefaultFilterHides(t *testing.T) {
	for _, path := range []string{"/images/42", "/search?q=a_tag"} {
		srv, filters := philomenaServer(t)
		res, ex, err := NewRegistry(&config.Config{}, testClient()).Extract(t.Context(), srv.URL+path, Options{})
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if ex.Name() != "booru" || len(res.Files) != 1 || res.Files[0].URL != srv.URL+"/img/view/42.png" {
			t.Errorf("%s: got %s with %+v, want the hidden image", path, ex.Name(), res.Files)
		}
		if n := filters.Load(); n != 1 {
			t.Errorf("%s: the filters were asked for %d times, want once", path, n)
		}
	}
}

// A Gelbooru 0.2 board renders much the same markup as a legacy one, so an
// API that did not answer is no reason to scrape it: its post page shows a
// resized sample, its listing thumbnails no original can be derived from.
// Only the legacy network's own image layout is scraped.
func TestBooruAPIDownIsNotALegacyBoard(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") == "dapi" {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	client := testClient()
	modern := map[string]string{
		"/index.php?page=post&s=view&id=42": `<html><title>A Board</title><body><div class="content">` +
			`<img id="image" src="/samples/1/sample_abc.jpg"><a href="index.php?page=post&s=list">more</a></div></body></html>`,
		"/index.php?page=post&s=list&tags=cat": `<html><title>A Board</title><body><div class="content">` +
			`<span class="thumb"><a id="p42" href="index.php?page=post&s=view&id=42">` +
			`<img src="/thumbnails/1/thumbnail_abc.jpg"></a></span></div></body></html>`,
	}
	for path, doc := range modern {
		p := htmlPage(t, srv.URL+path, doc)
		if site := booruDetect(context.Background(), p, client, p.Final); site != nil {
			t.Errorf("%s: detected %+v, want nothing while its API is down", path, site)
		}
	}
	legacy := map[string]string{
		"/index.php?page=post&s=view&id=159110": gelbooru01PostPage,
		"/index.php?page=post&s=list&tags=cat":  gelbooru01ListPage,
	}
	for path, doc := range legacy {
		p := htmlPage(t, srv.URL+path, doc)
		if site := booruDetect(context.Background(), p, client, p.Final); site == nil || site.api != apiGelbooru01 {
			t.Errorf("%s: detected %+v, want a legacy board", path, site)
		}
	}
}

// booruPagesServer answers every page of a Danbooru listing with page(n).
func booruPagesServer(t *testing.T, page func(n int) string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var asked atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked.Add(1)
		n, _ := strconv.Atoi(r.URL.Query().Get("page"))
		w.Header().Set(httpx.HeaderContentType, httpx.ContentTypeJSON)
		_, _ = io.WriteString(w, page(n))
	}))
	t.Cleanup(srv.Close)
	return srv, &asked
}

// booruPostsJSON is a Danbooru page of posts from first to last.
func booruPostsJSON(first, last int) string {
	var rows []string
	for id := first; id <= last; id++ {
		rows = append(rows, fmt.Sprintf(`{"id":%d,"file_url":"/data/%d.jpg"}`, id, id))
	}
	return "[" + strings.Join(rows, ",") + "]"
}

// A board that ignores its page parameter past the end serves the last
// page again: that is the listing's end, not a cut to say.
func TestBooruRepeatedLastPageIsTheEnd(t *testing.T) {
	srv, _ := booruPagesServer(t, func(int) string { return booruPostsJSON(1, 3) })
	site := &booruSite{name: "board", root: srv.URL, api: apiDanbooru}
	files, note, err := testBooru(t).fetch(t.Context(), site, "a_tag", "", Options{})
	if err != nil || len(files) != 3 || note != "" {
		t.Errorf("files=%d note=%q err=%v, want 3 files and no note", len(files), note, err)
	}
}

// A tag search is held to the boards' own bound below the user's file cap,
// and says which of the two cut it.
func TestBooruSearchIsHeldToTheBoardBound(t *testing.T) {
	srv, asked := booruPagesServer(t, func(n int) string {
		return booruPostsJSON(n*booruPageSize, n*booruPageSize+booruPageSize-1)
	})
	site := &booruSite{name: "board", root: srv.URL, api: apiDanbooru}
	files, note, err := testBooru(t).fetch(t.Context(), site, "a_tag", "", Options{})
	if err != nil || len(files) != config.MaxBooruPosts || note != "partial — post limit" {
		t.Errorf("files=%d note=%q err=%v, want %d files cut by the post limit", len(files), note, err, config.MaxBooruPosts)
	}
	if want := int32(config.MaxBooruPosts / booruPageSize); asked.Load() != want {
		t.Errorf("asked %d pages, want %d", asked.Load(), want)
	}
	files, note, err = testBooru(t).fetch(t.Context(), site, "a_tag", "", Options{limits: Limits{Files: 150}})
	if err != nil || len(files) != 150 || note != "partial — file limit" {
		t.Errorf("files=%d note=%q err=%v, want 150 files cut by the file limit", len(files), note, err)
	}
}

// Two boards under one shared first label are told apart by the next one.
func TestBooruLabel(t *testing.T) {
	for host, want := range map[string]string{
		"booru.first.example":  "first",
		"booru.second.example": "second",
		"aboard.example.test":  "aboard",
		"example.booru.org":    "example",
		"www.aboard.example":   "aboard",
		"booru.example":        "booru",
	} {
		if got := booruLabel(host); got != want {
			t.Errorf("booruLabel(%q) = %q, want %q", host, got, want)
		}
	}
}
