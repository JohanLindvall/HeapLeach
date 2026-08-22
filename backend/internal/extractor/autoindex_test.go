package extractor

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

// The fixtures below are written by hand in the shape each server emits,
// cut down to a handful of entries and carrying the traps that decide the
// parsing: the parent link in each of its spellings, Apache's column-sort
// links, a name nginx truncates in the link text but not in the href, a date
// column that must not be mistaken for a size, and a trailing column that
// must not override the size cell before it.

// Apache's default listing: one <pre> block, sizes abbreviated to a bare
// letter, and anything under a kilobyte printed as a plain byte count.
const autoindexApachePre = `<!DOCTYPE HTML PUBLIC "-//W3C//DTD HTML 3.2 Final//EN">
<html>
 <head><title>Index of /pub</title></head>
 <body>
<h1>Index of /pub</h1>
<pre><img src="/icons/blank.gif" alt="Icon "> <a href="?C=N;O=D">Name</a>                      <a href="?C=M;O=A">Last modified</a>      <a href="?C=S;O=A">Size</a>  <a href="?C=D;O=A">Description</a><hr><img src="/icons/back.gif" alt="[PARENTDIR]"> <a href="/">Parent Directory</a>                                 -
<img src="/icons/folder.gif" alt="[DIR]"> <a href="clips/">clips/</a>                    2024-01-02 03:04    -
<img src="/icons/text.gif" alt="[TXT]"> <a href="notes.txt">notes.txt</a>                 2024-01-02 03:05  745
<img src="/icons/movie.gif" alt="[VID]"> <a href="a%20clip.mp4">a clip.mp4</a>            2024-01-02 03:06   14M
<hr></pre>
</body></html>`

// nginx's listing: one <pre> block, exact byte counts, and a long name cut
// short in the link text while the href keeps all of it.
const autoindexNginxPre = `<html>
<head><title>Index of /pub/</title></head>
<body>
<h1>Index of /pub/</h1><hr><pre><a href="../">../</a>
<a href="clips/">clips/</a>                                            02-Jan-2024 03:04                   -
<a href="a-name-long-enough-that-nginx-shortens-what-it-prints.bin">a-name-long-enough-that-nginx-shortens-what-it-p..&gt;</a> 02-Jan-2024 03:05             1474560
<a href="small.txt">small.txt</a>                                         02-Jan-2024 03:06                  12
</pre><hr></body>
</html>`

// Apache's IndexOptions HTMLTable mode, which is also the shape archive.org
// serves an item's files in. The description column holds free text that
// must not be read as a size.
const autoindexApacheTable = `<html><head><title>Index of /media</title></head><body>
<h1>Index of /media</h1>
<table>
 <tr><th valign="top"><img src="/icons/blank.gif" alt="[ICO]"></th><th><a href="?C=N;O=D">Name</a></th><th><a href="?C=M;O=A">Last modified</a></th><th><a href="?C=S;O=A">Size</a></th><th><a href="?C=D;O=A">Description</a></th></tr>
 <tr><th colspan="5"><hr></th></tr>
 <tr><td valign="top"><img src="/icons/back.gif" alt="[PARENTDIR]"></td><td><a href="/">Parent Directory</a></td><td>&nbsp;</td><td align="right">  - </td><td>&nbsp;</td></tr>
 <tr><td valign="top"><img src="/icons/folder.gif" alt="[DIR]"></td><td><a href="raw/">raw/</a></td><td align="right">2024-01-02 03:04  </td><td align="right">  - </td><td>&nbsp;</td></tr>
 <tr><td valign="top"><img src="/icons/movie.gif" alt="[VID]"></td><td><a href="reel.mkv">reel.mkv</a></td><td align="right">2024-01-02 03:05  </td><td align="right">1.4G</td><td>&nbsp;</td></tr>
 <tr><td valign="top"><img src="/icons/text.gif" alt="[TXT]"></td><td><a href="readme">readme</a></td><td align="right">2024-01-02 03:06  </td><td align="right">745 </td><td>2024 release notes</td></tr>
</table>
</body></html>`

// lighttpd's listing: a table whose last column is a MIME type, sitting
// after the size where a careless read would pick it up instead.
const autoindexLighttpdTable = `<html><head><title>Index of /files/</title></head><body>
<h2>Index of /files/</h2>
<table summary="Directory Listing" cellpadding="0" cellspacing="0">
<thead><tr><th class="n">Name</th><th class="m">Last Modified</th><th class="s">Size</th><th class="t">Type</th></tr></thead>
<tbody>
<tr class="d"><td class="n"><a href="../">Parent Directory</a>/</td><td class="m">&nbsp;</td><td class="s">- &nbsp;</td><td class="t">Directory</td></tr>
<tr class="d"><td class="n"><a href="raw/">raw</a>/</td><td class="m">2024-Jan-02 03:04:05</td><td class="s">- &nbsp;</td><td class="t">Directory</td></tr>
<tr><td class="n"><a href="song.mp3">song.mp3</a></td><td class="m">2024-Jan-02 03:04:05</td><td class="s">4.2M</td><td class="t">audio/mpeg</td></tr>
</tbody></table>
</body></html>`

// An ordinary page that links to its own subpaths, which is what most of the
// web is. Nothing about the links distinguishes it from a listing; only the
// title does.
const autoindexNotAListing = `<html><head><title>Field notes | Example</title></head><body>
<h1>Field notes</h1>
<ul>
 <li><a href="first-post">The first post</a></li>
 <li><a href="second-post">The second post</a></li>
</ul>
</body></html>`

// autoindexRounded is what an abbreviated size comes to. Written as the
// parser writes it because the unit and the rounding flag are what is under
// test, not float64's last digit.
func autoindexRounded(value float64, unit int64) int64 { return int64(value * float64(unit)) }

func autoindexParse(t *testing.T, doc, base string) ([]autoindexEntry, *url.URL) {
	t.Helper()
	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	root, err := parseHTML(doc)
	if err != nil {
		t.Fatal(err)
	}
	b := autoindexBase(u)
	if !autoindexIsListing(root, b) {
		t.Fatalf("%s was not recognised as a directory listing", base)
	}
	return autoindexEntries(root, b), b
}

// TestAutoindexReadsApachePreListing pins the default Apache shape: the
// parent link and the column-sort links are not entries, an abbreviated size
// is a rounded hint, and a plain byte count is exact.
func TestAutoindexReadsApachePreListing(t *testing.T) {
	entries, _ := autoindexParse(t, autoindexApachePre, "https://files.example.test/pub")

	if len(entries) != 3 {
		t.Fatalf("read %d entries, want 3 (the parent and the sort links are not entries): %+v", len(entries), entries)
	}
	if !entries[0].dir || entries[0].name != "clips" {
		t.Errorf("first entry = %+v, want the clips subdirectory", entries[0])
	}
	if want := "https://files.example.test/pub/clips/"; entries[0].url.String() != want {
		t.Errorf("subdirectory url = %q, want %q", entries[0].url, want)
	}

	// Apache prints anything under a kilobyte as the byte count itself, and
	// a number with no unit has not been rounded.
	if entries[1].size != 745 || entries[1].approx {
		t.Errorf("notes.txt size = %d approx = %v, want 745 exactly", entries[1].size, entries[1].approx)
	}
	// A number with a unit has been cut to three digits and can only be a hint.
	if entries[2].size != 14<<20 || !entries[2].approx {
		t.Errorf("clip size = %d approx = %v, want a rounded 14M", entries[2].size, entries[2].approx)
	}
	// The name is decoded for disk; the URL keeps the encoding it was given.
	if entries[2].name != "a clip.mp4" {
		t.Errorf("name = %q, want the percent-decoded name", entries[2].name)
	}
	if want := "https://files.example.test/pub/a%20clip.mp4"; entries[2].url.String() != want {
		t.Errorf("url = %q, want %q", entries[2].url, want)
	}
}

// TestAutoindexReadsNginxPreListing pins the nginx shape, where the size is
// an exact byte count and the link text is not the name.
func TestAutoindexReadsNginxPreListing(t *testing.T) {
	entries, _ := autoindexParse(t, autoindexNginxPre, "https://files.example.test/pub/")

	if len(entries) != 3 {
		t.Fatalf("read %d entries, want 3 (../ is not an entry): %+v", len(entries), entries)
	}
	// nginx shortens what it prints, never what it links, so the href is the
	// only place the whole name exists.
	long := entries[1]
	if long.name != "a-name-long-enough-that-nginx-shortens-what-it-prints.bin" {
		t.Errorf("name = %q, want the full name from the href rather than the printed one", long.name)
	}
	if long.size != 1474560 || long.approx {
		t.Errorf("size = %d approx = %v, want 1474560 exactly", long.size, long.approx)
	}
	if entries[2].size != 12 || entries[2].approx {
		t.Errorf("small.txt size = %d approx = %v, want 12 exactly", entries[2].size, entries[2].approx)
	}
	if !entries[0].dir {
		t.Errorf("clips/ read as a file")
	}
}

// TestAutoindexReadsApacheTableListing pins the HTMLTable shape, where the
// size is a cell rather than the rest of the line.
func TestAutoindexReadsApacheTableListing(t *testing.T) {
	entries, _ := autoindexParse(t, autoindexApacheTable, "https://files.example.test/media/")

	if len(entries) != 3 {
		t.Fatalf("read %d entries, want 3: %+v", len(entries), entries)
	}
	if !entries[0].dir || entries[0].name != "raw" {
		t.Errorf("first entry = %+v, want the raw subdirectory", entries[0])
	}
	if entries[1].size != autoindexRounded(1.4, 1<<30) || !entries[1].approx {
		t.Errorf("reel.mkv size = %d approx = %v, want a rounded 1.4G", entries[1].size, entries[1].approx)
	}
	// The date cell must not read as a size, and the free text after the
	// size cell must not replace it.
	if entries[2].size != 745 || entries[2].approx {
		t.Errorf("readme size = %d approx = %v, want the size cell taken exactly, "+
			"neither the date before it nor the notes after it", entries[2].size, entries[2].approx)
	}
}

// TestAutoindexReadsLighttpdTableListing pins the variant whose last column
// is a MIME type.
func TestAutoindexReadsLighttpdTableListing(t *testing.T) {
	entries, _ := autoindexParse(t, autoindexLighttpdTable, "https://files.example.test/files/")

	if len(entries) != 2 {
		t.Fatalf("read %d entries, want 2 (the parent is not one): %+v", len(entries), entries)
	}
	if !entries[0].dir || entries[0].name != "raw" {
		t.Errorf("first entry = %+v, want the raw subdirectory", entries[0])
	}
	if entries[1].size != autoindexRounded(4.2, 1<<20) || !entries[1].approx {
		t.Errorf("song.mp3 size = %d approx = %v, want a rounded 4.2M and not the MIME type",
			entries[1].size, entries[1].approx)
	}
}

// TestAutoindexRecognisesAListing covers the one decision that has to be
// right, in both directions: a page of links to its own subpaths is not a
// listing unless it says it is.
func TestAutoindexRecognisesAListing(t *testing.T) {
	tests := []struct {
		name string
		doc  string
		base string
		want bool
	}{
		{"apache and nginx say it verbatim", autoindexApachePre, "https://files.example.test/pub/", true},
		{"lighttpd puts it in an h2", autoindexLighttpdTable, "https://files.example.test/files/", true},
		{
			"python's server words it differently",
			`<html><head><title>Directory listing for /pub/</title></head><body></body></html>`,
			"https://files.example.test/pub/", true,
		},
		{
			"archive.org trails the words after the item name",
			`<html><head><title>an-item directory listing</title></head><body></body></html>`,
			"https://archive.org/download/an-item/", true,
		},
		{
			"a gateway names the page after the directory it lists",
			`<html><head><title>/ipfs/bafyexamplecid/</title></head><body></body></html>`,
			"https://gateway.example.test/ipfs/bafyexamplecid/", true,
		},
		{"an ordinary page of links is not one", autoindexNotAListing, "https://blog.example.test/notes/", false},
		{
			"nor is one titled after its last segment alone",
			`<html><head><title>notes</title></head><body></body></html>`,
			"https://blog.example.test/notes/", false,
		},
		{
			"nor is one with no title at all",
			`<html><body><a href="a.bin">a.bin</a></body></html>`,
			"https://blog.example.test/", false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			u, err := url.Parse(tc.base)
			if err != nil {
				t.Fatal(err)
			}
			root, err := parseHTML(tc.doc)
			if err != nil {
				t.Fatal(err)
			}
			if got := autoindexIsListing(root, autoindexBase(u)); got != tc.want {
				t.Errorf("autoindexIsListing = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestAutoindexChildRejectsEverythingOutsideTheDirectory is the containment
// guarantee: whatever a listing asks for, only a URL one segment below the
// directory is ever taken.
func TestAutoindexChildRejectsEverythingOutsideTheDirectory(t *testing.T) {
	base, err := url.Parse("https://files.example.test/pub/")
	if err != nil {
		t.Fatal(err)
	}

	accepted := map[string]string{
		"clip.mp4":                              "clip.mp4",
		"clips/":                                "clips",
		"a%20clip.mp4":                          "a clip.mp4",
		"./clip.mp4":                            "clip.mp4",
		"/pub/clip.mp4":                         "clip.mp4",
		"https://files.example.test/pub/clip.4": "clip.4",
	}
	for href, want := range accepted {
		name, target, ok := autoindexChild(base, href)
		if !ok {
			t.Errorf("autoindexChild(%q) rejected an entry of this directory", href)
			continue
		}
		if name != want {
			t.Errorf("autoindexChild(%q) named it %q, want %q", href, name, want)
		}
		if !strings.HasPrefix(target.Path, base.Path) {
			t.Errorf("autoindexChild(%q) resolved to %s, outside %s", href, target, base)
		}
	}

	rejected := []string{
		"",                                    // an empty href
		"../",                                 // the parent, nginx's spelling
		"/",                                   // the parent, Apache's spelling
		"/pub/",                               // the directory itself
		"..",                                  // the parent with no slash
		"?C=N;O=D",                            // Apache's column-sort link
		"clip.mp4?download=1",                 // anything carrying a query
		"#top",                                // an in-page anchor
		"/other/clip.mp4",                     // elsewhere on the host
		"//elsewhere.example.test/pub/clip.4", // another host entirely
		"https://elsewhere.example.test/pub/clip.4",
		"mailto:nobody@example.test",
		"javascript:void(0)",
		"deeper/still/clip.mp4", // more than one segment below
		"a%2Fb",                 // a separator hidden in an escape
	}
	for _, href := range rejected {
		if name, target, ok := autoindexChild(base, href); ok {
			t.Errorf("autoindexChild(%q) accepted %q at %s, want it rejected", href, name, target)
		}
	}
}

// TestAutoindexSizeSeparatesExactFromRounded pins the distinction the
// downloader acts on: an exact length may settle that a file on disk is
// already this one, a rounded one may not.
func TestAutoindexSizeSeparatesExactFromRounded(t *testing.T) {
	tests := []struct {
		text   string
		size   int64
		approx bool
	}{
		{"  2024-01-02 03:05  745   ", 745, false},       // Apache, under a kilobyte
		{"02-Jan-2024 03:05    1474560", 1474560, false}, // nginx, always exact
		{"0", 0, false},
		{"2024-01-02 03:06   14M  ", 14 << 20, true},
		{"1.4G", autoindexRounded(1.4, 1<<30), true},
		{"404K", 404 << 10, true},
		{"4.2m", autoindexRounded(4.2, 1<<20), true}, // some templates lowercase it
		{"1.4 MiB", autoindexRounded(1.4, 1<<20), true},
		{"57.80 MB", autoindexRounded(57.80, 1<<20), true},
		{"  - ", -1, false},                 // a directory, or a size the server withheld
		{"2024-01-02 03:04  ", -1, false},   // a date column on its own
		{"2024-Jan-02 03:04:05", -1, false}, // and lighttpd's spelling of one
		{"audio/mpeg", -1, false},           // a MIME type column
		{"", -1, false},
		{"&nbsp;", -1, false},
	}
	for _, tc := range tests {
		size, approx := autoindexSize(tc.text)
		if size != tc.size || approx != tc.approx {
			t.Errorf("autoindexSize(%q) = %d, approx %v; want %d, approx %v",
				tc.text, size, approx, tc.size, tc.approx)
		}
	}
}

// TestAutoindexMatchesArchiveOrgItems pins the one host shape claimed by URL
// alone. A file inside an item must stay with the direct downloader.
func TestAutoindexMatchesArchiveOrgItems(t *testing.T) {
	a := NewAutoindex(nil)
	tests := map[string]bool{
		"https://archive.org/download/an-item":          true,
		"https://archive.org/download/an-item/":         true,
		"https://archive.org/download/an-item/raw/":     true,
		"https://archive.org/download/an-item/file.mp4": false,
		"https://archive.org/details/an-item":           false,
		"https://archive.org/download":                  false,
		"https://files.example.test/pub/":               false,
	}
	for raw, want := range tests {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		if got := a.Match(u); got != want {
			t.Errorf("Match(%s) = %v, want %v", raw, got, want)
		}
	}
}

// ------------------------------------------------------------- crawling

// autoindexServer serves a fixed set of listings, and 404s anything else.
func autoindexServer(t *testing.T, pages map[string]string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		doc, ok := pages[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, doc)
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

func autoindexTestExtractor() *Autoindex {
	return NewAutoindex(httpx.New("test-agent", "en-US", 0, 5*time.Second))
}

// autoindexListing renders a listing in the nginx shape from a list of raw
// rows, so a test can describe a tree in one line per entry.
func autoindexListing(path string, rows ...string) string {
	return fmt.Sprintf("<html><head><title>Index of %s</title></head><body><pre><a href=\"../\">../</a>\n%s</pre></body></html>",
		path, strings.Join(rows, "\n")+"\n")
}

func autoindexEntryRow(name string, size string) string {
	return fmt.Sprintf(`<a href="%s">%s</a>    02-Jan-2024 03:04    %s`, name, name, size)
}

func autoindexCrawl(t *testing.T, base string) (*Result, error) {
	t.Helper()
	u, err := ParseURL(base)
	if err != nil {
		t.Fatal(err)
	}
	return autoindexTestExtractor().crawl(context.Background(), u)
}

// TestAutoindexCrawlsSubdirectoriesInOrder pins both the recursion and the
// order it produces: a directory's own files, then each subdirectory where
// the listing named it, with the tree reproduced in Dir.
func TestAutoindexCrawlsSubdirectoriesInOrder(t *testing.T) {
	srv, hits := autoindexServer(t, map[string]string{
		"/pub/": autoindexListing("/pub/",
			autoindexEntryRow("first.bin", "1024"),
			autoindexEntryRow("clips/", "-"),
			autoindexEntryRow("last.bin", "2048"),
		),
		"/pub/clips/": autoindexListing("/pub/clips/",
			autoindexEntryRow("inner.mp4", "4096"),
			autoindexEntryRow("deeper/", "-"),
		),
		"/pub/clips/deeper/": autoindexListing("/pub/clips/deeper/",
			autoindexEntryRow("bottom.mp4", "8192"),
		),
	})

	res, err := autoindexCrawl(t, srv.URL+"/pub/")
	if err != nil {
		t.Fatalf("crawl: %v", err)
	}
	if res.Title != "pub" {
		t.Errorf("title = %q, want the directory's own name", res.Title)
	}

	type row struct {
		name string
		dir  string
		size int64
	}
	want := []row{
		{"first.bin", "", 1024},
		{"last.bin", "", 2048},
		{"inner.mp4", "clips", 4096},
		{"bottom.mp4", "clips/deeper", 8192},
	}
	if len(res.Files) != len(want) {
		t.Fatalf("found %d files, want %d: %+v", len(res.Files), len(want), res.Files)
	}
	for i, w := range want {
		got := res.Files[i]
		if got.Name != w.name || got.Dir != w.dir || got.Size != w.size || got.SizeApprox {
			t.Errorf("file %d = %q in %q, %d bytes (approx %v); want %q in %q, %d bytes exactly",
				i, got.Name, got.Dir, got.Size, got.SizeApprox, w.name, w.dir, w.size)
		}
		if !strings.HasPrefix(got.URL, srv.URL+"/pub/") {
			t.Errorf("file %d url = %q, which is not under the directory crawled", i, got.URL)
		}
	}
	if n := hits.Load(); n != 3 {
		t.Errorf("made %d requests, want one per directory", n)
	}
}

// TestAutoindexStopsAtTheDepthCap covers the tree that does not end: a
// directory containing itself, which a symlink produces and which no listing
// ever admits to. Every traversal has a fresh, longer URL, so the visited set
// cannot see it and the depth cap is what stops it.
func TestAutoindexStopsAtTheDepthCap(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		fmt.Fprint(w, autoindexListing(r.URL.Path,
			autoindexEntryRow("file.bin", "1024"),
			autoindexEntryRow("self/", "-"),
		))
	}))
	t.Cleanup(srv.Close)

	res, err := autoindexCrawl(t, srv.URL+"/pub/")
	if err != nil {
		t.Fatalf("crawl: %v", err)
	}
	want := config.MaxAutoindexDepth + 1 // the directory asked for, plus each level below it
	if len(res.Files) != want {
		t.Errorf("found %d files, want %d — one per level down to the depth cap", len(res.Files), want)
	}
	if n := int(hits.Load()); n != want {
		t.Errorf("made %d requests, want %d", n, want)
	}
}

// TestAutoindexStopsAtTheFileCap keeps one very large directory from
// becoming a job nobody meant to queue.
func TestAutoindexStopsAtTheFileCap(t *testing.T) {
	rows := make([]string, 0, config.MaxAutoindexFiles+1)
	for i := 0; i <= config.MaxAutoindexFiles; i++ {
		rows = append(rows, autoindexEntryRow(fmt.Sprintf("file-%05d.bin", i), "1024"))
	}
	srv, _ := autoindexServer(t, map[string]string{"/pub/": autoindexListing("/pub/", rows...)})

	res, err := autoindexCrawl(t, srv.URL+"/pub/")
	if err != nil {
		t.Fatalf("crawl: %v", err)
	}
	if len(res.Files) != config.MaxAutoindexFiles {
		t.Errorf("found %d files, want the cap of %d", len(res.Files), config.MaxAutoindexFiles)
	}
	// Truncation takes the tail, never the head: the listing's own order is
	// what the user sees, so the first file must still be the first.
	if res.Files[0].Name != "file-00000.bin" {
		t.Errorf("first file = %q, want the listing's first entry", res.Files[0].Name)
	}
}

// TestAutoindexDescendVisitsADirectoryOnce pins the cycle guard on its own
// terms: the same resolved directory is never read twice, however many
// listings name it.
func TestAutoindexDescendVisitsADirectoryOnce(t *testing.T) {
	base, err := url.Parse("https://files.example.test/pub/clips/")
	if err != nil {
		t.Fatal(err)
	}
	node := &autoindexNode{
		base:    autoindexBase(base),
		pending: []autoindexEntry{{name: "clips", url: base, dir: true}},
	}
	visited := map[string]bool{}
	budget := autoindexBudget{}

	if next := autoindexDescend([]*autoindexNode{node}, visited, &budget); len(next) != 1 {
		t.Fatalf("first descent produced %d directories, want 1", len(next))
	}
	node.pending = []autoindexEntry{{name: "clips", url: base, dir: true}}
	if next := autoindexDescend([]*autoindexNode{node}, visited, &budget); len(next) != 0 {
		t.Errorf("a directory already read was descended into again")
	}

	// And a spent budget stops the descent whatever the listing named.
	budget = autoindexBudget{dirs: config.MaxAutoindexDirs}
	node.pending = []autoindexEntry{{name: "other", url: base, dir: true}}
	if next := autoindexDescend([]*autoindexNode{node}, map[string]bool{}, &budget); len(next) != 0 {
		t.Errorf("the descent continued past the directory cap")
	}
}

// TestAutoindexSkipsASubdirectoryThatWillNotLoad keeps one bad branch from
// costing the whole tree.
func TestAutoindexSkipsASubdirectoryThatWillNotLoad(t *testing.T) {
	srv, _ := autoindexServer(t, map[string]string{
		"/pub/": autoindexListing("/pub/",
			autoindexEntryRow("kept.bin", "1024"),
			autoindexEntryRow("gone/", "-"),
		),
	})

	res, err := autoindexCrawl(t, srv.URL+"/pub/")
	if err != nil {
		t.Fatalf("crawl: %v", err)
	}
	if len(res.Files) != 1 || res.Files[0].Name != "kept.bin" {
		t.Errorf("got %+v, want the one file the readable directory listed", res.Files)
	}
}

// TestAutoindexRefusesAPageThatIsNotAListing is the failure that matters: a
// page wrongly taken for a listing is a file the user asked for and did not
// get.
func TestAutoindexRefusesAPageThatIsNotAListing(t *testing.T) {
	srv, _ := autoindexServer(t, map[string]string{"/notes/": autoindexNotAListing})

	if _, err := autoindexCrawl(t, srv.URL+"/notes/"); err == nil {
		t.Fatal("an ordinary page of links resolved as a directory")
	}

	u, err := ParseURL(srv.URL + "/notes/")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := autoindexSniff(context.Background(), httpx.New("test-agent", "en-US", 0, 5*time.Second), u); ok {
		t.Error("the sniff claimed a page that is not a listing")
	}
}

// TestAutoindexSniffLeavesFileURLsAlone: the fallback downloads files, and a
// URL with no trailing slash is not a directory. Spending a speculative GET
// on one would be a poor trade — some are only good once.
func TestAutoindexSniffLeavesFileURLsAlone(t *testing.T) {
	srv, hits := autoindexServer(t, map[string]string{"/pub/": autoindexListing("/pub/", autoindexEntryRow("a.bin", "1024"))})

	u, err := ParseURL(srv.URL + "/pub/a.bin")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := autoindexSniff(context.Background(), httpx.New("test-agent", "en-US", 0, 5*time.Second), u); ok {
		t.Error("the sniff claimed a URL that is not a directory")
	}
	if n := hits.Load(); n != 0 {
		t.Errorf("the sniff made %d requests for a file URL, want none", n)
	}
}

// TestAutoindexSniffResolvesAnOpenDirectory is the shape the whole extractor
// exists for: a directory on a host nobody has heard of.
func TestAutoindexSniffResolvesAnOpenDirectory(t *testing.T) {
	srv, _ := autoindexServer(t, map[string]string{
		"/pub/": autoindexListing("/pub/", autoindexEntryRow("a.bin", "1024"), autoindexEntryRow("b.bin", "2048")),
	})

	u, err := ParseURL(srv.URL + "/pub/")
	if err != nil {
		t.Fatal(err)
	}
	res, ok := autoindexSniff(context.Background(), httpx.New("test-agent", "en-US", 0, 5*time.Second), u)
	if !ok {
		t.Fatal("an open directory was not recognised")
	}
	if len(res.Files) != 2 {
		t.Errorf("found %d files, want 2: %+v", len(res.Files), res.Files)
	}
}

// TestAutoindexResolvesAgainstTheDirectory covers the URL served without a
// trailing slash, where resolving a relative link against the path as given
// would put every file in the parent directory.
func TestAutoindexResolvesAgainstTheDirectory(t *testing.T) {
	srv, _ := autoindexServer(t, map[string]string{
		"/pub/": autoindexListing("/pub/", autoindexEntryRow("a.bin", "1024")),
	})

	res, err := autoindexCrawl(t, srv.URL+"/pub")
	if err != nil {
		t.Fatalf("crawl: %v", err)
	}
	if want := srv.URL + "/pub/a.bin"; res.Files[0].URL != want {
		t.Errorf("url = %q, want %q", res.Files[0].URL, want)
	}
}
