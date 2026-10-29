// SPDX-License-Identifier: MIT

package extractor

import (
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
	hostSet
	client *httpx.Client
}

func NewCum(client *httpx.Client) *Cum {
	return &Cum{hostSet: hostSet{"cum.st"}, client: client}
}

func (c *Cum) Name() string { return "cum" }

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
	doc, final, err := c.client.GetPage(ctx, page.String(), nil, nil)
	if err != nil {
		return nil, fmt.Errorf("cum: fetch page: %w", err)
	}
	if strings.TrimRight(final.Path, "/") != strings.TrimRight(page.Path, "/") {
		return nil, fmt.Errorf("cum: creator or post redirected to another page")
	}
	edges, err := cumEdges(doc)
	if err != nil {
		return nil, err
	}
	base := util.Origin(final) + "/api/v1/" + url.PathEscape(segs[1]) + "/user/" + url.PathEscape(segs[2])
	if len(segs) == 5 {
		post, err := c.post(ctx, base, segs[1], segs[4], final.String())
		if err != nil {
			return nil, err
		}
		res := &Result{Title: util.FirstNonEmpty(post.Title, post.ID)}
		seen := make(map[string]bool)
		for _, att := range post.Attachments {
			if seen[att.SHA256] {
				continue
			}
			seen[att.SHA256] = true
			if len(res.Files) == opts.maxFiles() {
				res.Note = "partial — file limit"
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
	res := &Result{Title: util.FirstNonEmpty(profile.DisplayName, profile.Name, service+"-"+creator)}
	params := cumPostQuery(query)
	seen := make(map[string]bool)
	offset, total := 0, 0
	for pageNo := 0; pageNo < config.MaxAlbumPages; pageNo++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		ResolveNote(ctx, fmt.Sprintf("Reading creator posts, page %d", pageNo+1))
		params.Set("o", strconv.Itoa(offset))
		params.Set("n", strconv.Itoa(min(config.CumPageSize, opts.maxSources()-len(seen))))
		var batch struct {
			Total *int      `json:"total"`
			Posts []cumPost `json:"posts"`
		}
		err := c.client.GetJSON(ctx, base+"/posts?"+params.Encode(), headers, &batch)
		if err == nil && (batch.Total == nil || *batch.Total < 0 || batch.Posts == nil) {
			err = fmt.Errorf("cum: invalid post listing")
		}
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if pageNo == 0 {
				return nil, err
			}
			res.Note = "partial — could not read the next post page"
			break
		}
		total = *batch.Total
		before := len(seen)
		for _, post := range batch.Posts {
			if post.ID == "" || post.Service != service {
				res.Note = "partial — invalid posts in listing"
				continue
			}
			if seen[post.ID] {
				continue
			}
			if len(seen) == opts.maxSources() {
				res.Note = "partial — source limit"
				return cumCreatorResult(res)
			}
			seen[post.ID] = true
			postURL := util.Origin(page) + "/creators/" + url.PathEscape(service) + "/" + url.PathEscape(creator) + "/post/" + url.PathEscape(post.ID)
			loader := &cumPostLoader{c: c, base: base, service: service, id: post.ID, referer: postURL, edges: edges}
			seenAttachments := make(map[string]bool)
			for _, att := range post.Attachments {
				if !cumAttachmentKind(att.Kind, params.Get("type")) || seenAttachments[att.SHA256] {
					continue
				}
				if !cumValidHash(att.SHA256) {
					res.Note = "partial — invalid attachments in listing"
					continue
				}
				seenAttachments[att.SHA256] = true
				if len(res.Files) == opts.maxFiles() {
					res.Note = "partial — file limit"
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
		offset += len(batch.Posts)
		if offset >= total {
			if len(seen) < total && res.Note == "" {
				res.Note = fmt.Sprintf("partial — %d of %d posts", len(seen), total)
			}
			break
		}
		if len(seen) == before {
			res.Note = "partial — repeated or empty post page"
			break
		}
		if len(res.Files) == opts.maxFiles() {
			res.Note = "partial — file limit"
			break
		}
		if len(seen) == opts.maxSources() {
			res.Note = "partial — source limit"
			break
		}
		if pageNo == config.MaxAlbumPages-1 {
			res.Note = "partial — page limit"
		}
	}
	return cumCreatorResult(res)
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
	return &Target{URL: file.URL, Name: file.Name, Size: file.Size, Headers: file.Headers}, nil
}

var cumEdgesRE = regexp.MustCompile(`\bEDGE_BASE_URLS\s*:\s*("(?:[^"\\]|\\.)*")`)

func cumEdges(doc string) ([]string, error) {
	root, err := parseHTML(doc)
	if err != nil {
		return nil, err
	}
	for _, script := range findAll(root, func(n *html.Node) bool { return isElem(n, atom.Script) }) {
		match := cumEdgesRE.FindStringSubmatch(textOf(script))
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
	if name == "" || strings.EqualFold(strings.TrimSuffix(name, path.Ext(name)), "original") {
		return a.SHA256 + ext
	}
	if ext != "" && !strings.EqualFold(path.Ext(name), ext) {
		name = strings.TrimSuffix(name, path.Ext(name)) + ext
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
		if variant.Bytes != nil && *variant.Bytes >= 0 {
			size = *variant.Bytes
		}
		links := make([]string, 0, len(edges))
		for _, edge := range edges {
			links = append(links, edge+"/media/"+a.SHA256+"/"+url.PathEscape(variant.Name))
		}
		return Mirrored(File{Name: a.name(path.Ext(variant.Name)), Size: size, Headers: httpx.Referer(referer)}, links), nil
	}
	return File{}, fmt.Errorf("cum: attachment has no original media variant")
}
