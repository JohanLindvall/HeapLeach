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

	"github.com/JohanLindvall/HeapLeach/internal/httpx"
	"github.com/JohanLindvall/HeapLeach/internal/util"
)

// Booru covers the image-board sites that share one of a handful of APIs.
//
// gallery-dl gets most of its reach this way rather than from bespoke
// scrapers: a dozen or so sites run the same software, so one adapter per
// API family covers all of them. The families differ only in the endpoint,
// the pagination parameter and where the file URL sits in the response.

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
)

// booruSite is one supported host.
type booruSite struct {
	name    string
	root    string
	api     booruAPI
	domains []string
	// filterID unlocks the default-hidden results on philomena sites.
	filterID string
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
}

const (
	// booruPageSize is what each family accepts as a page limit.
	booruPageSize = 100
	// booruMaxPosts bounds a tag search, which can otherwise be endless.
	booruMaxPosts = 1000
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

// Extract resolves a tag search, or a single post, into files.
func (b *Booru) Extract(ctx context.Context, u *url.URL, _ Options) (*Result, error) {
	site := siteFor(u)
	if site == nil {
		return nil, fmt.Errorf("booru: %s is not a supported board", u.Redacted())
	}

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
	case apiGelbooru:
		// /index.php?page=post&s=list&tags=... or &s=view&id=...
		if id := query.Get("id"); id != "" && query.Get("s") == "view" {
			return "", id, true
		}
		return query.Get("tags"), "", hasTags || query.Get("page") == "post"
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

// fetch pages through the API until the results run out.
func (b *Booru) fetch(ctx context.Context, site *booruSite, tags, single string) ([]File, error) {
	var files []File
	headers := httpx.Referer(site.root + "/")

	for page := 0; len(files) < booruMaxPosts; page++ {
		endpoint := site.endpoint(tags, page)

		posts, err := b.page(ctx, endpoint, headers)
		if err != nil {
			return nil, fmt.Errorf("booru: %s: %w", site.name, err)
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
				Headers: headers,
			})
		}
		// A short page is the last page, and a single post never pages.
		if len(posts) < booruPageSize || single != "" {
			break
		}
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("booru: %s returned no posts for %q", site.name, tags)
	}
	return files, nil
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
	// object keyed by "posts", "images" or "post".
	if len(raw) > 0 && raw[0] == '{' {
		var wrapper struct {
			Posts  []booruPost `json:"posts"`
			Images []booruPost `json:"images"`
			Post   []booruPost `json:"post"`
		}
		if err := json.Unmarshal(raw, &wrapper); err != nil {
			return nil, fmt.Errorf("unexpected response: %w", err)
		}
		switch {
		case wrapper.Posts != nil:
			return wrapper.Posts, nil
		case wrapper.Images != nil:
			return wrapper.Images, nil
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
}

// fileURL finds the full-size file for a post.
func (p *booruPost) fileURL(site *booruSite) string {
	if link := util.FirstNonEmpty(p.FileURL, p.File.URL, p.ViewURL, p.Representations.Full); link != "" {
		if strings.HasPrefix(link, "//") {
			return "https:" + link
		}
		if strings.HasPrefix(link, "/") {
			return site.root + link
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
	return -1
}
