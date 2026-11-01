// SPDX-License-Identifier: MIT

package extractor

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"path"
	"strconv"
	"strings"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
	"github.com/JohanLindvall/HeapLeach/internal/util"
	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// Booru covers the image-board sites that share one of a handful of APIs.
//
// gallery-dl gets most of its reach this way rather than from bespoke
// scrapers: a dozen or so sites run the same software, so one adapter per
// API family covers all of them. The families differ only in the endpoint,
// the pagination parameter and where the file URL sits in the response.
//
// One of them differs in more than that. Gelbooru 0.1 — the booru.org
// network — publishes no API at all: its page=dapi endpoint redirects away,
// so the listing HTML is the listing, and that family is scraped instead of
// queried. It is kept here rather than given a file of its own because
// everything after "where does one page of posts come from" is identical, and
// splitting on the one step that differs would duplicate the seven that do
// not.

// booruAPI is one API family, its dialect in booruDialects.
type booruAPI int

const (
	apiDanbooru booruAPI = iota
	apiE621
	apiMoebooru
	apiGelbooru
	apiPhilomena
	apiTwibooru
	apiSzurubooru
	apiGelbooru01
)

// booruDialect is how a family is asked for one page of posts: its
// endpoint and the constant part of its query, the parameters naming the
// search, the page and the page's size (none when limit is ""), and how the
// page parameter counts — from first, by step a page.
type booruDialect struct {
	path, fixed       string
	tags, page, limit string
	first, step       int
	// json marks a family whose API answers a request that does not name
	// JSON in its Accept header with 406 rather than with HTML.
	json bool
}

// booruDialects are the families' listings, and what each answers with.
var booruDialects = [...]booruDialect{
	// [ {file_url, id, ...} ]
	apiDanbooru: {path: "/posts.json", tags: "tags", page: "page", limit: "limit", first: 1, step: 1},
	// {posts: [ {file: {url}}, ... ]}, at Danbooru's endpoint
	apiE621: {path: "/posts.json", tags: "tags", page: "page", limit: "limit", first: 1, step: 1},
	// [ {file_url, id, ...} ]
	apiMoebooru: {path: "/post.json", tags: "tags", page: "page", limit: "limit", first: 1, step: 1},
	// {post: [...]}, its pid counting pages from nought
	apiGelbooru: {path: "/index.php", fixed: "page=dapi&s=post&q=index&json=1",
		tags: "tags", page: "pid", limit: "limit", step: 1},
	// {images: [...], total}
	apiPhilomena: {path: "/api/v1/json/search/images", tags: "q", page: "page", limit: "per_page", first: 1, step: 1},
	// a later philomena: {posts: [...], total}
	apiTwibooru: {path: "/api/v3/search/posts", tags: "q", page: "page", limit: "per_page", first: 1, step: 1},
	// {results: [...], total}, its offset counting posts
	apiSzurubooru: {path: "/api/posts/", tags: "query", page: "offset", limit: "limit", step: booruPageSize, json: true},
	// the booru.org boards, which have no JSON API at all — page=dapi
	// redirects away — so the listing HTML is what is read. Its pid counts
	// posts rather than pages: the board's own paginator steps it by the
	// size of a page, which the board offers no way to change.
	apiGelbooru01: {path: "/index.php", fixed: "page=post&s=list", tags: "tags", page: "pid", step: gelbooru01PageSize},
}

// booruSite is the board one URL is on, as recognition established it.
type booruSite struct {
	// name is the board's label: its host's first label, or a booru.org
	// board's own subdomain.
	name string
	root string
	api  booruAPI
	// filterID unlocks the default-hidden results on philomena sites: the
	// id of the site's own "everything" filter, which each site numbers
	// differently and publishes among its system filters.
	filterID string
	// probe is the page recognition asked the API through, whose answer
	// for the first page the walk reuses rather than asks again.
	probe *Page
}

// booruPageSize is what each family accepts as a page limit.
const booruPageSize = 100

// Gelbooru 0.1, which is scraped rather than queried.
const (
	// gelbooru01PageSize is what one listing page holds. The board offers no
	// way to ask for more, and its pid counts posts, so this is the step from
	// one page to the next.
	gelbooru01PageSize = 20
	// Markers on the pages a board serves: a thumbnail on a listing, and the
	// full image on a post's own page.
	gelbooru01ThumbClass = "thumb"
	gelbooru01ImageID    = "image"
)

// philomenaEverything is the name philomena sites give the system filter
// that hides nothing.
const philomenaEverything = "Everything"

// Booru resolves tag searches and single posts on image-board sites.
//
// The boards claimed by their hosts (familyInstalls) are known, and each
// is read as the software it is known to run (booruBoards), with nothing
// guessed and nothing asked ahead of the extraction's own requests.
//
// Any other board is recognised: a page by the form its family lays posts
// out at, by the markup its software renders — the posts controller and
// request-forgery token of the Rails boards, the thumbnail anchors of the
// PHP ones, the post registry of Moebooru, the image containers of
// Philomena, the app shell of szurubooru — and then by the family's API
// answering for that very post or listing. Several of these sites answer
// the page with a challenge while serving their JSON openly, so a page
// that cannot say is asked of the API alone.
type Booru struct {
	about
	client *httpx.Client
	// boards are the known boards, a field so a test can stand in for one.
	boards map[string]booruBoard
	// szurubooru remembers per origin whether it answered szurubooru's
	// post API at all: its /post/<n> is every forum's and link
	// aggregator's post too, and a page of them would otherwise put the
	// same guess to the same site once a link.
	szurubooru verdictMemo
}

// booruBoard is what a known board runs: its API family and, on a
// Philomena board, the id of its "Everything" filter.
type booruBoard struct {
	api      booruAPI
	filterID string
}

// booruBoards are the boards claimed by their hosts, by domain (a domain
// takes its subdomains, which is every board booru.org hosts). The
// Philomena boards' "Everything" filters are numbered by each board, and
// are its own system filters; one not known here is looked up.
var booruBoards = map[string]booruBoard{
	"aibooru.online": {api: apiDanbooru}, "booru.borvar.art": {api: apiDanbooru},
	"e621.net": {api: apiE621}, "e926.net": {api: apiE621}, "e6ai.net": {api: apiE621},
	"yande.re": {api: apiMoebooru}, "konachan.com": {api: apiMoebooru}, "konachan.net": {api: apiMoebooru},
	"sakugabooru.com": {api: apiMoebooru},
	"safebooru.org":   {api: apiGelbooru}, "tbib.org": {api: apiGelbooru}, "hypnohub.net": {api: apiGelbooru},
	"xbooru.com":     {api: apiGelbooru},
	"derpibooru.org": {api: apiPhilomena, filterID: "56027"}, "ponybooru.org": {api: apiPhilomena, filterID: "3"},
	"furbooru.org": {api: apiPhilomena, filterID: "2"}, "twibooru.org": {api: apiTwibooru},
	"snootbooru.com": {api: apiSzurubooru}, "booru.foalcon.com": {api: apiSzurubooru},
	"booru.org": {api: apiGelbooru01},
}

// NewBooru builds the image-board extractor. Its shape is the forms the
// families lay their posts out at.
func NewBooru(client *httpx.Client) *Booru {
	return &Booru{client: client, boards: booruBoards, about: about{name: "booru",
		shape: func(u *url.URL) bool { return len(booruCandidates(u)) > 0 },
		recognises: "image-board tag searches and posts on Danbooru, e621, Moebooru, Gelbooru, Philomena, " +
			"Twibooru and szurubooru boards (`/posts?tags=`, `/posts/<id>`, `/post/show/<id>`, " +
			"`/index.php?page=post`, `/images/<id>`, `/post/<id>`) whose page carries the software's " +
			"markup and whose API answers for the same post or listing; Gelbooru 0.1 boards by their " +
			"listing markup alone"}}
}

// Recognise takes a post or listing form whose page is a board's, by its
// markup, and whose API then answers; or, where the page cannot say, whose
// API answers alone.
func (b *Booru) Recognise(ctx context.Context, p *Page) bool {
	return b.detect(ctx, p, p.Final) != nil
}

// Extract resolves a tag search, or a single post, into files. A known
// board (booruBoards) is read as what it runs; any other is detected.
func (b *Booru) Extract(ctx context.Context, u *url.URL, opts Options) (*Result, error) {
	p := probed(ctx, u)
	if board, ok := b.board(u); ok {
		site := &booruSite{name: booruLabel(u.Hostname()), root: util.Origin(u), api: board.api,
			filterID: board.filterID, probe: p}
		if site.api == apiPhilomena && site.filterID == "" {
			site.filterID = philomenaEverythingFilter(ctx, p, b.client, site)
		}
		return b.extract(ctx, u, site, opts)
	}
	if p == nil {
		p = probePage(ctx, b.client, u)
	}
	site := b.detect(ctx, p, u)
	if site == nil {
		return nil, fmt.Errorf("booru: %s is not a board this can read", u.Redacted())
	}
	if site.api == apiPhilomena && site.filterID == "" {
		site.filterID = philomenaEverythingFilter(ctx, p, b.client, site)
	}
	return b.extract(ctx, u, site, opts)
}

func (b *Booru) extract(ctx context.Context, u *url.URL, site *booruSite, opts Options) (*Result, error) {
	tags, postID, listing := site.parseTarget(u)
	if postID != "" {
		files, _, err := b.fetch(ctx, site, "id:"+postID, postID, opts)
		if err != nil {
			return nil, err
		}
		return &Result{Title: site.name + "-" + postID, Files: files}, nil
	}
	// An empty tag list on a listing page means the board's latest posts,
	// which is a normal thing to browse. A bare domain is not: that is
	// almost certainly a mis-paste, not a request for everything.
	if !listing {
		return nil, fmt.Errorf("booru: %s is not a post listing or a single post — "+
			"paste a tag search or a post link", u.Redacted())
	}

	files, note, err := b.fetch(ctx, site, tags, "", opts)
	if err != nil {
		return nil, err
	}
	title := site.name
	if tags != "" {
		title += " " + tags
	} else {
		title += " latest"
	}
	return &Result{Title: title, Files: files, Note: note}, nil
}

// philomenaEverythingFilter looks up the philomena site's own "everything"
// filter, or "" to leave the site's default in force. It is asked through
// the page's memo, so recognition and extraction ask it once between them.
func philomenaEverythingFilter(ctx context.Context, p *Page, client *httpx.Client, site *booruSite) string {
	var resp struct {
		Filters []struct {
			ID   json.Number `json:"id"`
			Name string      `json:"name"`
		} `json:"filters"`
	}
	if !pageJSON(ctx, p, client, site.root+"/api/v1/json/filters/system", site.apiHeaders(), &resp) {
		return ""
	}
	for _, f := range resp.Filters {
		if strings.EqualFold(f.Name, philomenaEverything) {
			return f.ID.String()
		}
	}
	return ""
}

// booruCandidates are the API families a URL's form could belong to, most
// likely first; none when the form is no board's.
func booruCandidates(u *url.URL) []booruAPI {
	segs := util.PathSegments(u)
	switch {
	case u.Query().Get("page") == "post" && (u.Path == "/" || u.Path == "" || u.Path == "/index.php"):
		return []booruAPI{apiGelbooru, apiGelbooru01}
	case len(segs) > 0 && segs[0] == "posts" && len(segs) <= 2:
		return []booruAPI{apiDanbooru, apiSzurubooru, apiTwibooru, apiPhilomena}
	case len(segs) > 0 && segs[0] == "post":
		// Moebooru writes a post as /post/show/<id>, and links it with the
		// post's tags after the id as a slug.
		switch {
		case len(segs) == 1:
			return []booruAPI{apiMoebooru, apiSzurubooru}
		case len(segs) >= 3 && segs[1] == "show":
			return []booruAPI{apiMoebooru}
		case len(segs) == 2:
			return []booruAPI{apiSzurubooru}
		}
	case len(segs) == 2 && segs[0] == "images":
		return []booruAPI{apiPhilomena, apiTwibooru}
	case len(segs) == 1 && segs[0] == "search" && u.Query().Has("q"):
		return []booruAPI{apiPhilomena, apiTwibooru}
	case len(segs) == 0 && u.Query().Has("tags"):
		return []booruAPI{apiDanbooru, apiMoebooru, apiGelbooru, apiSzurubooru, apiPhilomena, apiTwibooru}
	}
	return nil
}

// booruMarked reports whether a page carries a board's own markup: the
// structure one of the families renders, which no ordinary page shares.
func booruMarked(p *Page) bool {
	if p.HTML() == nil {
		return false
	}
	if p.Has("Post.register") || p.Has(`data-uris`) || strings.HasPrefix(p.Generator(), "booru-on-rails") ||
		p.Script("js/app.min.js") && p.Script("js/vendor.min.js") {
		return true
	}
	return p.hasElement(func(n *html.Node) bool {
		switch {
		case n.DataAtom == atom.Body:
			// The Rails boards' posts controller.
			return attr(n, "data-controller") == "posts"
		case n.DataAtom == atom.Img:
			return attr(n, "id") == gelbooru01ImageID && p.Has("page=post")
		case n.DataAtom == atom.A:
			return strings.HasPrefix(attr(n, "id"), "p") && strings.Contains(attr(n, "href"), "s=view")
		case n.DataAtom == atom.Div:
			return attr(n, "id") == "image-container" || hasClass(n, "image-show-container")
		}
		return false
	})
}

// booruDetect works out which board family a page belongs to, or nil.
//
// The URL selects plausible families, even when a custom frontend carries
// no recognised markup. The API must answer for this very target with a
// post id and original-media metadata. Danbooru and e621 share an endpoint;
// the nested file in the answer distinguishes them. A Gelbooru-shaped page
// whose API does not answer is a Gelbooru 0.1 board, which publishes none
// and is scraped — taken only on its markup, never on silence. Every API
// question goes through the page's memo, so recognising and extracting ask
// it once.
func booruDetect(ctx context.Context, p *Page, client *httpx.Client, u *url.URL) *booruSite {
	return booruDetectGated(ctx, p, client, u, nil)
}

// detect is booruDetect with szurubooru's guess put once per origin
// (Booru.szurubooru).
func (b *Booru) detect(ctx context.Context, p *Page, u *url.URL) *booruSite {
	return booruDetectGated(ctx, p, b.client, u, &b.szurubooru)
}

// board is the known board u is on, if any.
func (b *Booru) board(u *url.URL) (booruBoard, bool) {
	for domain, board := range b.boards {
		if util.HostMatches(u.Hostname(), domain) {
			return board, true
		}
	}
	return booruBoard{}, false
}

// booruDetectGated is booruDetect, asking szurubooru's API of an origin
// only while szurubooru (when set) has not found that it answers in
// another shape or not at all.
func booruDetectGated(ctx context.Context, p *Page, client *httpx.Client, u *url.URL, szurubooru *verdictMemo) *booruSite {
	candidates := booruCandidates(u)
	marked := booruMarked(p)
	if len(candidates) == 0 && marked && u.Path == "/search" {
		// A bare /search is the latest images on a Philomena board, and
		// too common a form for the link scanner: it is taken only from a
		// page that carries a board's own markup.
		candidates = []booruAPI{apiPhilomena, apiTwibooru}
	}
	if len(candidates) == 0 {
		return nil
	}
	segs := util.PathSegments(u)
	name := booruLabel(u.Hostname())
	for _, api := range candidates {
		if ctx.Err() != nil {
			return nil
		}
		if api == apiSzurubooru && len(segs) > 1 && segs[0] == "posts" {
			continue // szurubooru's single-post route is /post/<id>
		}
		site := &booruSite{name: name, root: util.Origin(u), api: api, probe: p}
		tags, postID, listing := site.parseTarget(u)
		if !listing && postID == "" {
			continue
		}
		if api == apiGelbooru01 {
			// A Gelbooru 0.2 board renders much the same markup, so an API
			// that did not answer once is not taken for one that does not
			// exist: only a page whose images are on the legacy network's
			// own thumbnail and image layout is scraped. Otherwise a 0.2
			// board whose API failed would be read for its samples.
			if marked && p.OK() && gelbooru01Layout(p) {
				return site
			}
			continue
		}
		if postID != "" {
			tags = "id:" + postID
		}
		if api == apiSzurubooru && szurubooru != nil && !szurubooru.ask(ctx, u.Host, func() (yes, definite bool) {
			return szurubooruAnswered(ctx, p, client, site, tags)
		}) {
			continue
		}
		if booruAnswers(ctx, p, client, site, tags, postID, marked) {
			return site
		}
		// A philomena site's default filter hides much of what it holds,
		// so a post or search it hides answers empty. Asked again under
		// the site's own "everything" filter, it answers for what it has.
		if api == apiPhilomena && philomenaAnswered(ctx, p, client, site, tags) {
			if site.filterID = philomenaEverythingFilter(ctx, p, client, site); site.filterID != "" &&
				booruAnswers(ctx, p, client, site, tags, postID, marked) {
				return site
			}
		}
	}
	return nil
}

// booruAnswers reports whether the site's API answers the first page of tags
// with a post, the post postID when one is named, that has a file; the
// answer's nested file says e621 apart from Danbooru, which share an
// endpoint. On a page that carries a board's own markup (marked), a search
// whose first page holds only posts that withhold their file from an
// anonymous caller answers too: later pages may still hold files.
func booruAnswers(ctx context.Context, p *Page, client *httpx.Client, site *booruSite, tags, postID string, marked bool) bool {
	var raw json.RawMessage
	if !pageJSON(ctx, p, client, site.endpoint(tags, 0), site.apiHeaders(), &raw) {
		return false
	}
	posts, err := booruDecode(raw)
	if err != nil {
		return false
	}
	hidden := false
	for _, post := range posts {
		if post.ID.String() == "" || (postID != "" && post.ID.String() != postID) {
			continue
		}
		if post.fileURL(site) == "" {
			hidden = hidden || marked
			continue
		}
		if site.api == apiDanbooru && post.File.URL != "" {
			site.api = apiE621
		}
		return true
	}
	return hidden && postID == ""
}

// philomenaAnswered reports whether the first page of tags was answered in
// philomena's own shape, a list of images however empty: a site that runs
// the software, whose filter may be what emptied it.
func philomenaAnswered(ctx context.Context, p *Page, client *httpx.Client, site *booruSite, tags string) bool {
	var answer struct {
		Images *[]json.RawMessage `json:"images"`
	}
	return pageJSON(ctx, p, client, site.endpoint(tags, 0), site.apiHeaders(), &answer) && answer.Images != nil
}

// szurubooruAnswered reports whether the first page of tags was answered in
// szurubooru's own shape, a list of results however empty: an origin that
// runs the software, whatever this one post or search holds. It is the
// question booruAnswers asks, read for its shape alone, so the first link
// on an origin costs no request of its own.
// It also reports whether the endpoint gave an answer of its own, so that
// only that is remembered as a no (verdictMemo).
func szurubooruAnswered(ctx context.Context, p *Page, client *httpx.Client, site *booruSite, tags string) (yes, answered bool) {
	var answer struct {
		Results *[]json.RawMessage `json:"results"`
	}
	ok, answered := pageJSONAnswer(ctx, p, client, site.endpoint(tags, 0), site.apiHeaders(), &answer)
	return ok && answer.Results != nil, answered
}

// gelbooru01Layout reports whether a page's images are laid out as the
// legacy network serves them: a listing thumbnail the original can be
// derived from (gelbooru01Image), or a post's image under its board's own
// images directory on an image host.
func gelbooru01Layout(p *Page) bool {
	return p.hasElement(func(n *html.Node) bool {
		if n.DataAtom != atom.Img {
			return false
		}
		src := attr(n, "src")
		if gelbooru01Image(src) != "" {
			return true
		}
		return attr(n, "id") == gelbooru01ImageID && gelbooru01Original(src)
	})
}

// gelbooru01Original reports whether a post's image is where the legacy
// network keeps originals: https://img.<network>/<board>/images/<dir>/<name>.
func gelbooru01Original(src string) bool {
	u, err := url.Parse(strings.TrimSpace(src))
	if err != nil || !strings.HasPrefix(u.Hostname(), "img.") {
		return false
	}
	segs := util.PathSegments(u)
	return len(segs) >= 4 && segs[1] == "images"
}

// booruLabel names a board by its host's first label, as hostLabel does,
// unless that label is a word boards share — "booru" itself, an image or
// mobile subdomain — when the next label is the board's own: two boards at
// booru.<a> and booru.<b> never share a name or a folder.
func booruLabel(host string) string {
	labels := strings.Split(strings.TrimPrefix(strings.ToLower(host), "www."), ".")
	if len(labels) > 2 {
		switch labels[0] {
		case "booru", "img", "m", "api", "boards":
			return labels[1]
		}
	}
	return hostLabel(host)
}

// parseTarget pulls a tag query or a single post id out of a URL, and
// reports whether the URL is a post listing at all.
func (s *booruSite) parseTarget(u *url.URL) (tags, postID string, listing bool) {
	query := u.Query()
	segs := util.PathSegments(u)
	_, hasTags := query["tags"]

	switch s.api {
	case apiGelbooru, apiGelbooru01:
		// /index.php?page=post&s=list&tags=... or &s=view&id=...
		if id := query.Get("id"); id != "" && query.Get("s") == "view" {
			return "", id, true
		}
		return query.Get("tags"), "", hasTags || query.Get("page") == "post"
	case apiSzurubooru:
		// /post/123  or  /posts?query=...
		if len(segs) >= 2 && segs[0] == "post" {
			return "", segs[1], true
		}
		_, hasQuery := query["query"]
		return cmp.Or(query.Get("query"), query.Get("tags")), "",
			hasQuery || hasTags || (len(segs) >= 1 && segs[0] == "posts")
	case apiPhilomena, apiTwibooru:
		// /search?q=...  or  /images/123
		if len(segs) == 2 && (segs[0] == "images" || segs[0] == "posts") {
			return "", segs[1], true
		}
		_, hasQuery := query["q"]
		return cmp.Or(query.Get("q"), query.Get("tags")), "",
			hasQuery || hasTags || (len(segs) == 1 && segs[0] == "search")
	case apiMoebooru:
		// /post?tags=...  or  /post/show/123
		if len(segs) >= 3 && segs[0] == "post" && segs[1] == "show" {
			return "", segs[2], true
		}
		return query.Get("tags"), "", hasTags || (len(segs) == 1 && segs[0] == "post")
	default: // danbooru and e621 share /posts?tags= and /posts/123
		if len(segs) == 2 && segs[0] == "posts" {
			if _, err := strconv.Atoi(segs[1]); err == nil {
				return "", segs[1], true
			}
		}
		return query.Get("tags"), "", hasTags || (len(segs) == 1 && segs[0] == "posts")
	}
}

// fetch pages through the board until the results run out: to the total
// the families that state one give, or else to the first empty page.
func (b *Booru) fetch(ctx context.Context, site *booruSite, tags, single string, opts Options) ([]File, string, error) {
	fileHeaders := httpx.Referer(site.root + "/")
	apiHeaders := site.apiHeaders()

	// Posts are what is walked, so that a page whose posts all withhold their
	// file from an anonymous caller is no page that adds nothing. A board
	// that stops answering partway through is not a reason to discard what
	// it did answer: booru.org ends a long walk exactly this way when its
	// rate limit runs out.
	//
	// The walk is held to the boards' own bound as well as the user's file
	// cap (hostLimit), since they rate-limit a long walk hard. A board that
	// ignores its page parameter past the end serves the last page again,
	// and a page that adds nothing is that end rather than a cut. A legacy
	// board's pages hold a fixed twenty, so a shorter one is its last.
	limit, limitNote := hostLimit(opts, config.MaxBooruPosts)
	posts, note, err := walkPages(ctx, pageWalk[booruPost]{
		key: func(post booruPost) string {
			return cmp.Or(post.ID.String(), post.fileURL(site), post.MD5)
		},
		limit: limit, limitNote: limitNote,
		// Gelbooru 0.2 answers a page past the end with an empty body,
		// which is that end, not a page that could not be fetched.
		ends: walkAccepts{repeated: true, undecodable: true},
		// The bound counts files: a post that withholds its file from an
		// anonymous caller uses none of it.
		weigh: func(posts []booruPost) int {
			n := 0
			for _, post := range posts {
				if post.fileURL(site) != "" {
					n++
				}
			}
			return n
		},
		fetch: func(ctx context.Context, n int) (pageOf[booruPost], error) {
			posts, total, short, err := b.posts(ctx, site, tags, single, n, apiHeaders)
			if err != nil {
				return pageOf[booruPost]{}, fmt.Errorf("booru: %s: %w", site.name, err)
			}
			// A single post never pages.
			return pageOf[booruPost]{items: posts, next: true, total: total,
				end: single != "" || len(posts) == 0 || short}, nil
		},
	})
	if err != nil {
		return nil, "", err
	}
	var files []File
	for _, post := range posts {
		link := post.fileURL(site)
		if link == "" {
			continue
		}
		files = append(files, File{
			Name:    post.filename(link),
			URL:     link,
			Size:    post.size(),
			Headers: fileHeaders,
		})
	}
	if len(files) == 0 {
		return nil, "", fmt.Errorf("booru: %s returned no posts for %q", site.name, tags)
	}
	// The page that crossed the bound was kept whole; what is past it is
	// this walk's to cut.
	if len(files) > limit {
		files = files[:limit]
	}
	return files, note, nil
}

// posts fetches one page of results in whichever form the board publishes
// them: as JSON from an API, or as the listing page itself, with the total
// the page states, 0 when it states none, and whether the page is short of
// the fixed size a board that sets one serves, which makes it the last.
func (b *Booru) posts(ctx context.Context, site *booruSite, tags, single string,
	page int, headers httpx.Header) ([]booruPost, int, bool, error) {

	if site.api == apiGelbooru01 {
		if single != "" {
			posts, err := b.scrapePost(ctx, site, single, headers)
			return posts, 0, false, err
		}
		if page > 0 {
			if err := util.SleepCtx(ctx, config.Gelbooru01Pace); err != nil {
				return nil, 0, false, err
			}
		}
		posts, thumbs, err := b.scrapeList(ctx, site, tags, page, headers)
		return posts, 0, thumbs < gelbooru01PageSize, err
	}
	posts, total, err := b.page(ctx, site, tags, page, headers)
	return posts, total, false, err
}

// hostLimit is the lower of the user's file cap and a host's own bound on a
// long walk, with the note naming which cut it.
func hostLimit(opts Options, bound int) (int, string) {
	if limit := opts.maxFiles(); limit <= bound {
		return limit, fileLimitNote
	}
	return bound, postLimitNote
}

// apiHeaders are what a request for a listing needs, which is not what a
// request for a file needs.
//
// szurubooru answers 406 to any request whose Accept header does not name
// JSON. The client's default happens to name it, but a default is not where a
// server's hard requirement belongs — stated here, it survives someone
// tuning that default for a different host. It is deliberately kept off the
// files: asking an image server for JSON is a different question.
func (s *booruSite) apiHeaders() httpx.Header {
	headers := httpx.Referer(s.root + "/")
	if booruDialects[s.api].json {
		headers[httpx.HeaderAccept] = httpx.ContentTypeJSON
	}
	return headers
}

// endpoint builds the API request for one page, counted from 0, in the
// family's dialect.
func (s *booruSite) endpoint(tags string, page int) string {
	d := booruDialects[s.api]
	query, _ := url.ParseQuery(d.fixed)
	query.Set(d.tags, tags)
	query.Set(d.page, strconv.Itoa(d.first+page*d.step))
	if d.limit != "" {
		query.Set(d.limit, strconv.Itoa(booruPageSize))
	}
	// Only a philomena site's is ever looked up.
	if s.filterID != "" {
		query.Set("filter_id", s.filterID)
	}
	return s.root + d.path + "?" + query.Encode()
}

// page fetches and normalises one response, whatever shape it arrives in,
// with the total that szurubooru and philomena state beside their results.
// The first page is the one recognition asked for, when it did, and its
// answer is taken rather than asked again.
func (b *Booru) page(ctx context.Context, site *booruSite, tags string, n int, headers httpx.Header) ([]booruPost, int, error) {
	endpoint := site.endpoint(tags, n)
	var raw json.RawMessage
	if n > 0 || !askedJSON(ctx, site.probe, b.client, endpoint, headers, &raw) {
		if err := b.client.GetJSON(ctx, endpoint, headers, &raw); err != nil {
			return nil, 0, err
		}
	}
	var stated struct {
		Total int `json:"total"`
	}
	if len(raw) > 0 && raw[0] == '{' {
		_ = json.Unmarshal(raw, &stated)
	}
	posts, err := booruDecode(raw)
	return posts, stated.Total, err
}

func booruDecode(raw json.RawMessage) ([]booruPost, error) {
	// The families wrap their results differently: a bare array, or an
	// object keyed by "posts", "images", "results" or "post".
	if len(raw) > 0 && raw[0] == '{' {
		var wrapper struct {
			Posts   []booruPost `json:"posts"`
			Images  []booruPost `json:"images"`
			Results []booruPost `json:"results"`
			Post    []booruPost `json:"post"`
		}
		if err := json.Unmarshal(raw, &wrapper); err != nil {
			return nil, fmt.Errorf("unexpected response: %w", err)
		}
		switch {
		case wrapper.Posts != nil:
			return wrapper.Posts, nil
		case wrapper.Images != nil:
			return wrapper.Images, nil
		case wrapper.Results != nil:
			return wrapper.Results, nil
		default:
			return wrapper.Post, nil
		}
	}

	var posts []booruPost
	if err := json.Unmarshal(raw, &posts); err != nil {
		return nil, fmt.Errorf("unexpected response: %w", err)
	}
	return posts, nil
}

// ------------------------------------------------------------ Gelbooru 0.1

// scrapeList reads one page of a listing on a board with no API.
//
// Reading the listing is not a fallback here, it is the cheap route: a
// thumbnail's URL says where the full image is, the two differing only by
// host prefix and one path segment, so twenty posts cost one request instead
// of twenty-one. If the board ever changes that mapping the downloads fail
// visibly with a 404 rather than quietly saving thumbnails.
func (b *Booru) scrapeList(ctx context.Context, site *booruSite, tags string,
	page int, headers httpx.Header) ([]booruPost, int, error) {

	doc, err := b.client.GetString(ctx, site.endpoint(tags, page), headers)
	if err != nil {
		return nil, 0, err
	}
	posts, thumbs := gelbooru01Posts(doc)
	return posts, thumbs, nil
}

// scrapePost reads a single post's own page, which is where a board with no
// API is the only one that states the full image outright.
func (b *Booru) scrapePost(ctx context.Context, site *booruSite, id string,
	headers httpx.Header) ([]booruPost, error) {

	doc, err := b.client.GetString(ctx, site.postURL(id), headers)
	if err != nil {
		return nil, err
	}
	return gelbooru01Post(doc, id), nil
}

// postURL is one post's own page.
func (s *booruSite) postURL(id string) string {
	query := url.Values{}
	query.Set("page", "post")
	query.Set("s", "view")
	query.Set("id", id)
	return s.root + "/index.php?" + query.Encode()
}

// gelbooru01Posts reads the posts out of a listing page. Kept apart from the
// fetch so a fixture can stand in for the board.
//
// Only the thumbnails count. A board's page carries plenty of other images —
// its own furniture, and the adverts it is paid for — and every one of them
// would otherwise be queued as a post. How many thumbnails the page held is
// returned beside the posts, read or not, since that is what says whether
// the page was a full one.
func gelbooru01Posts(doc string) ([]booruPost, int) {
	root, err := parseHTML(doc)
	if err != nil {
		return nil, 0
	}

	var posts []booruPost
	thumbs := findAll(root, func(n *html.Node) bool {
		return hasClass(n, gelbooru01ThumbClass)
	})
	for _, thumb := range thumbs {
		img := findFirst(thumb, func(n *html.Node) bool { return isElem(n, atom.Img) })
		if img == nil {
			continue
		}
		link := gelbooru01Image(attr(img, "src"))
		if link == "" {
			continue
		}
		posts = append(posts, booruPost{
			ID:      flexValue(gelbooru01PostID(thumb)),
			FileURL: link,
		})
	}
	return posts, len(thumbs)
}

// gelbooru01Post reads the one image a post's page shows.
func gelbooru01Post(doc, id string) []booruPost {
	root, err := parseHTML(doc)
	if err != nil {
		return nil
	}
	img := findFirst(root, func(n *html.Node) bool {
		return isElem(n, atom.Img) && attr(n, "id") == gelbooru01ImageID
	})
	if img == nil {
		return nil
	}
	link := strings.TrimSpace(attr(img, "src"))
	if link == "" {
		return nil
	}
	return []booruPost{{ID: flexValue(id), FileURL: link}}
}

// gelbooru01PostID reads a post's id off its thumbnail, which carries it
// twice: as the anchor's element id, and in the link's own query.
func gelbooru01PostID(thumb *html.Node) string {
	a := findFirst(thumb, func(n *html.Node) bool { return isElem(n, atom.A) })
	if a == nil {
		return ""
	}
	if id := strings.TrimPrefix(attr(a, "id"), "p"); id != "" {
		return id
	}
	if link, err := url.Parse(attr(a, "href")); err == nil {
		return link.Query().Get("id")
	}
	return ""
}

// gelbooru01Image turns a listing thumbnail into the original it stands for.
// The two differ only in the host's prefix and one path segment:
//
//	https://thumbs.booru.org/<board>/thumbnails/<dir>/thumbnail_<name>
//	https://img.booru.org/<board>/images/<dir>/<name>
//
// The thumbnail keeps the original's extension, animations included, which is
// what makes deriving one from the other safe. Anything that is not a
// thumbnail yields "" and is skipped.
func gelbooru01Image(thumb string) string {
	u, err := url.Parse(strings.TrimSpace(thumb))
	if err != nil || u.Host == "" {
		return ""
	}
	rest, ok := strings.CutPrefix(u.Host, "thumbs.")
	if !ok {
		return ""
	}
	// The board writes these paths with a doubled separator. What matters is
	// the segments, not the spelling, so they are taken apart and rebuilt.
	segs := util.PathSegments(u)
	if len(segs) < 4 || segs[1] != "thumbnails" {
		return ""
	}
	name, ok := strings.CutPrefix(segs[len(segs)-1], "thumbnail_")
	if !ok {
		return ""
	}

	rebuilt := append([]string{segs[0], "images"}, segs[2:len(segs)-1]...)
	u.Host = "img." + rest
	u.Path = "/" + path.Join(append(rebuilt, name)...)
	u.RawPath = ""
	return u.String()
}

// booruPost is the union of the fields the families use to locate a file.
type booruPost struct {
	ID  flexValue `json:"id"`
	MD5 string    `json:"md5"`

	// Danbooru, moebooru and most gelbooru boards.
	FileURL string `json:"file_url"`
	Size    int64  `json:"file_size"`

	// Gelbooru boards that omit file_url give the pieces to build one.
	// Directory is a number on some boards and a nested path on others.
	Directory flexValue `json:"directory"`
	Image     string    `json:"image"`

	// e621 nests the URL.
	File struct {
		URL  string `json:"url"`
		Ext  string `json:"ext"`
		Size int64  `json:"size"`
	} `json:"file"`

	// Philomena nests it under representations.
	ViewURL         string `json:"view_url"`
	Representations struct {
		Full string `json:"full"`
	} `json:"representations"`

	// szurubooru states the file as a path relative to the site root, and
	// names its length differently from everyone else.
	ContentURL string `json:"contentUrl"`
	Bytes      int64  `json:"fileSize"`
}

// fileURL finds the full-size file for a post.
func (p *booruPost) fileURL(site *booruSite) string {
	if link := cmp.Or(p.FileURL, p.File.URL, p.ContentURL,
		p.ViewURL, p.Representations.Full); link != "" {
		switch {
		case strings.HasPrefix(link, "//"):
			return "https:" + link
		case strings.HasPrefix(link, "/"):
			return site.root + link
		case !strings.Contains(link, "://"):
			// szurubooru writes a post's file as a path relative to the site
			// root, with nothing — not even a leading slash — to mark it as
			// one. Left alone it would be queued as a URL and fail to parse.
			return site.root + "/" + link
		}
		return link
	}
	// Gelbooru boards that omit file_url store the file by directory.
	if p.Directory.String() != "" && p.Image != "" {
		return site.root + "/images/" + p.Directory.String() + "/" + p.Image
	}
	return ""
}

// filename names the saved file after the post, so results sort by id and
// two boards never collide.
func (p *booruPost) filename(link string) string {
	ext := ""
	if p.File.Ext != "" {
		ext = "." + p.File.Ext
	}
	ext = linkExtAsIs(link, ext)
	if id := p.ID.String(); id != "" {
		return id + ext
	}
	return cmp.Or(util.NameFromURL(link), p.MD5+ext)
}

// size reports the byte length when the board included one.
func (p *booruPost) size() int64 {
	return knownSize(p.Size, p.File.Size, p.Bytes)
}
