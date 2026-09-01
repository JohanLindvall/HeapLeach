package extractor

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
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
	hostSet
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
	redgifsMaxPages = 25
)

// NewRedGifs builds the redgifs extractor.
func NewRedGifs(client *httpx.Client) *RedGifs {
	return &RedGifs{hostSet: hostSet{"redgifs.com"}, client: client}
}

func (r *RedGifs) Name() string { return "redgifs" }

// Extract handles a single item (/watch/<id>) or a user's whole feed
// (/users/<name>).
func (r *RedGifs) Extract(ctx context.Context, u *url.URL, _ Options) (*Result, error) {
	segs := util.PathSegments(u)
	if len(segs) < 2 {
		return nil, fmt.Errorf("redgifs: %s is not a watch or user link", u.Redacted())
	}

	switch strings.ToLower(segs[0]) {
	case "users":
		return r.user(ctx, segs[1])
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

// user pages through everything one account has posted.
func (r *RedGifs) user(ctx context.Context, name string) (*Result, error) {
	var files []File
	for page := 1; page <= redgifsMaxPages; page++ {
		var payload struct {
			Gifs  []redgifsItem `json:"gifs"`
			Pages int           `json:"pages"`
		}
		path := fmt.Sprintf("/users/%s/search?order=new&count=%d&page=%d",
			url.PathEscape(name), redgifsPageSize, page)
		if err := r.call(ctx, path, &payload); err != nil {
			if page == 1 {
				return nil, fmt.Errorf("redgifs: user %s: %w", name, err)
			}
			break
		}
		for _, item := range payload.Gifs {
			if file, ok := item.file(); ok {
				files = append(files, file)
			}
		}
		if len(payload.Gifs) == 0 || (payload.Pages > 0 && page >= payload.Pages) {
			break
		}
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("redgifs: user %s has no downloadable media", name)
	}
	return &Result{Title: name, Files: files}, nil
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
	UserName string  `json:"userName"`
	Duration float64 `json:"duration"`
}

// file turns an item into a downloadable entry, preferring the better copy.
func (i redgifsItem) file() (File, bool) {
	link := util.FirstNonEmpty(i.URLs.HD, i.URLs.SD)
	if link == "" {
		return File{}, false
	}
	name := i.name()
	if ext := util.NameFromURL(link); strings.Contains(ext, ".") {
		name += ext[strings.LastIndex(ext, "."):]
	}
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
