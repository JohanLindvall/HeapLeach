package extractor

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

// testFoolFuuka is an install on a reserved domain. What these tests pin is
// how a FoolFuuka response is read, which is the same on every install and
// cares nothing for whose archive it came from.
var testFoolFuuka = foolFuukaSite{
	name:    "example",
	root:    "https://archive.example.test",
	domains: []string{"archive.example.test"},
}

func testFoolFuukaExtractor() *FoolFuuka {
	return &FoolFuuka{site: testFoolFuuka}
}

// foolFuukaThreadJSON is a thread in the shape the API serves one, and every
// trap in it is deliberate:
//
//   - The replies are a JSON object, and its keys are written out of order and
//     at two different widths — the board's numbering crossed 10000 partway
//     through. Both a Go map's iteration and a sort on the key's text would
//     get this wrong, and differently each run for the first.
//   - Three posts uploaded a file called "IMG_0001.jpg". Named after the
//     poster's filename they would collide; named after the stored one they
//     do not.
//   - One post's media_status is "banned": a row with a name and a size and
//     no file behind it.
//   - One post is spoilered, which hides its thumbnail on the page and says
//     nothing whatever about the file. It must still be collected.
//   - One post has no media_link at all, only the link the archive recorded
//     for somebody else's copy.
const foolFuukaThreadJSON = `{
 "9998": {
  "op": {
   "num": "9998", "title": "A synthetic thread",
   "media": {
    "media": "1600000000001.png", "media_filename": "OP.png",
    "media_link": "https://media.example.test/b/image/1600/00/1600000000001.png",
    "remote_media_link": "https://media.example.test/b/image/1600/00/1600000000001.png",
    "media_size": "1329565", "media_status": "normal", "spoiler": "0"
   }
  },
  "posts": {
   "10001": {
    "num": "10001", "title": null,
    "media": {
     "media": "1600000000005.webm", "media_filename": "IMG_0001.jpg",
     "media_link": "https://media.example.test/b/image/1600/00/1600000000005.webm",
     "media_size": "8388608", "media_status": "normal", "spoiler": "1"
    }
   },
   "9999": {
    "num": "9999", "title": null,
    "media": {
     "media": "1600000000002.jpg", "media_filename": "IMG_0001.jpg",
     "media_link": "https://media.example.test/b/image/1600/00/1600000000002.jpg",
     "media_size": "524288", "media_status": "normal", "spoiler": "0"
    }
   },
   "10002": {
    "num": "10002", "title": null,
    "media": {
     "media": "1600000000006.gif", "media_filename": "never-mirrored.gif",
     "media_link": "",
     "remote_media_link": "https://origin.example.test/b/1600000000006.gif",
     "media_size": "4096", "media_status": "normal", "spoiler": "0"
    }
   },
   "10000": {
    "num": "10000", "title": null,
    "media": {
     "media": "1600000000004.png", "media_filename": "IMG_0001.jpg",
     "media_link": "https://media.example.test/b/image/1600/00/1600000000004.png",
     "media_size": "262144", "media_status": "banned", "spoiler": "0"
    }
   },
   "10003": { "num": "10003", "title": null, "media": null }
  }
 }
}`

// TestFoolFuukaThreadKeepsThePostedOrder is the one that would rot silently.
// The replies arrive as a JSON object, so nothing about the transport imposes
// an order and a Go map imposes a fresh wrong one on every run.
func TestFoolFuukaThreadKeepsThePostedOrder(t *testing.T) {
	entries, err := foolFuukaDecode([]byte(foolFuukaThreadJSON))
	if err != nil {
		t.Fatalf("foolFuukaDecode: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want the one thread", len(entries))
	}

	var got []string
	for _, post := range entries[0].posts() {
		got = append(got, post.Num.String())
	}
	want := []string{"9998", "9999", "10000", "10001", "10002", "10003"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("posts came out as %v,\nwant the opening post first and the replies\n"+
			"in posted order %v (numeric, not lexical)", got, want)
	}
}

// TestFoolFuukaThreadOrderIsTheSameEveryRun is the other half of the ordering
// problem, and the half a single run cannot show. Decoding builds its list by
// ranging a Go map, whose order is deliberately randomised, so any post the
// sort cannot separate would come out somewhere new each time. Here two rows
// carry no number of their own and only the key they are filed under.
func TestFoolFuukaThreadOrderIsTheSameEveryRun(t *testing.T) {
	const body = `{"500":{"op":{"num":"500","title":"T"},"posts":{
		"503":{"title":null,"media":null},
		"501":{"num":"501","title":null,"media":null},
		"502":{"title":null,"media":null}}}}`

	var first []string
	for run := 0; run < 25; run++ {
		entries, err := foolFuukaDecode([]byte(body))
		if err != nil {
			t.Fatalf("foolFuukaDecode: %v", err)
		}
		var got []string
		for _, post := range entries[0].posts() {
			got = append(got, post.Num.String())
		}
		if run == 0 {
			first = got
			continue
		}
		if strings.Join(got, ",") != strings.Join(first, ",") {
			t.Fatalf("run %d gave %v, run 0 gave %v — the order is not settled", run, got, first)
		}
	}
	if want := "500,501,502,503"; strings.Join(first, ",") != want {
		t.Errorf("order = %v, want %s: a post that omits its own number is still "+
			"filed under it", first, want)
	}
}

// TestFoolFuukaFilesSkipOnlyWhatIsGone pins the two halves of the media_status
// rule together, because getting either one alone is easy: a banned row must
// be dropped, and a spoilered row must not be — the spoiler flag is about the
// thumbnail on the page, not about the file.
func TestFoolFuukaFilesSkipOnlyWhatIsGone(t *testing.T) {
	entries, err := foolFuukaDecode([]byte(foolFuukaThreadJSON))
	if err != nil {
		t.Fatal(err)
	}
	files := testFoolFuukaExtractor().files(entries[0].posts(), "")

	var names []string
	for _, f := range files {
		names = append(names, f.Name)
	}
	want := []string{
		"1600000000001.png", // the opening post
		"1600000000002.jpg",
		"1600000000005.webm", // spoilered, and perfectly downloadable
		"1600000000006.gif",  // no local copy, resolved at download time
	}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Errorf("collected %v,\nwant %v", names, want)
	}
	for _, f := range files {
		if strings.Contains(f.Name, "1600000000004") {
			t.Error("a banned row was collected: it has a name and a size and no file")
		}
	}
}

// TestFoolFuukaFilesNameByTheStoredFile guards the naming choice. The poster's
// own filename is what a person would reach for and it is exactly the wrong
// one: three posts here uploaded "IMG_0001.jpg".
func TestFoolFuukaFilesNameByTheStoredFile(t *testing.T) {
	entries, err := foolFuukaDecode([]byte(foolFuukaThreadJSON))
	if err != nil {
		t.Fatal(err)
	}
	files := testFoolFuukaExtractor().files(entries[0].posts(), "")

	seen := make(map[string]bool, len(files))
	for _, f := range files {
		if seen[f.Name] {
			t.Errorf("two files are both named %q", f.Name)
		}
		seen[f.Name] = true
		if f.Name == "IMG_0001.jpg" || f.Name == "OP.png" {
			t.Errorf("a file was named after the poster's upload (%q) rather than "+
				"the name the archive stored it under", f.Name)
		}
	}
}

// TestFoolFuukaFilesReportExactSizes matters because the downloader acts on
// the difference: an exact length settles whether a file already on disk is
// this one, and a SizeApprox length may never be used that way. The archive
// sends the real byte count, as a string.
func TestFoolFuukaFilesReportExactSizes(t *testing.T) {
	entries, err := foolFuukaDecode([]byte(foolFuukaThreadJSON))
	if err != nil {
		t.Fatal(err)
	}
	files := testFoolFuukaExtractor().files(entries[0].posts(), "")

	want := map[string]int64{
		"1600000000001.png":  1329565,
		"1600000000002.jpg":  524288,
		"1600000000005.webm": 8388608,
		"1600000000006.gif":  4096,
	}
	for _, f := range files {
		if f.SizeApprox {
			t.Errorf("%s was marked approximate; media_size is the exact length", f.Name)
		}
		if got := want[f.Name]; f.Size != got {
			t.Errorf("%s: size = %d, want %d", f.Name, f.Size, got)
		}
	}
}

// TestFoolFuukaFilesDeferOnlyTheUnmirrored pins which files carry a resolver.
// A link the archive serves itself is known now; the link it merely recorded
// for someone else's copy may not even be the file, so it is settled when the
// transfer starts.
func TestFoolFuukaFilesDeferOnlyTheUnmirrored(t *testing.T) {
	entries, err := foolFuukaDecode([]byte(foolFuukaThreadJSON))
	if err != nil {
		t.Fatal(err)
	}
	files := testFoolFuukaExtractor().files(entries[0].posts(), "")

	for _, f := range files {
		unmirrored := f.Name == "1600000000006.gif"
		switch {
		case unmirrored && f.Resolve == nil:
			t.Errorf("%s has no local copy but was given a fixed URL %q", f.Name, f.URL)
		case unmirrored && f.URL != "":
			t.Errorf("%s carries both a URL and a resolver; the URL would be used unresolved", f.Name)
		case !unmirrored && f.Resolve != nil:
			t.Errorf("%s is served by the archive and needs no resolving", f.Name)
		case !unmirrored && f.URL == "":
			t.Errorf("%s has neither a URL nor a resolver", f.Name)
		}
	}
}

// TestFoolFuukaDecodeReportsAnErrorSentWithA200 is the trap that would turn a
// missing thread into "no downloadable files found": the archive answers 200
// and puts the failure in the body.
func TestFoolFuukaDecodeReportsAnErrorSentWithA200(t *testing.T) {
	for _, body := range []string{
		`{"error":"Thread not found."}`,
		`{"error":"No results found."}`,
	} {
		entries, err := foolFuukaDecode([]byte(body))
		if err == nil {
			t.Fatalf("%s was accepted, yielding %d entries", body, len(entries))
		}
		if strings.HasSuffix(err.Error(), ".") {
			t.Errorf("error %q keeps its full stop, which lands mid-sentence when wrapped", err)
		}
	}

	if _, err := foolFuukaDecode([]byte(`{"error":""}`)); err == nil {
		t.Error("an error with no message was accepted")
	}
	if _, err := foolFuukaDecode([]byte(`[1,2,3]`)); err == nil {
		t.Error("a JSON array was accepted as an API response")
	}
}

// TestFoolFuukaDecodeAcceptsAnEmptyReplyList covers PHP's rendering of an
// empty associative array. A thread with no replies sends "posts": [] on the
// endpoint that otherwise keys them by number, so a decoder that only knows
// the object shape fails on exactly the simplest thread there is.
func TestFoolFuukaDecodeAcceptsAnEmptyReplyList(t *testing.T) {
	const body = `{"7744":{"op":{"num":"7744","title":"Alone","media":{
		"media":"1600000000009.jpg","media_filename":"a.jpg",
		"media_link":"https://media.example.test/b/image/1600/00/1600000000009.jpg",
		"media_size":"1024","media_status":"normal"}},"posts":[]}}`

	entries, err := foolFuukaDecode([]byte(body))
	if err != nil {
		t.Fatalf("foolFuukaDecode: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(entries))
	}
	if posts := entries[0].posts(); len(posts) != 1 {
		t.Fatalf("got %d posts, want just the opening one", len(posts))
	}
	if got := entries[0].number(); got != "7744" {
		t.Errorf("thread number = %q, want 7744", got)
	}
}

// TestFoolFuukaDecodeKeepsTheBoardOrder covers the index, whose keys are in
// the board's bump order — which is neither the order a map would produce nor
// the numeric order of the thread numbers.
func TestFoolFuukaDecodeKeepsTheBoardOrder(t *testing.T) {
	const index = `{
		"400": {"omitted": 12, "images_omitted": 4, "op": {"num":"400","title":"Bumped"}},
		"900": {"omitted": 3,  "images_omitted": 1, "op": {"num":"900","title":"Newer"}},
		"100": {"omitted": 0,  "images_omitted": 0, "op": {"num":"100","title":"Sticky"}}
	}`
	entries, err := foolFuukaDecode([]byte(index))
	if err != nil {
		t.Fatalf("foolFuukaDecode: %v", err)
	}
	var got []string
	for _, entry := range entries {
		got = append(got, entry.number())
	}
	if strings.Join(got, ",") != "400,900,100" {
		t.Errorf("threads came out as %v, want the board's own order 400,900,100", got)
	}
}

// TestFoolFuukaDecodeReadsSearchResults covers the third shape the same
// top-level object arrives in: results under a page index, replies as a flat
// array, and a "meta" block beside them that is not a thread.
func TestFoolFuukaDecodeReadsSearchResults(t *testing.T) {
	const search = `{
		"0": {"posts": [
			{"num":"501","title":null,"media":{"media":"a.jpg","media_filename":"a.jpg",
			 "media_link":"https://media.example.test/b/image/1/1/a.jpg",
			 "media_size":"11","media_status":"normal"}},
			{"num":"502","title":null,"media":null}
		]},
		"meta": {"total_found": 12701523, "max_results": "5000"}
	}`
	entries, err := foolFuukaDecode([]byte(search))
	if err != nil {
		t.Fatalf("foolFuukaDecode: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want the results page alone (meta is not a thread)", len(entries))
	}
	posts := entries[0].posts()
	if len(posts) != 2 {
		t.Fatalf("got %d posts, want 2", len(posts))
	}
	// The array shape carries its own order and must be left in it.
	if posts[0].Num.String() != "501" || posts[1].Num.String() != "502" {
		t.Errorf("search results were reordered: %s then %s",
			posts[0].Num.String(), posts[1].Num.String())
	}
}

// TestFoolFuukaRefreshTarget covers the page that is served instead of a file.
// The decoy is the second case: Cloudflare's interstitial carries a refresh
// too, with no url at all, and taking it for a redirect would send the
// download to the page it is already on.
func TestFoolFuukaRefreshTarget(t *testing.T) {
	const base = "https://archive.example.test/b/redirect/1600000000006.gif"
	tests := []struct {
		name string
		doc  string
		want string
	}{
		{
			name: "the archive's own redirect page",
			doc: `<!DOCTYPE html><html><head><title>/b/ &raquo; Redirecting</title>
				<meta http-equiv="Refresh" content="0; url=https://origin.example.test/b/1600000000006.gif">
				</head><body>You are being redirected.</body></html>`,
			want: "https://origin.example.test/b/1600000000006.gif",
		},
		{
			name: "a challenge page, which refreshes itself and goes nowhere",
			doc: `<!DOCTYPE html><html><head><title>Just a moment...</title>
				<meta http-equiv="refresh" content="360"></head><body></body></html>`,
			want: "",
		},
		{
			name: "a relative target",
			doc:  `<html><head><meta http-equiv="refresh" content="0;url=/b/full/x.gif"></head></html>`,
			want: "https://archive.example.test/b/full/x.gif",
		},
		{
			name: "a quoted target with the spacing left to taste",
			doc:  `<html><head><meta HTTP-EQUIV="REFRESH" CONTENT="0 ; URL = 'https://origin.example.test/x.gif'"></head></html>`,
			want: "https://origin.example.test/x.gif",
		},
		{
			name: "an ordinary page with nothing to follow",
			doc:  `<html><head><title>Not found</title></head><body>404</body></html>`,
			want: "",
		},
	}
	for _, tc := range tests {
		if got := foolFuukaRefreshTarget(tc.doc, base); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestFoolFuukaParse pins how an input URL is read, which is all this has to
// go on: the pages themselves answer a plain client with a challenge, so
// nothing can be fetched to check a guess.
func TestFoolFuukaParse(t *testing.T) {
	tests := []struct {
		raw   string
		kind  int
		board string
		num   string
		page  int
		query string
	}{
		{raw: "https://archive.example.test/a/thread/290337858/", kind: foolFuukaThreadTarget, board: "a", num: "290337858", page: 1},
		{raw: "https://archive.example.test/vt/thread/114289085", kind: foolFuukaThreadTarget, board: "vt", num: "114289085", page: 1},
		{raw: "https://archive.example.test/trash/post/12345/", kind: foolFuukaPostTarget, board: "trash", num: "12345", page: 1},
		{raw: "https://archive.example.test/b/", kind: foolFuukaIndexTarget, board: "b", page: 1},
		{raw: "https://archive.example.test/b", kind: foolFuukaIndexTarget, board: "b", page: 1},
		{raw: "https://archive.example.test/r9k/page/7/", kind: foolFuukaIndexTarget, board: "r9k", page: 7},
		{raw: "https://archive.example.test/a/search/text/a%20phrase/", kind: foolFuukaSearchTarget, board: "a", page: 1, query: "text=a+phrase"},
		{raw: "https://archive.example.test/a/search/text/thing/page/3/", kind: foolFuukaSearchTarget, board: "a", page: 3, query: "text=thing"},
		{raw: "https://archive.example.test/a/search/filename/x.jpg/deleted/not-deleted/", kind: foolFuukaSearchTarget, board: "a", page: 1, query: "deleted=not-deleted&filename=x.jpg"},
	}
	for _, tc := range tests {
		u, err := ParseURL(tc.raw)
		if err != nil {
			t.Fatal(err)
		}
		got, err := foolFuukaParse(u)
		if err != nil {
			t.Errorf("%s: %v", tc.raw, err)
			continue
		}
		if got.kind != tc.kind || got.board != tc.board || got.num != tc.num || got.page != tc.page {
			t.Errorf("%s: kind=%d board=%q num=%q page=%d, want kind=%d board=%q num=%q page=%d",
				tc.raw, got.kind, got.board, got.num, got.page, tc.kind, tc.board, tc.num, tc.page)
		}
		if tc.query != "" && got.search.Encode() != tc.query {
			t.Errorf("%s: query = %q, want %q", tc.raw, got.search.Encode(), tc.query)
		}
	}
}

// TestFoolFuukaParseRejectsWhatIsNotABoard guards the other direction. Every
// path here is one the site serves, and sending any of them to the API as
// though the first segment named a board would ask for a board that cannot
// exist.
func TestFoolFuukaParseRejectsWhatIsNotABoard(t *testing.T) {
	for _, raw := range []string{
		"https://archive.example.test/",
		"https://archive.example.test/foolfuuka/theme/style.css",
		"https://archive.example.test/_/search/text/global/",
		"https://archive.example.test/a/search/",
		"https://archive.example.test/a/gallery/",
		"https://archive.example.test/a/thread/",
	} {
		u, err := ParseURL(raw)
		if err != nil {
			t.Fatal(err)
		}
		if got, err := foolFuukaParse(u); err == nil {
			t.Errorf("%s was accepted as board %q kind %d", raw, got.board, got.kind)
		}
	}
}

func TestFoolFuukaIsBoard(t *testing.T) {
	tests := map[string]bool{
		"a":         true,
		"vt":        true,
		"r9k":       true,
		"trash":     true, // the longest 4chan board name
		"3":         true,
		"":          false,
		"_":         false,
		"gallery":   false,
		"foolfuuka": false,
		"A":         false,
		"a-b":       false,
	}
	for seg, want := range tests {
		if got := foolFuukaIsBoard(seg); got != want {
			t.Errorf("foolFuukaIsBoard(%q) = %v, want %v", seg, got, want)
		}
	}
}

// TestFoolFuukaSitesNameAndMatchTheirHosts covers the table, including the
// domain that is an alias rather than an install: a link to it has to resolve
// against the archive that absorbed it, not against itself.
func TestFoolFuukaSitesNameAndMatchTheirHosts(t *testing.T) {
	sites := NewFoolFuukaSites(nil, nil)
	if len(sites) != len(foolFuukaSites) {
		t.Fatalf("got %d extractors for %d installs", len(sites), len(foolFuukaSites))
	}
	for i, site := range foolFuukaSites {
		if got := sites[i].Name(); got != site.name {
			t.Errorf("%s is named %q, want %q", site.root, got, site.name)
		}
		for _, domain := range site.domains {
			u := &url.URL{Scheme: "https", Host: domain, Path: "/a/thread/1/"}
			if !sites[i].Match(u) {
				t.Errorf("%s did not match its own domain %s", site.name, domain)
			}
		}
		if sites[i].Match(&url.URL{Scheme: "https", Host: "unrelated.example.test"}) {
			t.Errorf("%s matched an unrelated host", site.name)
		}
	}
}

// TestFoolFuukaAliasResolvesAgainstItsArchive is the point of listing rbt.asia
// under desuarchive rather than as an install: everything on it redirects, so
// a request built against the pasted host would be one hop of guesswork.
func TestFoolFuukaAliasResolvesAgainstItsArchive(t *testing.T) {
	sites := NewFoolFuukaSites(nil, nil)
	u, err := ParseURL("https://rbt.asia/g/thread/1/")
	if err != nil {
		t.Fatal(err)
	}
	for _, ex := range sites {
		if !ex.Match(u) {
			continue
		}
		ff, ok := ex.(*FoolFuuka)
		if !ok {
			t.Fatalf("rbt.asia matched a %T", ex)
		}
		if ff.site.root != "https://desuarchive.org" {
			t.Errorf("rbt.asia is addressed at %s, want the archive it redirects to", ff.site.root)
		}
		return
	}
	t.Error("no extractor matched rbt.asia")
}

// foolFuukaFixture stands an install up on a local address serving canned
// responses. What that buys over the pure-function tests above is the paging:
// where the walk stops, and what it does with a page it has seen before, are
// decisions that only exist across several requests.
func foolFuukaFixture(t *testing.T, handler http.HandlerFunc) *FoolFuuka {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return &FoolFuuka{
		client: httpx.New(config.DefaultUserAgent, config.DefaultLanguage, 0, 10*time.Second),
		site:   foolFuukaSite{name: "example", root: srv.URL, domains: []string{"example.test"}},
	}
}

// foolFuukaSearchPage renders one page of results, numbered from base.
func foolFuukaSearchPage(base int) string {
	var posts []string
	for i := 0; i < 3; i++ {
		num := base + i
		posts = append(posts, fmt.Sprintf(`{"num":"%d","title":null,"media":{
			"media":"%d.jpg","media_filename":"upload.jpg",
			"media_link":"https://media.example.test/b/image/1/1/%d.jpg",
			"media_size":"100","media_status":"normal"}}`, num, num, num))
	}
	return `{"0":{"posts":[` + strings.Join(posts, ",") + `]},"meta":{"total_found":9}}`
}

// TestFoolFuukaSearchStopsWhenAPageRepeats covers a listing walked past its
// end. These archives answer that with the last page over again rather than
// with nothing, so a walk that only stopped at an empty page would collect
// the same three files until it hit the ceiling.
func TestFoolFuukaSearchStopsWhenAPageRepeats(t *testing.T) {
	var pages int
	f := foolFuukaFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, foolFuukaSearchAPI) {
			t.Errorf("unexpected request to %s", r.URL.Path)
		}
		pages++
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Query().Get("page") {
		case "1":
			_, _ = io.WriteString(w, foolFuukaSearchPage(100))
		default:
			// Every page from here on is page two, for ever.
			_, _ = io.WriteString(w, foolFuukaSearchPage(200))
		}
	})

	u, err := ParseURL("https://example.test/b/search/text/thing/")
	if err != nil {
		t.Fatal(err)
	}
	res, err := f.Extract(context.Background(), u, Options{})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if len(res.Files) != 6 {
		t.Errorf("got %d files, want the 6 distinct ones — a repeated page was collected twice",
			len(res.Files))
	}
	if pages > 3 {
		t.Errorf("the walk made %d requests; it should have stopped at the first page "+
			"that added nothing new", pages)
	}
	if res.Title != "b thing" {
		t.Errorf("title = %q, want the board and the search terms", res.Title)
	}
}

// TestFoolFuukaSearchReportsAnEmptyFirstPage separates the two meanings of
// "No results found.": at the end of a walk it is the end, and on the first
// page it is the answer.
func TestFoolFuukaSearchReportsAnEmptyFirstPage(t *testing.T) {
	f := foolFuukaFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"error":"No results found."}`)
	})

	u, err := ParseURL("https://example.test/b/search/text/nothing/")
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.Extract(context.Background(), u, Options{})
	if err == nil {
		t.Fatal("a search that matched nothing was accepted")
	}
	if !strings.Contains(err.Error(), "No results found") {
		t.Errorf("error %q does not carry the archive's own explanation", err)
	}
}

// TestFoolFuukaIndexOpensEveryThread is the point of the index route. The
// index endpoint returns opening posts only, so a board page taken at face
// value would collect one file per thread and silently leave the rest.
func TestFoolFuukaIndexOpensEveryThread(t *testing.T) {
	f := foolFuukaFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasPrefix(r.URL.Path, foolFuukaIndexAPI) {
			// Bump order, which is neither numeric nor the order a map would
			// hand back.
			_, _ = io.WriteString(w, `{
				"500":{"omitted":2,"images_omitted":1,"op":{"num":"500","title":"Second"}},
				"100":{"omitted":1,"images_omitted":1,"op":{"num":"100","title":"First"}}}`)
			return
		}
		num := r.URL.Query().Get("num")
		_, _ = io.WriteString(w, fmt.Sprintf(`{"%s":{
			"op":{"num":"%s","title":"T","media":{"media":"%s-op.jpg","media_filename":"a.jpg",
			 "media_link":"https://media.example.test/b/image/1/1/%s-op.jpg",
			 "media_size":"10","media_status":"normal"}},
			"posts":{"%s9":{"num":"%s9","title":null,"media":{
			 "media":"%s-reply.jpg","media_filename":"a.jpg",
			 "media_link":"https://media.example.test/b/image/1/1/%s-reply.jpg",
			 "media_size":"20","media_status":"normal"}}}}}`,
			num, num, num, num, num, num, num, num))
	})

	u, err := ParseURL("https://example.test/b/page/2/")
	if err != nil {
		t.Fatal(err)
	}
	res, err := f.Extract(context.Background(), u, Options{})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}

	var got []string
	for _, file := range res.Files {
		got = append(got, file.Dir+"/"+file.Name)
	}
	want := []string{
		// The board's order, and a folder per thread: a board page is
		// fifteen conversations, not one.
		"500/500-op.jpg", "500/500-reply.jpg",
		"100/100-op.jpg", "100/100-reply.jpg",
	}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("collected\n  %v\nwant\n  %v", got, want)
	}
	if res.Title != "example-b-page-2" {
		t.Errorf("title = %q, want the archive, board and page", res.Title)
	}
}

// TestFoolFuukaIndexSaysWhyAPageYieldedNothing separates a board whose
// threads are all text from a board none of whose threads would open. Only
// the second is worth trying again, and reporting it as the first is what
// sent me looking in the wrong place.
func TestFoolFuukaIndexSaysWhyAPageYieldedNothing(t *testing.T) {
	f := foolFuukaFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, foolFuukaIndexAPI) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"100":{"op":{"num":"100","title":"Text only"}}}`)
			return
		}
		// The thread endpoint behind a challenge, which is exactly what two
		// of these archives do while serving their index to anyone.
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, "<html><head><title>Just a moment...</title></head></html>")
	})

	u, err := ParseURL("https://example.test/b/")
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.Extract(context.Background(), u, Options{})
	if err == nil {
		t.Fatal("a page whose every thread was refused was accepted")
	}
	if !strings.Contains(err.Error(), "would open") {
		t.Errorf("error %q reads as though the threads held no files, "+
			"when in fact none of them could be opened", err)
	}
	if !strings.Contains(err.Error(), "403") {
		t.Errorf("error %q drops what the archive actually said", err)
	}
}

// TestFoolFuukaAcceptsExtraHosts covers the escape hatch for a list of
// volunteer-run archives that outlives no release intact.
func TestFoolFuukaAcceptsExtraHosts(t *testing.T) {
	cfg := &config.Config{ExtraHosts: map[string][]string{
		config.FamilyFoolFuuka: {"arch.example.test", "desuarchive.org"},
	}}
	sites := NewFoolFuukaSites(cfg, nil)

	if len(sites) != len(foolFuukaSites)+1 {
		t.Fatalf("got %d extractors, want the %d built in plus one — naming an "+
			"archive that is already covered must not register it twice",
			len(sites), len(foolFuukaSites))
	}
	added, ok := sites[len(sites)-1].(*FoolFuuka)
	if !ok {
		t.Fatalf("the added host is a %T", sites[len(sites)-1])
	}
	if added.Name() != "arch" {
		t.Errorf("the added host is named %q, want %q", added.Name(), "arch")
	}
	if added.site.root != "https://arch.example.test" {
		t.Errorf("the added host is addressed at %q", added.site.root)
	}
	u := &url.URL{Scheme: "https", Host: "arch.example.test", Path: "/g/thread/1/"}
	if !added.Match(u) {
		t.Error("the added host did not match itself")
	}
}
