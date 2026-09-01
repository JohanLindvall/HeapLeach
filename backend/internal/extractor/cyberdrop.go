package extractor

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/JohanLindvall/HeapLeach/internal/httpx"
	"github.com/JohanLindvall/HeapLeach/internal/util"
	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// Cyberdrop resolves albums (/a/<id>) and single files (/f/<slug>, /e/<slug>).
//
// It is bunkr's sibling and shares its audience, but the plumbing is far
// plainer. An album is one server-rendered page and it is generous with what
// it prints: every entry states its filename and its exact byte count, so a
// whole album resolves in a single request with real names and real lengths
// in the queue from the start. A single file is the other way round. Its page
// is a contentless shell — the name, the size and the link are all filled in
// by a script once the browser runs — so the page is skipped entirely and the
// metadata endpoint that script calls is asked directly.
//
// Only cyberdrop.cr is matched, and the two domains usually listed beside it
// must not be: they are not aliases of this service. cyberdrop.me does not
// resolve at all, and cyberdrop.to sits on a domain-parking address, which
// answers a request with advertising and a fingerprinting script rather than
// with an error. Matching that would hand a user someone else's payload and
// call it their download, which is worse than not recognising the host.
//
// Links are minted by a signing endpoint, so File.URL is empty and Resolve
// does the work — the shape bunkr and turbo use, for a different reason. The
// token here is good for twenty-four hours rather than twenty minutes, so
// nothing is racing an expiry while an item waits its turn. What deferring
// buys is the other two properties: one call is made when an item starts
// instead of six hundred at extraction time, and a file the API will not sign
// fails as that one item rather than taking the whole album with it. That is
// not hypothetical — signing and metadata disagree, and a slug the API
// describes quite happily can still answer the signing request with an error.
//
// DDoS-Guard fronts both the site and the API and passes cookieless requests,
// so nothing here carries a session.
type Cyberdrop struct {
	hostSet
	client *httpx.Client
}

const (
	cyberdropRoot = "https://cyberdrop.cr"
	// cyberdropAPI is a separate origin from the site, which is why the
	// requests to it carry an Origin header as well as a Referer.
	cyberdropAPI = "https://api.cyberdrop.cr"
)

// Markers on an album listing.
const (
	// cyberdropFileID is on the anchor of every entry — the same id, once per
	// file. That is invalid HTML and has been stable regardless, so entries
	// are found by scanning for the attribute rather than by looking it up.
	cyberdropFileID = "file"
	// cyberdropTitleID is on the album's heading.
	cyberdropTitleID = "title"
	// cyberdropSizeClass is on the byte count printed beside each entry.
	cyberdropSizeClass = "file-size"
)

// NewCyberdrop builds the cyberdrop extractor. It claims cyberdrop.cr alone;
// see the type comment for why the sibling domains are deliberately left to
// the fallback.
func NewCyberdrop(client *httpx.Client) *Cyberdrop {
	return &Cyberdrop{hostSet: hostSet{"cyberdrop.cr"}, client: client}
}

func (c *Cyberdrop) Name() string { return "cyberdrop" }

// Extract resolves an album or a single file.
func (c *Cyberdrop) Extract(ctx context.Context, u *url.URL, _ Options) (*Result, error) {
	segs := util.PathSegments(u)
	if len(segs) >= 2 {
		switch segs[0] {
		case "a":
			return c.album(ctx, u)
		case "f", "e":
			// The viewer and the embed player are two renderings of one file
			// and share a slug space, so both take the same route.
			return c.file(ctx, segs[1])
		}
	}
	return nil, fmt.Errorf("cyberdrop: %s is neither an album (/a/<id>) nor a file (/f/<slug>)",
		u.Redacted())
}

// album reads a listing in one request.
//
// There is no pagination to follow: the page carries every entry, however
// many there are.
func (c *Cyberdrop) album(ctx context.Context, u *url.URL) (*Result, error) {
	doc, err := c.client.GetString(ctx, u.String(), httpx.Referer(cyberdropRoot+"/"))
	if err != nil {
		return nil, fmt.Errorf("cyberdrop: fetch %s: %w", u.Redacted(), err)
	}
	root, err := parseHTML(doc)
	if err != nil {
		return nil, fmt.Errorf("cyberdrop: parse %s: %w", u.Redacted(), err)
	}
	return c.albumFrom(root, u)
}

// albumFrom turns a parsed listing into queued files.
//
// No entry is asked about individually. The metadata endpoint would answer
// for each of them, but it would answer with less: the signing URL is pure
// slug substitution, so the slug from the listing is all the resolver needs,
// and the listing already gave a filename and an exact length. A request per
// file would buy nothing and cost an album's worth of round trips.
func (c *Cyberdrop) albumFrom(root *html.Node, u *url.URL) (*Result, error) {
	entries := cyberdropEntries(root, u)
	if len(entries) == 0 {
		return nil, fmt.Errorf("cyberdrop: no files in %s (the album may have been removed)",
			u.Redacted())
	}

	files := make([]File, 0, len(entries))
	for _, e := range entries {
		files = append(files, File{
			Name: e.name,
			// Deliberately not SizeApprox: the listing prints the byte count
			// itself, and it is the browser that rounds it for display.
			Size:    e.size,
			Resolve: c.resolver(e.slug),
		})
	}
	return &Result{Title: cyberdropTitle(root, u), Files: files}, nil
}

// file resolves one viewer or embed page.
func (c *Cyberdrop) file(ctx context.Context, slug string) (*Result, error) {
	info, err := c.info(ctx, slug)
	if err != nil {
		return nil, err
	}
	name := util.FirstNonEmpty(strings.TrimSpace(info.Name), slug)
	size := info.Size
	if size <= 0 {
		size = -1
	}
	return &Result{Title: name, Files: []File{{
		Name:    name,
		Size:    size,
		Resolve: c.resolver(slug),
	}}}, nil
}

// cyberdropInfo is the part of the API's file metadata this needs. The
// response also states a content type, the page's own share link, and the
// signing endpoint's address — the last of which is built from the slug
// instead, because an album entry never has a metadata response to read.
type cyberdropInfo struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
}

// info asks the API about one file, because its page will not say.
//
// The /f/ page is a shell: it ships the layout and a script that fetches
// exactly this endpoint and fills the name, the size and the player in. So
// fetching the page first would be one wasted request and a document with
// nothing in it to parse.
func (c *Cyberdrop) info(ctx context.Context, slug string) (*cyberdropInfo, error) {
	var out cyberdropInfo
	if err := c.client.GetJSON(ctx, cyberdropInfoURL(slug), cyberdropAPIHeaders(), &out); err != nil {
		if httpx.HasStatus(err, http.StatusNotFound) {
			return nil, fmt.Errorf("cyberdrop: there is no file %s (it may have been removed)", slug)
		}
		return nil, fmt.Errorf("cyberdrop: file info for %s: %w", slug, err)
	}
	return &out, nil
}

// resolver mints a fresh download link for one slug, one call per attempt.
func (c *Cyberdrop) resolver(slug string) func(context.Context) (*Target, error) {
	return func(ctx context.Context) (*Target, error) {
		var out struct {
			URL string `json:"url"`
		}
		if err := c.client.GetJSON(ctx, cyberdropAuthURL(slug), cyberdropAPIHeaders(), &out); err != nil {
			// A file the API describes but refuses to sign is a real state,
			// not a transport failure, and it says nothing about the rest of
			// the album — so it is reported as this item's own problem.
			if httpx.HasStatus(err, http.StatusInternalServerError) {
				return nil, fmt.Errorf("cyberdrop: %s cannot be signed for download — "+
					"the API has the file's metadata but answers the signing "+
					"request with an error: %w", slug, err)
			}
			return nil, fmt.Errorf("cyberdrop: sign %s: %w", slug, err)
		}
		if out.URL == "" {
			return nil, fmt.Errorf("cyberdrop: the API returned no download link for %s", slug)
		}
		// Size and Name are left unset so the listing's own, which are exact,
		// survive the resolve.
		return &Target{
			URL:     out.URL,
			Size:    -1,
			Headers: httpx.Referer(cyberdropRoot + "/"),
		}, nil
	}
}

// cyberdropInfoURL and cyberdropAuthURL address one file. Both are pure slug
// substitution, which is what lets an album entry skip the first entirely.
func cyberdropInfoURL(slug string) string {
	return cyberdropAPI + "/api/file/info/" + url.PathEscape(slug)
}

func cyberdropAuthURL(slug string) string {
	return cyberdropAPI + "/api/file/auth/" + url.PathEscape(slug)
}

// cyberdropAPIHeaders are what the site's own script sends. The API is a
// separate origin, so a browser attaches Origin alongside Referer.
func cyberdropAPIHeaders() httpx.Header {
	return httpx.RefererOrigin(cyberdropRoot+"/", cyberdropRoot)
}

// cyberdropEntry is one row of an album listing.
type cyberdropEntry struct {
	slug string
	name string
	size int64
}

// cyberdropEntries reads every file an album listing names.
//
// The name is the anchor's title attribute, not its content: the anchor wraps
// a thumbnail and for most entries contains no text at all.
//
// The size is not inside the anchor but beside it, so the two are paired by
// document order — a size node belongs to the most recent entry that has not
// been given one. Two things follow, and both are the point. An anchor that
// is not a new entry (an advert wearing the same id, or the second link some
// entries carry to their own file) leaves the pending entry alone rather than
// closing it, so it cannot swallow a size. And an entry whose size is missing
// ends with no size rather than borrowing its neighbour's, because the next
// entry claims the next size node.
func cyberdropEntries(root *html.Node, base *url.URL) []cyberdropEntry {
	var (
		out []cyberdropEntry
		// open indexes the entry still waiting for its size, or -1.
		open = -1
		seen = make(map[string]bool)
	)
	walk(root, func(n *html.Node) {
		switch {
		case isElem(n, atom.A) && attr(n, "id") == cyberdropFileID:
			slug := cyberdropSlug(resolveRef(base, attr(n, "href")))
			if slug == "" || seen[slug] {
				return
			}
			seen[slug] = true
			out = append(out, cyberdropEntry{
				slug: slug,
				name: util.FirstNonEmpty(strings.TrimSpace(attr(n, "title")), slug),
				size: -1,
			})
			open = len(out) - 1
		case open >= 0 && hasClass(n, cyberdropSizeClass):
			out[open].size = cyberdropBytes(textOf(n))
			open = -1
		}
	})
	return out
}

// cyberdropTitle names the job after the album.
//
// The heading's own text is padded by the template, so the element's title
// attribute is read first: it carries the name verbatim. The text remains the
// fallback, for a template that stops setting the attribute.
func cyberdropTitle(root *html.Node, u *url.URL) string {
	if n := findFirst(root, func(n *html.Node) bool {
		return n.Type == html.ElementNode && attr(n, "id") == cyberdropTitleID
	}); n != nil {
		if name := util.FirstNonEmpty(strings.TrimSpace(attr(n, "title")), textOf(n)); name != "" {
			return name
		}
	}
	return util.FirstNonEmpty(
		metaContent(root, "og:title"),
		trimSiteSuffix(firstText(root, atom.Title)),
		strings.Trim(u.Path, "/"),
	)
}

// cyberdropSlug reads the slug out of a link to a file, and yields "" for
// anything else the listing links to.
func cyberdropSlug(link string) string {
	u, err := url.Parse(link)
	if err != nil {
		return ""
	}
	segs := util.PathSegments(u)
	if len(segs) != 2 || (segs[0] != "f" && segs[0] != "e") {
		return ""
	}
	return segs[1]
}

// cyberdropBytes reads the length a listing prints, which is a plain integer
// count followed by "B".
//
// It is exact because the rounding happens later and elsewhere: the page's own
// script walks these nodes on load and rewrites each one into "779.45 KB", so
// what a browser shows is not what the server sent. Hence parseHumanSize is
// deliberately not used here — it would also accept the rounded form, and a
// rounded number treated as exact is precisely what lets the downloader
// conclude that some other file already on disk is this one. Anything but the
// raw count yields -1.
func cyberdropBytes(text string) int64 {
	digits, ok := strings.CutSuffix(strings.TrimSpace(text), "B")
	if !ok {
		return -1
	}
	n, err := strconv.ParseInt(strings.TrimSpace(digits), 10, 64)
	if err != nil || n < 0 {
		return -1
	}
	return n
}
