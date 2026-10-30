// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"path"

	"github.com/JohanLindvall/HeapLeach/internal/httpx"
	"github.com/JohanLindvall/HeapLeach/internal/util"
	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// Kemono covers kemono and coomer, which run the same software and expose
// the same API.
//
// Both answer an ordinary browser request with 403 and a message naming the
// header they want instead: "if you want to scrape, use Accept: text/css".
// That is the site telling callers how to ask politely, so that is what is
// sent.
type Kemono struct {
	client *httpx.Client
}

// kemonoScrapeAccept is the header the sites themselves ask scrapers to use.
const kemonoScrapeAccept = "text/css"

// kemonoPageSize is the fixed page stride of their post listing.
const (
	kemonoPageSize = 50
	kemonoMaxPages = 40
)

// NewKemono builds the kemono/coomer extractor.
func NewKemono(client *httpx.Client) *Kemono { return &Kemono{client: client} }

func (k *Kemono) Name() string { return "kemono" }

// kemonoShape is a creator, /<service>/user/<id>, or one of their posts,
// /<service>/user/<id>/post/<post>.
var kemonoShape = pathShape(`^/[a-z0-9_-]+/user/[^/]+/?$`, `^/[a-z0-9_-]+/user/[^/]+/post/[^/]+/?$`)

// kemonoShell is the script the sites' app shell loads first, before its
// hashed bundle; kemonoNames are what the shell calls the sites.
const kemonoShell = "/static/js/lazy-styles.js"

var kemonoNames = []string{"Kemono", "Coomer"}

// Recognises is the inventory's description.
func (k *Kemono) Recognises() string {
	return "Kemono and Coomer creators and posts (`/<service>/user/<id>`, `…/post/<id>`) served " +
		"by the sites' app shell, or whose origin's API returns the creator's matching profile"
}

// Shape reports a creator or a post.
func (k *Kemono) Shape(u *url.URL) bool { return kemonoShape(u) }

// Recognise takes a creator or post link served by the sites' app shell:
// the shell's own loader script, its empty application root and its name.
// The shell carries nothing else — the app fetches the creator afterwards —
// so a page that cannot say is settled the way the app would settle it, by
// the API's profile of exactly this creator.
func (k *Kemono) Recognise(ctx context.Context, p *Page) bool {
	if !kemonoShape(p.Final) {
		return false
	}
	if root := p.HTML(); root != nil && p.Script(kemonoShell) && p.Named(kemonoNames...) &&
		findFirst(root, func(n *html.Node) bool { return isElem(n, atom.Div) && attr(n, "id") == "root" }) != nil {
		return true
	}
	return p.Silent() && kemonoProfile(ctx, p, k.client, p.Final)
}

// kemonoProfile asks for the creator's profile, whose service and id must
// agree with the URL. This also identifies an empty creator without
// mistaking an arbitrary JSON array for this platform.
func kemonoProfile(ctx context.Context, p *Page, client *httpx.Client, u *url.URL) bool {
	segs := util.PathSegments(u)
	root := util.Origin(u)
	endpoint := fmt.Sprintf("%s/api/v1/%s/user/%s/profile", root, url.PathEscape(segs[0]), url.PathEscape(segs[2]))
	var profile struct {
		ID      string `json:"id"`
		Service string `json:"service"`
		Name    string `json:"name"`
	}
	return pageJSON(ctx, p, client, endpoint, httpx.Header{
		httpx.HeaderAccept: kemonoScrapeAccept, httpx.HeaderReferer: root + "/",
	}, &profile) && profile.ID == segs[2] && profile.Service == segs[0] && profile.Name != ""
}

// Extract handles a creator (/​<service>/user/<id>) or a single post, on
// whichever site the link names.
func (k *Kemono) Extract(ctx context.Context, u *url.URL, _ Options) (*Result, error) {
	return k.extract(ctx, u, util.Origin(u))
}

func (k *Kemono) extract(ctx context.Context, u *url.URL, root string) (*Result, error) {
	segs := util.PathSegments(u)
	if len(segs) < 3 || segs[1] != "user" {
		return nil, fmt.Errorf("kemono: %s is not a creator or post link", u.Redacted())
	}
	service, creator := segs[0], segs[2]

	name := k.creatorName(ctx, root, service, creator)

	// /<service>/user/<id>/post/<postId>
	if len(segs) >= 5 && segs[3] == "post" {
		post, err := k.post(ctx, root, service, creator, segs[4])
		if err != nil {
			return nil, err
		}
		files := k.filesOf(root, []kemonoPost{post}, false)
		if len(files) == 0 {
			return nil, fmt.Errorf("kemono: post %s has no attachments", segs[4])
		}
		return &Result{Title: util.FirstNonEmpty(post.Title, name), Files: files}, nil
	}

	var all []kemonoPost
	for page := range kemonoMaxPages {
		posts, err := k.posts(ctx, root, service, creator, page*kemonoPageSize)
		if err != nil {
			if page == 0 {
				return nil, err
			}
			break
		}
		all = append(all, posts...)
		if len(posts) < kemonoPageSize {
			break
		}
	}
	files := k.filesOf(root, all, true)
	if len(files) == 0 {
		return nil, fmt.Errorf("kemono: %s has no downloadable attachments", u.Redacted())
	}
	return &Result{Title: name, Files: files}, nil
}

// filesOf flattens posts into downloadable entries, optionally filing each
// post's attachments into their own folder.
func (k *Kemono) filesOf(root string, posts []kemonoPost, group bool) []File {
	headers := httpx.Header{
		httpx.HeaderReferer: root + "/",
		httpx.HeaderAccept:  kemonoScrapeAccept,
	}
	seen := make(map[string]bool)
	var files []File

	for _, post := range posts {
		folder := ""
		if group {
			folder = folderName(util.FirstNonEmpty(post.Title, post.ID))
		}
		for _, item := range append([]kemonoAttachment{post.File}, post.Attachments...) {
			if item.Path == "" || seen[item.Path] {
				continue
			}
			seen[item.Path] = true
			files = append(files, File{
				Name:    util.FirstNonEmpty(item.Name, path.Base(item.Path)),
				URL:     root + "/data" + item.Path,
				Size:    -1,
				Dir:     folder,
				Headers: headers,
			})
		}
	}
	return files
}

// posts fetches one page of a creator's posts.
func (k *Kemono) posts(ctx context.Context, root, service, creator string, offset int) ([]kemonoPost, error) {
	endpoint := fmt.Sprintf("%s/api/v1/%s/user/%s/posts?o=%d",
		root, url.PathEscape(service), url.PathEscape(creator), offset)

	var posts []kemonoPost
	if err := k.get(ctx, endpoint, root, &posts); err != nil {
		return nil, fmt.Errorf("kemono: posts for %s/%s: %w", service, creator, err)
	}
	return posts, nil
}

// post fetches one post.
func (k *Kemono) post(ctx context.Context, root, service, creator, id string) (kemonoPost, error) {
	endpoint := fmt.Sprintf("%s/api/v1/%s/user/%s/post/%s",
		root, url.PathEscape(service), url.PathEscape(creator), url.PathEscape(id))

	// The post arrives wrapped on current instances and bare on older ones.
	// One response is read both ways rather than asked for twice: the body
	// is the same either way, and only the envelope differs.
	var raw json.RawMessage
	if err := k.get(ctx, endpoint, root, &raw); err != nil {
		return kemonoPost{}, fmt.Errorf("kemono: post %s: %w", id, err)
	}
	var payload struct {
		Post kemonoPost `json:"post"`
	}
	if err := json.Unmarshal(raw, &payload); err == nil && payload.Post.ID != "" {
		return payload.Post, nil
	}
	var direct kemonoPost
	if err := json.Unmarshal(raw, &direct); err != nil {
		return kemonoPost{}, fmt.Errorf("kemono: post %s: %w", id, err)
	}
	return direct, nil
}

// creatorName looks up a display name, falling back to the id.
func (k *Kemono) creatorName(ctx context.Context, root, service, creator string) string {
	endpoint := fmt.Sprintf("%s/api/v1/%s/user/%s/profile",
		root, url.PathEscape(service), url.PathEscape(creator))

	var profile struct {
		Name string `json:"name"`
	}
	if err := k.get(ctx, endpoint, root, &profile); err == nil && profile.Name != "" {
		return profile.Name
	}
	return service + "-" + creator
}

// get performs an API request with the header these sites ask for.
func (k *Kemono) get(ctx context.Context, endpoint, root string, out any) error {
	return k.client.GetJSON(ctx, endpoint, httpx.Header{
		httpx.HeaderAccept:  kemonoScrapeAccept,
		httpx.HeaderReferer: root + "/",
	}, out)
}

// kemonoPost is one post and its attachments.
type kemonoPost struct {
	ID          string             `json:"id"`
	User        string             `json:"user"`
	Service     string             `json:"service"`
	Title       string             `json:"title"`
	File        kemonoAttachment   `json:"file"`
	Attachments []kemonoAttachment `json:"attachments"`
}

type kemonoAttachment struct {
	Name string `json:"name"`
	Path string `json:"path"`
}
