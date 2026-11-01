// SPDX-License-Identifier: MIT

package extractor

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
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
	about
	client   *httpx.Client
	fallback string
	lang     string
	// scriptURL is where the signing secret is read from, kept as a field
	// so the caching and fallback behaviour can be tested without gofile —
	// which matters here, since the way to test a secret against gofile is
	// to sign badly with it.
	scriptURL string
	// api is the content API's base, a field so a test can serve it.
	api string

	// account is the guest account token every request is signed with.
	account renewable[string]

	mu sync.Mutex
	// secret is what currently signs, and secretUntil is when it must be
	// looked up again.
	secret      string
	secretUntil time.Time
	// rejectedAt is when gofile last refused a signature.
	rejectedAt time.Time
}

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
)

// NewGofile builds the gofile extractor.
func NewGofile(cfg *config.Config, client *httpx.Client) *Gofile {
	return &Gofile{
		about: about{name: "gofile", shape: gofileShape,
			recognises: "share links (`/d/<code>`, and the older `/<code>` and `/?c=<code>`) whose page is the " +
				"Gofile app shell, loading its signing script"},
		client:    client,
		fallback:  cfg.GofileSecret,
		lang:      cfg.Language,
		scriptURL: gofileRoot + gofileScriptPath,
		api:       gofileAPI,
	}
}

// gofileShape is a share link: /d/<code>, or the legacy /?c=<code>.
var (
	gofileShape = func(u *url.URL) bool { return gofileShareShape(u) || gofileLegacyShape(u) }
	// gofileShareShape is /d/<code>, or /d/<content id> — a folder's UUID,
	// which the web app shows once a subfolder is opened.
	gofileShareShape = pathShape(`^/d/[A-Za-z0-9][A-Za-z0-9-]*/?$`)
	// gofileLegacyShape is the old /?c=<code>.
	gofileLegacyShape = pathQueryShape(`^/?\?(?:.*&)?c=[A-Za-z0-9][A-Za-z0-9-]*(?:&|$)`)
	// gofileBareShape is the bare /<code> older links use: too common a form
	// for the link scanner, and taken only when the app shell answers it.
	gofileBareShape = pathShape(`^/[A-Za-z0-9]{4,}/?$`)
)

// Recognise takes a share link served by the site's app shell. The folder
// is fetched by the app afterwards, so the shell is all a share link ever
// answers with, deleted or not — and the shell is checked for what this
// extractor depends on, not only for the form of the link: it loads the
// script the request signature is read from (gofilewt.go), and names itself.
//
// A link on gofile's own host never gets here: the registry gives it to
// Extract unprobed (hostClaims), which reads the content API whatever the
// shell looks like. A page of the form on any other host that the probe was
// kept from is not taken: the API is not asked about another host's code,
// which would cost a guest account, a signed request and, were it refused,
// the cooldown real links wait out (signatureRejected).
func (g *Gofile) Recognise(_ context.Context, p *Page) bool {
	return (gofileShape(p.Final) || gofileBareShape(p.Final)) && p.Script(gofileScriptPath) && p.Named("Gofile")
}

// gofileCode is the content code a link names: /d/<code>, /<code> or
// /?c=<code>.
func gofileCode(u *url.URL) string {
	code := u.Query().Get("c")
	if segs := util.PathSegments(u); code == "" && len(segs) > 0 {
		code = segs[len(segs)-1]
	}
	return code
}

// Extract resolves a share code into its files, walking nested folders.
func (g *Gofile) Extract(ctx context.Context, u *url.URL, opts Options) (*Result, error) {
	// Accept /d/<code>, /<code> and the odd /?c=<code> shape.
	code := gofileCode(u)
	if code == "" {
		return nil, fmt.Errorf("gofile: no content code in %s", u.Redacted())
	}

	token, err := g.account.get(ctx, g.mintAccount)
	if err != nil {
		return nil, err
	}

	root, end, err := g.fetchContent(ctx, code, token, opts.Password, opts.maxFiles())
	if errors.Is(err, errGofileStaleToken) {
		if token, err = g.account.renew(ctx, token, g.mintAccount); err != nil {
			return nil, err
		}
		root, end, err = g.fetchContent(ctx, code, token, opts.Password, opts.maxFiles())
	}
	if err != nil {
		return nil, err
	}

	// A root listing cut short is noted before the walk adds its own.
	res := &Result{Title: root.Name, Note: end.render()}
	if err := walkTree(ctx, opts, res, root.ID, root, g.lister(token, opts.Password)); err != nil {
		return nil, err
	}
	return res, nil
}

// lister reads one folder for walkTree. The root arrives already fetched,
// since reading it is where a stale token shows; a subfolder arrives as its
// listing entry and is fetched here.
func (g *Gofile) lister(token, password string) treeList[*gofileContent] {
	return func(ctx context.Context, w *treeWalk, c *gofileContent, depth int) ([]treeItem[*gofileContent], error) {
		if depth > 0 {
			var (
				end walkEnd
				err error
			)
			if c, end, err = g.fetchContent(ctx, c.ID, token, password, w.room()); err != nil {
				return nil, err
			}
			w.ended(end)
		}
		var items []treeItem[*gofileContent]
		if c.Type == "file" { // a share of one file
			if c.Link != "" {
				items = append(items, treeItem[*gofileContent]{File: g.file(c, token)})
			}
			return items, nil
		}
		// Children arrive keyed by id; they are taken in the order gofile
		// listed them, so the queue matches the website.
		for _, child := range c.orderedChildren() {
			switch child.Type {
			case "file":
				if child.Link != "" {
					items = append(items, treeItem[*gofileContent]{File: g.file(child, token)})
				}
			case "folder":
				items = append(items, treeItem[*gofileContent]{
					Folder: child, Key: child.ID, Name: child.Name, Fallback: child.ID,
				})
			}
		}
		return items, nil
	}
}

// file is one listed file as a download.
func (g *Gofile) file(c *gofileContent, token string) *File {
	file := File{
		Name: cmp.Or(c.Name, util.NameFromURL(c.Link)),
		URL:  c.Link,
		Size: c.Size,
		// What the CDN needs to serve the file to us.
		Headers: httpx.Header{
			httpx.HeaderCookie:  gofileCookie + "=" + token,
			httpx.HeaderReferer: gofileRoot + "/",
		},
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
	return &file
}

var errGofileStaleToken = errors.New("gofile: account token rejected")

// fetchContent GETs one content id, following its pages and merging them,
// with why the listing ended (walkEnd), for a caller to note one that
// stopped short of its end. room is how
// many files the caller can still take: once a page brings the folder's
// files past it the rest are not read, since none of them would fit, and
// the tree walk says the file limit cut the share.
func (g *Gofile) fetchContent(ctx context.Context, id, token, password string, room int) (*gofileContent, walkEnd, error) {
	secret, err := g.ensureSecret(ctx)
	if err != nil {
		return nil, walkEnd{}, err
	}

	var (
		head  *gofileContent
		files = make(map[string]bool)
		stale error
	)
	walk, err := walkPagesFor(ctx, pageWalk[*gofileContent]{
		key:   func(c *gofileContent) string { return c.ID },
		limit: -1,
		fetch: func(ctx context.Context, n int) (pageOf[*gofileContent], error) {
			q := url.Values{
				"page":          {strconv.Itoa(n + 1)},
				"pageSize":      {strconv.Itoa(config.FolderPageSize)},
				"sortField":     {"createTime"},
				"sortDirection": {"-1"},
			}
			if password != "" {
				sum := sha256.Sum256([]byte(password))
				q.Set("password", hex.EncodeToString(sum[:]))
			}
			endpoint := g.api + "/contents/" + url.PathEscape(id) + "?" + q.Encode()

			var env gofileEnvelope
			err := g.client.GetJSON(ctx, endpoint, httpx.Header{
				httpx.HeaderAuthorization: "Bearer " + token,
				gofileHeaderWebsiteToken:  g.websiteToken(token, secret, time.Now()),
				gofileHeaderLanguage:      g.lang,
				httpx.HeaderOrigin:        gofileRoot,
				httpx.HeaderReferer:       gofileRoot + "/",
			}, &env)
			if err == nil && env.Status != "ok" {
				err = g.statusError(env.Status)
			} else if err != nil {
				err = g.explain(err)
			}
			if err != nil {
				if n > 0 && errors.Is(err, errGofileStaleToken) {
					// A token gone stale between pages is no gap in the
					// listing: the whole content is asked for again with a
					// renewed one (Extract), rather than kept as a part.
					stale = err
				}
				return pageOf[*gofileContent]{}, err
			}
			if env.Data.CanAccess != nil && !*env.Data.CanAccess {
				return pageOf[*gofileContent]{}, env.Data.accessError()
			}
			if head == nil {
				head = &env.Data
			}
			// The API says which page is the last; a page without
			// children ends the listing too, whatever its metadata says.
			p := pageOf[*gofileContent]{items: env.Data.orderedChildren(), next: env.Metadata.HasNextPage}
			p.end = !p.next || len(p.items) == 0
			for _, c := range p.items {
				if c.Type == "file" && c.Link != "" {
					files[c.ID] = true
				}
			}
			if len(files) > room {
				p.end = true
			}
			return p, nil
		},
	})
	if err == nil && stale != nil {
		err = stale
	}
	if err != nil {
		return nil, walkEnd{}, err
	}
	children := walk.items
	merged := *head
	merged.Children = make(map[string]gofileContent, len(children))
	merged.ChildrenIDs = make([]string, 0, len(children))
	for _, c := range children {
		merged.Children[c.ID] = *c
		merged.ChildrenIDs = append(merged.ChildrenIDs, c.ID)
	}
	return &merged, walk.walkEnd, nil
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

// mintAccount creates a guest account, for g.account.
func (g *Gofile) mintAccount(ctx context.Context) (string, time.Time, error) {
	var env struct {
		Status string `json:"status"`
		Data   struct {
			Token string `json:"token"`
		} `json:"data"`
	}
	err := g.client.PostJSON(ctx, gofileAPI+"/accounts",
		httpx.SiteOrigin(gofileRoot), nil, &env)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("gofile: create guest account: %w", err)
	}
	if env.Status != "ok" || env.Data.Token == "" {
		return "", time.Time{}, fmt.Errorf("gofile: create guest account: unexpected status %q", env.Status)
	}
	return env.Data.Token, time.Now().Add(config.GofileTokenTTL), nil
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
	Servers        []string                 `json:"servers"`
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
