package extractor

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
	"github.com/JohanLindvall/HeapLeach/internal/util"
	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// Bunkr resolves bunkr file and album pages across its rotating domains
// (bunkr.cr, bunkr.si, bunkrr.su, ...).
//
// A direct link takes three hops: the file page carries a numeric file id,
// a metadata endpoint turns that into a CDN host plus path, and a separate
// signing service issues the token/expiry pair the CDN demands. The signed
// URL lives about 20 minutes, so album entries resolve lazily at download
// time rather than up front.
type Bunkr struct {
	client *httpx.Client
}

const (
	// bunkrSignService issues the token/expiry pair the CDN demands.
	bunkrSignService = "https://glb-apisign.cdn.cr/sign"
	// bunkrSignReferer is the origin the signing service expects.
	bunkrSignReferer = "https://dl.bunkr.cr"
	// bunkrMetaPath turns a numeric file id into a CDN host and path.
	bunkrMetaPath = "/api/_001_v2"
)

// NewBunkr builds the bunkr extractor.
func NewBunkr(client *httpx.Client) *Bunkr { return &Bunkr{client: client} }

func (b *Bunkr) Name() string { return "bunkr" }

// Match accepts any host whose registrable name starts with "bunkr", which
// covers the domain rotation without needing a hard-coded list.
func (b *Bunkr) Match(u *url.URL) bool {
	host := strings.ToLower(u.Hostname())
	for _, label := range strings.Split(host, ".") {
		if strings.HasPrefix(label, "bunkr") {
			return true
		}
	}
	return false
}

// Extract handles both /f/<slug> (one file) and /a/<slug> (an album).
func (b *Bunkr) Extract(ctx context.Context, u *url.URL, _ Options) (*Result, error) {
	segs := util.PathSegments(u)
	if len(segs) >= 2 && segs[0] == "a" {
		return b.album(ctx, u)
	}
	name, err := b.pageName(ctx, u)
	if err != nil {
		return nil, err
	}
	return &Result{
		Title: name,
		Files: []File{{
			Name:    name,
			Size:    -1,
			Resolve: b.resolver(u.String()),
		}},
	}, nil
}

// album lists the file pages an album links to, following its pagination.
// Each entry resolves later.
//
// Large albums are split across ?page=N, and the nav only ever links a
// window of them, so the last page number is re-read from every page rather
// than trusted from the first. Asking for a page past the end serves the
// last page again, which is why a page that adds nothing new ends the walk.
func (b *Bunkr) album(ctx context.Context, u *url.URL) (*Result, error) {
	var (
		files []File
		title string
		seen  = make(map[string]bool)
	)

	last := 1
	for page := 1; page <= last && page <= config.MaxAlbumPages; page++ {
		root, err := b.albumPage(ctx, u, page)
		if err != nil {
			return nil, err
		}
		if page == 1 {
			title = util.FirstNonEmpty(metaContent(root, "og:title"), firstText(root, atom.H1),
				trimSiteSuffix(firstText(root, atom.Title)))
		}

		added := 0
		for _, f := range b.albumEntries(root, u, seen) {
			added++
			files = append(files, f)
		}
		if page > 1 && added == 0 {
			break
		}
		if n := lastAlbumPage(root); n > last {
			last = n
		}
	}

	if len(files) == 0 {
		return nil, fmt.Errorf("bunkr: no files linked from album %s", u.Redacted())
	}
	return &Result{Title: title, Files: files}, nil
}

// albumEntries reads one listing page.
//
// The name and size sit on the card, not on the link: the anchor covering
// each tile is empty and carries only an aria-label. Reading the card means
// the queue shows real names and sizes straight away instead of a wall of
// blank rows waiting on 300 separate resolves. Listings that do not use
// cards fall back to a plain scan for file links.
func (b *Bunkr) albumEntries(root *html.Node, base *url.URL, seen map[string]bool) []File {
	var files []File

	for _, card := range findAll(root, func(n *html.Node) bool { return hasClass(n, "theItem") }) {
		link := findFirst(card, func(n *html.Node) bool {
			return isElem(n, atom.A) && isBunkrFilePage(resolveRef(base, attr(n, "href")))
		})
		if link == nil {
			continue
		}
		href := resolveRef(base, attr(link, "href"))
		if seen[href] {
			continue
		}
		seen[href] = true

		name := strings.TrimSpace(attr(card, "title"))
		if name == "" {
			if el := findFirst(card, func(n *html.Node) bool { return hasClass(n, "theName") }); el != nil {
				name = strings.TrimSpace(textOf(el))
			}
		}
		size := int64(-1)
		if el := findFirst(card, func(n *html.Node) bool { return hasClass(n, "theSize") }); el != nil {
			size = parseHumanSize(textOf(el))
		}

		files = append(files, File{
			Name: util.FirstNonEmpty(name, linkLabel(link)),
			// Listings round these, so they are a display hint only.
			Size:       size,
			SizeApprox: size > 0,
			Resolve:    b.resolver(href),
		})
	}
	if len(files) > 0 {
		return files
	}

	for _, a := range findAll(root, func(n *html.Node) bool { return isElem(n, atom.A) }) {
		href := resolveRef(base, attr(a, "href"))
		if href == "" || !isBunkrFilePage(href) || seen[href] {
			continue
		}
		seen[href] = true
		files = append(files, File{
			Name:    util.FirstNonEmpty(strings.TrimSpace(attr(a, "title")), linkLabel(a)),
			Size:    -1,
			Resolve: b.resolver(href),
		})
	}
	return files
}

// albumPage fetches one page of an album listing.
func (b *Bunkr) albumPage(ctx context.Context, u *url.URL, page int) (*html.Node, error) {
	target := *u
	query := target.Query()
	if page > 1 {
		query.Set("page", strconv.Itoa(page))
	} else {
		// Start from the beginning even when handed a link to page five.
		query.Del("page")
	}
	target.RawQuery = query.Encode()

	doc, err := b.client.GetString(ctx, target.String(), httpx.Referer(util.Origin(u)+"/"))
	if err != nil {
		return nil, fmt.Errorf("bunkr: fetch album page %d: %w", page, err)
	}
	root, err := parseHTML(doc)
	if err != nil {
		return nil, fmt.Errorf("bunkr: %w", err)
	}
	return root, nil
}

// lastAlbumPage reads the highest page number the listing links to.
func lastAlbumPage(root *html.Node) int {
	last := 0
	for _, a := range findAll(root, func(n *html.Node) bool { return isElem(n, atom.A) }) {
		href := attr(a, "href")
		if href == "" {
			continue
		}
		ref, err := url.Parse(href)
		if err != nil {
			continue
		}
		n, err := strconv.Atoi(ref.Query().Get("page"))
		if err == nil && n > last {
			last = n
		}
	}
	return last
}

// resolver returns a closure that signs a fresh direct link for a file page.
func (b *Bunkr) resolver(pageURL string) func(context.Context) (*Target, error) {
	return func(ctx context.Context) (*Target, error) {
		u, err := ParseURL(pageURL)
		if err != nil {
			return nil, err
		}
		id, name, dlHost, err := b.pageInfo(ctx, u)
		if err != nil {
			return nil, err
		}

		// Hop 2: numeric id -> CDN host + storage path.
		var meta struct {
			MediaFiles string `json:"mediafiles"`
			Path       string `json:"path"`
			Original   string `json:"original"`
		}
		err = b.client.PostJSON(ctx, dlHost+bunkrMetaPath,
			httpx.RefererOrigin(dlHost+"/file/"+id, dlHost),
			map[string]string{"id": id}, &meta)
		if err != nil {
			return nil, fmt.Errorf("bunkr: metadata for %s: %w", id, err)
		}
		if meta.MediaFiles == "" || meta.Path == "" {
			return nil, fmt.Errorf("bunkr: metadata for %s is missing the media location", id)
		}

		raw, err := url.Parse(meta.MediaFiles + meta.Path)
		if err != nil {
			return nil, fmt.Errorf("bunkr: bad media url: %w", err)
		}
		filename := util.FirstNonEmpty(meta.Original, name, util.NameFromURL(raw.Path))

		// Hop 3: the CDN only serves paths carrying a current signature.
		token, expiry, err := b.sign(ctx, raw.Path)
		if err != nil {
			return nil, err
		}
		q := raw.Query()
		if filename != "" {
			q.Set("n", filename)
		}
		q.Set("token", token)
		q.Set("ex", expiry)
		raw.RawQuery = q.Encode()

		return &Target{
			URL:     raw.String(),
			Name:    filename,
			Size:    -1,
			Headers: httpx.RefererOrigin(util.Origin(u)+"/", util.Origin(u)),
		}, nil
	}
}

// sign asks the signing service for a token/expiry pair for a storage path.
func (b *Bunkr) sign(ctx context.Context, storagePath string) (token, expiry string, err error) {
	// The site decodes the pathname before signing, so a path with escaped
	// characters is signed in its decoded form.
	decoded, err := url.PathUnescape(storagePath)
	if err != nil {
		decoded = storagePath
	}
	var out struct {
		Token string `json:"token"`
		Ex    any    `json:"ex"`
	}
	endpoint := bunkrSignService + "?path=" + url.QueryEscape(decoded)
	if err := b.client.GetJSON(ctx, endpoint,
		httpx.RefererOrigin(bunkrSignReferer+"/", bunkrSignReferer), &out); err != nil {
		return "", "", fmt.Errorf("bunkr: sign %s: %w", decoded, err)
	}
	if out.Token == "" {
		return "", "", fmt.Errorf("bunkr: signing service returned no token for %s", decoded)
	}
	return out.Token, formatNumber(out.Ex), nil
}

// pageInfo scrapes a file page for its numeric id, display name and the
// download host that serves it.
func (b *Bunkr) pageInfo(ctx context.Context, u *url.URL) (id, name, dlHost string, err error) {
	doc, err := b.client.GetString(ctx, u.String(), httpx.Referer(util.Origin(u)+"/"))
	if err != nil {
		return "", "", "", fmt.Errorf("bunkr: fetch %s: %w", u.Redacted(), err)
	}
	root, err := parseHTML(doc)
	if err != nil {
		return "", "", "", fmt.Errorf("bunkr: %w", err)
	}

	if n := findFirst(root, func(n *html.Node) bool {
		return n.Type == html.ElementNode && attr(n, "data-file-id") != ""
	}); n != nil {
		id = attr(n, "data-file-id")
	}

	// The download button points at the host that owns the metadata API.
	for _, a := range findAll(root, func(n *html.Node) bool { return isElem(n, atom.A) }) {
		href := attr(a, "href")
		if h, fileID, ok := parseBunkrDownloadHref(href); ok {
			dlHost = h
			if id == "" {
				id = fileID
			}
			break
		}
	}
	if id == "" {
		return "", "", "", fmt.Errorf("bunkr: could not find a file id on %s", u.Redacted())
	}
	if dlHost == "" {
		dlHost = "https://dl." + u.Hostname()
	}

	name = util.FirstNonEmpty(
		metaContent(root, "og:title"),
		firstText(root, atom.H1),
		trimSiteSuffix(firstText(root, atom.Title)),
	)
	return id, name, dlHost, nil
}

// pageName fetches just the display name of a file page.
func (b *Bunkr) pageName(ctx context.Context, u *url.URL) (string, error) {
	_, name, _, err := b.pageInfo(ctx, u)
	if err != nil {
		return "", err
	}
	if name == "" {
		name = strings.Join(util.PathSegments(u), "-")
	}
	return name, nil
}

// parseBunkrDownloadHref recognises https://dl.bunkr.<tld>/file/<id>.
func parseBunkrDownloadHref(href string) (host, id string, ok bool) {
	u, err := url.Parse(strings.TrimSpace(href))
	if err != nil || u.Host == "" {
		return "", "", false
	}
	segs := util.PathSegments(u)
	if len(segs) != 2 || segs[0] != "file" {
		return "", "", false
	}
	if !strings.HasPrefix(strings.ToLower(u.Hostname()), "dl.") {
		return "", "", false
	}
	return u.Scheme + "://" + u.Host, segs[1], true
}

// isBunkrFilePage reports whether a link points at a /f/<slug> page.
func isBunkrFilePage(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	segs := util.PathSegments(u)
	return len(segs) == 2 && segs[0] == "f"
}

// linkLabel picks a readable label out of an anchor.
func linkLabel(a *html.Node) string {
	if img := findFirst(a, func(n *html.Node) bool { return isElem(n, atom.Img) }); img != nil {
		if alt := strings.TrimSpace(attr(img, "alt")); alt != "" {
			return alt
		}
	}
	if t := textOf(a); t != "" && len(t) < 200 {
		return t
	}
	return ""
}

// formatNumber renders a JSON number-or-string as a plain string.
func formatNumber(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case float64:
		return fmt.Sprintf("%d", int64(t))
	case nil:
		return ""
	default:
		return fmt.Sprintf("%v", t)
	}
}
