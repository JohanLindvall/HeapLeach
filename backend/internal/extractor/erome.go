package extractor

import (
	"context"
	"fmt"
	"net/http"
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
	albums, title, limited, err := e.profileAlbums(ctx, u)
	if err != nil {
		return nil, err
	}

	if len(albums) == 0 {
		return nil, fmt.Errorf("erome: no albums on profile %s", u.Redacted())
	}

	// Every album needs its own page read for the media it holds, and a
	// profile runs to hundreds; one after another leaves the job resolving
	// for minutes before a byte moves. FanOut keeps the profile's own order
	// however the requests interleave, and an unavailable album is skipped
	// rather than sinking the rest.
	resolved := FanOut(ctx, albums, func(ctx context.Context, link string) ([]eromeAlbum, error) {
		albumURL, err := ParseURL(link)
		if err != nil {
			return nil, err
		}
		album, err := e.album(ctx, albumURL)
		if err != nil {
			return nil, err
		}
		// isEromeAlbum has already established the /a/<id> shape, so the
		// id is there to be read.
		return []eromeAlbum{{
			title: album.Title,
			id:    util.PathSegments(albumURL)[1],
			files: album.Files,
		}}, nil
	})

	result := &Result{Title: eromeProfileTitle(title, limited)}
	folders := eromeFolders(resolved)
	for i, album := range resolved {
		for _, file := range album.files {
			file.Dir = path.Join(file.Dir, folders[i])
			result.Files = append(result.Files, file)
		}
	}
	if len(result.Files) == 0 {
		return nil, fmt.Errorf("erome: profile %s had %d albums but no downloadable media",
			u.Redacted(), len(albums))
	}
	return result, nil
}

// profileAlbums walks a profile's pages for the albums they list, and
// reports what the walk learned along the way: the profile's own name, and
// whether the host cut the listing short rather than the listing ending.
//
// Kept apart from the expansion so a fixture can stand in for the pages —
// the expansion fetches an album apiece, which a test of the walk has no
// business doing.
func (e *Erome) profileAlbums(ctx context.Context, u *url.URL) (albums []string, title string, limited bool, err error) {
	seen := make(map[string]bool)

	for page := 1; page <= eromeMaxProfilePages && len(albums) < eromeMaxAlbums; page++ {
		pageURL := eromeProfilePage(u, page)

		doc, err := e.client.GetString(ctx, pageURL.String(), httpx.Referer(eromeReferer+"/"))
		if err != nil {
			if page == 1 {
				return nil, "", false, fmt.Errorf("erome: fetch profile: %w", err)
			}
			// A page that will not load is ordinarily the end of the
			// listing: asking past the last one is how the walk learns
			// where to stop. A rate limit is not that. The host has said
			// come back, the client has already waited it out as far as it
			// will go, and reading it as the end would hand back part of a
			// profile as though it were the whole of it.
			limited = httpx.HasStatus(err, http.StatusTooManyRequests)
			break
		}
		root, err := parseHTML(doc)
		if err != nil {
			return nil, "", false, fmt.Errorf("erome: %w", err)
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

		found := eromeAlbumLinks(root, &pageURL, seen)
		albums = append(albums, found...)
		// Nothing new on this page means there are no further pages.
		if len(found) == 0 {
			break
		}
	}
	return albums, title, limited, nil
}

// eromeProfileTitle names the job, admitting a listing the host cut short.
//
// An extractor has no logger, and a Result carries nothing but a title and
// its files, so the title is the only place a partial answer can be
// declared — and it is a good one, being what names the job in the UI and
// the folder on disk, which is exactly where somebody comparing the two
// against the profile will look. Handing back the first two pages of a
// profile without a word would be indistinguishable from that profile
// having two pages.
func eromeProfileTitle(title string, limited bool) string {
	if !limited {
		return title
	}
	return title + " (partial — rate limited)"
}

// eromeAlbum is one of a profile's albums, once its page has been read.
type eromeAlbum struct {
	title string
	id    string
	files []File
}

// eromeFolders names the folder each album's files are filed under.
//
// The album's own title is what a person recognises, so that is the name.
// But a title is not unique within a profile — a creator posting a series
// gives every part the same one, and reposts carry whatever title they were
// given elsewhere — and two albums sharing a folder interleave their files
// and report the profile as holding one album fewer than it does. So where a
// title repeats, every album carrying it takes its own id alongside: the site
// mints those, they are unique, and they never change.
//
// Only the repeats are qualified. An album whose title is its own keeps
// exactly the folder it has always had, so a re-run skips what it already
// holds rather than fetching it again into a renamed one.
func eromeFolders(albums []eromeAlbum) []string {
	seen := make(map[string]int, len(albums))
	for _, album := range albums {
		seen[album.title]++
	}

	names := make([]string, len(albums))
	for i, album := range albums {
		switch {
		case album.title == "":
			// Nothing to disambiguate against, and the id is already unique.
			names[i] = album.id
		case seen[album.title] > 1:
			names[i] = album.title + " " + album.id
		default:
			names[i] = album.title
		}
	}
	return names
}

// eromeProfilePage is the URL of one page of a profile.
//
// Whatever the profile URL already carried is kept, and on this host that is
// load-bearing rather than tidiness: a profile lists its own albums under
// ?t=posts and the ones it has reposted under ?t=reposts, which are
// different sets. A page number appended without the tab would walk the
// default listing from page two onward — collecting the wrong albums, and
// silently, since they are albums either way.
func eromeProfilePage(u *url.URL, page int) url.URL {
	next := *u
	if page > 1 {
		query := next.Query()
		query.Set("page", strconv.Itoa(page))
		next.RawQuery = query.Encode()
	}
	return next
}

// eromeAlbumLinks reads the album links one page of a profile lists, in the
// order the page gives them and skipping any already collected.
//
// Kept apart from the fetch so a fixture can stand in for the page. The
// profile's own furniture links out to the site's sections and to other
// creators, so what makes a link an album is its shape rather than where it
// sits in the markup.
func eromeAlbumLinks(root *html.Node, base *url.URL, seen map[string]bool) []string {
	var out []string
	for _, anchor := range findAll(root, func(n *html.Node) bool { return isElem(n, atom.A) }) {
		href := resolveRef(base, attr(anchor, "href"))
		if !isEromeAlbum(href) || seen[href] {
			continue
		}
		seen[href] = true
		out = append(out, href)
	}
	return out
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
		// A deleted album is answered with 410 and a full page of the
		// site's own furniture, which the status error would quote 200
		// characters of. The code already says everything there is to
		// know, so it is said instead of shown.
		if httpx.HasStatus(err, http.StatusGone) {
			return nil, fmt.Errorf("erome: the album %s has been deleted", u.Redacted())
		}
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
