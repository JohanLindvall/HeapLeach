package extractor

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"sync"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
	"github.com/JohanLindvall/HeapLeach/internal/util"
	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// Chevereto installs.
//
// Chevereto is image-hosting software an operator buys and runs, not a site,
// so a great many hosts serve the identical pages. Every listing — an album, a
// user's uploads, a search — prints one list item per file carrying a
// data-object attribute: URL-encoded JSON naming the full-size link, the
// stored filename and the exact byte length. One page fetch therefore yields
// names, sizes and links together, which is the same economy pixhost gets from
// rewriting thumbnails, and one extractor covers every install.
//
// Two properties of these pages decided the shape of what follows, and both
// are cases where the obvious element is the wrong one:
//
//   - The markup's own <img> is never the file. A viewer shows the medium
//     rendition (.md.) or, for a video, the poster frame (.fr.), and swaps in
//     the original by script afterwards; a listing shows the thumbnail (.th.).
//     So the link comes from the metadata, and the infix is stripped from
//     whatever arrives, rather than being read off an element.
//   - og:image is the file for an image item but the poster for a video one,
//     because the template writes the original over the display URL only when
//     the item is an image. og:video is therefore read first — the same
//     ordering, for the same reason, that imagepond needed.
//
// The host list has the two problems the Kernel Video Sharing one has, and the
// same two answers: the "chevereto" family of HEAPLEACH_EXTRA_HOSTS extends it at runtime, and the
// direct-link fallback sniffs a Chevereto-shaped URL for the software's own
// fingerprint, which covers the self-hosted tail without naming a host.
type Chevereto struct {
	client  *httpx.Client
	name    string
	domains []string
}

// cheveretoSite is one install this build ships knowing about.
type cheveretoSite struct {
	name    string
	domains []string
}

// cheveretoKnownHosts are the installs that answer a plain client.
//
// Several of the best-known ones do not, and are left out rather than shipped
// as something that looks supported and is not: pixl.li and lensdump.com both
// sit behind Cloudflare and answer 403 without a browser challenge being
// solved, and img.kiwi gates every page behind a fingerprinting interstitial.
// The same judgement booru.go makes about gelbooru.com. They still resolve if
// a user names them in HEAPLEACH_EXTRA_HOSTS behind a proxy that can pass.
//
// Note that imagepond.net is *not* here despite having been a Chevereto
// install: it has since been rewritten onto software of its own and keeps its
// own extractor. A host that leaves the platform has to leave this list, since
// nothing about these pages would parse.
var cheveretoKnownHosts = []cheveretoSite{
	{name: "imgbb", domains: []string{"imgbb.com", "ibb.co"}},
	{name: "freeimage", domains: []string{"freeimage.host"}},
	{name: "gifyu", domains: []string{"gifyu.com"}},
}

// Markers on the pages this reads. Each is emitted by the platform's own
// templates rather than by a theme, which is what makes them portable across
// installs that look nothing alike.
const (
	// cheveretoObjectAttr carries one item's metadata as URL-encoded JSON.
	// It is printed on the outer list item by both major versions; only the
	// surrounding classes moved between them, which is why nothing here keys
	// on a class.
	cheveretoObjectAttr = "data-object"
	// cheveretoSizeAttr is the exact byte length version 4 prints beside it.
	cheveretoSizeAttr = "data-size"
	// cheveretoViewerID marks the single-item viewer, and its loader carries
	// the item's exact length.
	cheveretoViewerID       = "image-viewer"
	cheveretoViewerLoaderID = "image-viewer-loader"
	// cheveretoMoreClass is printed only when a listing filled its page, so
	// it is the one honest "there may be more" signal in the markup.
	cheveretoMoreClass = "content-listing-loading"
	// cheveretoAlbumType marks a tile that stands for an album rather than a
	// file, and cheveretoAlbumTitleClass its name link. Both versions print
	// these; only version 4 adds the name as an attribute of its own, which
	// is why the link's text is read as well.
	cheveretoAlbumType       = "album"
	cheveretoAlbumTitleClass = "list-item-desc-title-link"
	// The password gate's two fields.
	cheveretoPasswordField = "content-password"
	cheveretoTokenField    = "auth_token"
)

// cheveretoThumbInfixes are the size markers the platform inserts before the
// extension: medium, thumbnail, and the frame it takes off a video.
var cheveretoThumbInfixes = []string{"md", "th", "fr"}

// NewCheveretoSites builds one extractor per known install, so each is matched
// on its host alone and named for itself in the UI, exactly as a hand-written
// host would be.
//
// The environment is read here rather than through config.Config because this
// setting names hosts rather than tuning behaviour: the list rots faster than
// releases do, and nothing else in the process has an opinion about it.
func NewCheveretoSites(cfg *config.Config, client *httpx.Client) []Extractor {
	sites := append([]cheveretoSite(nil), cheveretoKnownHosts...)
	for _, host := range util.Dedupe(cfg.ExtraHostsFor(config.FamilyChevereto)) {
		name, _, _ := strings.Cut(host, ".")
		sites = append(sites, cheveretoSite{name: name, domains: []string{host}})
	}

	out := make([]Extractor, 0, len(sites))
	for _, site := range sites {
		domains := make([]string, 0, len(site.domains))
		for _, domain := range site.domains {
			domains = append(domains, strings.ToLower(domain))
		}
		out = append(out, &Chevereto{client: client, name: site.name, domains: domains})
	}
	return out
}

func (c *Chevereto) Name() string { return c.name }

func (c *Chevereto) Match(u *url.URL) bool {
	for _, domain := range c.domains {
		if util.HostMatches(u.Host, domain) {
			return true
		}
	}
	return false
}

// Extract resolves an album, a user's listing, or a single item.
func (c *Chevereto) Extract(ctx context.Context, u *url.URL, opts Options) (*Result, error) {
	return cheveretoExtract(ctx, c.client, u, c.name, opts, false)
}

// cheveretoExtract resolves one page, whatever kind it turns out to be.
//
// Which kind is decided by what the page carries rather than by its path,
// because the routes are renameable — an operator can serve items from /i/,
// /image/ or /photo/, and the short-link domains use no route segment at all.
// Reading the page settles it in the fetch that had to happen anyway.
//
// requireFingerprint is set only for the sniff, where the host is unknown: a
// named install is parsed on its own say-so, since an install that has dropped
// the platform's credit still serves the platform's markup.
func cheveretoExtract(ctx context.Context, client *httpx.Client, u *url.URL,
	label string, opts Options, requireFingerprint bool) (*Result, error) {

	// A bare domain is the site's front page, which carries a cover image in
	// its metadata and would otherwise resolve to the operator's own artwork.
	// It is a mis-paste rather than a request for anything, the same judgement
	// booru.go makes about a board's root.
	if len(util.PathSegments(u)) == 0 {
		return nil, fmt.Errorf("%s: %s names no item, album or user — "+
			"paste a link to one rather than to the site", label, u.Redacted())
	}

	doc, err := cheveretoFetch(ctx, client, u, opts)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", label, err)
	}
	root, err := parseHTML(doc)
	if err != nil {
		return nil, fmt.Errorf("%s: parse %s: %w", label, u.Redacted(), err)
	}
	if requireFingerprint && !cheveretoFingerprint(root, doc) {
		return nil, fmt.Errorf("%s: %s is not a Chevereto page", label, u.Redacted())
	}

	listing := cheveretoWalk(ctx, client, u, root, doc, opts)
	switch {
	case len(listing.files) > 0:
		return &Result{Title: listing.title, Files: listing.files}, nil
	case len(listing.albums) > 0:
		files := cheveretoExpand(ctx, client, listing.albums, opts)
		if len(files) == 0 {
			return nil, fmt.Errorf("%s: none of the %d albums on %s could be read",
				label, len(listing.albums), u.Redacted())
		}
		return &Result{Title: listing.title, Files: files}, nil
	}
	return cheveretoSingle(root, u, label, listing.title)
}

// ---------------------------------------------------------------- listings

// cheveretoListing is what one walk over a paginated listing found.
type cheveretoListing struct {
	files []File
	// albums holds the sub-albums of a page that lists albums rather than
	// files, which is what a user's album tab is.
	albums []cheveretoAlbum
	title  string
}

// cheveretoAlbum is one album a listing pointed at.
type cheveretoAlbum struct {
	URL  string
	Name string
}

// cheveretoWalk follows a listing's own pagination, collecting items.
//
// It stops at a page that adds nothing new as well as at the end of the
// pagination, because a listing asked for a page past its last tends to answer
// with the last one again rather than with nothing.
func cheveretoWalk(ctx context.Context, client *httpx.Client, u *url.URL, root *html.Node,
	doc string, opts Options) *cheveretoListing {

	out := &cheveretoListing{title: cheveretoTitle(root, doc, u)}
	seen := make(map[string]bool)
	page := u

	for i := 0; i < config.MaxAlbumPages; i++ {
		if i > 0 {
			fetched, err := cheveretoFetch(ctx, client, page, opts)
			if err != nil {
				break // a later page failing still leaves the earlier ones
			}
			root, err = parseHTML(fetched)
			if err != nil {
				break
			}
		}

		added := 0
		for _, file := range cheveretoItems(root, page) {
			if seen[file.URL] {
				continue
			}
			seen[file.URL] = true
			out.files = append(out.files, file)
			added++
		}
		for _, album := range cheveretoAlbumLinks(root, page) {
			if seen[album.URL] {
				continue
			}
			seen[album.URL] = true
			out.albums = append(out.albums, album)
			added++
		}
		if added == 0 {
			break
		}

		next := cheveretoNextPage(root, page)
		if next == "" {
			// The pagination anchor is printed on every page and only carries
			// an href while a next page exists, so its absence is usually the
			// end. A theme that omits the anchor still prints the "page was
			// full" marker, and that is when a page number is worth guessing.
			if !cheveretoHasMore(root) {
				break
			}
			next = cheveretoPageAt(page, i+2)
		}
		parsed, err := ParseURL(next)
		if err != nil {
			break
		}
		page = parsed
	}

	return out
}

// cheveretoItems reads every file a listing page names.
//
// The metadata is the whole of it: the tile's own image is the thumbnail, and
// the JSON beside it carries the full-size link, the stored filename and the
// exact length together.
func cheveretoItems(root *html.Node, base *url.URL) []File {
	var files []File
	headers := httpx.Referer(util.Origin(base) + "/")

	for _, n := range findAll(root, func(n *html.Node) bool {
		return attr(n, cheveretoObjectAttr) != ""
	}) {
		obj, ok := cheveretoObjectOf(n)
		if !ok {
			continue
		}
		link := cheveretoOriginal(resolveRef(base, util.FirstNonEmpty(obj.URL, obj.Image.URL)))
		if link == "" {
			continue
		}
		files = append(files, File{
			// The stored filename rather than the uploader's own: two files
			// in one album routinely share an original name, and this one is
			// minted from the item's id, so it is unique and keeps its
			// extension. Same reasoning as pixhost's id prefix.
			Name:    util.FirstNonEmpty(obj.Filename, obj.Image.Filename, util.NameFromURL(link)),
			URL:     link,
			Size:    obj.size(link, attr(n, cheveretoSizeAttr)),
			Headers: headers,
		})
	}
	return files
}

// cheveretoObjectOf decodes one item's metadata.
//
// The attribute is URL-encoded JSON, so it survives being an attribute at all;
// an item whose JSON will not decode is skipped rather than failing the page,
// since the rest of an album is still worth having.
func cheveretoObjectOf(n *html.Node) (*cheveretoObject, bool) {
	decoded, err := url.QueryUnescape(attr(n, cheveretoObjectAttr))
	if err != nil {
		return nil, false
	}
	var obj cheveretoObject
	if err := json.Unmarshal([]byte(decoded), &obj); err != nil {
		return nil, false
	}
	return &obj, true
}

// cheveretoObject is the part of a list item's JSON this needs. The nested
// image object describes the stored original; the medium and thumb objects
// beside it are renditions and are deliberately not read.
type cheveretoObject struct {
	URL      string    `json:"url"`
	Filename string    `json:"filename"`
	Size     flexValue `json:"size"`
	Image    struct {
		URL      string    `json:"url"`
		Filename string    `json:"filename"`
		Size     flexValue `json:"size"`
	} `json:"image"`
}

// size reports the exact byte length of the file at link, or -1.
//
// The nested length is only accepted when the nested object is describing that
// very file. Size is what lets the downloader conclude a file already on disk
// is this one, so an exact number belonging to something else — a poster frame
// beside a video, say — is worse than no number at all. Some installs write
// the length as a JSON string and others as a number, which is what flexValue
// is for.
func (o *cheveretoObject) size(link, attrSize string) int64 {
	if n := cheveretoBytes(o.Size.String()); n > 0 {
		return n
	}
	if o.Image.URL != "" && cheveretoOriginal(o.Image.URL) == link {
		if n := cheveretoBytes(o.Image.Size.String()); n > 0 {
			return n
		}
	}
	if n := cheveretoBytes(attrSize); n > 0 {
		return n
	}
	return -1
}

// cheveretoBytes parses an exact byte count, or reports 0.
func cheveretoBytes(text string) int64 {
	n, err := strconv.ParseInt(strings.TrimSpace(text), 10, 64)
	if err != nil || n <= 0 {
		return 0
	}
	return n
}

// cheveretoAlbumLinks reads the albums a page lists.
//
// An album tile carries no metadata attribute of its own — there is no file
// behind it to describe — so a page of albums yields no items and is expanded
// a level instead. What both versions do mark it with is its type, which is
// why that is the test rather than anything about its contents: version 3
// names the album only in the text of its title link, and version 4 adds an
// attribute for it that version 3 has not got.
func cheveretoAlbumLinks(root *html.Node, base *url.URL) []cheveretoAlbum {
	var out []cheveretoAlbum
	for _, tile := range findAll(root, func(n *html.Node) bool {
		return attr(n, "data-type") == cheveretoAlbumType
	}) {
		title := findFirst(tile, func(n *html.Node) bool {
			return isElem(n, atom.A) && hasClass(n, cheveretoAlbumTitleClass)
		})
		anchor := title
		if anchor == nil {
			anchor = findFirst(tile, func(n *html.Node) bool {
				return isElem(n, atom.A) && attr(n, "href") != ""
			})
		}
		if anchor == nil {
			continue
		}
		link := resolveRef(base, attr(anchor, "href"))
		if link == "" {
			continue
		}
		name := strings.TrimSpace(attr(tile, "data-name"))
		if name == "" && title != nil {
			name = strings.TrimSpace(textOf(title))
		}
		out = append(out, cheveretoAlbum{URL: link, Name: name})
	}
	return out
}

// cheveretoExpand reads every album a listing pointed at, several at a time.
//
// Results are collected by index rather than appended as they arrive, so the
// job lists its files in the order the page did however the requests
// interleave; an album that will not load is skipped rather than failing a
// profile of dozens. Each album's files go in a folder of their own, since two
// albums by one uploader may well hold the same filenames.
//
// Nesting is followed one level only. An album inside an album is rare, and
// recursion here would be unbounded in a way nothing on the page bounds.
func cheveretoExpand(ctx context.Context, client *httpx.Client, albums []cheveretoAlbum,
	opts Options) []File {

	groups := make([][]File, len(albums))

	var wg sync.WaitGroup
	slots := make(chan struct{}, config.PageFetchConcurrency)
	for i, album := range albums {
		wg.Add(1)
		go func(i int, album cheveretoAlbum) {
			defer wg.Done()
			select {
			case slots <- struct{}{}:
				defer func() { <-slots }()
			case <-ctx.Done():
				return
			}
			u, err := ParseURL(album.URL)
			if err != nil {
				return
			}
			doc, err := cheveretoFetch(ctx, client, u, opts)
			if err != nil {
				return
			}
			root, err := parseHTML(doc)
			if err != nil {
				return
			}
			listing := cheveretoWalk(ctx, client, u, root, doc, opts)
			dir := util.FirstNonEmpty(album.Name, listing.title)
			for j := range listing.files {
				listing.files[j].Dir = dir
			}
			groups[i] = listing.files
		}(i, album)
	}
	wg.Wait()

	var files []File
	for _, group := range groups {
		files = append(files, group...)
	}
	return files
}

// -------------------------------------------------------------- pagination

// cheveretoNextPage follows the listing's own next link.
//
// The anchor is printed on every paginated listing whether or not there is a
// next page; only the href is added when there is one. Reading the attribute
// rather than the element's presence is the whole of the difference between
// stopping at the end and walking the last page until the page cap.
func cheveretoNextPage(root *html.Node, base *url.URL) string {
	next := findFirst(root, func(n *html.Node) bool {
		return isElem(n, atom.A) && attr(n, "data-pagination") == "next" && attr(n, "href") != ""
	})
	if next == nil {
		return ""
	}
	return resolveRef(base, attr(next, "href"))
}

// cheveretoHasMore reports whether the listing filled its page. The platform
// prints this marker on exactly that condition, so it is what a missing
// pagination anchor is worth second-guessing on.
func cheveretoHasMore(root *html.Node) bool {
	return findFirst(root, func(n *html.Node) bool {
		return hasClass(n, cheveretoMoreClass)
	}) != nil
}

// cheveretoPageAt builds the numbered page URL, keeping whatever sort or
// filter the caller's own URL carried. The seek cursor the platform appends to
// its own links is dropped: it points into the page we have already read.
func cheveretoPageAt(base *url.URL, page int) string {
	next := *base
	query := next.Query()
	query.Del("seek")
	query.Del("peek")
	query.Set("page", strconv.Itoa(page))
	next.RawQuery = query.Encode()
	return next.String()
}

// ------------------------------------------------------------ single items

// cheveretoSingle resolves a viewer page to the one file behind it.
//
// The viewer element has to be there before any of the page's metadata is
// believed, and that is not a formality. A page listing nothing — an empty
// album, a user with no uploads, the site's own front page — still carries an
// og:image: the cover art, or the user's avatar. Reading the metadata without
// first establishing that the page is showing an item would queue those as
// downloads. The element is safe to require because the platform's own
// scripts fetch it by that id, so a theme cannot rename it and still work.
func cheveretoSingle(root *html.Node, u *url.URL, label, title string) (*Result, error) {
	viewer := findFirst(root, func(n *html.Node) bool {
		return attr(n, "id") == cheveretoViewerID
	})
	if viewer == nil {
		return nil, fmt.Errorf("%s: %s shows no item and lists no files "+
			"(it may have been removed, or be a page this does not handle)",
			label, u.Redacted())
	}

	link := cheveretoMedia(root, viewer, u)
	if link == "" {
		return nil, fmt.Errorf("%s: nothing to download on %s "+
			"(the item may have been removed, or the page layout changed)",
			label, u.Redacted())
	}
	name := util.FirstNonEmpty(util.NameFromURL(link), title)
	return &Result{Title: util.FirstNonEmpty(title, name), Files: []File{{
		Name:    name,
		URL:     link,
		Size:    cheveretoViewerSize(viewer),
		Headers: httpx.Referer(util.Origin(u) + "/"),
	}}}, nil
}

// cheveretoMedia finds the file a viewer page is showing.
//
// og:video first: for a video item the metadata names the poster under
// og:image and the file under og:video, so reading og:image first would save
// a JPEG under the video's name and call it done. For an image item the
// template writes the original over the display URL, so og:image is the file.
//
// The viewer's own element is the last resort and the reason the infix strip
// exists at all: it is set to the medium rendition, with the original loaded
// by script afterwards. Stripping is applied to every candidate rather than
// only that one, since it is a no-op on a link that is already the original
// and there are installs old enough to publish the display URL as og:image.
func cheveretoMedia(root, viewer *html.Node, base *url.URL) string {
	if link := cheveretoOriginal(resolveRef(base, metaContent(root, "og:video"))); link != "" {
		return link
	}
	if link := cheveretoOriginal(resolveRef(base, metaContent(root, "og:image"))); link != "" {
		return link
	}
	img := findFirst(viewer, func(n *html.Node) bool { return isElem(n, atom.Img) })
	if img == nil {
		return ""
	}
	return cheveretoOriginal(resolveRef(base, attr(img, "src")))
}

// cheveretoViewerSize reads the exact length the viewer's loader states, which
// is the original's rather than the rendition on screen. A page without one —
// the loader is only drawn when a rendition stands in for the file — leaves
// the length to the response headers.
func cheveretoViewerSize(viewer *html.Node) int64 {
	loader := findFirst(viewer, func(n *html.Node) bool {
		return attr(n, "id") == cheveretoViewerLoaderID
	})
	if loader == nil {
		return -1
	}
	if n := cheveretoBytes(attr(loader, cheveretoSizeAttr)); n > 0 {
		return n
	}
	return -1
}

// cheveretoOriginal turns a rendition link into the stored file it stands for.
//
// The platform names its renditions by inserting a marker before the
// extension — name.md.jpg, name.th.jpg, name.fr.jpg — so removing it is the
// whole mapping. Only that one component is touched, and only when it is one
// of the three markers, so a filename that merely contains dots is left alone.
//
// A video's frame is a special case worth stating: stripping .fr. from
// name.fr.jpg gives name.jpg, which is not the video. That is why og:video is
// read before anything derived from an image element, and not something this
// function can fix.
func cheveretoOriginal(link string) string {
	link = strings.TrimSpace(link)
	if link == "" {
		return ""
	}
	u, err := url.Parse(link)
	if err != nil || u.Host == "" {
		return ""
	}
	dir, base := path.Split(u.Path)
	ext := path.Ext(base)
	if ext == "" {
		return link
	}
	stem := strings.TrimSuffix(base, ext)
	for _, infix := range cheveretoThumbInfixes {
		if rest, ok := strings.CutSuffix(stem, "."+infix); ok && rest != "" {
			u.Path = dir + rest + ext
			return u.String()
		}
	}
	return link
}

// cheveretoTitle names the job after the album, the user or the item.
//
// The metadata carries it in full; the page's own heading is truncated to fit
// its box on several themes, which is why it is not read first.
func cheveretoTitle(root *html.Node, doc string, u *url.URL) string {
	return util.FirstNonEmpty(
		strings.TrimSpace(metaContent(root, "og:title")),
		trimSiteSuffix(firstTitleOf(doc)),
		strings.Trim(u.Path, "/"),
	)
}

// ----------------------------------------------------------- password gate

// cheveretoFetch reads a page, unlocking it first when it turns out to be the
// password gate rather than the content.
func cheveretoFetch(ctx context.Context, client *httpx.Client, u *url.URL, opts Options) (string, error) {
	page := u.String()
	doc, err := client.GetString(ctx, page, httpx.Referer(util.Origin(u)+"/"))
	if err != nil {
		return "", fmt.Errorf("fetch %s: %w", u.Redacted(), err)
	}
	root, err := parseHTML(doc)
	if err != nil {
		return "", fmt.Errorf("parse %s: %w", u.Redacted(), err)
	}
	if !cheveretoGated(root) {
		return doc, nil
	}
	if opts.Password == "" {
		return "", ErrPasswordRequired
	}
	return cheveretoUnlock(ctx, client, u, root, opts.Password)
}

// cheveretoUnlock posts the password and returns the page behind the gate.
//
// The form posts back to the same address, and the session cookie the answer
// sets is what unlocks every later request — which the shared client's cookie
// jar already keeps, so the remaining pages of a protected album need no
// handling of their own. The token posted with it is the per-request value the
// gate prints as a hidden field; an install with a captcha on the gate is out
// of reach either way, and says so by answering with the gate again.
func cheveretoUnlock(ctx context.Context, client *httpx.Client, u *url.URL,
	root *html.Node, password string) (string, error) {

	form := url.Values{}
	form.Set(cheveretoPasswordField, password)
	if token := cheveretoAuthToken(root); token != "" {
		form.Set(cheveretoTokenField, token)
	}

	req, err := client.NewRequest(ctx, http.MethodPost, u.String(), []byte(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set(httpx.HeaderContentType, httpx.ContentTypeForm)
	req.Header.Set(httpx.HeaderAccept, httpx.AcceptHTML)
	req.Header.Set(httpx.HeaderReferer, u.String())
	req.Header.Set(httpx.HeaderOrigin, util.Origin(u))

	body, err := client.Bytes(req)
	if err != nil {
		return "", fmt.Errorf("unlock %s: %w", u.Redacted(), err)
	}
	doc := string(body)
	unlocked, err := parseHTML(doc)
	if err != nil {
		return "", fmt.Errorf("parse %s: %w", u.Redacted(), err)
	}
	// The gate answers a wrong password with itself, so the way to tell a
	// refusal from an acceptance is that the form is still there.
	if cheveretoGated(unlocked) {
		return "", fmt.Errorf("the password for %s was not accepted", u.Redacted())
	}
	return doc, nil
}

// cheveretoGated reports whether a page is the password gate.
func cheveretoGated(root *html.Node) bool {
	return findFirst(root, func(n *html.Node) bool {
		return isElem(n, atom.Input) && attr(n, "name") == cheveretoPasswordField
	}) != nil
}

// cheveretoAuthToken reads the per-session token the gate wants posted back.
func cheveretoAuthToken(root *html.Node) string {
	n := findFirst(root, func(n *html.Node) bool {
		return isElem(n, atom.Input) && attr(n, "name") == cheveretoTokenField
	})
	if n == nil {
		return ""
	}
	return strings.TrimSpace(attr(n, "value"))
}

// ------------------------------------------------------------------- sniff

// cheveretoSniff resolves a URL that is shaped like a Chevereto page but whose
// host nobody registered.
//
// The platform is sold rather than run, so the hosts serving it outnumber any
// list by orders of magnitude, and the routes it lays them out with are
// distinctive enough to be worth one GET: an item, an album or a user's
// albums. The page then has to identify itself as Chevereto before anything is
// read out of it, which is what keeps the guess cheap to be wrong about — a
// page that is not one falls straight through to the direct-link handling that
// would have run anyway.
func cheveretoSniff(ctx context.Context, client *httpx.Client, u *url.URL, opts Options) (*Result, bool) {
	if !cheveretoPagePath(u) {
		return nil, false
	}
	res, err := cheveretoExtract(ctx, client, u, "chevereto", opts, true)
	if err != nil || res == nil || len(res.Files) == 0 {
		return nil, false
	}
	return res, true
}

// cheveretoFingerprint reports whether a page came off a Chevereto install.
//
// Three signals, because no single one survives every install. Version 3
// prints a generator meta and version 4 does not; version 4 leaves a footer
// credit, which a paid licence may remove; and an install that has done both
// still ships the platform's own JavaScript namespace, which its pages cannot
// work without.
func cheveretoFingerprint(root *html.Node, doc string) bool {
	if strings.Contains(strings.ToLower(metaContent(root, "generator")), "chevereto") {
		return true
	}
	credit := findFirst(root, func(n *html.Node) bool {
		return isElem(n, atom.A) && attr(n, "rel") == "generator" &&
			strings.Contains(strings.ToLower(attr(n, "href")), "chevereto.com")
	})
	if credit != nil {
		return true
	}
	return strings.Contains(doc, "CHV.obj.")
}

// cheveretoRoutes are the path segments the platform's own routes are named
// after, including the alternatives an operator may rename them to.
var cheveretoRoutes = map[string]bool{
	"i": true, "img": true, "image": true, "photo": true,
	"a": true, "album": true, "albums": true,
}

// cheveretoProfileTab is the one route that follows the segment it belongs to
// rather than leading it: a user's albums live under their own name.
const cheveretoProfileTab = "albums"

// cheveretoMediaExts are the extensions that make a final path segment a file
// rather than a page. They are needed because an item's own path carries a
// dot — the viewer accepts <slug>.<id> — so "has an extension" cannot serve
// as the test the way it does for a KVS video page.
var cheveretoMediaExts = map[string]bool{
	".jpg": true, ".jpeg": true, ".png": true, ".gif": true, ".webp": true,
	".bmp": true, ".avif": true, ".mp4": true, ".webm": true, ".mov": true,
	".m4v": true, ".html": true, ".htm": true, ".php": true,
}

// cheveretoPagePath reports whether a path looks like a Chevereto page.
func cheveretoPagePath(u *url.URL) bool {
	segs := util.PathSegments(u)
	if len(segs) < 2 {
		return false
	}
	last := strings.ToLower(segs[len(segs)-1])
	if cheveretoMediaExts[path.Ext(last)] {
		return false // already a file, not a page to read
	}
	// A user's album tab is /<user>/albums, which is the one shape that is
	// recognisable without knowing the user's name.
	if len(segs) == 2 && last == cheveretoProfileTab {
		return true
	}
	return cheveretoRoutes[strings.ToLower(segs[0])]
}
