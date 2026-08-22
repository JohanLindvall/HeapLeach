package extractor

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
	"github.com/JohanLindvall/HeapLeach/internal/util"
)

// The Lemmy dialect of the fediverse extractor, which shares nothing with
// the Mastodon client API. Split from fediverse.go; the shared discovery and
// URL shapes stay there.

// --------------------------------------------------------------------- lemmy

// lemmyAPIs are the API roots to try, in the order a probe should ask.
//
// v3 comes first because it is what every instance in the wild answers today;
// v4 arrives with Lemmy 1.0. Which one an instance speaks is not something to
// assume in either direction, so it is discovered — once per job, on the
// first request, rather than as a request of its own.
var lemmyAPIs = []string{"/api/v3", "/api/v4"}

// lemmyClient remembers which version answered, so the probe is not repeated
// for every page of a listing.
type lemmyClient struct {
	f    *Fediverse
	root string
	api  string
}

// get performs one API call, resolving the version on the way if it is not
// settled yet.
//
// A 404 is what the wrong version answers — and, awkwardly, also what an
// unknown community answers. That ambiguity is why a total failure reports
// the *first* version's error rather than the last: on every instance
// deployed today that is the one whose message names what was really missing.
func (l *lemmyClient) get(ctx context.Context, route string, query url.Values, out any) error {
	if l.api != "" {
		return l.f.client.GetJSON(ctx, l.endpoint(l.api, route, query), fediHeaders(l.root), out)
	}

	var first error
	for _, api := range lemmyAPIs {
		err := l.f.client.GetJSON(ctx, l.endpoint(api, route, query), fediHeaders(l.root), out)
		if err == nil {
			l.api = api
			return nil
		}
		if first == nil {
			first = err
		}
		if !httpx.HasStatus(err, http.StatusNotFound) {
			return err
		}
	}
	return first
}

func (l *lemmyClient) endpoint(api, route string, query url.Values) string {
	return l.root + api + route + "?" + query.Encode()
}

// lemmyPost is one submission.
//
// thumbnail_url is deliberately absent: it is the instance's own scaled copy
// of whatever the post points at, served from its pict-rs no matter where the
// original lives, and taking it would fill a folder with previews.
type lemmyPost struct {
	ID      int64  `json:"id"`
	Name    string `json:"name"`
	URL     string `json:"url"`
	Deleted bool   `json:"deleted"`
	Removed bool   `json:"removed"`
	// ContentType is what the instance saw when it fetched the link. It is
	// the only honest way to tell an image submission from a link to an
	// article, and versions before 0.19 do not send it.
	ContentType string `json:"url_content_type"`
}

// lemmyPostView wraps a post with the context the API attaches to it.
type lemmyPostView struct {
	Post lemmyPost `json:"post"`
}

// lemmy resolves a community, a person's submissions, or a single post.
func (f *Fediverse) lemmy(ctx context.Context, u *url.URL, t *fediTarget) (*Result, error) {
	client := &lemmyClient{f: f, root: util.Origin(u)}

	if t.kind == fediPost {
		var payload struct {
			PostView lemmyPostView `json:"post_view"`
		}
		if err := client.get(ctx, "/post", url.Values{"id": {t.id}}, &payload); err != nil {
			return nil, fmt.Errorf("fediverse: read post %s: %w", t.id, err)
		}
		files := lemmyFiles([]lemmyPostView{payload.PostView})
		if len(files) == 0 {
			return nil, fmt.Errorf("fediverse: post %s links to no media — it is a text post, "+
				"or a link to a page rather than to a file", t.id)
		}
		title := util.FirstNonEmpty(payload.PostView.Post.Name, "post "+t.id)
		return &Result{Title: folderName(title), Files: files}, nil
	}

	route, key := "/post/list", "community_name"
	title := "!" + fediHandle(t.handle, u.Hostname())
	if t.kind == fediAccount {
		// A person's page is the same listing keyed differently. The response
		// carries their comments as well, which hold nothing to download.
		route, key = "/user", "username"
		title = "@" + fediHandle(t.handle, u.Hostname())
	}

	var views []lemmyPostView
	for page := 1; page <= config.MaxTimelinePages; page++ {
		query := url.Values{}
		query.Set(key, t.handle)
		query.Set("limit", strconv.Itoa(fediLemmyPageSize))
		query.Set("page", strconv.Itoa(page))
		query.Set("sort", "New")

		var payload struct {
			Posts []lemmyPostView `json:"posts"`
		}
		if err := client.get(ctx, route, query, &payload); err != nil {
			if page == 1 {
				return nil, fmt.Errorf("fediverse: read %s: %w", title, err)
			}
			break
		}
		// Lemmy 1.0 replaces page numbers with a cursor, and an instance that
		// ignores a parameter it no longer knows would hand back the first
		// page for ever. Stopping once a page adds nothing new is what makes
		// that one wasted request instead of an endless loop.
		before := len(views)
		lemmyAppend(&views, payload.Posts)
		if len(views) == before || len(payload.Posts) < fediLemmyPageSize {
			break
		}
	}

	files := lemmyFiles(views)
	if len(files) == 0 {
		return nil, fmt.Errorf("fediverse: %s has no image or video submissions among its most "+
			"recent %d posts", title, config.MaxTimelinePages*fediLemmyPageSize)
	}
	return &Result{Title: title, Files: files}, nil
}

// lemmyAppend adds the posts of a page that are not held already.
func lemmyAppend(views *[]lemmyPostView, page []lemmyPostView) {
	seen := make(map[int64]bool, len(*views))
	for _, view := range *views {
		seen[view.Post.ID] = true
	}
	for _, view := range page {
		if !seen[view.Post.ID] {
			seen[view.Post.ID] = true
			*views = append(*views, view)
		}
	}
}

// lemmyFiles keeps the submissions that are a file rather than a page.
//
// A post's url is whatever was submitted: for an image post it is the pict-rs
// object — often on another instance, since a federated community carries
// everyone's — but for a link post it is a news article, and queueing that
// would save an HTML page under a picture's name. The saved name pairs the
// post id with its title, because pict-rs names everything after a UUID and
// two posts can share a title.
func lemmyFiles(views []lemmyPostView) []File {
	seen := make(map[string]bool)
	var files []File

	for _, view := range views {
		post := view.Post
		if post.URL == "" || post.Deleted || post.Removed || seen[post.URL] {
			continue
		}
		if !lemmyIsMedia(&post) {
			continue
		}
		seen[post.URL] = true

		name := strconv.FormatInt(post.ID, 10)
		if title := folderName(post.Name); title != "" {
			name += " " + title
		}
		files = append(files, File{
			Name: name + fediExt(post.URL),
			URL:  post.URL,
			Size: -1,
		})
	}
	return files
}

// lemmyIsMedia reports whether a submission points at a file worth fetching.
//
// The instance's own reading of the link is trusted where it says something:
// media is media, and text/html is a page whatever its path suggests. What it
// records for a link it could not identify is neither — a video site's watch
// page comes back as "application/binary" — so those fall through to the path,
// which is also all that an instance too old to record a type ever leaves.
func lemmyIsMedia(post *lemmyPost) bool {
	switch kind, _, _ := strings.Cut(post.ContentType, "/"); kind {
	case "image", "video", "audio":
		return true
	case "text":
		return false
	}
	return fediIsMediaURL(post.URL)
}
