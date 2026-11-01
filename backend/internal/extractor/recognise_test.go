// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/config"
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
			if !ok || fn.Body == nil || !recognition(fn) {
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

// recognition reports whether fn decides recognition: a Shape or Recognise
// method, or a plain function named for a shape, as rows hand to about.
func recognition(fn *ast.FuncDecl) bool {
	if fn.Recv != nil {
		return fn.Name.Name == "Shape" || fn.Name.Name == "Recognise"
	}
	return strings.HasSuffix(fn.Name.Name, "Shape")
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
	client := testClient()
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

// TestPageCacheFreshReadProbesAgain: a fresh read takes no kept page; its
// answer replaces the kept one, which later readers then share, and the
// bodies held are counted once.
func TestPageCacheFreshReadProbesAgain(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(httpx.HeaderContentType, "text/html")
		fmt.Fprintf(w, "<html><title>Page %d</title></html>", hits.Add(1))
	}))
	defer srv.Close()
	client := testClient()
	u, _ := url.Parse(srv.URL + "/a")
	var cache pageCache
	first := cache.get(context.Background(), client, u)
	fresh := cache.get(freshRead(context.Background()), client, u)
	if first.Title() != "Page 1" || fresh.Title() != "Page 2" {
		t.Fatalf("titles %q then %q, want a second probe", first.Title(), fresh.Title())
	}
	if later := cache.get(context.Background(), client, u); later != fresh {
		t.Errorf("a later reader got %q, want the fresh read's page", later.Title())
	}
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if want := config.PageCacheEntryBytes + len(fresh.Body); cache.bytes != want || cache.order.Len() != 1 {
		t.Errorf("cache holds %d bytes in %d entries, want the one page's %d", cache.bytes, cache.order.Len(), want)
	}
}

// TestPageCacheSharesASuccessAcrossRoutes: a route making the same request
// as another — the same URL, language and cookies, whatever agent the route
// presents — reads the page that route already had, and its readers take it
// as the probe of their URL. A route that would send a cookie its jar holds
// for the host asks for its own.
func TestPageCacheSharesASuccessAcrossRoutes(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set(httpx.HeaderContentType, "text/html")
		_, _ = w.Write([]byte("<html><title>Page</title></html>"))
	}))
	defer srv.Close()
	client := testClient()
	u, _ := url.Parse(srv.URL + "/a")
	var cache pageCache
	routeA := httpx.WithRoute(context.Background(), "a", testClient(), nil)
	routeB := httpx.WithRoute(context.Background(), "b", testClient(), nil)
	first := cache.get(routeA, client, u)
	second := cache.get(routeB, client, u)
	if second != first || hits.Load() != 1 {
		t.Fatalf("second route: same page %v after %d fetches, want the first route's page and 1 fetch", second == first, hits.Load())
	}
	if probed(withPage(routeB, second), u) != second {
		t.Error("the second route's reader refused the shared page as its probe")
	}

	jar, _ := cookiejar.New(nil)
	jar.SetCookies(u, []*http.Cookie{{Name: "session", Value: "x"}})
	withCookie := httpx.WithCookieJar(httpx.WithRoute(context.Background(), "c", testClient(), nil), jar)
	if cache.get(withCookie, client, u) == first || hits.Load() != 2 {
		t.Errorf("a route sending a cookie: %d fetches, want its own probe", hits.Load())
	}
	ownAgent := httpx.WithRouteAgent(context.Background(), "d", testClient(), nil, "other-agent")
	if cache.get(ownAgent, client, u) != first || hits.Load() != 2 {
		t.Errorf("a route with its own agent: %d fetches, want the first route's page", hits.Load())
	}
	if probed(withPage(ownAgent, first), u) != first {
		t.Error("a route with its own agent refused another route's page as its probe")
	}
}

// TestPageCacheKeepsNoRefusal: an answer other than a success is handed to
// the readers waiting for that probe and then dropped, so the next reader
// on any route is answered by the site rather than by the refusal; and a
// page of another route's refusal is no reader's probe.
func TestPageCacheKeepsNoRefusal(t *testing.T) {
	var hits atomic.Int32
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) == 1 {
			<-release
		}
		w.Header().Set(httpx.HeaderContentType, "text/html")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte("<html><title>Refused</title></html>"))
	}))
	defer srv.Close()
	client := testClient()
	u, _ := url.Parse(srv.URL + "/a")
	var cache pageCache
	pages := make(chan *Page, 3)
	for range 3 {
		go func() { pages <- cache.get(context.Background(), client, u) }()
	}
	for hits.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond)
	close(release)
	refused := <-pages
	for range 2 {
		if p := <-pages; p != refused {
			t.Error("a reader waiting for the probe was not handed its answer")
		}
	}
	// A 403 probe is asked twice, the second time with the origin as its
	// Referer.
	if refused.Status != http.StatusForbidden || hits.Load() != 2 {
		t.Fatalf("status %d after %d fetches, want one 403 probe of two requests", refused.Status, hits.Load())
	}
	if cache.get(context.Background(), client, u) == refused || hits.Load() != 4 {
		t.Errorf("a later reader: %d fetches, want its own probe", hits.Load())
	}
	routeB := httpx.WithRoute(context.Background(), "b", testClient(), nil)
	if probed(withPage(routeB, refused), u) != nil {
		t.Error("another route took a refusal as its probe")
	}
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if cache.order.Len() != 0 || cache.bytes != 0 {
		t.Errorf("the cache holds %d entries, %d bytes after refusals", cache.order.Len(), cache.bytes)
	}
}

// TestPageCacheCountsWhatItKeeps: every kept page counts an allowance of
// its own, a header-only answer included, and its memoised answers count
// as they are kept; the bound drops the oldest when they outgrow it, and an
// answer memoised on a page no longer kept counts nothing.
func TestPageCacheCountsWhatItKeeps(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(httpx.HeaderContentType, "video/mp4")
	}))
	defer srv.Close()
	client := testClient()
	var cache pageCache
	first, _ := url.Parse(srv.URL + "/a.mp4")
	media := cache.get(context.Background(), client, first)
	if media.Body != "" || media.Err != nil {
		t.Fatalf("media probe: %+v", media)
	}
	cache.mu.Lock()
	if cache.bytes != config.PageCacheEntryBytes {
		t.Errorf("a header-only page counts %d bytes, want %d", cache.bytes, config.PageCacheEntryBytes)
	}
	cache.mu.Unlock()

	answer := strings.Repeat("x", 1000)
	memoAsked(context.Background(), media, "doc", func() string { return answer })
	memo(media, "raw", func() json.RawMessage { return json.RawMessage(`[1,2,3]`) })
	memo(media, "player", func() *Page { return &Page{Body: "abc"} })
	want := config.PageCacheEntryBytes + 1000 + 7 + config.PageCacheEntryBytes + 3
	cache.mu.Lock()
	if cache.bytes != want {
		t.Errorf("with its answers the page counts %d bytes, want %d", cache.bytes, want)
	}
	cache.mu.Unlock()

	second, _ := url.Parse(srv.URL + "/b.mp4")
	later := cache.get(context.Background(), client, second)
	memo(later, "huge", func() string { return strings.Repeat("y", config.PageCacheBytes) })
	cache.mu.Lock()
	if cache.order.Len() != 1 || cache.entries[" "+httpx.RequestKey(context.Background(), client, first)] != nil {
		t.Errorf("past the bound the cache holds %d entries, want only the newest", cache.order.Len())
	}
	cache.mu.Unlock()

	memo(media, "after", func() string { return answer })
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if want := config.PageCacheEntryBytes + config.PageCacheBytes; cache.bytes != want {
		t.Errorf("after an answer on a dropped page the cache counts %d bytes, want %d", cache.bytes, want)
	}
}

// TestPageMemoAskedWaiterLeavesWithItsContext: a caller waiting for an
// answer another job is asking returns when its own context ends, and the
// asker's answer, once it comes, is kept for the next caller.
func TestPageMemoAskedWaiterLeavesWithItsContext(t *testing.T) {
	p := &Page{}
	release := make(chan struct{})
	started := make(chan struct{})
	go memoAsked(context.Background(), p, "k", func() int {
		close(started)
		<-release
		return 7
	})
	<-started
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() {
		done <- memoAsked(ctx, p, "k", func() int { t.Error("a waiter asked again"); return 0 })
	}()
	time.Sleep(10 * time.Millisecond)
	cancel()
	select {
	case got := <-done:
		if got != 0 {
			t.Errorf("a cancelled waiter got %d", got)
		}
	case <-time.After(time.Second):
		t.Fatal("a cancelled waiter stayed blocked behind another's question")
	}
	close(release)
	if got := memoAsked(context.Background(), p, "k", func() int { return 9 }); got != 7 {
		t.Errorf("later caller got %d, want the asker's kept 7", got)
	}
}

// TestPageMemoAskedDropsAnAnswerAfterCancel: an answer computed after its
// asker's context ended is not kept, and a waiter asks again with its own.
func TestPageMemoAskedDropsAnAnswerAfterCancel(t *testing.T) {
	p := &Page{}
	ctx, cancel := context.WithCancel(context.Background())
	release := make(chan struct{})
	started := make(chan struct{})
	go memoAsked(ctx, p, "k", func() int {
		close(started)
		<-release
		cancel()
		return 1
	})
	<-started
	done := make(chan int, 1)
	go func() { done <- memoAsked(context.Background(), p, "k", func() int { return 2 }) }()
	time.Sleep(10 * time.Millisecond)
	close(release)
	if got := <-done; got != 2 {
		t.Errorf("waiter got %d, want its own answer 2", got)
	}
}

// TestVerdictMemoKeepsOnlyDefiniteAnswers: a yes is kept; a no only when
// the site gave it, and not for a fresh read.
func TestVerdictMemoKeepsOnlyDefiniteAnswers(t *testing.T) {
	var v verdictMemo
	var calls int
	ask := func(ctx context.Context, yes, definite bool) bool {
		return v.ask(ctx, "Origin.example.test", func() (bool, bool) { calls++; return yes, definite })
	}
	bg := context.Background()
	ask(bg, false, false)
	ask(bg, false, false)
	if calls != 2 {
		t.Fatalf("an unanswered no was kept: %d asks, want 2", calls)
	}
	ask(bg, false, true)
	if ask(bg, true, true) || calls != 3 {
		t.Fatalf("a definite no was not kept: %d asks, want 3", calls)
	}
	if !ask(freshRead(bg), true, true) || calls != 4 {
		t.Fatalf("a fresh read took a kept no: %d asks, want 4", calls)
	}
	if !ask(bg, false, true) || calls != 4 {
		t.Fatalf("a yes was not kept: %d asks, want 4", calls)
	}
}

// TestPageJSONAnswerSaysWhetherTheEndpointAnswered: only the endpoint's
// own no — a success that is not the shape asked for, a client error — is
// definite; a server error, a rate limit or a check skipped is not.
func TestPageJSONAnswerSaysWhetherTheEndpointAnswered(t *testing.T) {
	saved := platformProbeTimeout
	platformProbeTimeout = 2 * time.Second
	t.Cleanup(func() { platformProbeTimeout = saved })
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok":
			w.Header().Set(httpx.HeaderContentType, httpx.ContentTypeJSON)
			_, _ = w.Write([]byte(`{"a":1}`))
		case "/html":
			w.Header().Set(httpx.HeaderContentType, "text/html")
			_, _ = w.Write([]byte(`<html></html>`))
		case "/busy":
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
		case "/broken":
			w.WriteHeader(http.StatusBadGateway)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	client := testClient()
	for path, want := range map[string][2]bool{
		"/ok": {true, true}, "/html": {false, true}, "/missing": {false, true},
		"/busy": {false, false}, "/broken": {false, false},
	} {
		page := &Page{}
		var out map[string]any
		ok, answered := pageJSONAnswer(context.Background(), page, client, srv.URL+path, nil, &out)
		if ok != want[0] || answered != want[1] {
			t.Errorf("%s: ok %v, answered %v; want %v, %v", path, ok, answered, want[0], want[1])
		}
	}
	if _, answered := pageJSONAnswer(context.Background(), &Page{}, nil, srv.URL+"/ok", nil, new(any)); answered {
		t.Error("a check skipped for want of a client counted as answered")
	}
}

// TestPageMemoAskedIsPerRoute: a question put to the network from one
// route is not another route's answer, though both read one page.
func TestPageMemoAskedIsPerRoute(t *testing.T) {
	p := &Page{}
	var calls atomic.Int32
	ask := func(ctx context.Context) int {
		return memoAsked(ctx, p, "k", func() int { return int(calls.Add(1)) })
	}
	routeA := httpx.WithRoute(context.Background(), "a", testClient(), nil)
	routeB := httpx.WithRoute(context.Background(), "b", testClient(), nil)
	if a, again, b := ask(routeA), ask(routeA), ask(routeB); a != 1 || again != 1 || b != 2 {
		t.Errorf("answers %d, %d, %d; want 1 shared on route a and 2 on route b", a, again, b)
	}
}

// TestPageCacheKeepsNoFailure: a page that did not load is asked again.
// The probe itself asks a server error twice before giving up.
func TestPageCacheKeepsNoFailure(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) <= 2 {
			http.Error(w, "busy", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()
	client := testClient()
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

// TestRegistryHoldsTheFileCap cuts an extractor that listed past the cap,
// and says so in the job's note.
func TestRegistryHoldsTheFileCap(t *testing.T) {
	reg := &Registry{fallback: NewDirect(nil), limits: Limits{Files: 2}}
	reg.extractors = []Extractor{stubExtractor{hostClaim: "tube.example.test", title: "Clip"}}
	many := &manyFiles{stubExtractor: stubExtractor{hostClaim: "many.example.test"}, n: 5}
	reg.extractors = append(reg.extractors, many)
	res, _, err := reg.Extract(context.Background(), "https://many.example.test/a", Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Files) != 2 || res.Note != "already partial; partial — file limit" {
		t.Fatalf("files=%d note=%q", len(res.Files), res.Note)
	}
}

type manyFiles struct {
	stubExtractor
	n int
}

func (m *manyFiles) Extract(_ context.Context, u *url.URL, _ Options) (*Result, error) {
	res := &Result{Title: "Many", Note: "already partial"}
	for i := range m.n {
		res.Files = append(res.Files, File{Name: fmt.Sprintf("f%d", i), URL: u.String(), Size: -1})
	}
	return res, nil
}

func TestFolderNameKeepsOneFolder(t *testing.T) {
	if got := folderName(" A/B\\C "); got != "A_B_C" {
		t.Errorf("folderName = %q", got)
	}
}
