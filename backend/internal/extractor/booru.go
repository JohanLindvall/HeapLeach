// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

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

// booruAPI is one API family.
type booruAPI int

const (
	// apiDanbooru: GET /posts.json -> [ {file_url, id, ...} ], page=N
	apiDanbooru booruAPI = iota
	// apiE621: GET /posts.json -> {posts: [ {file: {url}}, ... ]}, page=N
	apiE621
	// apiMoebooru: GET /post.json -> [ {file_url, id, ...} ], page=N
	apiMoebooru
	// apiGelbooru: GET /index.php?page=dapi&s=post&q=index&json=1, pid=N
	apiGelbooru
	// apiPhilomena: GET /api/v1/json/search/images -> {images: [...]}, page=N
	apiPhilomena
	// apiTwibooru: a later philomena at /api/v3/search/posts -> {posts: [...]}
	apiTwibooru
	// apiSzurubooru: GET /api/posts/?query=<tags> -> {results: [...]},
	// offset=N counting posts. The Accept header is not optional here: a
	// request that does not name JSON is answered 406, not with HTML.
	apiSzurubooru
	// apiGelbooru01: the booru.org boards, which have no JSON API at all —
	// page=dapi redirects away — so the listing HTML is what is read.
	apiGelbooru01
)

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
}

const (
	// booruPageSize is what each family accepts as a page limit.
	booruPageSize = 100
	// booruMaxPosts bounds a tag search, which can otherwise be endless.
	booruMaxPosts = 1000
)

// Gelbooru 0.1, which is scraped rather than queried.
const (
	// gelbooru01PageSize is what one listing page holds. The board offers no
	// way to ask for more, and this is what tells a last page from a full
	// one, so it has to be what the site does rather than what we would like.
	gelbooru01PageSize = 20
	// gelbooru01Pace is the wait between listing pages.
	//
	// Twenty posts to a page makes a long search a great many requests in a
	// row, and these boards answer a burst of them with 429: measured, a walk
	// with no pause is refused around the thirteenth page, and the same walk
	// with a second between pages is not refused at all. Backing off after
	// the refusal costs more than this does, and yields less.
	gelbooru01Pace = time.Second
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
// No board is known by name. A page is recognised by the form its family
// lays posts out at, by the markup its software renders — the posts
// controller and request-forgery token of the Rails boards, the thumbnail
// anchors of the PHP ones, the post registry of Moebooru, the image
// containers of Philomena, the app shell of szurubooru — and then by the
// family's API answering for that very post or listing. Several of these
// sites answer the page with a challenge while serving their JSON openly,
// so a page that cannot say is asked of the API alone.
type Booru struct {
	client *httpx.Client
}

// NewBooru builds the image-board extractor.
func NewBooru(client *httpx.Client) *Booru { return &Booru{client: client} }

func (b *Booru) Name() string { return "booru" }

// Recognises is the inventory's description.
func (b *Booru) Recognises() string {
	return "image-board tag searches and posts on Danbooru, e621, Moebooru, Gelbooru, Philomena, " +
		"Twibooru and szurubooru boards (`/posts?tags=`, `/posts/<id>`, `/post/show/<id>`, " +
		"`/index.php?page=post`, `/images/<id>`, `/post/<id>`) whose page carries the software's " +
		"markup and whose API answers for the same post or listing; Gelbooru 0.1 boards by their " +
		"listing markup alone"
}

// Shape reports the forms the families lay their posts out at.
func (b *Booru) Shape(u *url.URL) bool { return len(booruCandidates(u)) > 0 }

// Recognise takes a post or listing form whose page is a board's, by its
// markup, and whose API then answers; or, where the page cannot say, whose
// API answers alone.
func (b *Booru) Recognise(ctx context.Context, p *Page) bool {
	return booruDetect(ctx, p, b.client, p.Final) != nil
}

// Extract resolves a tag search, or a single post, into files.
func (b *Booru) Extract(ctx context.Context, u *url.URL, _ Options) (*Result, error) {
	p := probed(ctx, u)
	if p == nil {
		p = probePage(ctx, b.client, u)
	}
	site := booruDetect(ctx, p, b.client, u)
	if site == nil {
		return nil, fmt.Errorf("booru: %s is not a board this can read", u.Redacted())
	}
	if site.api == apiPhilomena {
		site.filterID = b.everythingFilter(ctx, site)
	}
	return b.extract(ctx, u, site)
}

func (b *Booru) extract(ctx context.Context, u *url.URL, site *booruSite) (*Result, error) {
	tags, postID, listing := site.parseTarget(u)
	if postID != "" {
		files, err := b.fetch(ctx, site, "id:"+postID, postID)
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

	files, err := b.fetch(ctx, site, tags, "")
	if err != nil {
		return nil, err
	}
	title := site.name
	if tags != "" {
		title += " " + tags
	} else {
		title += " latest"
	}
	return &Result{Title: title, Files: files}, nil
}

// everythingFilter looks up the philomena site's own "everything" filter,
// or "" to leave the site's default in force.
func (b *Booru) everythingFilter(ctx context.Context, site *booruSite) string {
	var resp struct {
		Filters []struct {
			ID   json.Number `json:"id"`
			Name string      `json:"name"`
		} `json:"filters"`
	}
	if !platformJSON(ctx, b.client, site.root+"/api/v1/json/filters/system", site.apiHeaders(), &resp) {
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
	case len(segs) > 0 && segs[0] == "post" && len(segs) <= 3:
		if len(segs) == 1 || (len(segs) == 3 && segs[1] == "show") {
			return []booruAPI{apiMoebooru, apiSzurubooru}
		}
		if len(segs) == 2 {
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
// The page has to carry a board's markup first, unless it cannot say; the
// family is then settled by its API answering for this very target with a
// post id and original-media metadata. Danbooru and e621 share an endpoint;
// the nested file in the answer distinguishes them. A Gelbooru-shaped page
// whose API does not answer is a Gelbooru 0.1 board, which publishes none
// and is scraped — taken only on its markup, never on silence. Every API
// question goes through the page's memo, so recognising and extracting ask
// it once.
func booruDetect(ctx context.Context, p *Page, client *httpx.Client, u *url.URL) *booruSite {
	candidates := booruCandidates(u)
	marked := booruMarked(p)
	if len(candidates) == 0 || (!marked && !p.Silent()) {
		return nil
	}
	segs := util.PathSegments(u)
	name := hostLabel(u.Hostname())
	for _, api := range candidates {
		if ctx.Err() != nil {
			return nil
		}
		if api == apiSzurubooru && len(segs) > 1 && segs[0] == "posts" {
			continue // szurubooru's single-post route is /post/<id>
		}
		site := &booruSite{name: name, root: util.Origin(u), api: api}
		tags, postID, listing := site.parseTarget(u)
		if !listing && postID == "" {
			continue
		}
		if api == apiGelbooru01 {
			if marked && p.OK() {
				return site
			}
			continue
		}
		if postID != "" {
			tags = "id:" + postID
		}
		var raw json.RawMessage
		if !pageJSON(ctx, p, client, site.endpoint(tags, 0), site.apiHeaders(), &raw) {
			continue
		}
		posts, err := booruDecode(raw)
		if err != nil {
			continue
		}
		for _, post := range posts {
			if post.ID.String() == "" || post.fileURL(site) == "" || (postID != "" && post.ID.String() != postID) {
				continue
			}
			if api == apiDanbooru && post.File.URL != "" {
				site.api = apiE621
			}
			return site
		}
	}
	return nil
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
		return util.FirstNonEmpty(query.Get("query"), query.Get("tags")), "",
			hasQuery || hasTags || (len(segs) >= 1 && segs[0] == "posts")
	case apiPhilomena, apiTwibooru:
		// /search?q=...  or  /images/123
		if len(segs) == 2 && (segs[0] == "images" || segs[0] == "posts") {
			return "", segs[1], true
		}
		_, hasQuery := query["q"]
		return util.FirstNonEmpty(query.Get("q"), query.Get("tags")), "",
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

// fetch pages through the board until the results run out.
func (b *Booru) fetch(ctx context.Context, site *booruSite, tags, single string) ([]File, error) {
	var files []File
	fileHeaders := httpx.Referer(site.root + "/")
	apiHeaders := site.apiHeaders()

	// Capped on pages as well as posts: a tag whose posts all withhold their
	// file from an anonymous caller adds nothing per page and would
	// otherwise be walked to the board's end.
	for page := 0; len(files) < booruMaxPosts && page < config.MaxAlbumPages; page++ {
		posts, err := b.posts(ctx, site, tags, single, page, apiHeaders)
		if err != nil {
			if page == 0 {
				return nil, fmt.Errorf("booru: %s: %w", site.name, err)
			}
			// A board that stops answering partway through is not a reason
			// to discard what it did answer: several hundred posts already
			// in hand beat an error message. booru.org ends a long walk
			// exactly this way when its rate limit runs out.
			break
		}
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
		// A short page is the last page, and a single post never pages.
		if len(posts) < site.pageSize() || single != "" {
			break
		}
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("booru: %s returned no posts for %q", site.name, tags)
	}
	return files, nil
}

// posts fetches one page of results in whichever form the board publishes
// them: as JSON from an API, or as the listing page itself.
func (b *Booru) posts(ctx context.Context, site *booruSite, tags, single string,
	page int, headers httpx.Header) ([]booruPost, error) {

	if site.api == apiGelbooru01 {
		if single != "" {
			return b.scrapePost(ctx, site, single, headers)
		}
		if page > 0 {
			if err := util.SleepCtx(ctx, gelbooru01Pace); err != nil {
				return nil, err
			}
		}
		return b.scrapeList(ctx, site, tags, page, headers)
	}
	return b.page(ctx, site.endpoint(tags, page), headers)
}

// pageSize is how many posts one request returns when there are more to come.
// It is what tells the last page from a full one, so it has to be what the
// board does rather than what was asked of it.
func (s *booruSite) pageSize() int {
	if s.api == apiGelbooru01 {
		return gelbooru01PageSize
	}
	return booruPageSize
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
	if s.api == apiSzurubooru {
		headers[httpx.HeaderAccept] = httpx.ContentTypeJSON
	}
	return headers
}

// endpoint builds the API request for one page.
func (s *booruSite) endpoint(tags string, page int) string {
	query := url.Values{}
	switch s.api {
	case apiGelbooru:
		query.Set("page", "dapi")
		query.Set("s", "post")
		query.Set("q", "index")
		query.Set("json", "1")
		query.Set("tags", tags)
		query.Set("limit", strconv.Itoa(booruPageSize))
		query.Set("pid", strconv.Itoa(page)) // zero-based
		return s.root + "/index.php?" + query.Encode()
	case apiGelbooru01:
		query.Set("page", "post")
		query.Set("s", "list")
		query.Set("tags", tags)
		// pid counts posts rather than pages here: the board's own paginator
		// steps it by the size of a page.
		query.Set("pid", strconv.Itoa(page*gelbooru01PageSize))
		return s.root + "/index.php?" + query.Encode()
	case apiSzurubooru:
		query.Set("query", tags)
		query.Set("offset", strconv.Itoa(page*booruPageSize))
		query.Set("limit", strconv.Itoa(booruPageSize))
		return s.root + "/api/posts/?" + query.Encode()
	case apiPhilomena:
		query.Set("q", tags)
		query.Set("per_page", strconv.Itoa(booruPageSize))
		query.Set("page", strconv.Itoa(page+1))
		if s.filterID != "" {
			query.Set("filter_id", s.filterID)
		}
		return s.root + "/api/v1/json/search/images?" + query.Encode()
	case apiTwibooru:
		query.Set("q", tags)
		query.Set("per_page", strconv.Itoa(booruPageSize))
		query.Set("page", strconv.Itoa(page+1))
		return s.root + "/api/v3/search/posts?" + query.Encode()
	case apiMoebooru:
		query.Set("tags", tags)
		query.Set("limit", strconv.Itoa(booruPageSize))
		query.Set("page", strconv.Itoa(page+1))
		return s.root + "/post.json?" + query.Encode()
	default: // danbooru, e621
		query.Set("tags", tags)
		query.Set("limit", strconv.Itoa(booruPageSize))
		query.Set("page", strconv.Itoa(page+1))
		return s.root + "/posts.json?" + query.Encode()
	}
}

// page fetches and normalises one response, whatever shape it arrives in.
func (b *Booru) page(ctx context.Context, endpoint string, headers httpx.Header) ([]booruPost, error) {
	var raw json.RawMessage
	if err := b.client.GetJSON(ctx, endpoint, headers, &raw); err != nil {
		return nil, err
	}
	return booruDecode(raw)
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
	page int, headers httpx.Header) ([]booruPost, error) {

	doc, err := b.client.GetString(ctx, site.endpoint(tags, page), headers)
	if err != nil {
		return nil, err
	}
	return gelbooru01Posts(doc), nil
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
// would otherwise be queued as a post.
func gelbooru01Posts(doc string) []booruPost {
	root, err := parseHTML(doc)
	if err != nil {
		return nil
	}

	var posts []booruPost
	for _, thumb := range findAll(root, func(n *html.Node) bool {
		return hasClass(n, gelbooru01ThumbClass)
	}) {
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
	return posts
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
	if link := util.FirstNonEmpty(p.FileURL, p.File.URL, p.ContentURL,
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
	ext := path.Ext(util.NameFromURL(link))
	if ext == "" && p.File.Ext != "" {
		ext = "." + p.File.Ext
	}
	if id := p.ID.String(); id != "" {
		return id + ext
	}
	return util.FirstNonEmpty(util.NameFromURL(link), p.MD5+ext)
}

// size reports the byte length when the board included one.
func (p *booruPost) size() int64 {
	return knownSize(p.Size, p.File.Size, p.Bytes)
}
