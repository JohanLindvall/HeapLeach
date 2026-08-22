package extractor

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"sync"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
	"github.com/JohanLindvall/HeapLeach/internal/util"
	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// CoomerFans republishes creator posts as ordinary pages: a creator's page
// lists their posts, and each post holds its own media.
//
// The listing shows a thumbnail per post, and for an image post that
// thumbnail is already the full-size file — but working from the listing
// alone would still be wrong, because a post holding a video shows no
// thumbnail at all. Seeing everything means opening every post, which is one
// request each, which is why they are fetched several at a time rather than
// one after another: a creator with a few hundred posts would otherwise
// spend minutes in "resolving" before a single byte moved.
//
// Video links are signed and carry an expiry, so they are resolved when the
// item starts rather than when the page is read. Image links are plain and
// need no such treatment.
type CoomerFans struct {
	client *httpx.Client
}

const coomerFansRoot = "https://coomerfans.com"

// Markers on the pages this reads.
const (
	coomerFansBodyClass = "post-body"
	coomerFansNextClass = "next"
)

// NewCoomerFans builds the coomerfans extractor.
func NewCoomerFans(client *httpx.Client) *CoomerFans { return &CoomerFans{client: client} }

func (c *CoomerFans) Name() string { return "coomerfans" }

func (c *CoomerFans) Match(u *url.URL) bool { return util.HostMatches(u.Host, "coomerfans.com") }

// Extract resolves a creator's page or a single post.
func (c *CoomerFans) Extract(ctx context.Context, u *url.URL, _ Options) (*Result, error) {
	segs := util.PathSegments(u)
	switch {
	case len(segs) >= 2 && segs[0] == "p":
		return c.post(ctx, u)
	case len(segs) >= 2 && segs[0] == "u":
		return c.profile(ctx, u)
	}
	return nil, fmt.Errorf("coomerfans: %s is neither a creator page (/u/<service>/<id>/<name>) "+
		"nor a post (/p/<post>/<id>/<service>)", u.Redacted())
}

// ------------------------------------------------------------------- post

// post resolves one post page.
func (c *CoomerFans) post(ctx context.Context, u *url.URL) (*Result, error) {
	files, title, err := c.postFiles(ctx, u)
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("coomerfans: %s holds no media (it may be text only)", u.Redacted())
	}
	return &Result{Title: util.FirstNonEmpty(title, util.NameFromURL(u.String())), Files: files}, nil
}

// postFiles reads one post page into downloadable files.
func (c *CoomerFans) postFiles(ctx context.Context, u *url.URL) ([]File, string, error) {
	page := u.String()
	doc, err := c.client.GetString(ctx, page, httpx.Referer(coomerFansRoot+"/"))
	if err != nil {
		return nil, "", fmt.Errorf("coomerfans: fetch %s: %w", page, err)
	}

	title, media, err := coomerFansPost(doc, u)
	if err != nil {
		return nil, "", fmt.Errorf("coomerfans: %s: %w", page, err)
	}

	files := make([]File, 0, len(media))
	for _, item := range media {
		name := util.FirstNonEmpty(util.NameFromURL(item.URL), title)
		file := File{
			Name:    name,
			URL:     item.URL,
			Size:    -1,
			Headers: httpx.Referer(page),
		}
		// A video link is signed and expires; an image link is not.
		if item.Video {
			file.Resolve = c.videoResolver(u, item.URL, name)
		}
		files = append(files, file)
	}
	return files, title, nil
}

// videoResolver re-reads the post page for a fresh signed link.
//
// The video is found again by its storage path rather than by position: the
// signature changes on every visit, and a post can hold more than one video,
// so neither the whole URL nor an index identifies it.
func (c *CoomerFans) videoResolver(u *url.URL, original, name string) func(context.Context) (*Target, error) {
	page := u.String()
	want := coomerFansMediaPath(original)

	return func(ctx context.Context) (*Target, error) {
		doc, err := c.client.GetString(ctx, page, httpx.Referer(coomerFansRoot+"/"))
		if err != nil {
			return nil, fmt.Errorf("coomerfans: fetch %s: %w", page, err)
		}
		_, media, err := coomerFansPost(doc, u)
		if err != nil {
			return nil, fmt.Errorf("coomerfans: %s: %w", page, err)
		}
		for _, item := range media {
			if item.Video && coomerFansMediaPath(item.URL) == want {
				return &Target{URL: item.URL, Name: name, Size: -1, Headers: httpx.Referer(page)}, nil
			}
		}
		return nil, fmt.Errorf("coomerfans: the video is no longer on %s", page)
	}
}

// coomerFansMediaPath is the part of a media link that identifies the file,
// which is everything but the signature.
func coomerFansMediaPath(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	return u.Host + u.Path
}

// coomerFansTitle tidies a page title.
//
// The site writes document titles as "<creator> / <post>", so a post with no
// text of its own leaves the separator dangling at the end. Trimming it is
// worth doing because a multi-file job's title becomes the folder name.
func coomerFansTitle(title string) string {
	return strings.TrimRight(strings.TrimSpace(title), " /|-–—·")
}

// coomerFansItem is one piece of media on a post page.
type coomerFansItem struct {
	URL string
	// Video marks a link that carries an expiry and so has to be resolved
	// again when the transfer starts.
	Video bool
}

// coomerFansPost reads a post page's title and media. Kept apart from the
// fetch so it can be tested against a page rather than against the site.
func coomerFansPost(doc string, base *url.URL) (string, []coomerFansItem, error) {
	root, err := parseHTML(doc)
	if err != nil {
		return "", nil, err
	}
	body := findFirst(root, func(n *html.Node) bool { return hasClass(n, coomerFansBodyClass) })
	if body == nil {
		return "", nil, fmt.Errorf("no post body (the post may have been removed)")
	}

	title := coomerFansTitle(util.FirstNonEmpty(firstText(root, atom.H1), firstTitleOf(doc)))

	seen := make(map[string]bool)
	var media []coomerFansItem
	for _, n := range findAll(body, func(n *html.Node) bool {
		return isElem(n, atom.Img) || isElem(n, atom.Source) || isElem(n, atom.Video)
	}) {
		link := resolveRef(base, attr(n, "src"))
		if link == "" || seen[link] {
			continue
		}
		seen[link] = true
		media = append(media, coomerFansItem{
			URL: link,
			// Only the player's own elements carry a video; an <img> inside
			// the body is the picture itself, not a poster.
			Video: isElem(n, atom.Source) || isElem(n, atom.Video),
		})
	}
	return title, media, nil
}

// ---------------------------------------------------------------- profile

// profile expands a creator's page into every post they have.
func (c *CoomerFans) profile(ctx context.Context, u *url.URL) (*Result, error) {
	links, title, err := c.postLinks(ctx, u)
	if err != nil {
		return nil, err
	}
	if len(links) == 0 {
		return nil, fmt.Errorf("coomerfans: no posts on %s", u.Redacted())
	}
	return &Result{Title: title, Files: c.expand(ctx, links)}, nil
}

// postLinks walks the creator's pages for the posts they list.
//
// Paging stops at a page that adds nothing new as well as at the absence of
// a next link, because a listing that runs past its end tends to answer with
// the last page again rather than with nothing.
func (c *CoomerFans) postLinks(ctx context.Context, u *url.URL) ([]string, string, error) {
	var (
		links []string
		title string
		seen  = make(map[string]bool)
		page  = u.String()
	)

	for i := 0; i < config.MaxAlbumPages && page != ""; i++ {
		doc, err := c.client.GetString(ctx, page, httpx.Referer(coomerFansRoot+"/"))
		if err != nil {
			if i == 0 {
				return nil, "", fmt.Errorf("coomerfans: fetch %s: %w", page, err)
			}
			break // a later page failing still leaves the earlier ones
		}
		root, parseErr := parseHTML(doc)
		if parseErr != nil {
			break
		}
		if title == "" {
			title = coomerFansTitle(util.FirstNonEmpty(firstText(root, atom.H1), firstTitleOf(doc)))
		}

		added := 0
		for _, a := range findAll(root, func(n *html.Node) bool { return isElem(n, atom.A) }) {
			link := resolveRef(u, attr(a, "href"))
			if !coomerFansIsPost(link) || seen[link] {
				continue
			}
			seen[link] = true
			links = append(links, link)
			added++
		}
		if added == 0 {
			break
		}
		page = coomerFansNextPage(root, u)
	}

	return links, util.FirstNonEmpty(title, strings.Trim(u.Path, "/")), nil
}

// coomerFansNextPage follows the listing's own next link.
func coomerFansNextPage(root *html.Node, base *url.URL) string {
	next := findFirst(root, func(n *html.Node) bool {
		return isElem(n, atom.A) && hasClass(n, coomerFansNextClass)
	})
	if next == nil {
		return ""
	}
	return resolveRef(base, attr(next, "href"))
}

// coomerFansIsPost reports whether a link is a post on this site. Listings
// carry recommendations for other creators alongside the posts, so the shape
// of the path is what separates them.
func coomerFansIsPost(link string) bool {
	u, err := url.Parse(link)
	if err != nil || !util.HostMatches(u.Host, "coomerfans.com") {
		return false
	}
	segs := util.PathSegments(u)
	return len(segs) >= 2 && segs[0] == "p"
}

// expand reads every post page, several at a time.
//
// Results are collected by index rather than appended as they arrive, so the
// job lists its files in the order the creator's page did however the
// requests interleave. One post that will not load is skipped rather than
// failing the job: on a page of hundreds, the rest are still worth having.
func (c *CoomerFans) expand(ctx context.Context, links []string) []File {
	groups := make([][]File, len(links))

	var wg sync.WaitGroup
	slots := make(chan struct{}, config.PageFetchConcurrency)
	for i, link := range links {
		wg.Add(1)
		go func(i int, link string) {
			defer wg.Done()
			select {
			case slots <- struct{}{}:
				defer func() { <-slots }()
			case <-ctx.Done():
				return
			}
			u, err := ParseURL(link)
			if err != nil {
				return
			}
			files, _, err := c.postFiles(ctx, u)
			if err != nil {
				return
			}
			groups[i] = files
		}(i, link)
	}
	wg.Wait()

	var files []File
	for _, group := range groups {
		files = append(files, group...)
	}
	return files
}
