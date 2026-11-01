// SPDX-License-Identifier: MIT

package extractor

import (
	"cmp"
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
	"github.com/JohanLindvall/HeapLeach/internal/util"
)

// RedGifs resolves redgifs links through its public API.
//
// Every call needs a bearer token, but the token is handed out on request
// with no account behind it, so this is only a preliminary step rather than
// a login. Tokens are reused until they age out.
type RedGifs struct {
	about
	client *httpx.Client
	// api is the API's base, a field so a test can serve it.
	api string

	token renewable[string]
}

const (
	redgifsAPI  = "https://api.redgifs.com/v2"
	redgifsRoot = "https://www.redgifs.com"
)

// NewRedGifs builds the redgifs extractor.
func NewRedGifs(client *httpx.Client) *RedGifs {
	return &RedGifs{client: client, api: redgifsAPI, about: about{name: "redgifs", shape: redgifsShape,
		recognises: "clips (`/watch/<id>`, `/ifr/<id>`) and users (`/users/<name>`) served by the RedGIFs app, " +
			"a clip's page naming its embed player"}}
}

var (
	// redgifsShape is a clip, its player, or a user's clips.
	redgifsShape = pathShape(`^/(?:watch|ifr)/[A-Za-z]+/?$`, `^/users/[^/]+/?$`)
	// redgifsPlayer is the player a clip's page offers for embedding.
	redgifsPlayer = pathShape(`^/ifr/[A-Za-z]+`)
)

// redgifsBundle is the path the app's client data script is served under.
const redgifsBundle = "/client_data/"

// Recognise takes a page of the app — its bundle and name — and for a
// clip's page the player it names, the id Extract asks the API about. A
// user's page is the app shell alone.
//
// A page of that form the probe was kept from (probeWithheld) is taken when
// the API, asked once, answers for the clip or user it names: the API is
// what Extract reads.
func (r *RedGifs) Recognise(ctx context.Context, p *Page) bool {
	if !redgifsShape(p.Final) {
		return false
	}
	if probeWithheld(p) {
		return r.answers(ctx, p)
	}
	if !p.Script(redgifsBundle) || !p.Named("RedGIFs") {
		return false
	}
	return p.Segments()[0] != "watch" || p.playerMeta(redgifsPlayer)
}

// answers asks the API, once per page, about the clip or user a withheld
// page's link names: a clip with media, or a user's first page of clips.
func (r *RedGifs) answers(ctx context.Context, p *Page) bool {
	segs := p.Segments()
	if len(segs) < 2 || r.client == nil {
		return false
	}
	kind, id := strings.ToLower(segs[0]), segs[1]
	return withheldAsked(ctx, p, "redgifs "+kind+" "+id, func(ctx context.Context) bool {
		if kind == "users" {
			var payload struct {
				Gifs []redgifsItem `json:"gifs"`
			}
			path := fmt.Sprintf("/users/%s/search?order=new&count=1&page=1", url.PathEscape(id))
			return r.call(ctx, path, &payload) == nil && len(payload.Gifs) > 0
		}
		var payload struct {
			Gif redgifsItem `json:"gif"`
		}
		if r.call(ctx, "/gifs/"+url.PathEscape(strings.ToLower(id)), &payload) != nil {
			return false
		}
		_, ok := payload.Gif.file()
		return ok
	})
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
	type playableItem struct {
		id   string
		file File
	}
	items, note, err := walkPages(ctx, pageWalk[playableItem]{
		key:   func(item playableItem) string { return item.id },
		limit: opts.maxFiles(), limitNote: fileLimitNote,
		pages: config.RedGifsMaxPages,
		fetch: func(ctx context.Context, n int) (pageOf[playableItem], error) {
			var payload struct {
				Gifs  []redgifsItem `json:"gifs"`
				Pages int           `json:"pages"`
			}
			path := fmt.Sprintf("/users/%s/search?order=new&count=%d&page=%d",
				url.PathEscape(name), config.RedGifsPageSize, n+1)
			if err := r.call(ctx, path, &payload); err != nil {
				return pageOf[playableItem]{}, fmt.Errorf("redgifs: user %s: %w", name, err)
			}
			// Entries without media spend neither a file slot nor their ID:
			// a later page may carry a playable copy of the same entry.
			var playable []playableItem
			for _, item := range payload.Gifs {
				if file, ok := item.file(); ok {
					playable = append(playable, playableItem{id: item.ID, file: file})
				}
			}
			return pageOf[playableItem]{
				items: playable, next: true, pages: payload.Pages,
				// An empty response ends the feed unless the API names
				// later pages. Unusable batches use the bounded skip path.
				end:  len(payload.Gifs) == 0 && payload.Pages <= n+1,
				skip: len(playable) == 0,
			}, nil
		},
	})
	if err != nil {
		return nil, err
	}
	var files []File
	for _, item := range items {
		files = append(files, item.file)
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("redgifs: user %s has no downloadable media", name)
	}
	return &Result{Title: name, Files: files, Note: note}, nil
}

// call performs an authenticated request, minting a token when needed and
// retrying once if the one held has expired.
func (r *RedGifs) call(ctx context.Context, path string, out any) error {
	token, err := r.token.get(ctx, r.mintToken)
	if err != nil {
		return err
	}
	headers := httpx.Header{
		httpx.HeaderAuthorization: "Bearer " + token,
		httpx.HeaderReferer:       redgifsRoot + "/",
	}
	err = r.client.GetJSON(ctx, r.api+path, headers, out)
	if err == nil {
		return nil
	}
	if !httpx.HasStatus(err, http.StatusUnauthorized) {
		return err
	}
	if token, err = r.token.renew(ctx, token, r.mintToken); err != nil {
		return err
	}
	headers[httpx.HeaderAuthorization] = "Bearer " + token
	return r.client.GetJSON(ctx, r.api+path, headers, out)
}

// mintToken asks for a temporary bearer token, for r.token.
func (r *RedGifs) mintToken(ctx context.Context) (string, time.Time, error) {
	var payload struct {
		Token string `json:"token"`
	}
	if err := r.client.GetJSON(ctx, r.api+"/auth/temporary",
		httpx.Referer(redgifsRoot+"/"), &payload); err != nil {
		return "", time.Time{}, fmt.Errorf("request token: %w", err)
	}
	if payload.Token == "" {
		return "", time.Time{}, fmt.Errorf("no token returned")
	}
	return payload.Token, time.Now().Add(config.RedGifsTokenTTL), nil
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
	name := i.name() + linkExtAsIs(link, "")
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
