package extractor

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

// linksThreadPage is a forum thread in the shape these pages take, on reserved
// domains except where a real host shape is the thing being recognised. Every
// trap the harvest has to survive is in it:
//
//   - a link written only as text, because a board that does not linkify a
//     post still prints it;
//   - an anchor whose href goes through the board's own redirector while the
//     text carries the real URL, which is why the text is read at all;
//   - a mega link, whose fragment is the decryption key and not a page anchor;
//   - an embedded player, which is a video that appears in no anchor;
//   - the same link posted twice, as every thread of any length does;
//   - a stylesheet and an avatar served from hosts that are themselves
//     supported. These are the trap that a live sweep found and a synthetic
//     one would not have: extractors match on the host, so a page's own
//     furniture looks exactly like content until you notice it is CSS.
const linksThreadPage = `<html><head><title>A thread with things in it | Board</title>
<link rel="stylesheet" href="//static.example.test/board.css">
<script src="https://pixhost.to/styles/all.min.js"></script></head><body>
<nav><a href="/forums/">Forums</a> <a href="/threads/another-thread">Another thread</a></nav>
<article class="post">
  <img class="avatar" src="https://media.imagepond.net/media/avatar-1.png">
  <p>Here you go: <a href="https://gofile.io/d/AAAA" rel="nofollow">gofile.io/d/AAAA</a></p>
  <p>Mirror: <a href="/goto/link?url=https%3A%2F%2Fpixhost.to%2Fgallery%2Fabc123">https://pixhost.to/gallery/abc123</a></p>
  <p>And the big one, https://mega.nz/file/AAAAAAAA#SECRETKEY. Enjoy.</p>
  <p>Reposting <a href="https://gofile.io/d/AAAA">the first one</a> for visibility</p>
  <p>Clip: <iframe src="https://ok.ru/videoembed/1234567890"></iframe></p>
</article>
<footer><a href="mailto:staff@example.test">Contact</a>
<a href="javascript:void(0)">Top</a></footer>
</body></html>`

// linksSweep parses the fixture and collects its links, the way Extract does.
func linksSweep(t *testing.T, doc string) []string {
	t.Helper()
	base, err := ParseURL("https://board.example.test/threads/a-thread")
	if err != nil {
		t.Fatal(err)
	}
	root, err := parseHTML(doc)
	if err != nil {
		t.Fatal(err)
	}
	return linksCandidates(root, base)
}

// TestLinksCandidatesReadsAnchorsFramesAndText pins what a sweep of a page
// collects, which is the half of the harvest that never asks the registry.
func TestLinksCandidatesReadsAnchorsFramesAndText(t *testing.T) {
	got := linksSweep(t, linksThreadPage)
	index := make(map[string]int, len(got))
	for i, link := range got {
		if prev, dup := index[link]; dup {
			t.Errorf("%s was collected twice, at %d and %d", link, prev, i)
		}
		index[link] = i
	}

	for _, want := range []string{
		"https://gofile.io/d/AAAA",
		"https://pixhost.to/gallery/abc123",
		// The player is framed in, and is in no anchor on the page.
		"https://ok.ru/videoembed/1234567890",
		// A relative anchor resolves against the page it was read from.
		"https://board.example.test/threads/another-thread",
	} {
		if _, ok := index[want]; !ok {
			t.Errorf("%s was not collected: %v", want, got)
		}
	}

	// The page's own furniture is not a link somebody posted, and two pieces
	// of it here sit on hosts that are supported, so nothing downstream would
	// throw them out.
	for _, unwanted := range []string{
		"https://static.example.test/board.css",
		"https://pixhost.to/styles/all.min.js",
		"https://media.imagepond.net/media/avatar-1.png",
	} {
		if _, ok := index[unwanted]; ok {
			t.Errorf("%s was collected: a page's own assets are not its links", unwanted)
		}
	}

	// The mega key lives in the fragment, and the full stop after it in the
	// sentence does not belong to the link.
	const mega = "https://mega.nz/file/AAAAAAAA#SECRETKEY"
	if _, ok := index[mega]; !ok {
		t.Errorf("the mega link lost its key or kept the sentence's full stop: %v", got)
	}

	for _, unwanted := range []string{"mailto:", "javascript:"} {
		for _, link := range got {
			if strings.HasPrefix(link, unwanted) {
				t.Errorf("a %s reference was collected: %s", unwanted, link)
			}
		}
	}

	// Order is the page's own, which is what the job's file order is built on.
	if index["https://gofile.io/d/AAAA"] > index["https://ok.ru/videoembed/1234567890"] {
		t.Error("candidates came back out of page order")
	}
}

// TestLinksSweepIgnoresAPagesOwnFurniture runs the sweep and the filter
// together over the fixture, with the registry the user actually has.
//
// This is the one a live probe taught: a supported host's stylesheet, or an
// avatar on a supported image host, passes the "is this a host we know" filter
// exactly as a posted link does. Every one of them then costs a request and
// fails, and on a page belonging to a supported host there are hundreds.
func TestLinksSweepIgnoresAPagesOwnFurniture(t *testing.T) {
	harvester := &Links{registry: NewRegistry(&config.Config{}, nil)}

	got := harvester.supported(linksSweep(t, linksThreadPage))
	want := []string{
		"https://gofile.io/d/AAAA",
		"https://pixhost.to/gallery/abc123",
		"https://mega.nz/file/AAAAAAAA#SECRETKEY",
		"https://ok.ru/videoembed/1234567890",
	}
	if len(got) != len(want) {
		t.Fatalf("harvested %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("source[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestLinksCandidatesReadsThroughARedirector is the reason text nodes are
// swept at all: the href goes to the board, the readable copy goes to the host.
func TestLinksCandidatesReadsThroughARedirector(t *testing.T) {
	got := linksSweep(t, linksThreadPage)
	var found bool
	for _, link := range got {
		if link == "https://pixhost.to/gallery/abc123" {
			found = true
		}
	}
	if !found {
		t.Fatalf("the redirected link was only harvested as the redirector: %v", got)
	}
}

// TestLinksSupportedKeepsOnlyRealExtractors covers the filter the whole idea
// rests on. The fallback matches every link on the page, so a harvest that
// used Find would queue the board's navigation as files.
func TestLinksSupportedKeepsOnlyRealExtractors(t *testing.T) {
	reg := NewRegistry(&config.Config{}, nil)
	harvester := &Links{registry: reg}

	got := harvester.supported([]string{
		"https://gofile.io/d/AAAA",
		"https://board.example.test/threads/another-thread",
		"https://static.example.test/board.css",
		"https://mega.nz/file/AAAAAAAA#SECRETKEY",
		// A harvest inside a harvest, which is where the recursion would be.
		"links:https://board.example.test/threads/a-thread",
	})

	want := []string{"https://gofile.io/d/AAAA", "https://mega.nz/file/AAAAAAAA#SECRETKEY"}
	if len(got) != len(want) {
		t.Fatalf("kept %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("kept[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// --------------------------------------------------------------- the prefix

// TestLinksParseURLWrapsTheTarget covers every shape of the prefix, and the
// round trip through String(): the manager stores a job's source as the URL's
// own String() and parses that again when it resolves, so a form that does not
// survive the trip would harvest the wrong page.
func TestLinksParseURLWrapsTheTarget(t *testing.T) {
	tests := map[string]string{
		"links:https://board.example.test/threads/a-thread": "https://board.example.test/threads/a-thread",
		"links://board.example.test/threads/a-thread":       "https://board.example.test/threads/a-thread",
		"links:board.example.test/threads/a-thread":         "https://board.example.test/threads/a-thread",
		"LINKS:https://board.example.test/t":                "https://board.example.test/t",
		"  links: https://board.example.test/t  ":           "https://board.example.test/t",
		// A query and a fragment both belong to the page, not to us.
		"links:https://board.example.test/t?page=3#post-9": "https://board.example.test/t?page=3#post-9",
		// http is the one scheme that cannot be inferred back, so it has to be
		// carried rather than reconstructed.
		"links:http://board.example.test/t": "http://board.example.test/t",
	}
	for raw, want := range tests {
		u, err := ParseURL(raw)
		if err != nil {
			t.Errorf("ParseURL(%q): %v", raw, err)
			continue
		}
		if u.Scheme != linksScheme {
			t.Errorf("ParseURL(%q) has scheme %q, want %q", raw, u.Scheme, linksScheme)
			continue
		}
		if got := linksTargetOf(u); got != want {
			t.Errorf("ParseURL(%q) targets %q, want %q", raw, got, want)
		}

		again, err := ParseURL(u.String())
		if err != nil {
			t.Errorf("re-parsing %q: %v", u.String(), err)
			continue
		}
		if got := linksTargetOf(again); got != want {
			t.Errorf("%q did not survive the round trip: %q, want %q", raw, got, want)
		}
	}
}

func TestLinksParseURLRejectsAnEmptyTarget(t *testing.T) {
	for _, raw := range []string{"links:", "links://", "links: "} {
		if u, err := ParseURL(raw); err == nil {
			t.Errorf("ParseURL(%q) accepted %q, want an error naming the missing page", raw, u)
		}
	}
}

// TestLinksRoutesAheadOfTheHosts covers an ordering hazard that a live run
// found and every synthetic one missed: every other extractor matches on host
// alone and ignores the scheme, so a harvest of a page that happens to live on
// a supported host went to that host's extractor, which then tried to fetch
// "links://..." and failed on the protocol. The harvester has to be asked
// first.
func TestLinksRoutesAheadOfTheHosts(t *testing.T) {
	reg := NewRegistry(&config.Config{}, nil)
	for _, raw := range []string{
		"links:https://gofile.io/d/AAAA",
		"links:https://thisvid.com/",
		"links:https://board.example.test/threads/a-thread",
	} {
		u, err := ParseURL(raw)
		if err != nil {
			t.Fatal(err)
		}
		if got := reg.Find(u).Name(); got != linksScheme {
			t.Errorf("%s went to %q, want the harvester", raw, got)
		}
	}

	// And the prefix is the only thing that redirects it: the same pages
	// without it belong to their own hosts.
	u, err := ParseURL("https://gofile.io/d/AAAA")
	if err != nil {
		t.Fatal(err)
	}
	if got := reg.Find(u).Name(); got != "gofile" {
		t.Errorf("an ordinary link went to %q, want gofile", got)
	}
}

// TestLinksMatchIsPrefixOnly is the gate: no page is harvested by accident.
func TestLinksMatchIsPrefixOnly(t *testing.T) {
	harvester := &Links{}
	for raw, want := range map[string]bool{
		"links:https://board.example.test/t": true,
		"https://board.example.test/t":       false,
		"https://gofile.io/d/AAAA":           false,
	} {
		u, err := ParseURL(raw)
		if err != nil {
			t.Fatal(err)
		}
		if got := harvester.Match(u); got != want {
			t.Errorf("Match(%q) = %v, want %v", raw, got, want)
		}
	}
}

// -------------------------------------------------------------- the harvest

// linksStub stands in for the registry's real hosts, so an end-to-end harvest
// can be tested without a live one.
type linksStub struct {
	host  string
	files int
	// title is what the source calls itself, and so what its folder is named.
	title string
	// fail names a path this host refuses, standing in for the dead links
	// every thread of any age carries.
	fail string

	mu        sync.Mutex
	passwords []string
}

func (s *linksStub) Name() string { return "stub" }

func (s *linksStub) Match(u *url.URL) bool { return u.Host == s.host }

func (s *linksStub) Extract(_ context.Context, u *url.URL, opts Options) (*Result, error) {
	s.mu.Lock()
	s.passwords = append(s.passwords, opts.Password)
	s.mu.Unlock()

	if s.fail != "" && u.Path == s.fail {
		return nil, fmt.Errorf("stub: %s is gone", u.Path)
	}
	files := make([]File, 0, s.files)
	for i := 0; i < s.files; i++ {
		name := fmt.Sprintf("%s-%d.bin", strings.Trim(u.Path, "/"), i)
		files = append(files, File{
			Name: name,
			Size: -1,
			// A host that signs its links resolves them per attempt, and that
			// closure has to reach the downloader intact.
			Resolve: func(context.Context) (*Target, error) {
				return &Target{URL: "https://" + s.host + u.Path + "/signed/" + name, Name: name}, nil
			},
		})
	}
	return &Result{Title: s.title + " " + strings.Trim(u.Path, "/"), Files: files}, nil
}

// linksHarvester serves page and returns a harvester wired to a registry
// holding only stub, so nothing in the test reaches a real host.
func linksHarvester(t *testing.T, page string, stub Extractor) (*Links, string) {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set(httpx.HeaderContentType, "text/html; charset=utf-8")
		_, _ = io.WriteString(w, page)
	}))
	t.Cleanup(srv.Close)

	reg := &Registry{fallback: NewDirect(nil)}
	harvester := NewLinks(httpx.New("test-agent", "en-US", 0, 5*time.Second), reg)
	// Harvester first, as NewRegistry orders it.
	reg.extractors = []Extractor{harvester, stub}
	return harvester, "links:" + srv.URL + "/threads/a-thread"
}

func linksExtract(t *testing.T, harvester *Links, raw string, opts Options) (*Result, error) {
	t.Helper()
	u, err := ParseURL(raw)
	if err != nil {
		t.Fatal(err)
	}
	return harvester.Extract(context.Background(), u, opts)
}

// TestLinksExtractGroupsEverySourceIntoItsOwnFolder is the shape of the whole
// feature: files in the page's order, one folder per source, dead links
// skipped rather than fatal, and every resolver still attached.
func TestLinksExtractGroupsEverySourceIntoItsOwnFolder(t *testing.T) {
	stub := &linksStub{host: "files.example.test", files: 2, title: "Album / Two", fail: "/dead"}
	page := `<html><head><title>Thread | Board</title></head><body>
	<a href="https://files.example.test/first">one</a>
	<a href="https://files.example.test/dead">gone</a>
	<a href="https://elsewhere.example.test/not-a-host">unsupported</a>
	<p>https://files.example.test/second</p>
	</body></html>`

	harvester, raw := linksHarvester(t, page, stub)
	res, err := linksExtract(t, harvester, raw, Options{Password: "hunter2"})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}

	if res.Title != "Thread" {
		t.Errorf("title = %q, want the thread's own name without the board suffix", res.Title)
	}
	if len(res.Files) != 4 {
		t.Fatalf("got %d files, want 4 (two sources of two, one dead, one unsupported): %+v", len(res.Files), res.Files)
	}

	// Page order, whatever order the requests finished in.
	for i, want := range []string{"first-0.bin", "first-1.bin", "second-0.bin", "second-1.bin"} {
		if res.Files[i].Name != want {
			t.Errorf("file[%d] = %q, want %q", i, res.Files[i].Name, want)
		}
	}

	// One folder per source, and the separator in the source's title must not
	// become a second level of directory.
	for i, want := range []string{"Album _ Two first", "Album _ Two first", "Album _ Two second", "Album _ Two second"} {
		if res.Files[i].Dir != want {
			t.Errorf("file[%d] landed in %q, want %q", i, res.Files[i].Dir, want)
		}
	}

	// The resolver is what makes a signed host work at all, and it has to
	// survive being merged into somebody else's result.
	if res.Files[0].Resolve == nil {
		t.Fatal("the file lost its resolver in the merge")
	}
	target, err := res.Files[0].Resolve(context.Background())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !strings.Contains(target.URL, "/signed/") {
		t.Errorf("the resolver produced %q, want the host's freshly signed link", target.URL)
	}

	// A password given with the harvest is the only one there is, so every
	// source is offered it.
	stub.mu.Lock()
	defer stub.mu.Unlock()
	if len(stub.passwords) != 3 {
		t.Fatalf("the stub saw %d extractions, want 3", len(stub.passwords))
	}
	for _, pw := range stub.passwords {
		if pw != "hunter2" {
			t.Errorf("a source was resolved with password %q, want it passed through", pw)
		}
	}
}

// TestLinksExtractReportsAPageWithNothingOnIt covers the login wall, which is
// what the aggregator forums answer an anonymous fetch with. Reported as
// "nothing to download" it would look like an empty thread.
func TestLinksExtractReportsAPageWithNothingOnIt(t *testing.T) {
	stub := &linksStub{host: "files.example.test", files: 1, title: "Album"}
	page := `<html><body><h1>Log in</h1>
	<a href="/login">Log in</a> <a href="/register">Register</a></body></html>`

	harvester, raw := linksHarvester(t, page, stub)
	_, err := linksExtract(t, harvester, raw, Options{})
	if err == nil {
		t.Fatal("a login page was accepted as a harvest")
	}
	if !strings.Contains(err.Error(), "login") {
		t.Errorf("error = %q, want it to name the likely cause", err)
	}
}

// TestLinksExtractCapsTheLinksItFollows keeps one paste from queueing without
// end, and says so in the title rather than quietly returning a prefix.
func TestLinksExtractCapsTheLinksItFollows(t *testing.T) {
	const posted = linksMaxSources + 100

	stub := &linksStub{host: "files.example.test", files: 1, title: "Album"}
	var page strings.Builder
	page.WriteString(`<html><head><title>Big thread</title></head><body>`)
	for i := range posted {
		fmt.Fprintf(&page, `<a href="https://files.example.test/f%d">f%d</a>`, i, i)
	}
	page.WriteString(`</body></html>`)

	harvester, raw := linksHarvester(t, page.String(), stub)
	res, err := linksExtract(t, harvester, raw, Options{})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if len(res.Files) != linksMaxSources {
		t.Errorf("got %d files, want the first %d links followed", len(res.Files), linksMaxSources)
	}
	want := fmt.Sprintf("%d of %d links", linksMaxSources, posted)
	if !strings.Contains(res.Title, want) {
		t.Errorf("title = %q, want it to admit %q", res.Title, want)
	}
}

// TestLinksExtractCapsTheFilesItReturns is the other end of the same promise:
// a handful of links can still resolve to more files than a queue should hold.
func TestLinksExtractCapsTheFilesItReturns(t *testing.T) {
	const each = config.MaxListingFiles/2 + 100

	stub := &linksStub{host: "files.example.test", files: each, title: "Album"}
	page := `<html><head><title>Two big albums</title></head><body>
	<a href="https://files.example.test/one">one</a>
	<a href="https://files.example.test/two">two</a>
	</body></html>`

	harvester, raw := linksHarvester(t, page, stub)
	res, err := linksExtract(t, harvester, raw, Options{})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if len(res.Files) != config.MaxListingFiles {
		t.Errorf("got %d files, want %d", len(res.Files), config.MaxListingFiles)
	}
	want := fmt.Sprintf("%d of %d files", config.MaxListingFiles, each*2)
	if !strings.Contains(res.Title, want) {
		t.Errorf("title = %q, want it to admit %q", res.Title, want)
	}
}

// TestLinksExtractRefusesAStackedPrefix stops the one input that would make
// the harvester call itself.
func TestLinksExtractRefusesAStackedPrefix(t *testing.T) {
	stub := &linksStub{host: "files.example.test", files: 1, title: "Album"}
	harvester, raw := linksHarvester(t, "<html></html>", stub)

	_, err := linksExtract(t, harvester, "links:"+raw, Options{})
	if err == nil {
		t.Fatal("links:links: was accepted")
	}
	if !strings.Contains(err.Error(), "prefix") {
		t.Errorf("error = %q, want it to explain the stacked prefix", err)
	}
}

// ---------------------------------------------------------------- the parts

func TestLinksFolderStaysOneComponent(t *testing.T) {
	tests := map[string]string{
		"A Creator / An Album": "A Creator _ An Album",
		`Windows\Style`:        "Windows_Style",
		"  padded  ":           "padded",
		"an ordinary title":    "an ordinary title",
		"../../etc/passwd":     ".._.._etc_passwd",
	}
	for in, want := range tests {
		if got := linksFolder(in); got != want {
			t.Errorf("linksFolder(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLinksTitleAdmitsOnlyWhatWasDropped(t *testing.T) {
	if got := linksTitle("Thread", 10, 10, 40, 40); got != "Thread" {
		t.Errorf("a complete harvest was annotated: %q", got)
	}
	if got := linksTitle("Thread", 500, 812, 40, 40); got != "Thread (500 of 812 links)" {
		t.Errorf("got %q", got)
	}
	if got := linksTitle("Thread", 10, 10, 2000, 3120); got != "Thread (2000 of 3120 files)" {
		t.Errorf("got %q", got)
	}
	if got := linksTitle("Thread", 500, 812, 2000, 3120); got != "Thread (500 of 812 links, 2000 of 3120 files)" {
		t.Errorf("got %q", got)
	}
}
