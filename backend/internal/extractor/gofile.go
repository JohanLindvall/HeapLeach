package extractor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"path"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
	"github.com/JohanLindvall/HeapLeach/internal/util"
)

// Gofile resolves https://gofile.io/d/<code> links.
//
// Gofile's web app signs every content request with a "website token": the
// SHA-256 of the user agent, the browser language, the account token, a
// 4-hour time slot, and a shared secret, sent as X-Website-Token. The secret
// lives in gofile's obfuscated wt.obf.js and is rotated server-side, so it is
// read out of that script rather than pinned here — see gofilewt.go — and
// kept for as long as the script says it may be. What is pinned is only a
// fallback for when that fails, and HEAPLEACH_GOFILE_SECRET overrides both.
//
// Signing badly is worse than not signing: gofile blocks an address that
// does it. So a rejected signature is remembered, and until that memory
// expires the next link is refused here rather than sent — one wrong secret
// costs one request, not one per link in the queue.
type Gofile struct {
	hostSet
	client   *httpx.Client
	fallback string
	lang     string
	// scriptURL is where the signing secret is read from, kept as a field
	// so the caching and fallback behaviour can be tested without gofile —
	// which matters here, since the way to test a secret against gofile is
	// to sign badly with it.
	scriptURL string

	mu       sync.Mutex
	token    string
	tokenAge time.Time
	// secret is what currently signs, and secretUntil is when it must be
	// looked up again.
	secret      string
	secretUntil time.Time
	// rejectedAt is when gofile last refused a signature.
	rejectedAt time.Time
}

// gofileRejectionCooldown is how long a refused signature stops further
// signed requests. Long enough that a queue cannot become a burst of them.
const gofileRejectionCooldown = 15 * time.Minute

const (
	gofileAPI  = "https://api.gofile.io"
	gofileRoot = "https://gofile.io"

	// gofileStorageDomain is what a server name from the API is completed
	// with to address one storage server directly.
	gofileStorageDomain = "gofile.io"

	// gofileCookie carries the guest account token to the download CDN.
	gofileCookie = "accountToken"

	// Gofile's own request-signature headers.
	gofileHeaderWebsiteToken = "X-Website-Token"
	gofileHeaderLanguage     = "X-BL"

	// gofileSlot is the width of the time slot mixed into the signature.
	gofileSlot = 4 * time.Hour
	// gofileTokenTTL re-mints the guest account well inside the 6-hour
	// window the web app allows a page to keep signing.
	gofileTokenTTL = 3 * time.Hour
)

// NewGofile builds the gofile extractor.
func NewGofile(cfg *config.Config, client *httpx.Client) *Gofile {
	return &Gofile{
		hostSet:   hostSet{"gofile.io"},
		client:    client,
		fallback:  cfg.GofileSecret,
		lang:      cfg.Language,
		scriptURL: gofileRoot + gofileScriptPath,
	}
}

func (g *Gofile) Name() string { return "gofile" }

// Extract resolves a share code into its files, walking nested folders.
func (g *Gofile) Extract(ctx context.Context, u *url.URL, opts Options) (*Result, error) {
	segs := util.PathSegments(u)
	if len(segs) == 0 {
		return nil, fmt.Errorf("gofile: no content code in %s", u.Redacted())
	}
	// Accept /d/<code>, /<code> and the odd /?c=<code> shape.
	code := segs[len(segs)-1]
	if c := u.Query().Get("c"); c != "" {
		code = c
	}

	token, err := g.ensureToken(ctx, false)
	if err != nil {
		return nil, err
	}

	root, err := g.fetchContent(ctx, code, token, opts.Password)
	if errors.Is(err, errGofileStaleToken) {
		if token, err = g.ensureToken(ctx, true); err != nil {
			return nil, err
		}
		root, err = g.fetchContent(ctx, code, token, opts.Password)
	}
	if err != nil {
		return nil, err
	}

	res := &Result{Title: root.Name}
	if err := g.collect(ctx, root, token, opts.Password, "", 0, res); err != nil {
		return nil, err
	}
	return res, nil
}

// collect appends c's files to res, recursing into subfolders.
func (g *Gofile) collect(ctx context.Context, c *gofileContent, token, password, dir string, depth int, res *Result) error {
	// The shared listing ceiling, for a share that is really somebody's whole
	// drive: past it the queue has stopped being something anyone meant.
	if len(res.Files) >= config.MaxListingFiles {
		return nil
	}
	if c.Type == "file" {
		if c.Link == "" {
			return nil
		}
		file := File{
			Name:    util.FirstNonEmpty(c.Name, util.NameFromURL(c.Link)),
			URL:     c.Link,
			Size:    c.Size,
			Dir:     dir,
			Headers: g.downloadHeaders(token),
		}
		// A gofile file often lives on several storage servers, and a busy
		// one answers with a redirect to the web page instead of bytes.
		// Rotating on each attempt gives a retry a real chance of landing
		// somewhere that will serve it.
		if len(c.Servers) > 1 {
			hosts := make([]string, 0, len(c.Servers))
			for _, server := range c.Servers {
				hosts = append(hosts, server+"."+gofileStorageDomain)
			}
			file = Mirrored(file, MirrorHosts(c.Link, hosts))
		}
		res.Files = append(res.Files, file)
		return nil
	}
	if depth >= config.MaxFolderDepth {
		return nil
	}

	// Children arrive keyed by id; sort by the order gofile listed them so
	// the UI matches the website.
	for _, child := range c.orderedChildren() {
		switch child.Type {
		case "file":
			if err := g.collect(ctx, child, token, password, dir, depth+1, res); err != nil {
				return err
			}
		case "folder":
			sub, err := g.fetchContent(ctx, child.ID, token, password)
			if err != nil {
				// One unreadable subfolder should not sink the whole job.
				continue
			}
			next := path.Join(dir, util.FirstNonEmpty(child.Name, child.ID))
			if err := g.collect(ctx, sub, token, password, next, depth+1, res); err != nil {
				return err
			}
		}
	}
	return nil
}

// downloadHeaders are what the CDN needs to serve the file to us.
func (g *Gofile) downloadHeaders(token string) httpx.Header {
	return httpx.Header{
		httpx.HeaderCookie:  gofileCookie + "=" + token,
		httpx.HeaderReferer: gofileRoot + "/",
	}
}

var errGofileStaleToken = errors.New("gofile: account token rejected")

// fetchContent GETs one content id, following pagination and merging pages.
func (g *Gofile) fetchContent(ctx context.Context, id, token, password string) (*gofileContent, error) {
	secret, err := g.ensureSecret(ctx)
	if err != nil {
		return nil, err
	}

	var merged *gofileContent

	// Bounded the way every paginated listing here is: the API says when the
	// pages end, and the cap is the backstop against one that keeps saying
	// there is more. MaxAlbumPages of FolderPageSize children per folder sits
	// at the same order as MaxListingFiles, which bounds the job as a whole.
	for page := 1; page <= config.MaxAlbumPages; page++ {
		q := url.Values{
			"page":          {strconv.Itoa(page)},
			"pageSize":      {strconv.Itoa(config.FolderPageSize)},
			"sortField":     {"createTime"},
			"sortDirection": {"-1"},
		}
		if password != "" {
			sum := sha256.Sum256([]byte(password))
			q.Set("password", hex.EncodeToString(sum[:]))
		}
		endpoint := gofileAPI + "/contents/" + url.PathEscape(id) + "?" + q.Encode()

		var env gofileEnvelope
		err = g.client.GetJSON(ctx, endpoint, httpx.Header{
			httpx.HeaderAuthorization: "Bearer " + token,
			gofileHeaderWebsiteToken:  g.websiteToken(token, secret, time.Now()),
			gofileHeaderLanguage:      g.lang,
			httpx.HeaderOrigin:        gofileRoot,
			httpx.HeaderReferer:       gofileRoot + "/",
		}, &env)
		if err != nil {
			return nil, g.explain(err)
		}
		if env.Status != "ok" {
			return nil, g.statusError(env.Status)
		}
		if env.Data.CanAccess != nil && !*env.Data.CanAccess {
			return nil, env.Data.accessError()
		}

		if merged == nil {
			merged = &env.Data
		} else {
			maps.Copy(merged.Children, env.Data.Children)
			merged.ChildrenIDs = append(merged.ChildrenIDs, env.Data.ChildrenIDs...)
		}
		if !env.Metadata.HasNextPage || len(env.Data.Children) == 0 {
			return merged, nil
		}
	}
	// The cap bit before the API said it was done; what is in hand is a
	// truncated listing, which is still worth more than an error.
	return merged, nil
}

// websiteToken reproduces gofile's client-side request signature.
func (g *Gofile) websiteToken(accountToken, secret string, now time.Time) string {
	slot := strconv.FormatInt(now.Unix()/int64(gofileSlot/time.Second), 10)
	preimage := strings.Join([]string{
		g.client.UserAgent(), g.lang, accountToken, slot, secret,
	}, "::")
	sum := sha256.Sum256([]byte(preimage))
	return hex.EncodeToString(sum[:])
}

// ensureToken returns a guest account token, minting one when needed.
func (g *Gofile) ensureToken(ctx context.Context, force bool) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	if !force && g.token != "" && time.Since(g.tokenAge) < gofileTokenTTL {
		return g.token, nil
	}

	var env struct {
		Status string `json:"status"`
		Data   struct {
			Token string `json:"token"`
			Tier  string `json:"tier"`
		} `json:"data"`
	}
	err := g.client.PostJSON(ctx, gofileAPI+"/accounts",
		httpx.RefererOrigin(gofileRoot+"/", gofileRoot), nil, &env)
	if err != nil {
		return "", fmt.Errorf("gofile: create guest account: %w", err)
	}
	if env.Status != "ok" || env.Data.Token == "" {
		return "", fmt.Errorf("gofile: create guest account: unexpected status %q", env.Status)
	}

	g.token, g.tokenAge = env.Data.Token, time.Now()
	return g.token, nil
}

// explain turns an HTTP failure into an actionable message. A 401 with
// error-notPremium is what gofile returns for a bad signature, which in
// practice means the shared secret rotated.
func (g *Gofile) explain(err error) error {
	if se, ok := errors.AsType[*httpx.StatusError](err); ok {
		switch {
		case strings.Contains(se.Body, "error-notPremium"):
			return g.signatureRejected()
		case strings.Contains(se.Body, "error-token"), strings.Contains(se.Body, "error-wrongToken"):
			return errGofileStaleToken
		}
	}
	return fmt.Errorf("gofile: %w", err)
}

func (g *Gofile) statusError(status string) error {
	// Folded for comparison: gofile camel-cases its statuses ("error-wrongToken")
	// and the markers here are fragments of them.
	folded := strings.ToLower(status)
	switch {
	case strings.Contains(folded, "password"):
		return ErrPasswordRequired
	case strings.Contains(folded, "notfound"):
		return errors.New("gofile: content not found (removed or wrong link)")
	case strings.Contains(folded, "token"):
		return errGofileStaleToken
	}
	return fmt.Errorf("gofile: api returned %q", status)
}

// gofileEnvelope is the standard {status, data, metadata} response body.
type gofileEnvelope struct {
	Status   string        `json:"status"`
	Data     gofileContent `json:"data"`
	Metadata struct {
		TotalCount  int  `json:"totalCount"`
		TotalPages  int  `json:"totalPages"`
		Page        int  `json:"page"`
		PageSize    int  `json:"pageSize"`
		HasNextPage bool `json:"hasNextPage"`
	} `json:"metadata"`
}

// gofileContent is one node of the content tree: a file or a folder.
type gofileContent struct {
	CanAccess      *bool                    `json:"canAccess"`
	ID             string                   `json:"id"`
	Type           string                   `json:"type"`
	Name           string                   `json:"name"`
	Code           string                   `json:"code"`
	Size           int64                    `json:"size"`
	Link           string                   `json:"link"`
	MimeType       string                   `json:"mimetype"`
	ChildrenCount  int                      `json:"childrenCount"`
	Servers        []string                 `json:"servers"`
	ServerSelected string                   `json:"serverSelected"`
	ChildrenIDs    []string                 `json:"childrenIds"`
	Children       map[string]gofileContent `json:"children"`
	Password       json.RawMessage          `json:"password"`
	PasswordStatus string                   `json:"passwordStatus"`
	Expire         json.RawMessage          `json:"expire"`
}

// accessError explains a canAccess:false response.
func (c *gofileContent) accessError() error {
	if c.PasswordStatus != "" && c.PasswordStatus != "passwordOk" {
		return ErrPasswordRequired
	}
	if len(c.Password) > 0 && string(c.Password) != "null" && string(c.Password) != "false" {
		return ErrPasswordRequired
	}
	if len(c.Expire) > 0 && string(c.Expire) != "null" {
		return errors.New("gofile: this link has expired")
	}
	return errors.New("gofile: content is not accessible (private, expired or removed)")
}

// orderedChildren lists children in gofile's own order where it supplied one,
// falling back to a name sort so results are at least deterministic.
func (c *gofileContent) orderedChildren() []*gofileContent {
	out := make([]*gofileContent, 0, len(c.Children))
	seen := make(map[string]bool, len(c.Children))

	for _, id := range c.ChildrenIDs {
		if child, ok := c.Children[id]; ok && !seen[id] {
			seen[id] = true
			out = append(out, &child)
		}
	}
	rest := make([]string, 0, len(c.Children))
	for id := range c.Children {
		if !seen[id] {
			rest = append(rest, id)
		}
	}
	slices.SortFunc(rest, func(a, b string) int {
		return strings.Compare(strings.ToLower(c.Children[a].Name), strings.ToLower(c.Children[b].Name))
	})
	for _, id := range rest {
		child := c.Children[id]
		out = append(out, &child)
	}
	return out
}
