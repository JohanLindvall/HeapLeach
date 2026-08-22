package extractor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

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

// booruSite is one supported host.
type booruSite struct {
	name    string
	root    string
	api     booruAPI
	domains []string
	// filterID unlocks the default-hidden results on philomena sites.
	filterID string
	// multi marks one install serving many boards, where the root and the
	// label belong to whichever board the link named rather than to this
	// entry. See forURL.
	multi bool
}

// booruSites is the supported set. Each was checked against its live API;
// hosts that now demand an API key or sit behind a challenge are left out
// rather than shipped as something that looks supported but is not.
var booruSites = []booruSite{
	// Danbooru family. danbooru.donmai.us itself is behind a challenge that
	// a plain client cannot pass, so only its siblings are listed.
	{name: "aibooru", root: "https://aibooru.online", api: apiDanbooru, domains: []string{"aibooru.online"}},
	{name: "booruvar", root: "https://booru.borvar.art", api: apiDanbooru, domains: []string{"booru.borvar.art"}},

	// e621 family: same API, but the file URL is nested.
	{name: "e621", root: "https://e621.net", api: apiE621, domains: []string{"e621.net"}},
	{name: "e926", root: "https://e926.net", api: apiE621, domains: []string{"e926.net"}},
	{name: "e6ai", root: "https://e6ai.net", api: apiE621, domains: []string{"e6ai.net"}},

	// Moebooru family.
	{name: "yandere", root: "https://yande.re", api: apiMoebooru, domains: []string{"yande.re"}},
	{name: "konachan", root: "https://konachan.com", api: apiMoebooru, domains: []string{"konachan.com", "konachan.net"}},
	{name: "sakugabooru", root: "https://www.sakugabooru.com", api: apiMoebooru, domains: []string{"sakugabooru.com"}},

	// Gelbooru 0.2 family. gelbooru.com itself now requires an API key.
	{name: "safebooru", root: "https://safebooru.org", api: apiGelbooru, domains: []string{"safebooru.org"}},
	{name: "tbib", root: "https://tbib.org", api: apiGelbooru, domains: []string{"tbib.org"}},
	{name: "hypnohub", root: "https://hypnohub.net", api: apiGelbooru, domains: []string{"hypnohub.net"}},
	{name: "xbooru", root: "https://xbooru.com", api: apiGelbooru, domains: []string{"xbooru.com"}},

	// Philomena family. filterID selects the site's "everything" filter, so
	// a search returns what the site itself would show.
	{name: "derpibooru", root: "https://derpibooru.org", api: apiPhilomena, domains: []string{"derpibooru.org"}, filterID: "56027"},
	{name: "ponybooru", root: "https://ponybooru.org", api: apiPhilomena, domains: []string{"ponybooru.org"}, filterID: "3"},
	{name: "furbooru", root: "https://furbooru.org", api: apiPhilomena, domains: []string{"furbooru.org"}, filterID: "2"},
	{name: "twibooru", root: "https://twibooru.org", api: apiTwibooru, domains: []string{"twibooru.org"}},

	// szurubooru family. The software is self-hosted, so this is the set of
	// public instances that answer rather than a canonical list.
	// booru.bcbnsfw.space runs it too but serves an age-consent page in
	// place of every API answer, so it is left out for the same reason
	// danbooru.donmai.us is: shipping it would look like support and behave
	// like a parse failure.
	{name: "snootbooru", root: "https://snootbooru.com", api: apiSzurubooru, domains: []string{"snootbooru.com"}},
	{name: "foalcon", root: "https://booru.foalcon.com", api: apiSzurubooru, domains: []string{"booru.foalcon.com"}},

	// Gelbooru 0.1: booru.org is one install hosting a board per subdomain,
	// so a single entry covers every one of them. See forURL.
	{name: "booru.org", api: apiGelbooru01, domains: []string{"booru.org"}, multi: true},
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

// Booru resolves tag searches and single posts on image-board sites.
type Booru struct {
	client *httpx.Client
}

// NewBooru builds the image-board extractor.
func NewBooru(client *httpx.Client) *Booru { return &Booru{client: client} }

func (b *Booru) Name() string { return "booru" }

func (b *Booru) Match(u *url.URL) bool { return siteFor(u) != nil }

// siteFor finds the entry covering a URL.
func siteFor(u *url.URL) *booruSite {
	for i := range booruSites {
		for _, domain := range booruSites[i].domains {
			if util.HostMatches(u.Host, domain) {
				return &booruSites[i]
			}
		}
	}
	return nil
}

// forURL fills in the parts of a multi-tenant entry that only the link can
// supply.
//
// booru.org is one install serving a board per subdomain, and there are far
// more of them than anyone will enumerate — the same argument the KVS host
// list loses, and the same answer: the board's own subdomain is its root and
// its label, so both are read off the URL and neither is compiled in.
func (s *booruSite) forURL(u *url.URL) *booruSite {
	if !s.multi {
		return s
	}
	board := *s
	board.root = util.Origin(u)
	if label, _, _ := strings.Cut(strings.ToLower(u.Hostname()), "."); label != "" {
		board.name = label
	}
	return &board
}

// Extract resolves a tag search, or a single post, into files.
func (b *Booru) Extract(ctx context.Context, u *url.URL, _ Options) (*Result, error) {
	site := siteFor(u)
	if site == nil {
		return nil, fmt.Errorf("booru: %s is not a supported board", u.Redacted())
	}
	site = site.forURL(u)

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

	for page := 0; len(files) < booruMaxPosts; page++ {
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

// flexValue accepts a field that some boards send as a JSON string and
// others as a number — ids, and directories that may be a plain number or a
// nested path like "44/29".
type flexValue string

func (f *flexValue) UnmarshalJSON(raw []byte) error {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		*f = ""
		return nil
	}
	if raw[0] == '"' {
		var text string
		if err := json.Unmarshal(raw, &text); err != nil {
			return err
		}
		*f = flexValue(text)
		return nil
	}
	*f = flexValue(raw)
	return nil
}

func (f flexValue) String() string { return string(f) }

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
	if p.Size > 0 {
		return p.Size
	}
	if p.File.Size > 0 {
		return p.File.Size
	}
	if p.Bytes > 0 {
		return p.Bytes
	}
	return -1
}
