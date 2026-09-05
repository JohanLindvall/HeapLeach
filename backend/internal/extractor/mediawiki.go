package extractor

import (
	"context"
	"fmt"
	"maps"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
	"github.com/JohanLindvall/HeapLeach/internal/util"
)

// MediaWiki is not a site but the software a great many sites run, and its
// API is the one interface in this repository that was built to be called:
// documented, versioned, keyless, and answering with each file's exact byte
// length and its unmodified original URL. That is the whole argument for it.
// Wikimedia Commons alone holds something past a hundred million freely
// licensed files, every Wikipedia and sister project answers the identical
// query, and so do Fandom, wiki.gg, Miraheze and any self-hosted install.
// Nothing here is scraped, so nothing here breaks when a site redesigns.
//
// One paginated call does all the work, with the generator swapped to reach
// each shape: categorymembers for a category, images for the pictures one
// article uses, allimages for a small wiki's uploads.
//
// The awkward parts are all in the plumbing around that call:
//
//   - The API's location is the only per-install knowledge there is.
//     Wikimedia and Miraheze serve it at /w/api.php, Fandom and wiki.gg at
//     /api.php, so an unfamiliar host is probed for both. The probe asks for
//     siteinfo and insists the answer names MediaWiki, because Fandom
//     answers /w/api.php with an empty 200 and a status code alone would
//     take that for an API.
//   - "missing" does not mean missing. An image used on a Wikipedia article
//     almost always lives on Commons, and the local page for it comes back
//     flagged missing while carrying complete imageinfo. Whether there is a
//     file to fetch is decided by imageinfo's own url and never by that
//     flag; reading it the other way would discard nearly every file an
//     article has.
//   - Each batch arrives sorted by page id whatever order the generator
//     walked in, so paging a category yields a shuffle rather than a
//     listing. The files are sorted by name once collected.
//   - Fandom's image CDN re-encodes what it serves: the .jpg the API names
//     comes back as a WebP of a different length. Asking for
//     ?format=original gets the picture that was uploaded, and because that
//     is then not the file the API measured, its size goes in as
//     approximate. Everywhere else the count is exact, which is what lets a
//     re-run skip what it already has.
//
// Wikimedia asks automated clients to keep to roughly a request a second,
// and there is no key to throttle in lieu of that, so the API calls are
// spaced out per wiki. Only the API is paced: the files themselves come off
// a CDN built to serve them in parallel.
type MediaWiki struct {
	client *httpx.Client
	// extra names wikis whose URLs give nothing away — an install with no
	// /wiki/ prefix and pages outside the namespaces recognised below.
	extra []string
	// interval is how far apart this wiki's API calls are spaced. Zero is
	// valid and means no spacing at all, which is what the tests build:
	// nothing but a real host has a courtesy limit to keep to.
	interval time.Duration

	mu sync.Mutex
	// api caches one wiki's resolved API base, so a job of hundreds of
	// files probes for it once rather than once per request.
	api map[string]mediaWikiSite
	// next is the earliest a call to each API may go out. See pace.
	next map[string]time.Time
}

// mediaWikiSite is what probing a wiki established about it.
type mediaWikiSite struct {
	api  string
	name string
}

const (
	// mediaWikiPageSize is the per-page limit an anonymous caller may ask
	// for. Above it the API silently caps rather than failing, but asking
	// for what is allowed keeps the number of requests honest.
	mediaWikiPageSize = 500

	// mediaWikiInterval spaces out calls to one wiki's API.
	mediaWikiInterval = time.Second

	// mediaWikiMaxCategories bounds a recursive walk by the categories
	// visited rather than only by the files found, because a tree of empty
	// container categories costs requests while finding nothing to stop it.
	mediaWikiMaxCategories = 200
)

// mediaWikiAPIPaths are where installs put api.php, most likely first.
var mediaWikiAPIPaths = []string{"/w/api.php", "/api.php"}

// mediaWikiWikimedia are the Wikimedia projects. They all serve /w/api.php,
// so naming them saves the probe, and they are matched by name so that an
// article URL is understood as a wiki page and not merely a path.
var mediaWikiWikimedia = []string{
	"wikipedia.org", "wikimedia.org", "wiktionary.org", "wikiquote.org",
	"wikibooks.org", "wikisource.org", "wikinews.org", "wikiversity.org",
	"wikivoyage.org", "wikidata.org", "wikifunctions.org", "mediawiki.org",
}

// mediaWikiFarms are the wiki farms, where one domain covers thousands of
// wikis that all run the same software at the same paths.
//
// wiki.gg is listed although its edge currently answers this client with a
// challenge page rather than with the API — not for anything in the request,
// which is ordinary, but for the TLS fingerprint httpx presents by default;
// HEAPLEACH_UTLS=0 goes straight through and everything here then works. That
// is a property of the shared client rather than of this host, so it is
// recorded here rather than worked around.
var mediaWikiFarms = []string{"fandom.com", "wiki.gg", "miraheze.org", "wikitide.org"}

// mediaWikiPagePrefixes are the article paths installs are laid out with.
// The title is everything after one of these, "/" included: page titles have
// subpages, so the remainder is not split.
var mediaWikiPagePrefixes = []string{"/wiki/", "/index.php/"}

// mediaWikiKind is the sort of page a title names.
type mediaWikiKind int

const (
	// mediaWikiArticle is the zero value on purpose: a title with no
	// namespace, or one in a namespace nothing here knows, is an article.
	mediaWikiArticle mediaWikiKind = iota
	mediaWikiCategory
	mediaWikiFilePage
	mediaWikiSpecial
)

// mediaWikiPrefixes are the canonical namespace names, which every install
// accepts whatever language it presents itself in. A wiki that spells them
// in its own language — Catégorie:, Datei: — is not covered here and is
// asked instead; see kindOf.
var mediaWikiPrefixes = map[string]mediaWikiKind{
	"category": mediaWikiCategory,
	"file":     mediaWikiFilePage,
	// The file namespace was called Image until 2005 and the old name is
	// still an alias on every install, so links to it are still in the wild.
	"image":   mediaWikiFilePage,
	"special": mediaWikiSpecial,
}

// Namespace numbers, which is how the API answers when a prefix was in a
// language this cannot read.
const (
	mediaWikiNSSpecial  = -1
	mediaWikiNSFile     = 6
	mediaWikiNSCategory = 14
)

// mediaWikiUploadPages are the special pages that list a wiki's own uploads,
// under their canonical names and the aliases MediaWiki keeps for them.
// Anything else in the Special namespace is a tool, not a listing.
var mediaWikiUploadPages = map[string]bool{
	"listfiles": true, "filelist": true, "imagelist": true,
	"newfiles": true, "newimages": true,
}

// mediaWikiNewestPages are the subset that mean "the most recent uploads"
// rather than "all of them", which is a materially different request on a
// wiki with a million files.
var mediaWikiNewestPages = map[string]bool{"newfiles": true, "newimages": true}

// NewMediaWiki builds the MediaWiki extractor. Hosts named in
// the "mediawiki" family of HEAPLEACH_EXTRA_HOSTS are added to the ones matched by shape, for an
// install whose URLs look like nothing in particular.
func NewMediaWiki(cfg *config.Config, client *httpx.Client) *MediaWiki {
	return &MediaWiki{
		client:   client,
		extra:    cfg.ExtraHostsFor(config.FamilyMediaWiki),
		interval: mediaWikiInterval,
	}
}

func (m *MediaWiki) Name() string { return "mediawiki" }

// Match recognises the shape rather than the host, because the host is the
// part there is no list of: the software runs on a great many domains and
// only a handful of them are worth naming.
//
// A title in the File, Category or Special namespace is enough on its own —
// nothing else on the web lays a URL out that way. Everything else needs the
// host to be a wiki this knows, since /wiki/<anything> would otherwise claim
// every page of every site that happens to use that prefix.
func (m *MediaWiki) Match(u *url.URL) bool {
	title, explicit := mediaWikiTitle(u)
	switch {
	case title == "":
		return false
	case mediaWikiKindOf(title) != mediaWikiArticle:
		return true
	default:
		return explicit && m.knownHost(u.Host)
	}
}

// knownHost reports whether a host runs a wiki this build knows about.
func (m *MediaWiki) knownHost(host string) bool {
	if m.wikimedia(host) {
		return true
	}
	for _, domain := range mediaWikiFarms {
		if util.HostMatches(host, domain) {
			return true
		}
	}
	for _, domain := range m.extra {
		if util.HostMatches(host, strings.ToLower(strings.TrimSpace(domain))) {
			return true
		}
	}
	return false
}

// Extract resolves a category, an article, a file page or a wiki's uploads.
func (m *MediaWiki) Extract(ctx context.Context, u *url.URL, _ Options) (*Result, error) {
	title, _ := mediaWikiTitle(u)
	if title == "" {
		return nil, fmt.Errorf("mediawiki: %s does not name a wiki page — "+
			"paste a category, an article, or a File: page", u.Redacted())
	}

	site, err := m.site(ctx, u)
	if err != nil {
		return nil, err
	}

	kind := mediaWikiKindOf(title)
	if kind == mediaWikiArticle {
		// The prefix was in a language this does not read, or there was
		// none. One cheap query settles both, and settles it for every
		// language at once rather than by collecting translations here.
		if kind, title, err = m.namespaceOf(ctx, site, title); err != nil {
			return nil, err
		}
	}

	switch kind {
	case mediaWikiCategory:
		return m.category(ctx, site, u, title)
	case mediaWikiFilePage:
		return m.filePage(ctx, site, title)
	case mediaWikiSpecial:
		return m.uploads(ctx, site, u, title)
	default:
		return m.article(ctx, site, title)
	}
}

// ------------------------------------------------------------- the shapes

// category resolves a category's files, and its subcategories' files when
// the URL asked for them.
//
// It does not descend by default. Commons files its material in a graph
// rather than a tree — a category holds subcategories which hold their own,
// and the edges loop back — so descending by accident turns a category of
// forty pictures into a job of forty thousand. Adding ?depth=<n> to the URL
// is how that is asked for, which the wiki itself ignores and this reads.
func (m *MediaWiki) category(ctx context.Context, site mediaWikiSite, u *url.URL, title string) (*Result, error) {
	var (
		depth   = mediaWikiDepth(u)
		files   []File
		queue   = []string{title}
		seen    = map[string]bool{title: true}
		visited int
	)

	for level := 0; level <= depth && len(queue) > 0 && len(files) < config.MaxListingFiles; level++ {
		var next []string
		for _, category := range queue {
			if visited++; visited > mediaWikiMaxCategories {
				break
			}
			batch, err := m.list(ctx, site, mediaWikiCategoryQuery(category), config.MaxListingFiles-len(files))
			if err != nil {
				// The category the user named is the job; a subcategory
				// that will not answer is one branch of many.
				if category == title {
					return nil, err
				}
				continue
			}
			files = append(files, batch...)
			if len(files) >= config.MaxListingFiles {
				break
			}
			if level < depth {
				subs, err := m.subcategories(ctx, site, category)
				if err != nil {
					continue
				}
				for _, sub := range subs {
					if !seen[sub] {
						seen[sub] = true
						next = append(next, sub)
					}
				}
			}
		}
		queue = next
	}

	if len(files) == 0 {
		if depth == 0 {
			return nil, fmt.Errorf("mediawiki: %s holds no files of its own — "+
				"it may hold only subcategories, which ?depth=1 on the URL descends into", title)
		}
		return nil, fmt.Errorf("mediawiki: %s holds no files, %d levels down", title, depth)
	}
	return &Result{Title: mediaWikiName(title), Files: mediaWikiOrdered(files)}, nil
}

// subcategories lists one category's child categories.
func (m *MediaWiki) subcategories(ctx context.Context, site mediaWikiSite, title string) ([]string, error) {
	query := mediaWikiQuery()
	query.Set("list", "categorymembers")
	query.Set("cmtitle", title)
	query.Set("cmtype", "subcat")
	query.Set("cmlimit", strconv.Itoa(mediaWikiPageSize))

	var out []string
	for range config.MaxAlbumPages {
		resp, err := m.call(ctx, site.api, query)
		if err != nil {
			return nil, err
		}
		for _, member := range resp.Query.CategoryMembers {
			out = append(out, member.Title)
		}
		if len(resp.Continue) == 0 || len(out) >= mediaWikiMaxCategories {
			break
		}
		mediaWikiAdvance(query, resp.Continue)
	}
	return out, nil
}

// article resolves every image an article uses.
//
// That includes the logos and icons its templates draw, because the API
// reports what the page embeds and cannot tell decoration from content. It
// is what was asked for rather than a guess about which pictures were meant.
func (m *MediaWiki) article(ctx context.Context, site mediaWikiSite, title string) (*Result, error) {
	query := mediaWikiFileQuery()
	query.Set("generator", "images")
	query.Set("titles", title)
	query.Set("gimlimit", strconv.Itoa(mediaWikiPageSize))

	files, err := m.list(ctx, site, query, config.MaxListingFiles)
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("mediawiki: %s uses no images", title)
	}
	return &Result{Title: title, Files: mediaWikiOrdered(files)}, nil
}

// filePage resolves one File: page to the file it describes.
func (m *MediaWiki) filePage(ctx context.Context, site mediaWikiSite, title string) (*Result, error) {
	query := mediaWikiFileQuery()
	query.Set("titles", title)

	files, err := m.list(ctx, site, query, 1)
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("mediawiki: %s holds no file (it may have been deleted)", title)
	}
	return &Result{Title: mediaWikiName(title), Files: files}, nil
}

// uploads resolves a special page that lists what a wiki holds.
//
// Sorting by upload date is not decoration: a wiki's whole file list runs to
// millions on the big projects, and the cap would otherwise hand back
// whichever few thousand sort first alphabetically, which is nobody's idea
// of a download. The newest uploads are a bounded thing to ask for, so
// Special:NewFiles is answered on any wiki while Special:ListFiles is
// refused on the ones known to be too large for it to mean anything.
func (m *MediaWiki) uploads(ctx context.Context, site mediaWikiSite, u *url.URL, title string) (*Result, error) {
	page := mediaWikiSpecialPage(title)
	if !mediaWikiUploadPages[page] {
		return nil, fmt.Errorf("mediawiki: %s is not a page that lists files — "+
			"Special:ListFiles and Special:NewFiles are, as is any category page", title)
	}
	newest := mediaWikiNewestPages[page]
	if !newest && m.wikimedia(u.Host) {
		return nil, fmt.Errorf("mediawiki: %s lists far more files than one job can hold — "+
			"paste a category, or Special:NewFiles for the most recent uploads", title)
	}

	query := mediaWikiFileQuery()
	query.Set("generator", "allimages")
	query.Set("gailimit", strconv.Itoa(mediaWikiPageSize))
	if newest {
		query.Set("gaisort", "timestamp")
		query.Set("gaidir", "older") // newest first
	}

	files, err := m.list(ctx, site, query, config.MaxListingFiles)
	if err != nil {
		return nil, err
	}
	label := util.FirstNonEmpty(site.name, u.Host)
	if len(files) == 0 {
		return nil, fmt.Errorf("mediawiki: %s has no uploaded files", label)
	}
	if newest {
		// Left in the order it arrived rather than sorted by name, which is
		// the one listing here where the order carries something: the
		// batches come newest first, and that is what was asked for.
		return &Result{Title: label + " newest files", Files: files}, nil
	}
	return &Result{Title: label + " files", Files: mediaWikiOrdered(files)}, nil
}

// --------------------------------------------------------------- the call

// list runs one paginated query to its end, or to the point where enough
// files have been found.
//
// The cap is a first instalment rather than a refusal: a category of forty
// thousand is a real thing to point at, and handing back the first several
// thousand of them downloads, where an error downloads nothing.
func (m *MediaWiki) list(ctx context.Context, site mediaWikiSite, params url.Values, budget int) ([]File, error) {
	query := maps.Clone(params)
	seen := make(map[string]bool)
	var files []File

	for page := 0; page < config.MaxAlbumPages && len(files) < budget; page++ {
		resp, err := m.call(ctx, site.api, query)
		if err != nil {
			return nil, err
		}
		for _, p := range resp.Query.Pages {
			file, ok := mediaWikiFileOf(p)
			if !ok || seen[p.Title] {
				continue
			}
			seen[p.Title] = true
			files = append(files, file)
		}
		if len(resp.Continue) == 0 {
			break
		}
		mediaWikiAdvance(query, resp.Continue)
	}

	if len(files) > budget {
		files = files[:budget]
	}
	return files, nil
}

// call performs one API request, waiting for this wiki's turn first.
func (m *MediaWiki) call(ctx context.Context, api string, params url.Values) (*mediaWikiResponse, error) {
	if err := m.pace(ctx, api); err != nil {
		return nil, err
	}
	endpoint := api + "?" + params.Encode()

	var resp mediaWikiResponse
	if err := m.client.GetJSON(ctx, endpoint, nil, &resp); err != nil {
		return nil, fmt.Errorf("mediawiki: %w", err)
	}
	if resp.Error != nil {
		return nil, fmt.Errorf("mediawiki: %s (%s)", resp.Error.Info, resp.Error.Code)
	}
	return &resp, nil
}

// pace spaces one wiki's API calls out.
//
// Wikimedia asks automated clients for about a request a second and offers
// no key to be throttled by instead, so the courtesy is the whole of the
// arrangement and is kept unilaterally. It is per wiki because two jobs on
// two unrelated installs are not each other's business.
//
// Each caller claims the next slot before waiting for it, so concurrent
// extractions queue rather than all sleeping out the same second and then
// firing together.
func (m *MediaWiki) pace(ctx context.Context, api string) error {
	if m.interval <= 0 {
		return ctx.Err()
	}

	m.mu.Lock()
	if m.next == nil {
		m.next = make(map[string]time.Time)
	}
	at := time.Now()
	if next, ok := m.next[api]; ok && next.After(at) {
		at = next
	}
	m.next[api] = at.Add(m.interval)
	m.mu.Unlock()

	return util.SleepCtx(ctx, time.Until(at))
}

// namespaceOf asks the wiki which namespace a title is in.
//
// Only titles whose prefix was not recognised come here, which on an English
// wiki is exactly the articles — so the common path costs nothing, and a
// French Catégorie: or a German Datei: is understood by asking the one
// authority on the question rather than by shipping a translation table that
// would cover the languages someone thought of.
func (m *MediaWiki) namespaceOf(ctx context.Context, site mediaWikiSite, title string) (mediaWikiKind, string, error) {
	query := mediaWikiQuery()
	query.Set("titles", title)

	resp, err := m.call(ctx, site.api, query)
	if err != nil {
		return mediaWikiArticle, title, err
	}
	if len(resp.Query.Pages) == 0 {
		return mediaWikiArticle, title, fmt.Errorf("mediawiki: the wiki said nothing about %q", title)
	}

	page := resp.Query.Pages[0]
	if page.Invalid {
		return mediaWikiArticle, title, fmt.Errorf("mediawiki: %q is not a valid title: %s",
			title, util.FirstNonEmpty(page.InvalidReason, "the wiki did not say why"))
	}
	// A file page is reported missing when the file lives on a shared
	// repository, which on any Wikipedia is the usual case, so that flag is
	// only worth believing outside the file namespace.
	if page.Missing && page.NS != mediaWikiNSFile {
		return mediaWikiArticle, title, fmt.Errorf("mediawiki: %s has no page %q", site.name, title)
	}

	normalised := util.FirstNonEmpty(page.Title, title)
	switch page.NS {
	case mediaWikiNSCategory:
		return mediaWikiCategory, normalised, nil
	case mediaWikiNSFile:
		return mediaWikiFilePage, normalised, nil
	case mediaWikiNSSpecial:
		// Special page titles come back in the wiki's own language, which
		// is unreadable here; what the user typed is not.
		return mediaWikiSpecial, title, nil
	default:
		return mediaWikiArticle, normalised, nil
	}
}

// ------------------------------------------------------------ the API base

// site resolves, and remembers, where a host keeps api.php.
func (m *MediaWiki) site(ctx context.Context, u *url.URL) (mediaWikiSite, error) {
	host := strings.ToLower(u.Host)

	m.mu.Lock()
	cached, ok := m.api[host]
	m.mu.Unlock()
	if ok {
		return cached, nil
	}

	site, err := m.resolveSite(ctx, u)
	if err != nil {
		return site, err
	}

	m.mu.Lock()
	if m.api == nil {
		m.api = make(map[string]mediaWikiSite)
	}
	m.api[host] = site
	m.mu.Unlock()
	return site, nil
}

// resolveSite finds api.php, without asking when the answer is already known.
func (m *MediaWiki) resolveSite(ctx context.Context, u *url.URL) (mediaWikiSite, error) {
	origin := util.Origin(u)

	// A URL that names index.php has just given away the script path, and a
	// Wikimedia project's is known. Both are worth a request not spent.
	if script, ok := mediaWikiScriptPath(u.Path); ok {
		return mediaWikiSite{api: origin + script}, nil
	}
	if m.wikimedia(u.Host) {
		return mediaWikiSite{api: origin + mediaWikiAPIPaths[0]}, nil
	}

	var probed []string
	for _, path := range mediaWikiAPIPaths {
		api := origin + path
		if site, ok := m.probe(ctx, api); ok {
			return site, nil
		}
		probed = append(probed, path)
	}
	return mediaWikiSite{}, fmt.Errorf("mediawiki: no MediaWiki API at %s (tried %s) — "+
		"if this wiki keeps it elsewhere, paste a /index.php?title=... link from it",
		u.Host, strings.Join(probed, " and "))
}

// probe asks a candidate endpoint to identify itself.
//
// The answer has to name MediaWiki, not merely arrive: Fandom serves a
// blank 200 at /w/api.php, so a status code would take an empty body for a
// wiki and every query after it would fail for no stated reason.
func (m *MediaWiki) probe(ctx context.Context, api string) (mediaWikiSite, bool) {
	query := mediaWikiQuery()
	query.Set("meta", "siteinfo")
	query.Set("siprop", "general")

	resp, err := m.call(ctx, api, query)
	if err != nil || !strings.HasPrefix(resp.Query.General.Generator, "MediaWiki") {
		return mediaWikiSite{}, false
	}
	return mediaWikiSite{api: api, name: resp.Query.General.SiteName}, true
}

// wikimedia reports whether a host is one of the Wikimedia projects.
func (m *MediaWiki) wikimedia(host string) bool {
	for _, domain := range mediaWikiWikimedia {
		if util.HostMatches(host, domain) {
			return true
		}
	}
	return false
}

// --------------------------------------------------------------- the parts

// mediaWikiQuery is the base every request shares. formatversion=2 is what
// makes pages a list of objects instead of a map keyed by page id.
//
// maxlag, which Wikimedia recommends to bots, is deliberately not sent: it
// turns a lagging database replica into a failed job, and a bulk read has
// nothing better to do than wait its turn anyway.
func mediaWikiQuery() url.Values {
	return url.Values{
		"action":        {"query"},
		"format":        {"json"},
		"formatversion": {"2"},
	}
}

// mediaWikiFileQuery adds what turns pages into downloadable files.
//
// Only url and size are asked for. The API will also report a sha1 and a
// mime type, and both are tempting, but nothing in the download contract
// carries a checksum and the title already ends in the extension the file
// was uploaded under.
func mediaWikiFileQuery() url.Values {
	query := mediaWikiQuery()
	query.Set("prop", "imageinfo")
	query.Set("iiprop", "url|size")
	return query
}

// mediaWikiCategoryQuery lists the files in one category.
func mediaWikiCategoryQuery(title string) url.Values {
	query := mediaWikiFileQuery()
	query.Set("generator", "categorymembers")
	query.Set("gcmtitle", title)
	query.Set("gcmtype", "file")
	query.Set("gcmlimit", strconv.Itoa(mediaWikiPageSize))
	return query
}

// mediaWikiAdvance folds a continuation into the next request.
//
// Whatever the API handed back is handed straight back to it, key by key.
// Which key paginates depends on the generator — gcmcontinue, gimcontinue,
// gaicontinue — and the protocol is explicitly that the client does not need
// to know which.
func mediaWikiAdvance(query url.Values, cont map[string]flexValue) {
	for key, value := range cont {
		query.Set(key, value.String())
	}
}

// mediaWikiFileOf turns one page of a response into a file, or reports that
// there is nothing behind it.
func mediaWikiFileOf(page mediaWikiPage) (File, bool) {
	// A page with no imageinfo at all is a red link: a file some article
	// names that nobody ever uploaded. The flag to test is this one, never
	// page.Missing, which a file on a shared repository also carries.
	if len(page.ImageInfo) == 0 || page.ImageInfo[0].URL == "" {
		return File{}, false
	}
	info := page.ImageInfo[0]

	link, exact := mediaWikiSource(info.URL)
	size := info.Size
	if size <= 0 {
		size = -1
	}
	return File{
		Name: mediaWikiName(page.Title),
		URL:  link,
		Size: size,
		// An exact length is the point of this host: the downloader skips a
		// file already on disk at that length, so a re-run of a category
		// costs one request rather than a re-download.
		SizeApprox: size > 0 && !exact,
	}, true
}

// mediaWikiSource adapts the link the API gave to one that yields the
// original bytes, and reports whether the length the API stated still
// describes what will arrive.
//
// Fandom's CDN re-encodes images to WebP on the way out: a .jpg comes back
// as a WebP a third smaller, which is neither the file that was uploaded nor
// the file the name says it is. ?format=original turns that off. It does not
// return quite the byte count the API reports either, so the size stays as a
// figure to total the job with rather than one to compare a file on disk to.
func mediaWikiSource(raw string) (string, bool) {
	u, err := url.Parse(raw)
	if err != nil || !util.HostMatches(u.Host, "nocookie.net") {
		return raw, true
	}
	query := u.Query()
	if query.Get("format") == "" {
		query.Set("format", "original")
		u.RawQuery = query.Encode()
	}
	return u.String(), false
}

// mediaWikiOrdered sorts files by name.
//
// The API returns each batch sorted by page id however the generator walked
// its own index, so a category paged 500 at a time arrives in an order that
// is neither the category's nor anything else's. Sorting is the difference
// between a listing and a shuffle.
func mediaWikiOrdered(files []File) []File {
	slices.SortStableFunc(files, func(a, b File) int { return strings.Compare(a.Name, b.Name) })
	return files
}

// mediaWikiName is the filename to save under: the title without its
// namespace. Cutting at the first colon is right even for a file whose own
// name contains one, since the namespace is what comes before it.
//
// The URL's last segment would do on Wikimedia but not in general: Fandom
// serves every file from a path ending /revision/latest, which would save an
// album as several hundred files all called "latest".
func mediaWikiName(title string) string {
	if _, rest, ok := strings.Cut(title, ":"); ok && strings.TrimSpace(rest) != "" {
		return strings.TrimSpace(rest)
	}
	return strings.TrimSpace(title)
}

// mediaWikiSpecialPage names which special page a title points at, folded
// for comparison: "Special:ListFiles/Someone" is Special:ListFiles.
func mediaWikiSpecialPage(title string) string {
	_, rest, _ := strings.Cut(title, ":")
	page, _, _ := strings.Cut(rest, "/")
	return strings.ToLower(strings.ReplaceAll(strings.TrimSpace(page), " ", ""))
}

// mediaWikiKindOf reads the namespace off a title by its canonical prefix,
// and calls anything else an article — including a prefix in the wiki's own
// language, which only the wiki can resolve.
func mediaWikiKindOf(title string) mediaWikiKind {
	prefix, rest, ok := strings.Cut(title, ":")
	if !ok || strings.TrimSpace(rest) == "" {
		return mediaWikiArticle
	}
	return mediaWikiPrefixes[strings.ToLower(strings.TrimSpace(prefix))]
}

// mediaWikiTitle reads the page title out of a wiki URL, and reports whether
// the URL said outright that it was one.
//
// Three layouts cover essentially every install: the ?title= parameter,
// an article path, and a wiki whose pages sit at the root. The last is
// indistinguishable from any other URL on the web, so it is returned without
// the claim to be a page — Match is what decides whether that is enough.
func mediaWikiTitle(u *url.URL) (title string, explicit bool) {
	if raw := strings.TrimSpace(u.Query().Get("title")); raw != "" {
		return mediaWikiClean(raw), true
	}
	for _, prefix := range mediaWikiPagePrefixes {
		if i := strings.Index(u.Path, prefix); i >= 0 {
			if rest := mediaWikiClean(u.Path[i+len(prefix):]); rest != "" {
				return rest, true
			}
			return "", false
		}
	}
	return mediaWikiClean(strings.TrimPrefix(u.Path, "/")), false
}

// mediaWikiClean turns the title as it appears in a URL into the title the
// API takes: underscores are spaces, and the API is given the real
// characters rather than an encoding of them.
func mediaWikiClean(raw string) string {
	if decoded, err := url.PathUnescape(raw); err == nil {
		raw = decoded
	}
	return strings.TrimSpace(strings.ReplaceAll(raw, "_", " "))
}

// mediaWikiScriptPath spots a URL that names index.php, which says where
// this install keeps its scripts and so where api.php is.
func mediaWikiScriptPath(path string) (string, bool) {
	const script = "index.php"
	before, _, ok := strings.Cut(path, script)
	if !ok {
		return "", false
	}
	return before + "api.php", true
}

// mediaWikiDepth reads how far into subcategories the URL asked to go.
//
// Bounded twice over: by MaxFolderDepth, because a category graph has cycles
// and no natural bottom, and by the fact that it is off unless asked for.
func mediaWikiDepth(u *url.URL) int {
	depth, err := strconv.Atoi(strings.TrimSpace(u.Query().Get("depth")))
	if err != nil || depth < 0 {
		return 0
	}
	return min(depth, config.MaxFolderDepth)
}

// ------------------------------------------------------------- the answers

// mediaWikiResponse is the part of an API answer this reads. One shape
// serves every call here, since query is a union of what each module fills
// in rather than a different document per module.
type mediaWikiResponse struct {
	// Error arrives with HTTP 200 — a category that does not exist is a
	// well-formed answer as far as the transport is concerned — so it has to
	// be looked for rather than waited for.
	Error *struct {
		Code string `json:"code"`
		Info string `json:"info"`
	} `json:"error"`

	// Continue carries whatever the module wants handed back next time. It
	// is echoed verbatim rather than picked apart, and its values are read
	// loosely because a few modules paginate by number rather than by token.
	Continue map[string]flexValue `json:"continue"`

	Query struct {
		Pages []mediaWikiPage `json:"pages"`
		// CategoryMembers is filled in by the subcategory listing, which
		// asks for names rather than for files and so has no generator.
		CategoryMembers []struct {
			Title string `json:"title"`
		} `json:"categorymembers"`
		// General is the siteinfo the API base is identified by.
		General struct {
			Generator string `json:"generator"`
			SiteName  string `json:"sitename"`
		} `json:"general"`
	} `json:"query"`
}

// mediaWikiPage is one page of a result.
type mediaWikiPage struct {
	Title string `json:"title"`
	NS    int    `json:"ns"`
	// Missing means the page is absent from *this* wiki, which for a file
	// on a shared repository — every image a Wikipedia takes from Commons —
	// is the normal state of affairs, imageinfo and all. See mediaWikiFileOf.
	Missing       bool   `json:"missing"`
	Invalid       bool   `json:"invalid"`
	InvalidReason string `json:"invalidreason"`

	// ImageInfo carries at most one revision here, the current one, which is
	// the default and what iiprop describes.
	ImageInfo []struct {
		URL  string `json:"url"`
		Size int64  `json:"size"`
	} `json:"imageinfo"`
}
