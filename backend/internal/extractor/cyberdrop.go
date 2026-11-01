// SPDX-License-Identifier: MIT

package extractor

import (
	"cmp"
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

// cyberdrop resolves albums (/a/<id>) and single files (/f/<slug>, /e/<slug>).
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
// Pages are recognised by what the service's own pages carry — an album's
// entries, the file page's call to the metadata endpoint — never by name,
// and that matters here: two domains usually listed beside the service are
// not aliases of it. One does not resolve at all, and the other sits on a
// domain-parking address, which answers a request with advertising and a
// fingerprinting script rather than with an error. Taking that for the
// service would hand a user someone else's payload and call it their
// download, which is worse than not recognising it.
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

// NewCyberdrop builds the cyberdrop extractor.
func NewCyberdrop(client *httpx.Client) *Share { return newShare(client, &cyberdropSite) }

// cyberdropAlbumShape is an album, /a/<id>; cyberdropFileShape a file's
// viewer or embed player, /f/<slug> or /e/<slug>.
var (
	cyberdropAlbumShape = pathShape(`^/a/[A-Za-z0-9_-]+/?$`)
	cyberdropFileShape  = pathShape(`^/[fe]/[A-Za-z0-9_-]+/?$`)
)

// cyberdropInfoPath is the metadata endpoint a file page's script asks.
const cyberdropInfoPath = "/api/file/info/"

// cyberdropSite reads what extract does: an album's entries and the byte
// counts beside them, or a file page's call to the metadata endpoint
// extract asks in its place.
var cyberdropSite = shareSite{
	name: "cyberdrop",
	recognises: "albums (`/a/<id>`) listing `#file` entries with a `file-size`, and file pages (`/f/<slug>`, " +
		"`/e/<slug>`) whose script asks the `/api/file/info/` metadata endpoint",
	forms: []shareForm{
		{cyberdropAlbumShape, func(p *Page) bool {
			return p.hasElement(func(n *html.Node) bool {
				return isElem(n, atom.A) && attr(n, "id") == cyberdropFileID
			}) && p.hasElement(func(n *html.Node) bool { return hasClass(n, cyberdropSizeClass) })
		}},
		{cyberdropFileShape, func(p *Page) bool { return p.Has(cyberdropInfoPath) }},
	},
	extract: cyberdropExtract,
	api:     cyberdropAPI,
}

// cyberdropExtract resolves an album or a single file.
func cyberdropExtract(ctx context.Context, s *Share, u *url.URL, _ Options) (*Result, error) {
	segs := util.PathSegments(u)
	if len(segs) >= 2 {
		switch segs[0] {
		case "a":
			// There is no pagination to follow: the page carries every
			// entry, however many there are.
			root, err := s.root(ctx, u, cyberdropRoot+"/")
			if err != nil {
				return nil, err
			}
			return cyberdropAlbum(s, root, u)
		case "f", "e":
			// The viewer and the embed player are two renderings of one file
			// and share a slug space, so both take the same route.
			return cyberdropFile(ctx, s, segs[1])
		}
	}
	return nil, fmt.Errorf("cyberdrop: %s is neither an album (/a/<id>) nor a file (/f/<slug>)",
		u.Redacted())
}

// cyberdropAlbum turns a parsed listing into queued files.
//
// No entry is asked about individually. The metadata endpoint would answer
// for each of them, but it would answer with less: the signing URL is pure
// slug substitution, so the slug from the listing is all the resolver needs,
// and the listing already gave a filename and an exact length. A request per
// file would buy nothing and cost an album's worth of round trips.
func cyberdropAlbum(s *Share, root *html.Node, u *url.URL) (*Result, error) {
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
			Resolve: cyberdropResolver(s, e.slug),
		})
	}
	return &Result{Title: cyberdropTitle(root, u), Files: files}, nil
}

// cyberdropFile resolves one viewer or embed page.
func cyberdropFile(ctx context.Context, s *Share, slug string) (*Result, error) {
	info, err := cyberdropFileInfo(ctx, s, slug)
	if err != nil {
		return nil, err
	}
	name := cmp.Or(strings.TrimSpace(info.Name), slug)
	size := knownSize(info.Size)
	return &Result{Title: name, Files: []File{{
		Name:    name,
		Size:    size,
		Resolve: cyberdropResolver(s, slug),
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

// cyberdropFileInfo asks the API about one file, because its page will not say.
//
// The /f/ page is a shell: it ships the layout and a script that fetches
// exactly this endpoint and fills the name, the size and the player in. So
// fetching the page first would be one wasted request and a document with
// nothing in it to parse.
func cyberdropFileInfo(ctx context.Context, s *Share, slug string) (*cyberdropInfo, error) {
	var out cyberdropInfo
	if err := s.client.GetJSON(ctx, cyberdropInfoURL(s.api, slug), httpx.SiteOrigin(cyberdropRoot), &out); err != nil {
		if httpx.HasStatus(err, http.StatusNotFound) {
			return nil, fmt.Errorf("cyberdrop: there is no file %s (it may have been removed)", slug)
		}
		return nil, fmt.Errorf("cyberdrop: file info for %s: %w", slug, err)
	}
	return &out, nil
}

// cyberdropResolver mints a fresh download link for one slug, one call per attempt.
func cyberdropResolver(s *Share, slug string) func(context.Context) (*Target, error) {
	return func(ctx context.Context) (*Target, error) {
		var out struct {
			URL string `json:"url"`
		}
		if err := s.client.GetJSON(ctx, cyberdropAuthURL(s.api, slug), httpx.SiteOrigin(cyberdropRoot), &out); err != nil {
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

// cyberdropInfoURL and cyberdropAuthURL address one file on the API at api
// (the row's, a field so a test can serve it). Both are pure slug
// substitution, which is what lets an album entry skip the first entirely.
// Each is asked with what the site's own script sends: the API is a
// separate origin, so a browser attaches Origin alongside Referer.
func cyberdropInfoURL(api, slug string) string {
	return api + "/api/file/info/" + url.PathEscape(slug)
}

func cyberdropAuthURL(api, slug string) string {
	return api + "/api/file/auth/" + url.PathEscape(slug)
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
			slug := linkPathID(resolveRef(base, attr(n, "href")), "f", "e")
			if slug == "" || seen[slug] {
				return
			}
			seen[slug] = true
			out = append(out, cyberdropEntry{
				slug: slug,
				name: cmp.Or(strings.TrimSpace(attr(n, "title")), slug),
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
		if name := cmp.Or(strings.TrimSpace(attr(n, "title")), textOf(n)); name != "" {
			return name
		}
	}
	return albumTitle(root, u)
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
