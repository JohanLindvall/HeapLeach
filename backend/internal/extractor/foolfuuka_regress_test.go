// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

// foolFuukaIndexJSON is one board page as the index API serves it: opening
// posts, one carrying an attachment.
const foolFuukaIndexJSON = `{"100":{"omitted":0,"images_omitted":0,"op":{"num":"100","title":"First",
	"media":{"media":"100.jpg","media_filename":"a.jpg","media_link":"https://media.example.test/b/image/1/1/100.jpg",
	"media_size":"10","media_status":"normal"}}}}`

// Both an unfamiliar HTML frontend and a withheld board page are settled
// by the index API. A missing board or a bare slug asks no API question.
func TestFoolFuukaRecognisesAWithheldBoardPage(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if !strings.HasPrefix(r.URL.Path, foolFuukaIndexAPI) {
			http.NotFound(w, r)
			return
		}
		w.Header().Set(httpx.HeaderContentType, httpx.ContentTypeJSON)
		_, _ = io.WriteString(w, foolFuukaIndexJSON)
	}))
	t.Cleanup(srv.Close)
	f := NewFoolFuuka(testClient())
	for _, path := range []string{"/b/", "/b/page/2/"} {
		if !f.Recognise(context.Background(), challengedPage(t, srv.URL+path)) {
			t.Errorf("%s: a challenged board page whose index API answers was not recognised", path)
		}
		refused := fixturePage(t, srv.URL+path, http.StatusForbidden, "text/html", "<html>no</html>")
		if !f.Recognise(context.Background(), refused) {
			t.Errorf("%s: a refused board page whose index API answers was not recognised", path)
		}
		if !f.Recognise(context.Background(), htmlPage(t, srv.URL+path, "<html><title>A custom board</title></html>")) {
			t.Errorf("%s: a served board whose index API answers was not recognised", path)
		}
	}
	calls.Store(0)
	for _, p := range []*Page{
		fixturePage(t, srv.URL+"/b/", http.StatusNotFound, "text/html", "<html>gone</html>"),
		// A short path written without the board's trailing slash is some
		// other site's page, challenged or not.
		challengedPage(t, srv.URL+"/watch"),
	} {
		if f.Recognise(context.Background(), p) {
			t.Errorf("%s (%d): an ordinary board-shaped page was recognised", p.URL, p.Status)
		}
	}
	if n := calls.Load(); n != 0 {
		t.Errorf("ordinary pages cost %d API requests, want none", n)
	}
}

// An alias that redirects to an archive is read as the archive it lands on:
// every API call goes to that origin, none takes the redirect again.
func TestFoolFuukaAliasIsReadAtTheArchiveItRedirectsTo(t *testing.T) {
	var archiveAPI atomic.Int32
	archive := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/_/api/") {
			archiveAPI.Add(1)
			w.Header().Set(httpx.HeaderContentType, httpx.ContentTypeJSON)
			_, _ = io.WriteString(w, foolFuukaThreadJSON)
			return
		}
		w.Header().Set(httpx.HeaderContentType, "text/html")
		_, _ = io.WriteString(w, `<html><head><meta name="generator" content="FoolFuuka 2.2.0"><title>/b/</title>`+
			`<script src="/foolfuuka/foolz/theme/assets/board.js"></script></head><body></body></html>`)
	}))
	t.Cleanup(archive.Close)
	var alias atomic.Int32
	aliasSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		alias.Add(1)
		http.Redirect(w, r, archive.URL+r.URL.RequestURI(), http.StatusMovedPermanently)
	}))
	t.Cleanup(aliasSrv.Close)

	res, ex, err := NewRegistry(&config.Config{}, testClient()).Extract(t.Context(), aliasSrv.URL+"/b/thread/9998/", Options{})
	if err != nil {
		t.Fatal(err)
	}
	if ex.Name() != "foolfuuka" || len(res.Files) == 0 {
		t.Fatalf("got %s with %d files, want the archived thread", ex.Name(), len(res.Files))
	}
	if n := alias.Load(); n != 1 {
		t.Errorf("the alias was asked %d times, want only the probe", n)
	}
	if archiveAPI.Load() == 0 {
		t.Error("the archive's API was never asked")
	}
}

// A search walked to a page that adds nothing has reached its end, and says
// no more than that; a search that would run on is held to the archives'
// own bound below the file cap, and says which of the two cut it.
func TestFoolFuukaSearchEndsAndBounds(t *testing.T) {
	f := foolFuukaFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("page") == "1" {
			_, _ = io.WriteString(w, foolFuukaSearchPage(100))
			return
		}
		_, _ = io.WriteString(w, foolFuukaSearchPage(200))
	})
	res := extractOK(t, foolFuukaUnderTest{archive: f}, "https://example.test/b/search/text/thing/", Options{})
	if len(res.Files) != 6 || res.Note != "" {
		t.Errorf("%d files, note %q; want the 6 distinct files and no note", len(res.Files), res.Note)
	}

	// Ten attachments a page, numbered by the page asked for.
	endless := foolFuukaFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var posts []string
		for i := range 10 {
			num := r.URL.Query().Get("page") + fmt.Sprint(i)
			posts = append(posts, fmt.Sprintf(`{"num":"%s","title":null,"media":{"media":"%s.jpg",`+
				`"media_link":"https://media.example.test/b/image/1/1/%s.jpg","media_size":"1","media_status":"normal"}}`,
				num, num, num))
		}
		_, _ = io.WriteString(w, `{"0":{"posts":[`+strings.Join(posts, ",")+`]}}`)
	})
	res = extractOK(t, foolFuukaUnderTest{archive: endless}, "https://example.test/b/search/text/thing/", Options{})
	if len(res.Files) != config.MaxFoolFuukaSearchPosts || res.Note != "partial — post limit" {
		t.Errorf("%d files, note %q; want %d cut by the post limit",
			len(res.Files), res.Note, config.MaxFoolFuukaSearchPosts)
	}
	res = extractOK(t, foolFuukaUnderTest{archive: endless}, "https://example.test/b/search/text/thing/",
		Options{limits: Limits{Files: 25}})
	if len(res.Files) != 25 || res.Note != "partial — file limit" {
		t.Errorf("%d files, note %q; want the page that passed 25 cut to it and the file limit noted",
			len(res.Files), res.Note)
	}

	// Seven attachments a page do not divide the bound: the page that
	// crosses it is cut, so the search holds exactly the bound.
	uneven := foolFuukaFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var posts []string
		for i := range 7 {
			num := r.URL.Query().Get("page") + fmt.Sprint(i)
			posts = append(posts, fmt.Sprintf(`{"num":"%s","title":null,"media":{"media":"%s.jpg",`+
				`"media_link":"https://media.example.test/b/image/1/1/%s.jpg","media_size":"1","media_status":"normal"}}`,
				num, num, num))
		}
		_, _ = io.WriteString(w, `{"0":{"posts":[`+strings.Join(posts, ",")+`]}}`)
	})
	res = extractOK(t, foolFuukaUnderTest{archive: uneven}, "https://example.test/b/search/text/thing/", Options{})
	if len(res.Files) != config.MaxFoolFuukaSearchPosts || res.Note != "partial — post limit" {
		t.Errorf("%d files, note %q; want exactly %d", len(res.Files), res.Note, config.MaxFoolFuukaSearchPosts)
	}
}
