// SPDX-License-Identifier: MIT

package extractor

import (
	"cmp"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
	"github.com/JohanLindvall/HeapLeach/internal/util"
	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// Cum reads creator posts from a public API. Its compact listing names every
// attachment; full posts supply the original variants and their exact sizes.
// Media is content-addressed and served by the page's advertised edge pool.
type Cum struct {
	about
	client *httpx.Client
}

func NewCum(client *httpx.Client) *Cum {
	return &Cum{client: client, about: about{name: "cum", shape: cumShape,
		recognises: "creators and posts (`/creators/<service>/<id>[/post/<post>]`) whose page advertises its media " +
			"servers as `EDGE_BASE_URLS` in the app's configuration"}}
}

// cumShape is a creator, /creators/<service>/<id>, or one of their posts.
var cumShape = pathShape(`^/creators/[^/]+/[^/]+(?:/post/[^/]+)?/?$`)

// Recognise takes a creator or post page that advertises the edge pool
// Extract downloads from, which is read from the page before any API call.
func (c *Cum) Recognise(_ context.Context, p *Page) bool {
	if !cumShape(p.Final) || !p.Has("EDGE_BASE_URLS") {
		return false
	}
	edges, err := cumProbedEdges(p)
	return err == nil && len(edges) > 0
}

type cumPost struct {
	ID          string          `json:"id"`
	Service     string          `json:"service"`
	Title       string          `json:"title"`
	Attachments []cumAttachment `json:"attachments"`
}

type cumAttachment struct {
	SHA256           string `json:"sha256"`
	Kind             string `json:"kind"`
	OriginalFilename string `json:"originalFilename"`
	Variants         []struct {
		Name  string `json:"name"`
		Bytes *int64 `json:"bytes"`
	} `json:"variants"`
}

func (c *Cum) Extract(ctx context.Context, u *url.URL, opts Options) (*Result, error) {
	segs := util.PathSegments(u)
	if (len(segs) != 3 && len(segs) != 5) || segs[0] != "creators" || (len(segs) == 5 && segs[3] != "post") {
		return nil, fmt.Errorf("cum: expected /creators/<service>/<id> or /creators/<service>/<id>/post/<post>")
	}
	page := *u
	page.Fragment = ""
	query := page.Query()
	if tab := query.Get("tab"); tab != "" && tab != "posts" {
		return nil, fmt.Errorf("cum: only the creator's posts tab is supported")
	}
	query.Del("pppage")
	page.RawQuery = query.Encode()
	d, err := readDocument(ctx, c.client, &page, nil)
	if err != nil {
		return nil, fmt.Errorf("cum: fetch page: %w", err)
	}
	final := d.final
	if strings.TrimRight(final.Path, "/") != strings.TrimRight(page.Path, "/") {
		return nil, fmt.Errorf("cum: creator or post redirected to another page")
	}
	// The media servers recognition already decoded, when this is the
	// probed page; a page fetched here decodes its own.
	var edges []string
	if d.probe != nil {
		edges, err = cumProbedEdges(d.probe)
	} else {
		edges, err = cumEdges(d.body)
	}
	if err != nil {
		return nil, err
	}
	base := creatorAPI(util.Origin(final), segs[1], segs[2])
	if len(segs) == 5 {
		post, err := c.post(ctx, base, segs[1], segs[4], final.String())
		if err != nil {
			return nil, err
		}
		res := &Result{Title: cmp.Or(post.Title, post.ID)}
		seen := make(map[string]bool)
		for _, att := range post.Attachments {
			if seen[att.SHA256] {
				continue
			}
			seen[att.SHA256] = true
			if len(res.Files) == opts.maxFiles() {
				res.Note = fileLimitNote
				break
			}
			file, err := att.file(edges, final.String())
			if err != nil {
				res.Note = "partial — unavailable originals"
				continue
			}
			res.Files = append(res.Files, file)
		}
		if len(res.Files) == 0 {
			return nil, fmt.Errorf("cum: post has no downloadable originals")
		}
		return res, nil
	}
	return c.creator(ctx, final, base, segs[1], segs[2], query, edges, opts)
}

func (c *Cum) creator(ctx context.Context, page *url.URL, base, service, creator string, query url.Values, edges []string, opts Options) (*Result, error) {
	var profile struct {
		ID          string `json:"id"`
		Service     string `json:"service"`
		Name        string `json:"name"`
		DisplayName string `json:"displayName"`
	}
	headers := httpx.Referer(page.String())
	if err := c.client.GetJSON(ctx, base+"/profile", headers, &profile); err != nil {
		return nil, fmt.Errorf("cum: fetch creator: %w", err)
	}
	if profile.ID != creator || profile.Service != service {
		return nil, fmt.Errorf("cum: creator is missing from the API response")
	}
	res := &Result{Title: cmp.Or(profile.DisplayName, profile.Name, service+"-"+creator)}
	params := cumPostQuery(query)
	// The listing states its total; offset counts what it has served, the
	// posts it lists wrongly too.
	//
	// The listing names the posts' attachments, each resolved only at
	// transfer time, so the walk is bounded by the file cap, counted in the
	// attachments the selection keeps (cumKept), and stops paging once that
	// is reached rather than reading pages whose files would be thrown
	// away. Each post is a source as well: the source limit bounds how many
	// the walk takes, and no page asks for more than it leaves room for.
	offset, total, invalid := 0, 0, false
	listed := make(map[string]bool)
	posts, note, err := walkPages(ctx, pageWalk[cumPost]{
		key:   func(post cumPost) string { return post.ID },
		limit: opts.maxFiles(), limitNote: fileLimitNote,
		weigh: func(posts []cumPost) int {
			n := 0
			for _, post := range posts {
				n += len(cumKept(post, params.Get("type")))
			}
			return n
		},
		failed: func(error) (string, bool) { return "partial — could not read the next post page", true },
		fetch: func(ctx context.Context, n int) (pageOf[cumPost], error) {
			if len(listed) >= opts.maxSources() {
				return pageOf[cumPost]{end: true, note: sourceLimitNote}, nil
			}
			ResolveNote(ctx, fmt.Sprintf("Reading creator posts, page %d", n+1))
			params.Set("o", strconv.Itoa(offset))
			params.Set("n", strconv.Itoa(min(config.CumPageSize, opts.maxSources()-len(listed))))
			var batch struct {
				Total *int      `json:"total"`
				Posts []cumPost `json:"posts"`
			}
			err := c.client.GetJSON(ctx, base+"/posts?"+params.Encode(), headers, &batch)
			if err == nil && (batch.Total == nil || *batch.Total < 0 || batch.Posts == nil) {
				err = fmt.Errorf("cum: invalid post listing")
			}
			if err != nil {
				return pageOf[cumPost]{}, err
			}
			total = *batch.Total
			offset += len(batch.Posts)
			valid := make([]cumPost, 0, len(batch.Posts))
			for _, post := range batch.Posts {
				if post.ID == "" || post.Service != service {
					invalid = true
					continue
				}
				if !listed[post.ID] {
					if len(listed) >= opts.maxSources() {
						return pageOf[cumPost]{items: valid, end: true, note: sourceLimitNote}, nil
					}
					listed[post.ID] = true
				}
				valid = append(valid, post)
			}
			return pageOf[cumPost]{items: valid, next: true, end: offset >= total}, nil
		},
	})
	if err != nil {
		return nil, err
	}
	if invalid {
		note = joinNotes(note, "partial — invalid posts in listing")
	}
	if note == "" {
		note = partial(cappedNote(len(posts), total, "posts"))
	}
	res.Note = note
	for _, post := range posts {
		postURL := util.Origin(page) + "/creators/" + url.PathEscape(service) + "/" + url.PathEscape(creator) + "/post/" + url.PathEscape(post.ID)
		loader := &cumPostLoader{c: c, base: base, service: service, id: post.ID, referer: postURL, edges: edges}
		seenAttachments := make(map[string]bool)
		for _, att := range post.Attachments {
			if !cumAttachmentKind(att.Kind, params.Get("type")) || seenAttachments[att.SHA256] {
				continue
			}
			if !cumValidHash(att.SHA256) {
				res.Note = joinNotes(res.Note, "partial — invalid attachments in listing")
				continue
			}
			seenAttachments[att.SHA256] = true
			if len(res.Files) == opts.maxFiles() {
				res.Note = joinNotes(res.Note, fileLimitNote)
				return cumCreatorResult(res)
			}
			res.Files = append(res.Files, File{
				Name: att.name(""), Size: -1,
				Dir: cumPostFolder(post.Title, post.ID),
				Resolve: func(ctx context.Context) (*Target, error) {
					return loader.resolve(ctx, att.SHA256)
				},
			})
		}
	}
	return cumCreatorResult(res)
}

// cumKept is the post's attachments the selection keeps, as the creator's
// listing files them: of the filtered kind, with a valid hash, each once.
func cumKept(post cumPost, filter string) []cumAttachment {
	var kept []cumAttachment
	seen := make(map[string]bool)
	for _, att := range post.Attachments {
		if !cumAttachmentKind(att.Kind, filter) || seen[att.SHA256] || !cumValidHash(att.SHA256) {
			continue
		}
		seen[att.SHA256] = true
		kept = append(kept, att)
	}
	return kept
}

func cumCreatorResult(res *Result) (*Result, error) {
	if len(res.Files) == 0 {
		return nil, fmt.Errorf("cum: creator has no downloadable attachments matching this selection")
	}
	return res, nil
}

func cumPostQuery(query url.Values) url.Values {
	params := url.Values{"compact": {"1"}, "sort": {"recent"}}
	for page, api := range map[string]string{
		"ppq": "q", "pptype": "type", "ppsort": "sort", "ppexact": "exact",
		"ppmin_duration": "min_duration", "ppmax_duration": "max_duration", "pptag": "tag",
	} {
		if value := query.Get(page); value != "" {
			params.Set(api, value)
		}
	}
	if params.Get("q") != "" && query.Get("ppsort") == "" {
		params.Set("sort", "relevance")
	}
	if params.Get("type") == "all" {
		params.Del("type")
	}
	return params
}

func cumAttachmentKind(kind, filter string) bool {
	switch filter {
	case "photos":
		return kind == "image" || kind == "gif"
	case "videos":
		return kind == "video"
	case "audio":
		return kind == "audio"
	case "text":
		return false
	default:
		return true
	}
}

func cumPostFolder(title, id string) string {
	name := sourceFolder(util.Unescape(title))
	if name == "" || name == "." || name == ".." || name == id {
		return id
	}
	// The id keeps posts with repeated titles separate and folder names
	// stable when source or file caps change which other posts are present.
	return name + " [" + id + "]"
}

func (c *Cum) post(ctx context.Context, base, service, id, referer string) (*cumPost, error) {
	var post cumPost
	if err := c.client.GetJSON(ctx, base+"/post/"+url.PathEscape(id), httpx.Referer(referer), &post); err != nil {
		return nil, fmt.Errorf("cum: fetch post: %w", err)
	}
	if post.ID != id || post.Service != service {
		return nil, fmt.Errorf("cum: post is missing from the API response")
	}
	return &post, nil
}

// All of a post's queued attachments share one metadata fetch. Successful
// metadata is stable; failed or cancelled fetches remain retryable.
type cumPostLoader struct {
	c                          *Cum
	base, service, id, referer string
	edges                      []string
	mu                         sync.Mutex
	post                       *cumPost
	files                      map[string]File
}

func (l *cumPostLoader) resolve(ctx context.Context, hash string) (*Target, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if l.post == nil {
		post, err := l.c.post(ctx, l.base, l.service, l.id, l.referer)
		if err != nil {
			return nil, err
		}
		l.post = post
		l.files = make(map[string]File)
	}
	file, ok := l.files[hash]
	if !ok {
		for _, att := range l.post.Attachments {
			if att.SHA256 != hash {
				continue
			}
			var err error
			file, err = att.file(l.edges, l.referer)
			if err != nil {
				return nil, err
			}
			l.files[hash] = file
			ok = true
			break
		}
	}
	if !ok {
		return nil, fmt.Errorf("cum: attachment is no longer in this post")
	}
	if file.Resolve != nil {
		return file.Resolve(ctx)
	}
	return file.target(), nil
}

// cumProbedEdges is a probed page's media servers, decoded once per page
// and shared by recognition and extraction. The slice is shared: callers
// read it and never modify it.
func cumProbedEdges(p *Page) ([]string, error) {
	type decoded struct {
		edges []string
		err   error
	}
	got := memo(p, "cum.edges", func() decoded {
		edges, err := cumEdges(p.Body)
		return decoded{edges, err}
	})
	return got.edges, got.err
}

var cumEdgesRE = regexp.MustCompile(`\bEDGE_BASE_URLS\s*:\s*("(?:[^"\\]|\\.)*")`)

func cumEdges(doc string) ([]string, error) {
	root, err := parseHTML(doc)
	if err != nil {
		return nil, err
	}
	for _, script := range findAll(root, func(n *html.Node) bool { return isElem(n, atom.Script) }) {
		match := cumEdgesRE.FindStringSubmatch(rawText(script))
		if match == nil {
			continue
		}
		var raw string
		if err := json.Unmarshal([]byte(match[1]), &raw); err != nil {
			return nil, fmt.Errorf("cum: read media servers: %w", err)
		}
		var edges []string
		for _, value := range strings.Split(raw, ",") {
			value = strings.TrimRight(strings.TrimSpace(value), "/")
			u, err := url.Parse(value)
			if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
				continue
			}
			edges = append(edges, value)
		}
		if len(edges) > 0 {
			return util.Dedupe(edges), nil
		}
	}
	return nil, fmt.Errorf("cum: page does not advertise media servers")
}

func cumValidHash(hash string) bool {
	decoded, err := hex.DecodeString(hash)
	return err == nil && len(decoded) == 32
}

func (a cumAttachment) name(ext string) string {
	if ext == "" {
		ext = path.Ext(a.OriginalFilename)
	}
	name := strings.TrimSpace(a.OriginalFilename)
	if name == "" || strings.EqualFold(util.Stem(name), "original") {
		return a.SHA256 + ext
	}
	if ext != "" && !strings.EqualFold(path.Ext(name), ext) {
		name = util.WithExt(name, ext)
	}
	return name
}

func (a cumAttachment) file(edges []string, referer string) (File, error) {
	if !cumValidHash(a.SHA256) {
		return File{}, fmt.Errorf("cum: attachment has no valid content hash")
	}
	for _, variant := range a.Variants {
		if !strings.HasPrefix(variant.Name, "original.") || path.Ext(variant.Name) == "." || strings.ContainsAny(variant.Name, `/\`) {
			continue
		}
		size := int64(-1)
		if variant.Bytes != nil {
			size = knownSize(*variant.Bytes)
		}
		links := make([]string, 0, len(edges))
		for _, edge := range edges {
			links = append(links, edge+"/media/"+a.SHA256+"/"+url.PathEscape(variant.Name))
		}
		return Mirrored(File{Name: a.name(path.Ext(variant.Name)), Size: size, Headers: httpx.Referer(referer)}, links), nil
	}
	return File{}, fmt.Errorf("cum: attachment has no original media variant")
}
