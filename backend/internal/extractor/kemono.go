package extractor

import (
	"context"
	"fmt"
	"net/url"
	"path"

	"github.com/JohanLindvall/HeapLeach/internal/httpx"
	"github.com/JohanLindvall/HeapLeach/internal/util"
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

// kemonoHosts maps each supported domain to the instance root.
var kemonoHosts = map[string]string{
	"kemono.cr": "https://kemono.cr", "kemono.su": "https://kemono.su",
	"kemono.party": "https://kemono.party", "kemono.st": "https://kemono.st",
	"coomer.st": "https://coomer.st", "coomer.su": "https://coomer.su",
	"coomer.party": "https://coomer.party",
}

// NewKemono builds the kemono/coomer extractor.
func NewKemono(client *httpx.Client) *Kemono { return &Kemono{client: client} }

func (k *Kemono) Name() string { return "kemono" }

func (k *Kemono) Match(u *url.URL) bool { return kemonoRoot(u) != "" }

// kemonoRoot returns the instance root for a URL, or "".
func kemonoRoot(u *url.URL) string {
	for domain, root := range kemonoHosts {
		if util.HostMatches(u.Host, domain) {
			return root
		}
	}
	return ""
}

// Extract handles a creator (/​<service>/user/<id>) or a single post.
func (k *Kemono) Extract(ctx context.Context, u *url.URL, _ Options) (*Result, error) {
	root := kemonoRoot(u)
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
	for page := 0; page < kemonoMaxPages; page++ {
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

	var payload struct {
		Post kemonoPost `json:"post"`
	}
	if err := k.get(ctx, endpoint, root, &payload); err != nil {
		return kemonoPost{}, fmt.Errorf("kemono: post %s: %w", id, err)
	}
	if payload.Post.ID == "" {
		// Older instances return the post unwrapped.
		var direct kemonoPost
		if err := k.get(ctx, endpoint, root, &direct); err == nil {
			return direct, nil
		}
	}
	return payload.Post, nil
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
