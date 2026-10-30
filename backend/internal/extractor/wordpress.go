// SPDX-License-Identifier: MIT

package extractor

import (
	"cmp"
	"context"
	"fmt"
	stdhtml "html"
	"net/url"
	"path"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/httpx"
	"github.com/JohanLindvall/HeapLeach/internal/util"
	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// WordPress categories are recognised from the document, not a host list or
// a version number. Classic themes and block themes expose different post
// containers, but both give us post permalinks, content and actual next links.
func wordPressCategory(root *html.Node, page *url.URL) bool {
	category, wordpress := false, false
	walk(root, func(n *html.Node) {
		if isElem(n, atom.Body) && hasClass(n, "category") {
			category = true
			wordpress = wordpress || hasClass(n, "archive")
		}
		if isElem(n, atom.Meta) && strings.EqualFold(attr(n, "name"), "generator") &&
			strings.HasPrefix(strings.ToLower(attr(n, "content")), "wordpress") {
			wordpress = true
		}
		if isElem(n, atom.Link) || isElem(n, atom.Script) {
			ref := attr(n, "href") + attr(n, "src")
			if strings.Contains(ref, "/wp-content/") || strings.Contains(ref, "/wp-includes/") {
				wordpress = true
			}
		}
	})
	return wordpress && (category || strings.Contains(page.Path, "/category/") ||
		page.Query().Get("cat") != "" || page.Query().Get("category_name") != "")
}

func wordPressExtract(ctx context.Context, client *httpx.Client, registry *Registry, page *url.URL, root *html.Node, opts Options) (result *Result, err error) {
	log := extractionLogger(ctx).With("category", page.Redacted())
	ctx = WithLogger(ctx, log)
	start := time.Now()
	log.DebugContext(ctx, "wordpress: category detected", "source_limit", opts.maxSources(), "file_limit", opts.maxFiles())
	defer func() {
		if err != nil {
			log.DebugContext(ctx, "wordpress: extraction failed", "error", err, "elapsed", time.Since(start))
		} else {
			log.DebugContext(ctx, "wordpress: extraction complete", "files", len(result.Files), "note", result.Note, "elapsed", time.Since(start))
		}
	}()
	sources, title, note, err := wordPressList(ctx, client, page, root, opts.maxSources())
	if err != nil {
		return nil, err
	}
	log.DebugContext(ctx, "wordpress: resolving posts before queueing files", "posts", len(sources))
	var finished atomic.Int64
	e := expandSourcesWith(ctx, sources, opts, func(ctx context.Context, link string) (*Result, error) {
		u, _ := url.Parse(link)
		postLog := log.With("post", u.Redacted())
		ctx = WithLogger(ctx, postLog)
		start := time.Now()
		postLog.DebugContext(ctx, "wordpress: resolving post")
		res, err := wordPressPost(ctx, client, registry, u, opts)
		completed := finished.Add(1)
		if err != nil {
			postLog.DebugContext(ctx, "wordpress: post failed", "error", err, "posts_finished", completed, "posts_total", len(sources), "elapsed", time.Since(start))
		} else {
			postLog.DebugContext(ctx, "wordpress: post resolved", "files", len(res.Files), "note", res.Note, "posts_finished", completed, "posts_total", len(sources), "elapsed", time.Since(start))
		}
		return res, err
	})
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(e.files) == 0 {
		return nil, noneRead("wordpress", len(sources), "posts", page.Redacted(), e.refusals, e.failed)
	}
	note = expansionNote(note, e, len(sources), "posts")
	note = joinNotes(note, countNote(e.partial, "posts have incomplete downloads"))
	return &Result{Title: title, Note: note, Files: e.files}, nil
}

// wordPressList walks a category from its first page, whichever page was
// pasted. The pasted page's own tree stands for the first page when it is
// the first page; a later one still means the whole category.
func wordPressList(ctx context.Context, client *httpx.Client, pasted *url.URL, root *html.Node, limit int) (sources []string, title, note string, err error) {
	log := extractionLogger(ctx)
	defer func() {
		log.DebugContext(ctx, "wordpress: category listing finished", "posts", len(sources), "note", note, "error", err)
	}()
	first := wordPressFirstPage(pasted)
	cached := root != nil && wordPressPageKey(pasted) == wordPressPageKey(first)
	log.DebugContext(ctx, "wordpress: reading category page", "page", first.Redacted(), "page_number", 1, "cached", cached)
	page := first
	if !cached {
		var final *url.URL
		if root, final, err = wordPressFetch(ctx, client, first); err != nil {
			log.DebugContext(ctx, "wordpress: category page failed", "page", first.Redacted(), "error", err)
			if ctx.Err() != nil {
				return nil, "", "", ctx.Err()
			}
			return nil, "", "", err
		}
		page = final
	}
	// Each page is logged as it is read, and the next as it is followed:
	// the walk asks for a page only once it means to follow it.
	number, last := 1, page
	collected := make(map[string]bool)
	links, title, note, err := walkListing(ctx, client, page, listingWalk{
		name: "wordpress", title: cmp.Or(util.NameFromURL(first.String()), "WordPress category"),
		none: "category has no post permalinks", missing: "category page is missing",
		limit: limit, limitNote: sourceLimitNote,
		root: root, key: wordPressPageKey, anyRedirect: true,
		fetch: func(ctx context.Context, next, _ *url.URL) (*html.Node, *url.URL, error) {
			number++
			log.DebugContext(ctx, "wordpress: following next page", "page", last.Redacted(), "next", next.Redacted())
			log.DebugContext(ctx, "wordpress: reading category page", "page", next.Redacted(), "page_number", number, "cached", false)
			root, final, err := wordPressFetch(ctx, client, next)
			if err != nil {
				log.DebugContext(ctx, "wordpress: category page failed", "page", next.Redacted(), "error", err)
			}
			return root, final, err
		},
		read: func(root *html.Node, page, _ *url.URL) listingPage {
			last = page
			if !wordPressSameCategory(first, page) || !wordPressCategory(root, page) {
				log.DebugContext(ctx, "wordpress: category page not recognised", "page", page.Redacted())
				return listingPage{missing: true, note: "partial — category is missing on next page"}
			}
			area := wordPressArea(root)
			refs := wordPressPostLinks(area)
			log.DebugContext(ctx, "wordpress: category page parsed", "page", page.Redacted(), "post_links", len(refs))
			p := listingPage{title: cmp.Or(wordPressTitle(area, true), trimSiteSuffix(firstText(root, atomTitle)))}
			added := 0
			for _, ref := range refs {
				if link := wordPressLocalLink(page, ref); link != nil {
					key := wordPressPageKey(link)
					if !collected[key] {
						collected[key] = true
						added++
					}
					p.links = append(p.links, listingLink{key: key, url: link.String()})
				}
			}
			log.DebugContext(ctx, "wordpress: posts collected", "page", page.Redacted(), "new_posts", added, "posts_total", len(collected))
			ref, hasNext := wordPressNext(area)
			if !hasNext {
				// Some older themes put rel=next only in the document head.
				if head := findFirst(root, func(n *html.Node) bool { return isElem(n, atom.Head) }); head != nil {
					ref, hasNext = wordPressNext(head)
				}
			}
			if !hasNext {
				return p
			}
			if p.next = wordPressLocalLink(page, ref); p.next == nil || !wordPressSameCategory(first, p.next) {
				log.DebugContext(ctx, "wordpress: invalid next page", "page", page.Redacted(), "next", ref)
				p.next, p.badNext = nil, true
			}
			return p
		},
	})
	return listingURLs(links), title, note, err
}

func wordPressFetch(ctx context.Context, client *httpx.Client, page *url.URL) (*html.Node, *url.URL, error) {
	doc, final, ok := mediaPageFetchURL(ctx, client, page)
	if !ok {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		return nil, nil, fmt.Errorf("wordpress: could not read HTML page %s", page.Redacted())
	}
	root, err := parseHTML(doc)
	return root, final, err
}

// Pagination is sometimes a sibling of main (including the saved sample),
// so prefer the primary content area before the main element itself.
func wordPressArea(root *html.Node) *html.Node {
	for _, pred := range []func(*html.Node) bool{
		func(n *html.Node) bool { return attr(n, "id") == "primary" },
		func(n *html.Node) bool { return attr(n, "id") == "content" },
		func(n *html.Node) bool { return isElem(n, atom.Main) || attr(n, "role") == "main" },
		func(n *html.Node) bool { return attr(n, "id") == "main" },
	} {
		if area := findFirst(root, pred); area != nil {
			return area
		}
	}
	return root
}

func wordPressTitle(root *html.Node, category bool) string {
	classes := []string{"entry-title", "wp-block-post-title", "post-title"}
	if category {
		classes = []string{"page-title", "archive-title", "wp-block-query-title"}
	}
	title := findFirst(root, func(n *html.Node) bool {
		for _, class := range classes {
			if hasClass(n, class) {
				return true
			}
		}
		return false
	})
	if title != nil {
		return textOf(title)
	}
	return firstText(root, atom.H1)
}

func wordPressPostLinks(root *html.Node) []string {
	if main := findFirst(root, func(n *html.Node) bool { return isElem(n, atom.Main) }); main != nil {
		root = main
	}
	// A block theme can have more than one query loop, such as recommendations.
	if posts := findFirst(root, func(n *html.Node) bool { return hasClass(n, "wp-block-post-template") }); posts != nil {
		root = posts
	}
	var links []string
	for _, post := range findAll(root, func(n *html.Node) bool {
		return hasClass(n, "hentry") || hasClass(n, "post") || hasClass(n, "type-post") || hasClass(n, "wp-block-post") || isElem(n, atom.Article)
	}) {
		title := findFirst(post, func(n *html.Node) bool {
			return hasClass(n, "entry-title") || hasClass(n, "wp-block-post-title") || hasClass(n, "post-title")
		})
		var link *html.Node
		if title != nil {
			link = findFirst(title, func(n *html.Node) bool { return isElem(n, atom.A) })
		}
		if link == nil {
			link = findFirst(post, func(n *html.Node) bool {
				return isElem(n, atom.A) && strings.Contains(" "+attr(n, "rel")+" ", " bookmark ")
			})
		}
		if link != nil {
			links = append(links, attr(link, "href"))
		} else if heading := findFirst(post, func(n *html.Node) bool { return isElem(n, atom.H2) || isElem(n, atom.H3) }); heading != nil {
			if link := findFirst(heading, func(n *html.Node) bool { return isElem(n, atom.A) }); link != nil {
				links = append(links, attr(link, "href"))
			}
		}
	}
	return links
}

func wordPressNext(root *html.Node) (string, bool) {
	link := findFirst(root, func(n *html.Node) bool {
		return (isElem(n, atom.A) || isElem(n, atom.Link)) &&
			(hasClass(n, "wp-block-query-pagination-next") || hasClass(n, "nextpostslink") ||
				(hasClass(n, "next") && hasClass(n, "page-numbers")) ||
				strings.Contains(" "+attr(n, "rel")+" ", " next "))
	})
	if link == nil {
		// Older classic themes call the link to older posts nav-previous.
		if nav := findFirst(root, func(n *html.Node) bool { return hasClass(n, "nav-previous") }); nav != nil {
			link = findFirst(nav, func(n *html.Node) bool { return isElem(n, atom.A) })
		}
	}
	if link == nil {
		// paginate_links can omit Next/Previous while retaining numbered links.
		current := findFirst(root, func(n *html.Node) bool {
			return attr(n, "aria-current") == "page" ||
				(hasClass(n, "current") && (hasClass(n, "page-numbers") || (n.Parent != nil && hasClass(n.Parent, "wp-pagenavi"))))
		})
		if current != nil && current.Parent != nil {
			if number, err := strconv.Atoi(strings.TrimSpace(textOf(current))); err == nil {
				pager := current.Parent
				if isElem(pager, atom.Li) && pager.Parent != nil {
					pager = pager.Parent
				}
				link = findFirst(pager, func(n *html.Node) bool {
					return isElem(n, atom.A) && strings.TrimSpace(textOf(n)) == strconv.Itoa(number+1)
				})
			}
		}
	}
	if link == nil {
		return "", false
	}
	return attr(link, "href"), true
}

// wordPressLocalLink is ref as a web link on base's own site, other than a
// bare "#..." anchor into the page itself.
func wordPressLocalLink(base *url.URL, ref string) *url.URL {
	if strings.HasPrefix(strings.TrimSpace(ref), "#") {
		return nil
	}
	if u := webLink(base, ref); u != nil && strings.EqualFold(u.Host, base.Host) {
		return u
	}
	return nil
}

func wordPressPagingQuery(key string) bool {
	return key == "paged" || (strings.HasPrefix(key, "query-") && strings.HasSuffix(key, "-page"))
}

func wordPressFirstPage(u *url.URL) *url.URL {
	first := *u
	parts := strings.Split(strings.TrimRight(u.EscapedPath(), "/"), "/")
	if len(parts) >= 3 && parts[len(parts)-2] == "page" {
		if n, err := strconv.Atoi(parts[len(parts)-1]); err == nil && n > 0 {
			first.RawPath = strings.Join(parts[:len(parts)-2], "/") + "/"
			first.Path, _ = url.PathUnescape(first.RawPath)
		}
	}
	q := first.Query()
	for key := range q {
		if wordPressPagingQuery(key) {
			q.Del(key)
		}
	}
	first.RawQuery, first.Fragment = q.Encode(), ""
	return &first
}

func wordPressSameCategory(a, b *url.URL) bool {
	a, b = wordPressFirstPage(a), wordPressFirstPage(b)
	return strings.EqualFold(a.Host, b.Host) && strings.TrimRight(a.EscapedPath(), "/") == strings.TrimRight(b.EscapedPath(), "/") &&
		a.Query().Get("cat") == b.Query().Get("cat") && a.Query().Get("category_name") == b.Query().Get("category_name")
}

func wordPressPageKey(u *url.URL) string {
	key := *u
	key.Path = strings.TrimRight(key.Path, "/")
	key.RawPath = strings.TrimRight(key.RawPath, "/")
	key.Fragment = ""
	q := key.Query()
	for name := range q {
		if strings.HasPrefix(name, "utm_") || name == "fbclid" || name == "replytocom" {
			q.Del(name)
		}
	}
	key.RawQuery = q.Encode()
	return key.String()
}

func wordPressPost(ctx context.Context, client *httpx.Client, registry *Registry, page *url.URL, opts Options) (*Result, error) {
	root, page, err := wordPressFetch(ctx, client, page)
	if err != nil {
		return nil, err
	}
	area := wordPressArea(root)
	content := findFirst(area, func(n *html.Node) bool {
		return hasClass(n, "entry-content") || hasClass(n, "wp-block-post-content") || attr(n, "itemprop") == "articleBody"
	})
	if content == nil {
		content = findFirst(area, func(n *html.Node) bool { return hasClass(n, "post-content") })
	}
	if content == nil {
		return nil, fmt.Errorf("wordpress: post content is missing")
	}
	title := cmp.Or(wordPressTitle(area, false), trimSiteSuffix(firstText(root, atomTitle)), util.NameFromURL(page.String()))
	// Related-post plugins can append their own thumbnails inside entry-content.
	for _, related := range findAll(content, func(n *html.Node) bool {
		return hasClass(n, "crp_related") || hasClass(n, "related-posts") || hasClass(n, "wp-block-query")
	}) {
		if related != content && related.Parent != nil {
			related.Parent.RemoveChild(related)
		}
	}
	var candidates []string
	for _, link := range linksCandidates(content, page) {
		u, _ := url.Parse(link)
		// A JPEG is already a file, even when its host has an extractor.
		// Same-site navigation is never another category or post to crawl.
		if wordPressJPEGURL(page, link) == "" && !strings.EqualFold(u.Host, page.Host) {
			candidates = append(candidates, link)
		}
	}
	sources := fileLinkSources(registry, candidates)
	found := len(sources)
	sources = sources[:min(found, opts.maxSources())]
	log := extractionLogger(ctx)
	log.DebugContext(ctx, "wordpress: post links found", "page", page.Redacted(), "supported_links", found, "selected_links", len(sources))
	e := expandSourcesWith(ctx, sources, opts, func(ctx context.Context, link string) (*Result, error) {
		return wordPressLinkedSource(ctx, registry, link, opts)
	})
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var notes []string
	if len(sources) < found {
		notes = append(notes, sourceLimitNote)
	}
	notes = append(notes, resolvedNote(e.used, len(sources)-e.unrecognised, "links"))
	notes = append(notes, countNote(e.partial, "linked sources are incomplete"))
	// A supported download link takes precedence over an embedded preview,
	// just as it does on an ordinary page. Without one, use the same album
	// and player parsers, scoped to this post rather than its sidebar/head.
	if len(sources) == 0 {
		log.DebugContext(ctx, "wordpress: inspecting embedded media")
		media, err := wordPressMedia(ctx, client, page, content, title, opts)
		if err != nil {
			return nil, err
		}
		if media != nil {
			e.files = media.Files
			notes = append(notes, media.Note)
		}
	}
	images := wordPressJPEGs(content, page)
	log.DebugContext(ctx, "wordpress: post media collected", "resolved_links", e.used, "files", len(e.files), "jpeg_images", len(images))
	// A photo may also be printed beside its album. Keep one original.
	seen := make(map[string]bool, len(e.files))
	for _, file := range e.files {
		seen[file.URL] = true
	}
	unique := images[:0]
	for _, link := range images {
		if !seen[link] {
			unique = append(unique, link)
		}
	}
	images = unique
	room := opts.maxFiles() - len(e.files)
	if e.full || len(images) > room {
		notes = append(notes, fileLimitNote)
	}
	for _, link := range images[:min(len(images), room)] {
		e.files = append(e.files, File{Name: util.NameFromURL(link), URL: link, Size: -1, Headers: httpx.Referer(page.String())})
	}
	if len(e.files) == 0 {
		return nil, fmt.Errorf("wordpress: post has no accessible downloads")
	}
	return &Result{Title: title, Note: joinNotes(notes...), Files: e.files}, nil
}

// Log the source before entering its resolver: a player can spend a long time
// recovering through proxies while the category still has no queued files.
func wordPressLinkedSource(ctx context.Context, registry *Registry, link string, opts Options) (*Result, error) {
	u, _ := url.Parse(link)
	log := extractionLogger(ctx).With("source", u.Redacted(), "extractor", registry.Guess(u))
	ctx = WithLogger(ctx, log)
	progress := WithResolveNote(ctx, func(note string) {
		log.DebugContext(ctx, "wordpress: linked source progress", "stage", note)
		ResolveNote(ctx, note)
	})
	start := time.Now()
	log.DebugContext(ctx, "wordpress: resolving linked source")
	res, _, err := registry.Extract(progress, link, opts)
	if err != nil {
		log.DebugContext(ctx, "wordpress: linked source failed", "error", err, "elapsed", time.Since(start))
	} else {
		log.DebugContext(ctx, "wordpress: linked source resolved", "files", len(res.Files), "note", res.Note, "elapsed", time.Since(start))
	}
	return res, err
}

// Give the generic parsers only the post body and its own title. Rendering
// that subtree also keeps script-based players available without including
// a player or sharing metadata from elsewhere on the page. No second page
// request or recursive scan is needed.
func wordPressMedia(ctx context.Context, client *httpx.Client, page *url.URL, content *html.Node, title string, opts Options) (*Result, error) {
	var doc strings.Builder
	fmt.Fprintf(&doc, "<title>%s</title>", stdhtml.EscapeString(title))
	if err := html.Render(&doc, content); err != nil {
		return nil, err
	}
	root, err := parseHTML(doc.String())
	if err != nil {
		return nil, err
	}
	if album, err := darkGramResult(client, page, root, opts); album != nil || err != nil {
		return album, err
	}
	media, _ := mediaPageResult(ctx, client, page, root, doc.String())
	return media, ctx.Err()
}

func wordPressJPEGURL(base *url.URL, ref string) string {
	if u := webLink(base, ref); u != nil {
		switch strings.ToLower(path.Ext(u.Path)) {
		case ".jpg", ".jpeg":
			return u.String()
		}
	}
	return ""
}

func wordPressJPEGs(content *html.Node, page *url.URL) []string {
	var images []string
	seen := make(map[string]bool)
	add := func(ref string) bool {
		link := wordPressJPEGURL(page, ref)
		if link == "" {
			return false
		}
		if !seen[link] {
			seen[link] = true
			images = append(images, link)
		}
		return true
	}
	walk(content, func(n *html.Node) {
		if !isElem(n, atom.A) && !isElem(n, atom.Img) {
			return
		}
		// Player artwork is not an independent photograph. In particular,
		// DarkGram's originals come from its album data, never its thumbnails.
		for parent := n; parent != nil; parent = parent.Parent {
			if hasClass(parent, "darkgram-album") || isElem(parent, atom.Video) || isElem(parent, atom.Audio) {
				return
			}
			if parent == content {
				break
			}
		}
		if isElem(n, atom.A) {
			add(attr(n, "href"))
		}
		if !isElem(n, atom.Img) {
			return
		}
		for parent := n.Parent; parent != nil && parent != content; parent = parent.Parent {
			if isElem(parent, atom.A) && add(attr(parent, "href")) {
				return
			}
		}
		for _, key := range []string{"data-orig-file", "data-full-url"} {
			if add(attr(n, key)) {
				return
			}
		}
		var largest string
		var width float64
		for _, key := range []string{"data-srcset", "srcset"} {
			for _, candidate := range strings.Split(attr(n, key), ",") {
				parts := strings.Fields(candidate)
				if len(parts) == 2 && wordPressJPEGURL(page, parts[0]) != "" {
					if size, err := strconv.ParseFloat(strings.TrimRight(parts[1], "wx"), 64); err == nil && size > width {
						largest, width = parts[0], size
					}
				}
			}
		}
		if add(largest) {
			return
		}
		for _, key := range []string{"data-lazy-src", "data-src", "src"} {
			if add(attr(n, key)) {
				return
			}
		}
	})
	return images
}
