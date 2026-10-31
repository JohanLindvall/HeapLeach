// SPDX-License-Identifier: MIT

package extractor

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
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
	about
	client *httpx.Client
}

// kemonoScrapeAccept is the header the sites themselves ask scrapers to use.
const kemonoScrapeAccept = "text/css"

const kemonoPreviewsOnly = "original attachments have not been archived (previews only)"

// NewKemono builds the kemono/coomer extractor.
func NewKemono(client *httpx.Client) *Kemono {
	return &Kemono{client: client, about: about{name: "kemono", shape: kemonoShape,
		recognises: "Kemono and Coomer creators and posts (`/<service>/user/<id>`, `…/post/<id>`) served " +
			"by the sites' app shell, or whose origin's API returns the creator's matching profile"}}
}

// kemonoShape is a creator, /<service>/user/<id>, or one of their posts,
// /<service>/user/<id>/post/<post>.
var kemonoShape = pathShape(`^/[a-z0-9_-]+/user/[^/]+/?$`, `^/[a-z0-9_-]+/user/[^/]+/post/[^/]+/?$`)

// kemonoShell is the script the sites' app shell loads first, before its
// hashed bundle; kemonoNames are what the shell calls the sites.
const kemonoShell = "/static/js/lazy-styles.js"

var kemonoNames = []string{"Kemono", "Coomer"}

// Recognise takes a creator or post link served by the sites' app shell:
// the shell's own loader script, its empty application root and its name.
// Compatible installs may render their own frontend instead, so an
// unfamiliar page is settled by the API's profile of exactly this creator.
func (k *Kemono) Recognise(ctx context.Context, p *Page) bool {
	if !kemonoShape(p.Final) {
		return false
	}
	if p.Script(kemonoShell) && p.Named(kemonoNames...) &&
		p.hasElement(func(n *html.Node) bool { return isElem(n, atom.Div) && attr(n, "id") == "root" }) {
		return true
	}
	return kemonoProfile(ctx, p, k.client)
}

// kemonoProfile asks for the creator's profile, whose service and id must
// agree with the URL. This also identifies an empty creator without
// mistaking an arbitrary JSON array for this platform.
func kemonoProfile(ctx context.Context, p *Page, client *httpx.Client) bool {
	segs := p.Segments()
	root := util.Origin(p.Final)
	var profile struct {
		ID      string `json:"id"`
		Service string `json:"service"`
		Name    string `json:"name"`
	}
	return pageJSON(ctx, p, client, creatorAPI(root, segs[0], segs[2])+"/profile", kemonoHeaders(root), &profile) &&
		profile.ID == segs[2] && profile.Service == segs[0] && profile.Name != ""
}

// kemonoHeaders are what every request to the site at root carries: the
// header these sites ask for, and the site as referer.
func kemonoHeaders(root string) httpx.Header {
	return httpx.Header{httpx.HeaderAccept: kemonoScrapeAccept, httpx.HeaderReferer: root + "/"}
}

// Extract handles a creator (/​<service>/user/<id>) or a single post, on
// whichever site the link names.
func (k *Kemono) Extract(ctx context.Context, u *url.URL, opts Options) (*Result, error) {
	segs := util.PathSegments(u)
	if len(segs) < 3 || segs[1] != "user" {
		return nil, fmt.Errorf("kemono: %s is not a creator or post link", u.Redacted())
	}
	root := util.Origin(u)
	service, creator := segs[0], segs[2]
	api := creatorAPI(root, service, creator)

	name, total := k.creator(ctx, probed(ctx, u), api, root, service, creator)

	// /<service>/user/<id>/post/<postId>
	if len(segs) >= 5 && segs[3] == "post" {
		post, err := k.post(ctx, api, root, segs[4])
		if err != nil {
			return nil, err
		}
		if post.previewOnly() {
			return nil, fmt.Errorf("kemono: %s", kemonoPreviewsOnly)
		}
		files := k.filesOf(root, []kemonoPost{post}, false)
		if len(files) == 0 {
			return nil, fmt.Errorf("kemono: post %s has no attachments", segs[4])
		}
		return &Result{Title: cmp.Or(post.Title, name), Files: files}, nil
	}

	// The profile states how many posts there are. Without it, the end is
	// an empty page, or the 400 the API answers an offset past it with. The
	// file cap is counted in attachments: a post may carry several, or none.
	offset := 0
	attached := make(map[string]bool)
	all, note, err := walkPages(ctx, pageWalk[kemonoPost]{
		key:   func(post kemonoPost) string { return post.ID },
		limit: opts.maxFiles(), limitNote: fileLimitNote,
		weigh: func(posts []kemonoPost) int {
			n := 0
			for _, post := range posts {
				for _, item := range post.items() {
					if item.Path != "" && !attached[item.Path] {
						attached[item.Path] = true
						n++
					}
				}
			}
			return n
		},
		ends: walkAccepts{statuses: []int{http.StatusBadRequest}},
		fetch: func(ctx context.Context, _ int) (pageOf[kemonoPost], error) {
			var posts []kemonoPost
			if err := k.client.GetJSON(ctx, fmt.Sprintf("%s/posts?o=%d", api, offset), kemonoHeaders(root), &posts); err != nil {
				return pageOf[kemonoPost]{}, fmt.Errorf("kemono: posts for %s/%s: %w", service, creator, err)
			}
			offset += len(posts)
			return pageOf[kemonoPost]{items: posts, next: true, end: len(posts) == 0, total: total}, nil
		},
	})
	if err != nil {
		return nil, err
	}
	files := k.filesOf(root, all, true)
	previews := 0
	for _, post := range all {
		if post.previewOnly() {
			previews++
		}
	}
	if len(files) == 0 {
		if previews > 0 {
			return nil, fmt.Errorf("kemono: %s", kemonoPreviewsOnly)
		}
		return nil, fmt.Errorf("kemono: %s has no downloadable attachments", u.Redacted())
	}
	note = joinNotes(note, partial(leftOutNote(previews, ": "+kemonoPreviewsOnly)))
	return &Result{Title: name, Files: files, Note: note}, nil
}

// filesOf flattens posts into downloadable entries, optionally filing each
// post's attachments into their own folder.
func (k *Kemono) filesOf(root string, posts []kemonoPost, group bool) []File {
	headers := kemonoHeaders(root)
	seen := make(map[string]bool)
	var files []File

	for _, post := range posts {
		folder := ""
		if group {
			folder = folderName(cmp.Or(post.Title, post.ID))
		}
		for _, item := range post.items() {
			if item.Path == "" || seen[item.Path] {
				continue
			}
			seen[item.Path] = true
			files = append(files, File{
				Name:    cmp.Or(item.Name, path.Base(item.Path)),
				URL:     root + "/data" + item.Path,
				Size:    -1,
				Dir:     folder,
				Headers: headers,
			})
		}
	}
	return files
}

// post fetches one post of the creator whose records are at api.
func (k *Kemono) post(ctx context.Context, api, root, id string) (kemonoPost, error) {
	// The post arrives wrapped on current instances and bare on older ones.
	// One response is read both ways rather than asked for twice: the body
	// is the same either way, and only the envelope differs.
	var raw json.RawMessage
	if err := k.client.GetJSON(ctx, api+"/post/"+url.PathEscape(id), kemonoHeaders(root), &raw); err != nil {
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

// creator looks up a display name, falling back to the id, and how many
// posts the creator has, 0 when the profile does not say.
func (k *Kemono) creator(ctx context.Context, page *Page, api, root, service, creator string) (name string, posts int) {
	var profile struct {
		Name  string `json:"name"`
		Posts int    `json:"post_count"`
	}
	if (askedJSON(ctx, page, k.client, api+"/profile", kemonoHeaders(root), &profile) ||
		k.client.GetJSON(ctx, api+"/profile", kemonoHeaders(root), &profile) == nil) && profile.Name != "" {
		return profile.Name, profile.Posts
	}
	return service + "-" + creator, 0
}

// kemonoPost is one post and its attachments.
type kemonoPost struct {
	ID          string             `json:"id"`
	User        string             `json:"user"`
	Service     string             `json:"service"`
	Title       string             `json:"title"`
	File        kemonoAttachment   `json:"file"`
	Attachments []kemonoAttachment `json:"attachments"`
	// Compatible archives may list metadata before storing the originals.
	// An absent flag is the ordinary API, where attachments are available.
	HasFull *bool `json:"has_full"`
}

func (p kemonoPost) previewOnly() bool { return p.HasFull != nil && !*p.HasFull }

// items is the post's file and its attachments, in the order shown.
func (p kemonoPost) items() []kemonoAttachment {
	if p.previewOnly() {
		return nil
	}
	return append([]kemonoAttachment{p.File}, p.Attachments...)
}

type kemonoAttachment struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

// creatorAPI is where the API on origin keeps a creator's records — their
// /profile, /posts and each /post/<id> — on the creator-archive software
// kemono runs and on the API cum.st modelled on it.
func creatorAPI(origin, service, creator string) string {
	return origin + "/api/v1/" + url.PathEscape(service) + "/user/" + url.PathEscape(creator)
}
