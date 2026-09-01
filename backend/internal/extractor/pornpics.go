package extractor

import (
	"context"
	"fmt"
	"maps"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
	"github.com/JohanLindvall/HeapLeach/internal/util"
	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// PornPics publishes photo galleries with nothing whatsoever in the way. A
// gallery page carries the full-size link to every image it holds, so one
// request is a whole gallery — not even pixhost's thumbnail-to-original
// rewrite to get wrong — and the CDN serves those links unsigned, with ranges
// and a month of cache, to a client that sends no referer, no cookie and no
// user agent. Hence File.URL and no Resolve: there is nothing here that goes
// stale between queueing an item and starting it.
//
// All the difficulty is in the listings — a category, a tag, a pornstar, a
// channel — because each page carries only its first twenty galleries and
// fetches the rest itself, and the site has two entirely separate ways of
// doing that. Which one a page uses cannot be told from its URL: it is
// declared in a global the page's own script sets. Getting it wrong fails
// silently rather than loudly, which is what makes this the whole job. Ask a
// search-type page for ?offset=20 on its own path and it answers 200 with the
// ordinary HTML page, every time and forever, so a build that pages every
// listing that way returns exactly twenty galleries and looks like it works.
// pornpicsPagerFor is that decision: a page carrying QUERY is paged through
// the search endpoint, and every other page through its own path.
//
// The class the site puts on the links worth following, "rel-link", is reused
// for three unrelated things — categories on the home page, galleries on a
// listing, images on a gallery page — so it says nothing on its own and every
// link is judged by where it points instead. The quoting around those
// attributes is inconsistent too, single on gallery pages and double on
// category pages, which would be fatal to a regex and is beside the point to
// a real parser.
type PornPics struct {
	client *httpx.Client
}

// pornpicsDomains are the site's own hosts. The German mirror is the same
// platform down to the endpoints, with an image CDN of its own; the other
// languages are path prefixes on the main domain rather than domains, and are
// handled by never assuming where in a path a marker sits.
var pornpicsDomains = []string{"pornpics.com", "pornpics.de"}

const (
	// pornpicsSearchPath is the endpoint a search-type listing pages through.
	pornpicsSearchPath = "/search/srch.php"

	// pornpicsCDNPrefix names the image CDN. A prefix rather than a fixed
	// host because each mirror has its own — cdni.pornpics.de serves what
	// cdni.pornpics.com does — and because no page of the site is ever served
	// from that name, so the prefix alone separates an image link from a
	// gallery or a category link.
	pornpicsCDNPrefix = "cdni."

	// pornpicsLinkClass is on every link the site means to be followed,
	// whatever it leads to.
	pornpicsLinkClass = "rel-link"

	// pornpicsGallerySegment marks a single gallery's path.
	pornpicsGallerySegment = "galleries"

	// pornpicsQueryGlobal is the page's own search term, present only on the
	// page type that has to be paged through the search endpoint.
	pornpicsQueryGlobal = "QUERY"

	// pornpicsLangGlobal names the language the search endpoint is asked in.
	pornpicsLangGlobal  = "PP_LANG"
	pornpicsDefaultLang = "en"

	// pornpicsFirstOffset is where paging begins, because the page already
	// carried that many galleries in its markup. It is the site's own
	// startOffset, which it writes as 0x14.
	pornpicsFirstOffset = 20

	// pornpicsPageSize is how many galleries a block asks for. The own-path
	// route clamps offset+limit at a thousand and so runs out on its own;
	// the search endpoint does not clamp, which is what MaxListingFiles is
	// there to stop.
	pornpicsPageSize = 500
)

// NewPornPics builds the pornpics extractor.
func NewPornPics(client *httpx.Client) *PornPics { return &PornPics{client: client} }

func (p *PornPics) Name() string { return "pornpics" }

func (p *PornPics) Match(u *url.URL) bool {
	// A CDN link is already a file: routing it here would send an image to
	// the listing reader, which would find no galleries in it and refuse
	// something the direct extractor downloads happily.
	if pornpicsIsCDN(u.Host) {
		return false
	}
	for _, domain := range pornpicsDomains {
		if util.HostMatches(u.Host, domain) {
			return true
		}
	}
	return false
}

// Extract resolves one gallery, or a listing of them.
func (p *PornPics) Extract(ctx context.Context, u *url.URL, _ Options) (*Result, error) {
	if pornpicsIsGallery(u) {
		return p.gallery(ctx, u)
	}
	return p.listing(ctx, u)
}

// ---------------------------------------------------------------- gallery

// gallery reads one gallery page into its images.
func (p *PornPics) gallery(ctx context.Context, u *url.URL) (*Result, error) {
	// No headers: the pages need none, and neither do the images they name.
	doc, err := p.client.GetString(ctx, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("pornpics: fetch %s: %w", u.Redacted(), err)
	}
	res, err := pornpicsGallery(doc, u)
	if err != nil {
		return nil, fmt.Errorf("pornpics: %w", err)
	}
	return res, nil
}

// pornpicsGallery pulls the full-size images out of a fetched gallery page.
// Kept apart from the fetch so a fixture can stand in for the page.
func pornpicsGallery(doc string, u *url.URL) (*Result, error) {
	root, err := parseHTML(doc)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", u.Redacted(), err)
	}

	seen := make(map[string]bool)
	var files []File
	for _, a := range findAll(root, pornpicsIsLink) {
		link := pornpicsImageURL(u, attr(a, "href"))
		if link == "" || seen[link] {
			continue
		}
		seen[link] = true
		files = append(files, File{
			// The CDN names every image after the gallery it belongs to, so
			// these are already distinct across the whole site.
			Name: util.NameFromURL(link),
			URL:  link,
			// The page prints a count and each image's pixel dimensions but
			// never a length — and the count it prints is not reliably the
			// number of images it carries either, so there is nothing here
			// worth reporting as a size.
			Size: -1,
		})
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("no images in %s (the gallery may have been removed)", u.Redacted())
	}
	return &Result{Title: pornpicsTitle(root, u), Files: files}, nil
}

// ---------------------------------------------------------------- listing

// listing expands a category, tag, pornstar or channel page into every
// gallery it lists, and every gallery into its images.
func (p *PornPics) listing(ctx context.Context, u *url.URL) (*Result, error) {
	doc, err := p.client.GetString(ctx, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("pornpics: fetch %s: %w", u.Redacted(), err)
	}
	root, err := parseHTML(doc)
	if err != nil {
		return nil, fmt.Errorf("pornpics: parse %s: %w", u.Redacted(), err)
	}

	// The one fetch answers both questions: the galleries the page shows
	// without asking for more, and how to ask for more.
	links := pornpicsGalleryLinks(root, u)
	if len(links) == 0 {
		return nil, fmt.Errorf("pornpics: %s lists no galleries — a gallery, category, "+
			"tag, pornstar or channel page is what this expands", u.Redacted())
	}
	pager := pornpicsPagerFor(root, u)

	var (
		files []File
		seen  = make(map[string]bool)
	)
	for page := range config.MaxAlbumPages {
		fresh := make([]string, 0, len(links))
		for _, link := range links {
			if !seen[link] {
				seen[link] = true
				fresh = append(fresh, link)
			}
		}
		// A listing run past its end tends to answer with what it already
		// gave rather than with nothing, so a block that adds nothing is the
		// end whatever it said.
		if len(fresh) == 0 {
			break
		}
		files = append(files, p.expand(ctx, fresh)...)

		// Checked before asking for the next block rather than after: a block
		// is five hundred galleries, and there is no sense in opening them to
		// throw the files away.
		if len(files) >= config.MaxListingFiles {
			files = pornpicsTrim(files, config.MaxListingFiles)
			break
		}
		next, blockErr := p.block(ctx, pager(pornpicsFirstOffset+page*pornpicsPageSize), u)
		if blockErr != nil {
			break // a later block failing still leaves the earlier ones
		}
		links = next
	}

	if len(files) == 0 {
		return nil, fmt.Errorf("pornpics: none of the galleries listed on %s could be read",
			u.Redacted())
	}
	return &Result{Title: pornpicsTitle(root, u), Files: files}, nil
}

// pornpicsTrim cuts a listing down to the file ceiling on a gallery boundary.
//
// Only the search endpoint can reach that ceiling — the other route stops
// itself at a thousand galleries — but when it does, half a gallery is worse
// than one gallery fewer: a folder holding an arbitrary prefix of its pictures
// looks like a finished download and is not. The files of one gallery arrive
// together and share a Dir, so the boundary is where that changes.
func pornpicsTrim(files []File, ceiling int) []File {
	if len(files) <= ceiling {
		return files
	}
	cut := ceiling
	for cut > 0 && files[cut].Dir == files[cut-1].Dir {
		cut--
	}
	if cut == 0 {
		return files[:ceiling] // one gallery larger than the whole ceiling
	}
	return files[:cut]
}

// expand opens every gallery a listing named, several at a time.
//
// Each gallery's own title becomes the subdirectory rather than a prefix on
// each name: one category resolves to hundreds of galleries and thousands of
// images, and the CDN's filenames carry the gallery's id but nothing a person
// would recognise it by.
func (p *PornPics) expand(ctx context.Context, links []string) []File {
	return FanOut(ctx, links, func(ctx context.Context, link string) ([]File, error) {
		u, err := ParseURL(link)
		if err != nil {
			return nil, err
		}
		res, err := p.gallery(ctx, u)
		if err != nil {
			return nil, err
		}
		for i := range res.Files {
			res.Files[i].Dir = res.Title
		}
		return res.Files, nil
	})
}

// block fetches one page of a listing and reads the galleries out of it.
func (p *PornPics) block(ctx context.Context, endpoint string, base *url.URL) ([]string, error) {
	// The response carries a thumbnail and a description per entry as well,
	// but the gallery's own page is opened regardless — that is where the
	// images are — so only the link is read here.
	var entries []struct {
		URL string `json:"g_url"`
	}
	if err := p.client.GetJSON(ctx, endpoint, nil, &entries); err != nil {
		return nil, err
	}

	links := make([]string, 0, len(entries))
	for _, entry := range entries {
		if link := pornpicsGalleryURL(base, entry.URL); link != "" {
			links = append(links, link)
		}
	}
	return links, nil
}

// ----------------------------------------------------------------- paging

// pornpicsPagerFor decides how a listing is paged, which is the one decision
// on this host that has to be right.
//
// A page that carries a QUERY global is what the site calls a search — a
// pornstar and a channel are both that — and pages only through the search
// endpoint. Its own path answers ?offset= with a 200 and the whole HTML page
// instead of the next block, so a listing routed the wrong way here yields
// its first twenty galleries and no error at all.
func pornpicsPagerFor(root *html.Node, u *url.URL) func(offset int) string {
	globals := pornpicsGlobals(root)
	if query := globals[pornpicsQueryGlobal]; query != "" {
		return pornpicsSearchPager(u, query, globals[pornpicsLangGlobal])
	}
	return pornpicsPathPager(u)
}

// pornpicsSearchPager pages through the endpoint the site's own script uses.
//
// The query arrives already in query-string form, spaces written as plus
// signs, so it is decoded before url.Values encodes it again the same way.
// Splicing it in as it stands would work until the day a name carries an
// ampersand.
func pornpicsSearchPager(u *url.URL, query, lang string) func(int) string {
	if decoded, err := url.QueryUnescape(query); err == nil {
		query = decoded
	}
	endpoint := util.Origin(u) + pornpicsSearchPath
	lang = util.FirstNonEmpty(lang, pornpicsDefaultLang)

	return func(offset int) string {
		values := url.Values{}
		values.Set("q", query)
		values.Set("lang", lang)
		values.Set("offset", strconv.Itoa(offset))
		values.Set("limit", strconv.Itoa(pornpicsPageSize))
		return endpoint + "?" + values.Encode()
	}
}

// pornpicsPathPager pages a listing through its own path, which is what every
// page that is not a search does.
//
// Only offset and limit are added: whatever else the URL carried — a sort
// order, an orientation filter — chooses what is being paged and has to
// survive. Offset zero is never asked for on this route, because that is the
// one value it answers with HTML rather than with the block.
func pornpicsPathPager(u *url.URL) func(int) string {
	page := *u
	page.Fragment = ""
	base := page.Query()

	return func(offset int) string {
		values := maps.Clone(base)
		values.Set("offset", strconv.Itoa(offset))
		values.Set("limit", strconv.Itoa(pornpicsPageSize))
		next := page
		next.RawQuery = values.Encode()
		return next.String()
	}
}

// pornpicsGlobalVar matches one of the globals the page declares its routing
// in. They are ordinary script statements, quoted single or double depending
// on the page type, and the names are the only ones on the page in this shape
// — the site's other inline scripts are minified and declare nothing that
// begins with a capital.
var pornpicsGlobalVar = regexp.MustCompile(`\bvar\s+([A-Z][A-Z0-9_]*)\s*=\s*(?:'([^']*)'|"([^"]*)")`)

// pornpicsGlobals reads those declarations off the page. The first value wins,
// so a later script cannot shadow the routing the page opened with.
func pornpicsGlobals(root *html.Node) map[string]string {
	out := make(map[string]string)
	for _, script := range findAll(root, func(n *html.Node) bool { return isElem(n, atom.Script) }) {
		for _, m := range pornpicsGlobalVar.FindAllStringSubmatch(textOf(script), -1) {
			if _, seen := out[m[1]]; !seen {
				out[m[1]] = util.FirstNonEmpty(m[2], m[3])
			}
		}
	}
	return out
}

// ------------------------------------------------------------------- links

// pornpicsIsLink reports whether a node is one of the links the site expects
// to be followed. What it leads to is a separate question, and the only one
// that means anything.
func pornpicsIsLink(n *html.Node) bool {
	return isElem(n, atom.A) && hasClass(n, pornpicsLinkClass)
}

// pornpicsGalleryLinks reads the galleries a listing page shows in its markup,
// which is the first block of them.
func pornpicsGalleryLinks(root *html.Node, base *url.URL) []string {
	var out []string
	for _, a := range findAll(root, pornpicsIsLink) {
		if link := pornpicsGalleryURL(base, attr(a, "href")); link != "" {
			out = append(out, link)
		}
	}
	return out
}

// pornpicsGalleryURL accepts a reference only when it names a gallery on this
// site, which is what separates a gallery tile from the category tiles and the
// image links sharing its class.
func pornpicsGalleryURL(base *url.URL, ref string) string {
	link := resolveRef(base, ref)
	u, err := url.Parse(link)
	if err != nil || !pornpicsSameSite(base, u) || !pornpicsIsGallery(u) {
		return ""
	}
	return link
}

// pornpicsImageURL accepts a reference only when it points at the image CDN,
// which on a gallery page is what tells the pictures from the related
// galleries and categories listed beside them.
func pornpicsImageURL(base *url.URL, ref string) string {
	link := resolveRef(base, ref)
	u, err := url.Parse(link)
	if err != nil || !pornpicsIsCDN(u.Host) {
		return ""
	}
	return link
}

// pornpicsIsGallery reports whether a URL names one gallery rather than a
// listing. The site prefixes translated paths with a language code, so the
// marker is a "galleries" segment anywhere with something after it rather than
// the first segment.
func pornpicsIsGallery(u *url.URL) bool {
	segs := util.PathSegments(u)
	for i, seg := range segs {
		if seg == pornpicsGallerySegment && i+1 < len(segs) {
			return true
		}
	}
	return false
}

// pornpicsIsCDN reports whether a host serves images rather than pages.
func pornpicsIsCDN(host string) bool {
	return strings.HasPrefix(strings.ToLower(host), pornpicsCDNPrefix)
}

// pornpicsSameSite reports whether a link stays on the site the page came
// from. The "www." is discounted because the pages emit absolute links on the
// canonical host whichever form the user pasted.
func pornpicsSameSite(base, u *url.URL) bool {
	return pornpicsBareHost(base.Host) == pornpicsBareHost(u.Host)
}

func pornpicsBareHost(host string) string {
	return strings.TrimPrefix(strings.ToLower(host), "www.")
}

// pornpicsTitle names a gallery or a listing after its heading, which carries
// the name and nothing else; the document title appends the site's own.
func pornpicsTitle(root *html.Node, u *url.URL) string {
	return util.FirstNonEmpty(
		strings.TrimSpace(firstText(root, atom.H1)),
		trimSiteSuffix(firstText(root, atom.Title)),
		strings.Trim(u.Path, "/"),
	)
}
