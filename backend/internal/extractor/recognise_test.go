// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

// fixturePage is a probed page built in memory: what a recogniser would be
// handed for rawURL had it answered with this status, type and body.
func fixturePage(t *testing.T, rawURL string, status int, kind, body string) *Page {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	header := http.Header{}
	if kind != "" {
		header.Set(httpx.HeaderContentType, kind)
	}
	return &Page{URL: u, Final: u, Status: status, Header: header, Body: body}
}

// htmlPage is a 200 text/html fixture.
func htmlPage(t *testing.T, rawURL, body string) *Page {
	t.Helper()
	return fixturePage(t, rawURL, http.StatusOK, "text/html; charset=utf-8", body)
}

// challengedPage is what a probe met at rawURL when a WAF challenge stood in
// for the page: nothing of the site's own to read.
func challengedPage(t *testing.T, rawURL string) *Page {
	t.Helper()
	p := fixturePage(t, rawURL, http.StatusForbidden, "text/html", "")
	p.Challenged = true
	return p
}

// moveHost is u served from an unrelated host.
func moveHost(u *url.URL) *url.URL {
	moved := *u
	moved.Host = "moved.example.test"
	return &moved
}

// rehosted is the same page served from somewhere else entirely.
func rehosted(t *testing.T, p *Page) *Page {
	t.Helper()
	return &Page{URL: moveHost(p.URL), Final: moveHost(p.Final), Status: p.Status, Header: p.Header,
		Body: p.Body, Challenged: p.Challenged, Err: p.Err}
}

// expectRecognised asserts a recogniser's answer for a page, and that the
// answer is the same with the page served from an unrelated host: nothing
// about recognition may depend on where a page lives.
func expectRecognised(t *testing.T, r Recogniser, p *Page, want bool) {
	t.Helper()
	if got := r.Recognise(context.Background(), p); got != want {
		t.Errorf("Recognise(%s) = %v, want %v", p.URL, got, want)
	}
	if got := r.Recognise(context.Background(), rehosted(t, p)); got != want {
		t.Errorf("Recognise(%s, rehosted) = %v, want %v", p.URL, got, want)
	}
}

// expectShape asserts a URL-form answer, and its independence of the host.
func expectShape(t *testing.T, r Recogniser, rawURL string, want bool) {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	if got := r.Shape(u); got != want {
		t.Errorf("Shape(%s) = %v, want %v", rawURL, got, want)
	}
	if got := r.Shape(moveHost(u)); got != want {
		t.Errorf("Shape(%s, rehosted) = %v, want %v", rawURL, got, want)
	}
}

// recogniseCase is one extractor's recognition, pinned on invented pages: a
// page of the right form carrying the marker, the same form named for the
// site but without the marker, and the marked page under a wrong form.
type recogniseCase struct {
	name                  string
	r                     Recogniser
	good, wrongForm       string // URLs
	page, unmarked        string // bodies
	shapes, notShapes     []string
	extraGood, extraWrong []*recogniseExtra
}

type recogniseExtra struct{ url, body string }

func (c recogniseCase) run(t *testing.T) {
	t.Run(c.name, func(t *testing.T) {
		expectRecognised(t, c.r, htmlPage(t, c.good, c.page), true)
		expectRecognised(t, c.r, htmlPage(t, c.good, c.unmarked), false)
		if c.wrongForm != "" {
			expectRecognised(t, c.r, htmlPage(t, c.wrongForm, c.page), false)
		}
		expectRecognised(t, c.r, htmlPage(t, c.good, `<html><head><title>A blog</title></head></html>`), false)
		for _, e := range c.extraGood {
			expectRecognised(t, c.r, htmlPage(t, e.url, e.body), true)
		}
		for _, e := range c.extraWrong {
			expectRecognised(t, c.r, htmlPage(t, e.url, e.body), false)
		}
		for _, u := range c.shapes {
			expectShape(t, c.r, u, true)
		}
		for _, u := range c.notShapes {
			expectShape(t, c.r, u, false)
		}
		if d, ok := c.r.(Describer); !ok || d.Recognises() == "" {
			t.Error("no description for the inventory")
		}
	})
}

// TestRecognitionNeverReadsTheHost holds the package to its dispatch rule
// statically: no Shape or Recognise method may look at a URL's host, nor
// compare it with a domain. The fixture tests catch what a helper hides;
// this catches the direct case before any fixture is written.
func TestRecognitionNeverReadsTheHost(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, file, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv == nil || fn.Body == nil || (fn.Name.Name != "Shape" && fn.Name.Name != "Recognise") {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				var name string
				switch n := n.(type) {
				case *ast.SelectorExpr:
					name = n.Sel.Name
				case *ast.Ident:
					name = n.Name
				}
				switch name {
				case "Host", "Hostname", "HostMatches", "matchesHosts", "hostSet":
					t.Errorf("%s: %s reads %s", fset.Position(n.Pos()), fn.Name.Name, name)
				}
				return true
			})
		}
	}
}

// TestPageCacheFetchesAPageOnce: however many recognisers, extractors and
// sources ask for one URL at once, it is fetched once.
func TestPageCacheFetchesAPageOnce(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		time.Sleep(20 * time.Millisecond)
		w.Header().Set(httpx.HeaderContentType, "text/html")
		_, _ = w.Write([]byte("<html><title>Page</title></html>"))
	}))
	defer srv.Close()
	client := httpx.New("test", "en", 0, 5*time.Second)
	u, _ := url.Parse(srv.URL + "/a")
	var cache pageCache
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			if p := cache.get(context.Background(), client, u); p.Title() != "Page" {
				t.Errorf("title %q", p.Title())
			}
		})
	}
	wg.Wait()
	cache.get(context.Background(), client, u)
	if got := hits.Load(); got != 1 {
		t.Errorf("fetched %d times, want 1", got)
	}
	other, _ := url.Parse(srv.URL + "/b")
	cache.get(context.Background(), client, other)
	if got := hits.Load(); got != 2 {
		t.Errorf("another URL: fetched %d times in all, want 2", got)
	}
}

// TestPageCacheKeepsNoFailure: a page that did not load is asked again.
func TestPageCacheKeepsNoFailure(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) == 1 {
			http.Error(w, "busy", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()
	client := httpx.New("test", "en", 0, 5*time.Second)
	u, _ := url.Parse(srv.URL)
	var cache pageCache
	if p := cache.get(context.Background(), client, u); p.Err == nil {
		t.Fatal("the first probe should have failed")
	}
	if p := cache.get(context.Background(), client, u); p.Err != nil || p.Body != "ok" {
		t.Fatalf("second probe: %+v", p)
	}
}

// TestPageMemoComputesOnce shares one origin probe among recognisers.
func TestPageMemoComputesOnce(t *testing.T) {
	p := &Page{}
	var calls atomic.Int32
	for range 3 {
		if got := memo(p, "k", func() int { calls.Add(1); return 7 }); got != 7 {
			t.Fatalf("memo = %v", got)
		}
	}
	if calls.Load() != 1 {
		t.Errorf("computed %d times, want 1", calls.Load())
	}
}

// hostClaim stands in for a host's extractor in tests, claiming one host as
// no real recogniser may, so a test can route links without a fixture page.
type hostClaim string

func (h hostClaim) Shape(u *url.URL) bool                     { return u.Host == string(h) || u.Hostname() == string(h) }
func (h hostClaim) Recognise(_ context.Context, p *Page) bool { return h.Shape(p.URL) }
func (h hostClaim) Recognises() string                        { return "the test host " + string(h) }

// stubExtractor stands in for whichever host a link points at, answering
// every link with one file under the same title.
type stubExtractor struct {
	hostClaim
	title string
}

func (s stubExtractor) Name() string { return "stub" }

func (s stubExtractor) Extract(_ context.Context, u *url.URL, _ Options) (*Result, error) {
	return &Result{Title: s.title, Files: []File{{Name: s.title + ".mp4", URL: u.String(), Size: -1}}}, nil
}

// A stated length is kept only when it is one: absent, zero, negative and
// malformed counts all read as unknown, never as an empty file.
func TestKnownAndParsedSizes(t *testing.T) {
	for _, tc := range []struct {
		stated []int64
		want   int64
	}{
		{nil, -1}, {[]int64{0}, -1}, {[]int64{-5}, -1}, {[]int64{7}, 7},
		{[]int64{0, -1, 9, 4}, 9},
	} {
		if got := knownSize(tc.stated...); got != tc.want {
			t.Errorf("knownSize(%v) = %d, want %d", tc.stated, got, tc.want)
		}
	}
	for text, want := range map[string]int64{"": -1, " 42 ": 42, "0": -1, "-3": -1, "12kB": -1} {
		if got := parsedSize(text); got != want {
			t.Errorf("parsedSize(%q) = %d, want %d", text, got, want)
		}
	}
}
