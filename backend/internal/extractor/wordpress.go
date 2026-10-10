// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"fmt"
	"net/url"
	"path"
	"strconv"
	"strings"

	"github.com/JohanLindvall/HeapLeach/internal/config"
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

func wordPressExtract(ctx context.Context, client *httpx.Client, registry *Registry, page *url.URL, root *html.Node, opts Options) (*Result, error) {
	sources, title, note, err := wordPressList(ctx, client, page, root, opts.maxSources())
	if err != nil {
		return nil, err
	}
	e := expandSourcesWith(ctx, sources, opts, func(ctx context.Context, link string) (*Result, error) {
		u, _ := url.Parse(link)
		return wordPressPost(ctx, client, registry, u, opts)
	})
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(e.files) == 0 {
		return nil, fmt.Errorf("wordpress: no accessible file links or JPEG images in %d posts", len(sources))
	}
	notes := []string{}
	if note != "" {
		notes = append(notes, note)
	}
	if e.full {
		notes = append(notes, "partial — file limit")
	}
	if e.used < len(sources) {
		notes = append(notes, fmt.Sprintf("%d of %d posts resolved", e.used, len(sources)))
	}
	if e.partial > 0 {
		notes = append(notes, fmt.Sprintf("%d posts have incomplete downloads", e.partial))
	}
	return &Result{Title: title, Note: strings.Join(notes, "; "), Files: e.files}, nil
}

func wordPressList(ctx context.Context, client *httpx.Client, pasted *url.URL, root *html.Node, limit int) (sources []string, title, note string, err error) {
	first := wordPressFirstPage(pasted)
	page := first
	if wordPressPageKey(pasted) != wordPressPageKey(first) {
		root = nil // A pasted later page still means the whole category.
	}
	title = util.FirstNonEmpty(util.NameFromURL(first.String()), "WordPress category")
	seen, pages := make(map[string]bool), make(map[string]bool)
	for n := range config.MaxAlbumPages {
		if err := ctx.Err(); err != nil {
			return nil, "", "", err
		}
		key := wordPressPageKey(page)
		if pages[key] {
			return sources, title, "partial — repeated page", nil
		}
		pages[key] = true
		if root == nil {
			var final *url.URL
			root, final, err = wordPressFetch(ctx, client, page)
			if err != nil {
				if ctx.Err() != nil {
					return nil, "", "", ctx.Err()
				}
				if n == 0 {
					return nil, "", "", err
				}
				return sources, title, "partial — could not fetch next page", nil
			}
			if !wordPressSameCategory(first, final) || !wordPressCategory(root, final) {
				if n == 0 {
					return nil, "", "", fmt.Errorf("wordpress: category page is missing")
				}
				return sources, title, "partial — category is missing on next page", nil
			}
			page = final
		}
		area := wordPressArea(root)
		if n == 0 {
			title = util.FirstNonEmpty(wordPressTitle(area, true), trimSiteSuffix(firstText(root, atomTitle)), title)
		}
		before := len(sources)
		for _, ref := range wordPressPostLinks(area) {
			link := wordPressLocalLink(page, ref)
			if link == nil {
				continue
			}
			key := wordPressPageKey(link)
			if seen[key] {
				continue
			}
			seen[key] = true
			if len(sources) == limit {
				return sources, title, "partial — source limit", nil
			}
			sources = append(sources, link.String())
		}
		if len(sources) == before {
			if n == 0 {
				return nil, "", "", fmt.Errorf("wordpress: category has no post permalinks")
			}
			return sources, title, "partial — repeated or empty page", nil
		}
		ref, hasNext := wordPressNext(area)
		if !hasNext {
			// Some older themes put rel=next only in the document head.
			if head := findFirst(root, func(n *html.Node) bool { return isElem(n, atom.Head) }); head != nil {
				ref, hasNext = wordPressNext(head)
			}
		}
		if !hasNext {
			return sources, title, "", nil
		}
		next := wordPressLocalLink(page, ref)
		if next == nil || !wordPressSameCategory(first, next) {
			return sources, title, "partial — invalid next page", nil
		}
		if len(sources) == limit {
			return sources, title, "partial — source limit", nil
		}
		page, root = next, nil
	}
	return sources, title, "partial — page limit", nil
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

func wordPressLocalLink(base *url.URL, ref string) *url.URL {
	if strings.TrimSpace(ref) == "" || strings.HasPrefix(strings.TrimSpace(ref), "#") {
		return nil
	}
	u, err := base.Parse(strings.TrimSpace(ref))
	if err != nil || u.User != nil || (u.Scheme != "http" && u.Scheme != "https") || !strings.EqualFold(u.Host, base.Host) {
		return nil
	}
	u.Fragment = ""
	return u
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
	e := expandSources(ctx, registry, sources, opts)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var notes []string
	if len(sources) < found {
		notes = append(notes, "partial — source limit")
	}
	if e.used < len(sources) {
		notes = append(notes, fmt.Sprintf("%d of %d links resolved", e.used, len(sources)))
	}
	if e.partial > 0 {
		notes = append(notes, fmt.Sprintf("%d linked sources are incomplete", e.partial))
	}
	images := wordPressJPEGs(content, page)
	room := opts.maxFiles() - len(e.files)
	if e.full || len(images) > room {
		notes = append(notes, "partial — file limit")
	}
	for _, link := range images[:min(len(images), room)] {
		e.files = append(e.files, File{Name: util.NameFromURL(link), URL: link, Size: -1, Headers: httpx.Referer(page.String())})
	}
	if len(e.files) == 0 {
		return nil, fmt.Errorf("wordpress: post has no accessible file links or JPEG images")
	}
	title := util.FirstNonEmpty(wordPressTitle(area, false), trimSiteSuffix(firstText(root, atomTitle)), util.NameFromURL(page.String()))
	return &Result{Title: title, Note: strings.Join(notes, "; "), Files: e.files}, nil
}

func wordPressJPEGURL(base *url.URL, ref string) string {
	if strings.TrimSpace(ref) == "" {
		return ""
	}
	u, err := base.Parse(strings.TrimSpace(ref))
	if err != nil || u.User != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return ""
	}
	switch strings.ToLower(path.Ext(u.Path)) {
	case ".jpg", ".jpeg":
		u.Fragment = ""
		return u.String()
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
