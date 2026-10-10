// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// Synthetic classic-theme markup, including pagination outside main, as in
// the supplied browser archive. No content names or URLs from it are retained.
func wordPressIndexHTML(next string, posts ...string) string {
	var out strings.Builder
	out.WriteString(`<html><head><meta name="generator" content="WordPress 4.9"></head>
<body class="archive category"><header><a href="/navigation/">Menu</a></header>
<div id="primary"><h1 class="page-title ast-archive-title">Example Category</h1><main class="site-main">`)
	for _, post := range posts {
		fmt.Fprintf(&out, `<article class="post type-post hentry">
<a href="%s"><img src="/preview.jpg"></a><h2 class="entry-title"><a rel="bookmark" href="%s">Post title</a></h2>
<a rel="category tag" href="/category/unrelated/">Unrelated category</a><a href="/profile/">Author</a>
<div class="entry-content"></div></article>`, post, post)
	}
	out.WriteString(`</main><div class="ast-pagination"><nav class="navigation pagination">`)
	if next != "" {
		fmt.Fprintf(&out, `<a class="next page-numbers" href="%s">Next</a>`, next)
	}
	out.WriteString(`</nav></div></div><aside><article class="hentry"><h2 class="entry-title"><a href="/sidebar/">Related</a></h2></article></aside></body></html>`)
	return out.String()
}

func wordPressPostHTML(title, content string) string {
	return `<main><article class="post"><h1 class="entry-title">` + title + `</h1>
<img src="/featured-preview.jpg"><div class="entry-content">` + content + `
<div class="crp_related"><a href="https://keep2share.example.test/file/recommendation">Related</a><img src="/related.jpg"></div>
</div></article><aside><a href="https://keep2share.example.test/file/sidebar">Sidebar</a><img src="/avatar.jpg"></aside></main>`
}

func wordPressTestSite(t *testing.T, pages map[string]string) (*Registry, *Direct, string, func() map[string]int) {
	t.Helper()
	var mu sync.Mutex
	requests := make(map[string]int)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests[r.URL.RequestURI()]++
		mu.Unlock()
		if strings.HasPrefix(r.URL.Path, "/api/") {
			if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/getFileStatus") {
				t.Errorf("extraction attempted a download ticket: %s", r.URL.Path)
				http.Error(w, "unexpected API call", http.StatusBadRequest)
				return
			}
			var in map[string]string
			if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
				t.Error(err)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status": "success", "name": in["id"] + ".bin", "size": 1234,
				"is_available": in["id"] != "removed", "isAvailableForFree": true,
			})
			return
		}
		if doc, ok := pages[r.URL.RequestURI()]; ok {
			w.Header().Set(httpx.HeaderContentType, "text/html; charset=utf-8")
			fmt.Fprint(w, doc)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	client := httpx.New("test-agent", "en-US", 0, time.Second)
	reg := NewRegistry(&config.Config{}, client)
	for _, ex := range reg.extractors {
		if k, ok := ex.(*Keep2Share); ok {
			k.api = srv.URL + "/api/" + k.Name()
			k.hostSet = hostSet{k.Name() + ".example.test", k.Name() + "-alias.example.test"}
		}
	}
	return reg, reg.fallback.(*Direct), srv.URL, func() map[string]int {
		mu.Lock()
		defer mu.Unlock()
		return maps.Clone(requests)
	}
}

func TestWordPressCategoryCollectsPostDownloadsAndOriginalJPEGs(t *testing.T) {
	pages := map[string]string{
		"/category/example/":        wordPressIndexHTML("/category/example/page/2/", "/first/", "/second/"),
		"/category/example/page/2/": wordPressIndexHTML("", "/first/?utm_source=repeat#top", "/third/"),
		"/first/": wordPressPostHTML("First Post - Part One", `
<a href="https://keep2share.example.test/file/first">Download</a>
<a href="https://keep2share-alias.example.test/file/first/First.bin">Duplicate</a>
<p>https://fileboom.example.test/file/first</p>
<a href="/uploads/original.jpeg?token=example"><img src="/uploads/thumbnail.jpg"></a>
<img src="/uploads/placeholder.gif" data-src="/uploads/lazy.JPG">
<img src="/uploads/small.jpg" srcset="/uploads/small.jpg 300w, /uploads/large.jpg 1600w">
<img src="/uploads/lazy.JPG"><a href="/other-post/">Navigation</a>`),
		"/second/": wordPressPostHTML("Second Post", `<img src="/uploads/second.jpeg">`),
		"/third/":  wordPressPostHTML("Third Post", `<a href="https://keep2share.example.test/file/third">Download</a>`),
	}
	reg, _, base, asked := wordPressTestSite(t, pages)
	res, _, err := reg.Extract(context.Background(), base+"/category/example/", Options{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Title != "Example Category" || res.Note != "" || len(res.Files) != 7 {
		t.Fatalf("unexpected result: %+v", res)
	}
	for i, service := range []string{"keep2share", "fileboom"} {
		f := res.Files[i]
		if f.Name != "first.bin" || f.URL != "" || f.Resolve == nil || f.Size != 1234 || f.Pace == nil ||
			*f.Pace != (Pace{Streams: 1, Files: 1, Group: service, PerRoute: true}) {
			t.Fatalf("download behavior lost for %s: %+v", service, f)
		}
		if !strings.HasPrefix(f.Dir, "First Post - Part One/") {
			t.Fatalf("post title was truncated: %q", f.Dir)
		}
	}
	for i, suffix := range []string{"original.jpeg?token=example", "lazy.JPG", "large.jpg"} {
		f := res.Files[i+2]
		if f.URL != base+"/uploads/"+suffix || f.Dir != "First Post - Part One" || f.Size != -1 ||
			f.Headers[httpx.HeaderReferer] != base+"/first/" {
			t.Fatalf("JPEG %d: %+v", i, f)
		}
	}
	requests := asked()
	for _, post := range []string{"/first/", "/second/", "/third/"} {
		if requests[post] != 1 {
			t.Fatalf("post fetched %d times: %s", requests[post], post)
		}
	}
	for request := range requests {
		if _, ok := pages[request]; !ok && !strings.HasPrefix(request, "/api/") {
			t.Fatalf("followed navigation or fetched image bytes: %s", request)
		}
	}
}

func TestWordPressVersionsThemesAndCategoryPermalinks(t *testing.T) {
	for _, tc := range []struct {
		name, start, first, next, template, post string
	}{
		{"classic", "/blog/category/parent/child/page/7/", "/blog/category/parent/child/", "/blog/category/parent/child/page/2/",
			`<meta name="generator" content="WordPress 2.9"><body class="archive category"><div id="content"><h1 class="archive-title">Archive</h1><div class="hentry"><a rel="bookmark" href="/first/">First</a></div><div class="nav-previous"><a href="%s">Older posts</a></div></div>`,
			`<div id="content"><h1 class="entry-title">A Post</h1><div itemprop="articleBody"><img src="/original.jpg"></div></div>`},
		{"query category", "/?cat=42&paged=7", "/?cat=42", "/?cat=42&paged=2",
			`<meta name="generator" content="WordPress 5.0"><body class="category"><main><h1 class="page-title">Archive</h1><article><h2 class="entry-title"><a href="/first/">First</a></h2></article><a rel="next" href="%s">Next</a></main>`,
			wordPressPostHTML("A Post", `<img src="/original.jpg">`)},
		{"block custom base", "/journal/topics/example/?query-8-page=7", "/journal/topics/example/", "/journal/topics/example/?query-8-page=2",
			`<body class="archive category"><main><h1 class="wp-block-query-title">Archive</h1><ul class="wp-block-post-template"><li class="wp-block-post"><h2 class="wp-block-post-title"><a href="/first/">First</a></h2></li></ul><ul class="wp-block-post-template"><li class="wp-block-post"><h2 class="wp-block-post-title"><a href="/recommendation/">Other query</a></h2></li></ul><a class="wp-block-query-pagination-next" href="%s">Next</a></main>`,
			`<main><h1 class="wp-block-post-title">A Post</h1><div class="wp-block-post-content"><figure class="wp-block-image"><img data-orig-file="/original.jpg" src="/small.jpg"></figure></div></main>`},
		{"generator omitted", "/category/example/page/7/", "/category/example/", "/category/example/page/2/",
			`<link rel="stylesheet" href="/wp-content/themes/example/style.css"><main><h1 class="page-title">Archive</h1><article><h2 class="entry-title"><a href="/first/">First</a></h2></article><a class="nextpostslink" href="%s">Next</a></main>`,
			wordPressPostHTML("A Post", `<img src="/original.jpg">`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			initial := fmt.Sprintf(tc.template, tc.next)
			last := strings.ReplaceAll(fmt.Sprintf(tc.template, ""), `href="/first/"`, `href="/second/"`)
			// End the listing with no next control, not a link to the same page.
			root, _ := parseHTML(last)
			for _, node := range findAll(root, func(n *html.Node) bool { return attr(n, "href") == "" && isElem(n, atom.A) }) {
				node.Parent.RemoveChild(node)
			}
			var end strings.Builder
			_ = html.Render(&end, root)
			pages := map[string]string{tc.start: initial, tc.first: initial, tc.next: end.String(), "/first/": tc.post, "/second/": tc.post}
			_, direct, base, asked := wordPressTestSite(t, pages)
			u, _ := url.Parse(base + tc.start)
			res, err := direct.pageSniff(context.Background(), u, Options{})
			if err != nil || res == nil {
				t.Fatalf("category not recognised: result=%+v err=%v", res, err)
			}
			if len(res.Files) != 2 || res.Note != "" || res.Title != "Archive" {
				t.Fatalf("unexpected result: %+v", res)
			}
			if res.Files[0].Dir != "A Post [first]" || res.Files[1].Dir != "A Post [second]" {
				t.Fatalf("duplicate post titles share a folder: %+v", res.Files)
			}
			requests := asked()
			if requests[tc.first] != 1 || requests[tc.next] != 1 || requests["/recommendation/"] != 0 {
				t.Fatalf("wrong category traversal: %v", requests)
			}
		})
	}
}

func TestWordPressListingLimitsAndFailuresAreVisible(t *testing.T) {
	for _, tc := range []struct {
		name, next, second string
		limits             Limits
		want               int
		note               string
	}{
		{"source cap", "/category/example/page/2/", "", Limits{Sources: 1}, 1, "source limit"},
		{"file cap", "", "", Limits{Files: 1}, 1, "file limit"},
		{"dead post", "", "missing", Limits{}, 1, "1 of 2 posts"},
		{"next unavailable", "/category/example/page/2/", "", Limits{}, 2, "could not fetch next page"},
		{"repeated page", "/category/example/", "", Limits{}, 2, "repeated page"},
		{"other category", "/category/other/page/2/", "", Limits{}, 2, "invalid next page"},
		{"external pager", "https://elsewhere.example.test/category/example/page/2/", "", Limits{}, 2, "invalid next page"},
		{"incomplete post", "", "partial", Limits{}, 2, "incomplete downloads"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pages := map[string]string{
				"/category/example/": wordPressIndexHTML(tc.next, "/first/", "/second/"),
				"/first/":            wordPressPostHTML("First", `<img src="/first.jpg">`),
				"/second/":           wordPressPostHTML("Second", `<img src="/second.jpg">`),
			}
			if tc.second == "missing" {
				delete(pages, "/second/")
			} else if tc.second == "partial" {
				pages["/second/"] = wordPressPostHTML("Second", `<img src="/second.jpg"><a href="https://keep2share.example.test/file/removed">Gone</a>`)
			}
			_, direct, base, asked := wordPressTestSite(t, pages)
			u, _ := url.Parse(base + "/category/example/")
			res, err := direct.pageSniff(context.Background(), u, Options{limits: tc.limits})
			if err != nil {
				t.Fatal(err)
			}
			if len(res.Files) != tc.want || !strings.Contains(res.Note, tc.note) || res.Title != "Example Category" {
				t.Fatalf("missing partial result: %+v", res)
			}
			if tc.limits.Sources == 1 && (asked()[tc.next] != 0 || asked()["/second/"] != 0) {
				t.Fatal("continued fetching after the source cap")
			}
			if tc.name == "other category" && asked()[tc.next] != 0 {
				t.Fatal("left the selected category")
			}
		})
	}
}

func TestWordPressJPEGSelection(t *testing.T) {
	u, _ := url.Parse("https://blog.example.test/post/")
	root, _ := parseHTML(`<div>
<a href="/full.jpg#image"><img data-orig-file="/other.jpg" src="/thumb.jpg"></a>
<img data-orig-file="/original.jpeg" src="/small.jpeg">
<img data-full-url="/block.jpg" src="/block-small.jpg">
<img data-srcset="/medium.jpg 640w, /large.jpg 2048w" src="/placeholder.jpg">
<img data-lazy-src="//cdn.example.test/lazy.JPEG?signature=example" src="/placeholder.gif">
<img src="/plain.jpg"><a href="/plain.jpg">Same image</a>
<img src="data:image/jpeg;base64,ignored"><img src="/not-jpeg.png">
</div>`)
	got := wordPressJPEGs(root, u)
	want := []string{
		"https://blog.example.test/full.jpg", "https://blog.example.test/original.jpeg", "https://blog.example.test/block.jpg",
		"https://blog.example.test/large.jpg", "https://cdn.example.test/lazy.JPEG?signature=example", "https://blog.example.test/plain.jpg",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("images = %v, want %v", got, want)
	}
}

func TestWordPressNumberedAndHeadPagination(t *testing.T) {
	for _, pager := range []string{
		`<nav><ul><li><span aria-current="page" class="page-numbers current">1</span></li><li><a class="page-numbers" href="/category/example/page/2/">2</a></li><li><a href="/category/example/page/9/">9</a></li></ul></nav>`,
		`<div class="wp-pagenavi"><span class="current">1</span><a class="page" href="/category/example/page/2/">2</a></div>`,
		`<link rel="next" href="/category/example/page/2/">`,
	} {
		doc := wordPressIndexHTML("", "/first/")
		if strings.HasPrefix(pager, "<link") {
			doc = strings.Replace(doc, "</head>", pager+"</head>", 1)
		} else {
			doc = strings.Replace(doc, "</main>", "</main>"+pager, 1)
		}
		_, direct, base, asked := wordPressTestSite(t, map[string]string{
			"/category/example/":        doc,
			"/category/example/page/2/": wordPressIndexHTML("", "/second/"),
			"/first/":                   wordPressPostHTML("First", `<img src="/first.jpg">`),
			"/second/":                  wordPressPostHTML("Second", `<img src="/second.jpg">`),
		})
		u, _ := url.Parse(base + "/category/example/")
		res, err := direct.pageSniff(context.Background(), u, Options{})
		if err != nil || res == nil || len(res.Files) != 2 || res.Note != "" {
			t.Fatalf("pager %s: result=%+v err=%v", pager, res, err)
		}
		if asked()["/category/example/page/9/"] != 0 {
			t.Fatal("skipped ahead to the last page")
		}
	}
}

func TestWordPressKeepsNamedExtractorPrecedence(t *testing.T) {
	reg, _, base, asked := wordPressTestSite(t, map[string]string{
		"/category/example/": wordPressIndexHTML("", "/first/"),
	})
	u, _ := url.Parse(base)
	named := &linksStub{host: u.Host, title: "Dedicated extraction", files: 1}
	reg.extractors = append(reg.extractors, named)
	res, ex, err := reg.Extract(context.Background(), base+"/category/example/", Options{})
	if err != nil || ex != named || !strings.HasPrefix(res.Title, "Dedicated extraction") || len(asked()) != 0 {
		t.Fatalf("WordPress intercepted a named extractor: result=%+v err=%v requests=%v", res, err, asked())
	}
}

func TestWordPressHandsOtherHostLinksAndOptionsToRegistry(t *testing.T) {
	reg, _, base, _ := wordPressTestSite(t, map[string]string{
		"/category/example/": wordPressIndexHTML("", "/first/"),
		"/first/": wordPressPostHTML("First", `<iframe src="https://albums.example.test/collection"></iframe>
<a href="https://unknown.example.test/navigation">Unsupported navigation</a>`),
	})
	albums := &linksStub{host: "albums.example.test", title: "Collection", files: 2}
	reg.extractors = append(reg.extractors, albums)
	res, _, err := reg.Extract(context.Background(), base+"/category/example/", Options{Password: "example-password"})
	if err != nil || len(res.Files) != 2 || res.Note != "" {
		t.Fatalf("album handoff: result=%+v err=%v", res, err)
	}
	if !slices.Equal(albums.passwords, []string{"example-password"}) {
		t.Fatalf("options lost during handoff: %v", albums.passwords)
	}
}

func TestWordPressRecognitionDoesNotClaimOtherPages(t *testing.T) {
	for _, tc := range []struct {
		name, page, doc string
	}{
		{"other software", "/category/example/", `<main><article><h2 class="entry-title"><a href="/first/">Post</a></h2></article></main>`},
		{"WordPress home", "/", `<meta name="generator" content="WordPress 6.8"><body class="home blog"><article class="hentry"></article>`},
		{"WordPress post", "/post/", `<meta name="generator" content="WordPress 6.8"><body class="single"><article class="hentry"></article>`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, _ := parseHTML(tc.doc)
			u, _ := url.Parse("https://blog.example.test" + tc.page)
			if wordPressCategory(root, u) {
				t.Fatal("claimed a non-category page")
			}
		})
	}
}

func TestWordPressRedirectUsesDestinationForRelativePosts(t *testing.T) {
	_, direct, destination, _ := wordPressTestSite(t, map[string]string{
		"/category/example/": wordPressIndexHTML("", "/first/"),
		"/first/":            wordPressPostHTML("First", `<img src="/photo.jpg">`),
	})
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/old-category/" {
			t.Errorf("resolved a post against the old site: %s", r.URL.Path)
		}
		http.Redirect(w, r, destination+"/category/example/", http.StatusMovedPermanently)
	}))
	defer redirect.Close()
	u, _ := url.Parse(redirect.URL + "/old-category/")
	res, err := direct.pageSniff(context.Background(), u, Options{})
	if err != nil || res == nil || len(res.Files) != 1 || res.Files[0].URL != destination+"/photo.jpg" {
		t.Fatalf("redirect result=%+v err=%v", res, err)
	}
}

func TestWordPressUnavailableAndCancelledCategoriesFail(t *testing.T) {
	_, direct, base, _ := wordPressTestSite(t, map[string]string{
		"/category/empty/":   wordPressIndexHTML(""),
		"/category/missing/": wordPressIndexHTML("", "/post/"),
		"/post/":             wordPressPostHTML("Unavailable", `<a href="https://keep2share.example.test/file/removed">Gone</a>`),
	})
	for _, category := range []string{"empty", "missing"} {
		u, _ := url.Parse(base + "/category/" + category + "/")
		res, err := direct.pageSniff(context.Background(), u, Options{})
		if err == nil || res != nil {
			t.Fatalf("unavailable category succeeded: result=%+v err=%v", res, err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	u, _ := url.Parse(base + "/category/example/")
	root, _ := parseHTML(wordPressIndexHTML("", "/post/"))
	_, err := wordPressExtract(ctx, direct.client, direct.registry, u, root, Options{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("lost cancellation: %v", err)
	}
}
