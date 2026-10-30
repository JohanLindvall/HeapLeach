// SPDX-License-Identifier: MIT

package extractor

import (
	"cmp"
	"context"
	"fmt"
	"net/url"
	"regexp"
	"strings"

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
func NewCoomerFans(client *httpx.Client) *CoomerFans {
	return &CoomerFans{client: client}
}

func (c *CoomerFans) Name() string { return "coomerfans" }

var (
	// coomerFansPostLink is a link to a post, capturing the creator's id.
	coomerFansPostLink = regexp.MustCompile(`href="[^"]*?/p/\d+/(\d+)/`)
	// coomerFansProfileShape is a creator's page, /u/<service>/<id>[/<name>].
	coomerFansProfileShape = pathShape(`^/u/[A-Za-z]+/\d+(?:/[^/]+)?/?$`)
	// coomerFansPostShape is a post, /p/<post>/<id>/<service>.
	coomerFansPostShape = pathShape(`^/p/\d+/\d+/[A-Za-z]+/?$`)
)

// Recognises is the inventory's description.
func (c *CoomerFans) Recognises() string {
	return "creator pages (`/u/<service>/<id>/<name>`) listing that creator's `/p/` posts, and posts " +
		"(`/p/<post>/<id>/<service>`) carrying the `post-body` the media is read from"
}

// Shape reports a creator's page or a post.
func (c *CoomerFans) Shape(u *url.URL) bool {
	return coomerFansProfileShape(u) || coomerFansPostShape(u)
}

// Recognise takes a post whose page has the post-body coomerFansPost reads,
// or a creator's page linking posts of that same creator, which is what
// postLinks collects.
func (c *CoomerFans) Recognise(_ context.Context, p *Page) bool {
	switch segs := p.Segments(); {
	case coomerFansPostShape(p.Final):
		return p.hasElement(func(n *html.Node) bool { return hasClass(n, coomerFansBodyClass) })
	case coomerFansProfileShape(p.Final):
		for _, m := range coomerFansPostLink.FindAllStringSubmatch(p.Body, -1) {
			if m[1] == segs[2] {
				return true
			}
		}
	}
	return false
}

// Extract resolves a creator's page or a single post.
func (c *CoomerFans) Extract(ctx context.Context, u *url.URL, opts Options) (*Result, error) {
	// The site's random-creator and random-post links redirect to the page
	// they chose, which is the one recognised and the one to read.
	if _, final, ok := probedDocument(ctx, u); ok && final.String() != u.String() {
		u = final
	}
	segs := util.PathSegments(u)
	switch {
	case len(segs) >= 2 && segs[0] == "p":
		return c.post(ctx, u)
	case len(segs) >= 2 && segs[0] == "u":
		return c.profile(ctx, u, opts)
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
	return &Result{Title: cmp.Or(title, util.NameFromURL(u.String())), Files: files}, nil
}

// postFiles reads one post page into downloadable files.
func (c *CoomerFans) postFiles(ctx context.Context, u *url.URL) ([]File, string, error) {
	page := u.String()
	doc, _, err := fetchDocument(ctx, c.client, u, httpx.Referer(coomerFansRoot+"/"))
	if err != nil {
		return nil, "", fmt.Errorf("coomerfans: fetch %s: %w", page, err)
	}

	title, media, err := coomerFansPost(doc, u)
	if err != nil {
		return nil, "", fmt.Errorf("coomerfans: %s: %w", page, err)
	}

	files := make([]File, 0, len(media))
	for _, item := range media {
		name := cmp.Or(util.NameFromURL(item.URL), title)
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

	title := coomerFansTitle(cmp.Or(firstText(root, atom.H1), firstText(root, atomTitle)))

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

// profile expands a creator's page into every post they have, up to the
// listing cap: every post is a page to read.
func (c *CoomerFans) profile(ctx context.Context, u *url.URL, opts Options) (*Result, error) {
	links, title, note, err := c.postLinks(ctx, u, opts)
	if err != nil {
		return nil, err
	}
	if len(links) == 0 {
		return nil, fmt.Errorf("coomerfans: no posts on %s", u.Redacted())
	}
	files, refusals, failed := c.expand(ctx, links)
	if len(files) == 0 {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return nil, noneRead("coomerfans", len(links), "posts", u.Redacted(), refusals, failed)
	}
	return &Result{Title: title, Files: files, Note: joinNotes(note, refusedNote(refusals))}, nil
}

// postLinks walks the creator's pages for the posts they list, up to the
// listing cap, with the note for a walk the cap cut short.
//
// Paging stops at a page that adds nothing new as well as at the absence of
// a next link, because a listing that runs past its end tends to answer with
// the last page again rather than with nothing. A later page that will not
// load or parse ends the walk with the earlier ones, quietly: the listing
// says nothing of how long it is, so there is nothing to call partial.
func (c *CoomerFans) postLinks(ctx context.Context, u *url.URL, opts Options) ([]string, string, string, error) {
	headers := httpx.Referer(coomerFansRoot + "/")
	fallback := strings.Trim(u.Path, "/")
	root, _, err := fetchRoot(ctx, c.client, u, headers)
	if err != nil {
		return nil, "", "", fmt.Errorf("coomerfans: fetch %s: %w", u.String(), err)
	}
	limit, limitNote := listingLimit(opts)
	found, title, note, err := walkListing(ctx, c.client, u, listingWalk{
		name: "coomerfans", title: fallback, quiet: true, root: root,
		limit: limit, limitNote: limitNote,
		fetch: fetchAsAsked(c.client, headers),
		read: func(root *html.Node, _, _ *url.URL) listingPage {
			p := listingPage{title: coomerFansTitle(cmp.Or(firstText(root, atom.H1), firstText(root, atomTitle)))}
			for _, a := range findAll(root, func(n *html.Node) bool { return isElem(n, atom.A) }) {
				if link := resolveRef(u, attr(a, "href")); coomerFansIsPost(link) {
					p.links = append(p.links, listingLink{url: link})
				}
			}
			if next := coomerFansNextPage(root, u); next != "" {
				p.next, _ = url.Parse(next)
			}
			return p
		},
	})
	if err != nil {
		return nil, "", "", err
	}
	return listingURLs(found), title, note, nil
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

// expand reads every post page, several at a time, in the listing's order
// (resolveFiles); a post that will not load leaves the rest, and why comes
// back beside the files for when none could be read.
func (c *CoomerFans) expand(ctx context.Context, links []string) ([]File, []string, error) {
	return resolveFiles(ctx, links, func(ctx context.Context, link string) ([]File, error) {
		u, err := ParseURL(link)
		if err != nil {
			return nil, err
		}
		files, _, err := c.postFiles(ctx, u)
		return files, err
	})
}
