// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

// A Gelbooru 0.2 board answers a page past the end of a search with an
// empty body: that is the listing's end, not a page that could not be
// fetched, so a complete search carries no note and offers no retry.
func TestBooruEmptyBodyPastTheEndEndsTheSearch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(httpx.HeaderContentType, httpx.ContentTypeJSON)
		if r.URL.Query().Get("pid") == "0" {
			_, _ = io.WriteString(w, `[{"id":1,"file_url":"https://img.example.test/1.jpg"},{"id":2,"file_url":"https://img.example.test/2.jpg"}]`)
		}
		// Past the end: 200 and nothing in it.
	}))
	t.Cleanup(srv.Close)
	site := &booruSite{name: "board", root: srv.URL, api: apiGelbooru}
	files, note, err := testBooru(t).fetch(context.Background(), site, "a_tag", "", Options{})
	if err != nil || len(files) != 2 || note != "" {
		t.Fatalf("files=%d note=%q err=%v, want 2 files and no note", len(files), note, err)
	}
	if (&Result{Files: files, Note: note}).Incomplete() {
		t.Error("a complete search is offered for retry")
	}
}

// The post bound counts files, not posts: a search half of whose posts
// withhold their file still reaches the bound in files, and is cut exactly
// at it rather than a page past it.
func TestBooruBoundCountsFiles(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page := r.URL.Query().Get("page")
		var b strings.Builder
		b.WriteString("[")
		for i := range 30 {
			if i > 0 {
				b.WriteString(",")
			}
			id := page + fmt.Sprintf("%03d", i)
			if i%2 == 0 {
				fmt.Fprintf(&b, `{"id":%s,"file_url":"https://img.example.test/%s.jpg"}`, id, id)
			} else {
				fmt.Fprintf(&b, `{"id":%s,"file_url":null}`, id)
			}
		}
		b.WriteString("]")
		w.Header().Set(httpx.HeaderContentType, httpx.ContentTypeJSON)
		_, _ = io.WriteString(w, b.String())
	}))
	t.Cleanup(srv.Close)
	site := &booruSite{name: "board", root: srv.URL, api: apiDanbooru}
	files, note, err := testBooru(t).fetch(context.Background(), site, "a_tag", "", Options{limits: Limits{Files: 40}})
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 40 || !strings.Contains(note, "file limit") {
		t.Fatalf("files=%d note=%q, want 40 files cut at the file limit", len(files), note)
	}
}

// A bare /search on a page carrying a board's markup is its latest images,
// and a search whose first page holds only posts that withhold their file
// still identifies a marked board, whose later pages may hold files.
func TestBooruBareSearchAndHiddenFirstPage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(httpx.HeaderContentType, httpx.ContentTypeJSON)
		switch r.URL.Path {
		case "/api/v1/json/search/images":
			if r.URL.Query().Get("q") != "" {
				http.NotFound(w, r)
				return
			}
			_, _ = io.WriteString(w, `{"images":[{"id":7,"view_url":"https://img.example.test/7.png"}],"total":1}`)
		case "/posts.json":
			_, _ = io.WriteString(w, `[{"id":9,"md5":"abc","file_url":null}]`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	b := testBooru(t)
	marked := `<html><head><title>Board</title><meta name="generator" content="booru-on-rails"></head><body></body></html>`
	if !b.Recognise(context.Background(), htmlPage(t, srv.URL+"/search", marked)) {
		t.Error("a marked bare search was not recognised")
	}
	if b.Recognise(context.Background(), htmlPage(t, srv.URL+"/search", `<title>A page</title>`)) {
		t.Error("an unmarked bare search was recognised")
	}
	if b.Shape(mustParse(t, srv.URL+"/search")) {
		t.Error("a bare search became a link form")
	}
	if !b.Recognise(context.Background(), htmlPage(t, srv.URL+"/posts?tags=hidden", booruRailsPost)) {
		t.Error("a marked search whose first page withholds every file was not recognised")
	}
	if b.Recognise(context.Background(), htmlPage(t, srv.URL+"/posts?tags=hidden", `<title>A page</title>`)) {
		t.Error("an unmarked search of withheld posts was recognised")
	}
}
