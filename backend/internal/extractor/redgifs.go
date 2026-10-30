// SPDX-License-Identifier: MIT

package extractor

import (
	"cmp"
	"context"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/httpx"
	"github.com/JohanLindvall/HeapLeach/internal/util"
)

// RedGifs resolves redgifs links through its public API.
//
// Every call needs a bearer token, but the token is handed out on request
// with no account behind it, so this is only a preliminary step rather than
// a login. Tokens are reused until they age out.
type RedGifs struct {
	client *httpx.Client

	mu       sync.Mutex
	token    string
	tokenAge time.Time
}

const (
	redgifsAPI      = "https://api.redgifs.com/v2"
	redgifsRoot     = "https://www.redgifs.com"
	redgifsTokenTTL = time.Hour
	redgifsPageSize = 80
)

// NewRedGifs builds the redgifs extractor.
func NewRedGifs(client *httpx.Client) *RedGifs {
	return &RedGifs{client: client}
}

func (r *RedGifs) Name() string { return "redgifs" }

var (
	// redgifsShape is a clip, its player, or a user's clips.
	redgifsShape = pathShape(`^/(?:watch|ifr)/[A-Za-z]+/?$`, `^/users/[^/]+/?$`)
	// redgifsPlayer is the player a clip's page offers for embedding.
	redgifsPlayer = pathShape(`^/ifr/[A-Za-z]+`)
)

// redgifsBundle is the path the app's client data script is served under.
const redgifsBundle = "/client_data/"

// Recognises is the inventory's description.
func (r *RedGifs) Recognises() string {
	return "clips (`/watch/<id>`, `/ifr/<id>`) and users (`/users/<name>`) served by the RedGIFs app, " +
		"a clip's page naming its embed player"
}

// Shape reports a clip, a player or a user.
func (r *RedGifs) Shape(u *url.URL) bool { return redgifsShape(u) }

// Recognise takes a page of the app — its bundle and name — and for a
// clip's page the player it names, the id Extract asks the API about. A
// user's page is the app shell alone.
func (r *RedGifs) Recognise(_ context.Context, p *Page) bool {
	if !redgifsShape(p.Final) || !p.Script(redgifsBundle) || !p.Named("RedGIFs") {
		return false
	}
	return p.Segments()[0] != "watch" || p.playerMeta(redgifsPlayer)
}

// Extract handles a single item (/watch/<id>) or a user's whole feed
// (/users/<name>).
func (r *RedGifs) Extract(ctx context.Context, u *url.URL, opts Options) (*Result, error) {
	segs := util.PathSegments(u)
	if len(segs) < 2 {
		return nil, fmt.Errorf("redgifs: %s is not a watch or user link", u.Redacted())
	}

	switch strings.ToLower(segs[0]) {
	case "users":
		return r.user(ctx, segs[1], opts)
	case "watch", "ifr":
		return r.single(ctx, segs[1])
	default:
		return nil, fmt.Errorf("redgifs: %s is not a watch or user link", u.Redacted())
	}
}

// single resolves one item.
func (r *RedGifs) single(ctx context.Context, id string) (*Result, error) {
	var payload struct {
		Gif redgifsItem `json:"gif"`
	}
	if err := r.call(ctx, "/gifs/"+url.PathEscape(strings.ToLower(id)), &payload); err != nil {
		return nil, fmt.Errorf("redgifs: %s: %w", id, err)
	}
	file, ok := payload.Gif.file()
	if !ok {
		return nil, fmt.Errorf("redgifs: %s has no downloadable media", id)
	}
	return &Result{Title: payload.Gif.name(), Files: []File{file}}, nil
}

// user pages through everything one account has posted, to the page count
// the API states.
func (r *RedGifs) user(ctx context.Context, name string, opts Options) (*Result, error) {
	items, note, err := walkPages(ctx, pageWalk[redgifsItem]{
		key:   func(item redgifsItem) string { return item.ID },
		limit: opts.maxFiles(), limitNote: "partial — file limit",
		fetch: func(ctx context.Context, n int) (pageOf[redgifsItem], error) {
			var payload struct {
				Gifs  []redgifsItem `json:"gifs"`
				Pages int           `json:"pages"`
			}
			path := fmt.Sprintf("/users/%s/search?order=new&count=%d&page=%d",
				url.PathEscape(name), redgifsPageSize, n+1)
			if err := r.call(ctx, path, &payload); err != nil {
				return pageOf[redgifsItem]{}, fmt.Errorf("redgifs: user %s: %w", name, err)
			}
			return pageOf[redgifsItem]{items: payload.Gifs, next: true, end: len(payload.Gifs) == 0, pages: payload.Pages}, nil
		},
	})
	if err != nil {
		return nil, err
	}
	var files []File
	for _, item := range items {
		if file, ok := item.file(); ok {
			files = append(files, file)
		}
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("redgifs: user %s has no downloadable media", name)
	}
	return &Result{Title: name, Files: files, Note: note}, nil
}

// call performs an authenticated request, minting a token when needed and
// retrying once if the one held has expired.
func (r *RedGifs) call(ctx context.Context, path string, out any) error {
	token, err := r.ensureToken(ctx, false)
	if err != nil {
		return err
	}
	headers := httpx.Header{
		httpx.HeaderAuthorization: "Bearer " + token,
		httpx.HeaderReferer:       redgifsRoot + "/",
	}
	err = r.client.GetJSON(ctx, redgifsAPI+path, headers, out)
	if err == nil {
		return nil
	}
	if !httpx.HasStatus(err, http.StatusUnauthorized) {
		return err
	}
	if token, err = r.ensureToken(ctx, true); err != nil {
		return err
	}
	headers[httpx.HeaderAuthorization] = "Bearer " + token
	return r.client.GetJSON(ctx, redgifsAPI+path, headers, out)
}

// ensureToken returns a bearer token, requesting a fresh one when needed.
func (r *RedGifs) ensureToken(ctx context.Context, force bool) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if !force && r.token != "" && time.Since(r.tokenAge) < redgifsTokenTTL {
		return r.token, nil
	}
	var payload struct {
		Token string `json:"token"`
	}
	if err := r.client.GetJSON(ctx, redgifsAPI+"/auth/temporary",
		httpx.Referer(redgifsRoot+"/"), &payload); err != nil {
		return "", fmt.Errorf("request token: %w", err)
	}
	if payload.Token == "" {
		return "", fmt.Errorf("no token returned")
	}
	r.token, r.tokenAge = payload.Token, time.Now()
	return r.token, nil
}

// redgifsItem is one piece of media.
type redgifsItem struct {
	ID   string `json:"id"`
	URLs struct {
		HD string `json:"hd"`
		SD string `json:"sd"`
	} `json:"urls"`
	Duration float64 `json:"duration"`
}

// file turns an item into a downloadable entry, preferring the better copy.
func (i redgifsItem) file() (File, bool) {
	link := cmp.Or(i.URLs.HD, i.URLs.SD)
	if link == "" {
		return File{}, false
	}
	name := i.name() + path.Ext(util.NameFromURL(link))
	return File{
		Name:    name,
		URL:     link,
		Size:    -1,
		Headers: httpx.Referer(redgifsRoot + "/"),
	}, true
}

// name identifies an item, falling back to its id.
func (i redgifsItem) name() string {
	if i.ID != "" {
		return i.ID
	}
	return "redgifs-" + strconv.FormatInt(time.Now().Unix(), 10)
}
