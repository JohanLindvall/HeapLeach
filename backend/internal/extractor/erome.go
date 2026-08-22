package extractor

import (
	"context"
	"fmt"
	"net/url"
	"path"
	"strconv"
	"strings"

	"github.com/JohanLindvall/HeapLeach/internal/httpx"
	"github.com/JohanLindvall/HeapLeach/internal/util"
	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// Erome resolves https://www.erome.com/a/<id> albums.
//
// The album page carries every media URL inline; the only trick is that the
// CDN checks Referer, and that each item is rendered twice (once for the
// inline player, once for the lightbox), so the list needs de-duplicating.
type Erome struct {
	client *httpx.Client
}

// eromeReferer must be the bare origin — the CDN rejects a deeper path.
const eromeReferer = "https://www.erome.com"

// NewErome builds the erome extractor.
func NewErome(client *httpx.Client) *Erome { return &Erome{client: client} }

func (e *Erome) Name() string { return "erome" }

func (e *Erome) Match(u *url.URL) bool { return util.HostMatches(u.Host, "erome.com") }

// eromeSections are the paths that are not usernames, so a profile is never
// confused with one of the site's own pages.
var eromeSections = map[string]bool{
	"a": true, "search": true, "login": true, "register": true, "signup": true,
	"tags": true, "categories": true, "explore": true, "popular": true,
	"latest": true, "users": true, "terms": true, "privacy": true,
	"dmca": true, "faq": true, "contact": true, "settings": true, "upload": true,
}

const (
	// eromeMaxProfilePages bounds how far a profile is paged through.
	eromeMaxProfilePages = 50
	// eromeMaxAlbums bounds how many albums one profile expands to.
	eromeMaxAlbums = 500
)

// Extract handles an album (/a/<id>) or a whole profile (/<user>).
func (e *Erome) Extract(ctx context.Context, u *url.URL, _ Options) (*Result, error) {
	segs := util.PathSegments(u)
	switch {
	case len(segs) >= 2 && segs[0] == "a":
		return e.album(ctx, u)
	case len(segs) == 1 && !eromeSections[strings.ToLower(segs[0])]:
		return e.profile(ctx, u)
	default:
		return nil, fmt.Errorf("erome: %s is neither an album nor a profile", u.Redacted())
	}
}

// profile walks a user's pages and expands every album they list, filing
// each album's media into its own folder.
func (e *Erome) profile(ctx context.Context, u *url.URL) (*Result, error) {
	var (
		albums []string
		seen   = make(map[string]bool)
		title  string
	)

	for page := 1; page <= eromeMaxProfilePages && len(albums) < eromeMaxAlbums; page++ {
		pageURL := *u
		if page > 1 {
			query := pageURL.Query()
			query.Set("page", strconv.Itoa(page))
			pageURL.RawQuery = query.Encode()
		}

		doc, err := e.client.GetString(ctx, pageURL.String(), httpx.Referer(eromeReferer+"/"))
		if err != nil {
			if page == 1 {
				return nil, fmt.Errorf("erome: fetch profile: %w", err)
			}
			break // ran past the last page
		}
		root, err := parseHTML(doc)
		if err != nil {
			return nil, fmt.Errorf("erome: %w", err)
		}
		if title == "" {
			title = util.FirstNonEmpty(
				strings.TrimSpace(textOf(orEmpty(findFirst(root, func(n *html.Node) bool {
					return isElem(n, atom.H1)
				})))),
				trimSiteSuffix(firstTitleOf(doc)),
				strings.Join(util.PathSegments(u), "-"),
			)
		}

		var found int
		for _, anchor := range findAll(root, func(n *html.Node) bool { return isElem(n, atom.A) }) {
			href := resolveRef(&pageURL, attr(anchor, "href"))
			if !isEromeAlbum(href) || seen[href] {
				continue
			}
			seen[href] = true
			albums = append(albums, href)
			found++
		}
		// Nothing new on this page means there are no further pages.
		if found == 0 {
			break
		}
	}

	if len(albums) == 0 {
		return nil, fmt.Errorf("erome: no albums on profile %s", u.Redacted())
	}

	result := &Result{Title: title}
	for _, link := range albums {
		albumURL, err := ParseURL(link)
		if err != nil {
			continue
		}
		album, err := e.album(ctx, albumURL)
		if err != nil {
			// One unavailable album should not sink the whole profile.
			continue
		}
		folder := util.FirstNonEmpty(album.Title, util.PathSegments(albumURL)[1])
		for _, file := range album.Files {
			file.Dir = path.Join(file.Dir, folder)
			result.Files = append(result.Files, file)
		}
	}
	if len(result.Files) == 0 {
		return nil, fmt.Errorf("erome: profile %s had %d albums but no downloadable media",
			u.Redacted(), len(albums))
	}
	return result, nil
}

// isEromeAlbum reports whether a link points at an album page.
func isEromeAlbum(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || !util.HostMatches(u.Host, "erome.com") {
		return false
	}
	segs := util.PathSegments(u)
	return len(segs) == 2 && segs[0] == "a"
}

// album lists every video and image in one album.
func (e *Erome) album(ctx context.Context, u *url.URL) (*Result, error) {
	doc, err := e.client.GetString(ctx, u.String(), httpx.Referer(eromeReferer+"/"))
	if err != nil {
		return nil, fmt.Errorf("erome: fetch album: %w", err)
	}
	root, err := parseHTML(doc)
	if err != nil {
		return nil, fmt.Errorf("erome: %w", err)
	}

	title := util.FirstNonEmpty(
		strings.TrimSpace(textOf(orEmpty(findFirst(root, func(n *html.Node) bool {
			return isElem(n, atom.H1) && hasClass(n, "album-title-page")
		})))),
		metaContent(root, "og:title"),
		trimSiteSuffix(firstText(root, atom.Title)),
	)

	var urls []string
	walk(root, func(n *html.Node) {
		switch {
		case isElem(n, atom.Source):
			urls = append(urls, resolveRef(u, attr(n, "src")))
		case isElem(n, atom.Video):
			// Some albums put the URL on the element itself.
			urls = append(urls, resolveRef(u, attr(n, "src")))
		case isElem(n, atom.Img):
			// Full-resolution images are lazy-loaded via data-src; the
			// eager src is a thumbnail, so prefer data-src when present.
			if v := attr(n, "data-src"); v != "" {
				urls = append(urls, resolveRef(u, v))
			} else if hasClass(n, "img-front") || hasClass(n, "img-back") {
				urls = append(urls, resolveRef(u, attr(n, "src")))
			}
		}
	})

	files := make([]File, 0, len(urls))
	for _, raw := range util.Dedupe(urls) {
		if !isEromeMedia(raw) {
			continue
		}
		files = append(files, File{
			Name:    util.NameFromURL(raw),
			URL:     raw,
			Size:    -1,
			Headers: httpx.Referer(eromeReferer),
		})
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("erome: no media found in %s (album may be private or removed)", u.Redacted())
	}
	return &Result{Title: title, Files: files}, nil
}

// isEromeMedia filters out sprites, icons and poster thumbnails.
func isEromeMedia(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return false
	}
	name := strings.ToLower(util.NameFromURL(raw))
	if name == "" || strings.HasPrefix(name, "thumb") {
		return false
	}
	for _, ext := range []string{".mp4", ".m4v", ".webm", ".mov", ".jpg", ".jpeg", ".png", ".gif", ".webp", ".avif"} {
		if strings.HasSuffix(name, ext) {
			return true
		}
	}
	return false
}

// orEmpty guards textOf against a nil node.
func orEmpty(n *html.Node) *html.Node {
	if n == nil {
		return &html.Node{Type: html.TextNode}
	}
	return n
}
