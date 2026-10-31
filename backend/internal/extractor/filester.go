// SPDX-License-Identifier: MIT

package extractor

import (
	"cmp"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
	"github.com/JohanLindvall/HeapLeach/internal/util"
	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
	"golang.org/x/net/publicsuffix"
)

// filester resolves single-file shares (/d/<slug>) and folders (/f/<id>).
//
// The page itself carries nothing worth parsing: its script asks
// /v2/api/public/download for a token, and the answer names everything —
// the storage server, the stored file, the original name and the token,
// which lasts half an hour. So the link is minted through Resolve at
// download time, and minted once up front only so a dead link fails the job
// at once and the queue shows the real name. The storage server takes no
// referer or cookie and honours ranges.
//
// Folder pages list filenames and exact sizes. Protected folders grant an
// access cookie, also needed when signing their files; see the folder half below.
//
// The site moves between top-level domains, each answering the same API,
// which is why it is known by its pages — the download script on a file
// page, the cards or the password form on a folder — and never by a name.

// filesterTokenPath is the anonymous token endpoint, relative to whichever
// origin the link named.
const filesterTokenPath = "/v2/api/public/download"

// NewFilester builds the filester extractor.
func NewFilester(client *httpx.Client) *Share { return newShare(client, &filesterSite) }

// filesterFileShape is a file share, /d/<slug>; filesterFolderShape a
// folder, /f/<id>.
var (
	filesterFileShape   = pathShape(`^/d/[A-Za-z0-9_-]+/?$`)
	filesterFolderShape = pathShape(`^/f/[A-Za-z0-9_-]+/?$`)
)

// filesterDownloadScript is the file page's script, which asks the token
// endpoint for the link.
const filesterDownloadScript = "/js/file_dl.js"

// filesterSite takes a file page by the script that signs its download,
// which extract does in its place, and a folder by the file cards the walk
// reads or the password form it fills in.
var filesterSite = shareSite{
	name: "filester",
	recognises: "file shares (`/d/<slug>`) loading `file_dl.js` beside a `#downloadButton`, and folders " +
		"(`/f/<id>`) listing file cards or asking for their password",
	forms: []shareForm{
		{filesterFileShape, func(p *Page) bool {
			return p.Script(filesterDownloadScript) && p.hasElement(func(n *html.Node) bool {
				return attr(n, "id") == "downloadButton"
			})
		}},
		{filesterFolderShape, func(p *Page) bool {
			root := p.HTML()
			return root != nil && (filesterCardLinkRE.MatchString(p.Body) || filesterPasswordForm(root) != nil)
		}},
	},
	extract: filesterExtract,
}

// filesterExtract lists a folder or produces a single lazily-signed entry.
func filesterExtract(ctx context.Context, s *Share, u *url.URL, opts Options) (*Result, error) {
	segs := util.PathSegments(u)
	if len(segs) < 2 || (segs[0] != "d" && segs[0] != "f") {
		return nil, fmt.Errorf("filester: %s is not a file (/d/<id>) or folder (/f/<id>) link", u.Redacted())
	}
	if segs[0] == "f" {
		return filesterFolder(ctx, s, u, opts)
	}
	slug, base := segs[1], util.Origin(u)
	return signedResult(ctx, func(ctx context.Context) (*Target, error) { return filesterSign(ctx, s, base, slug) })
}

// filesterSign asks for a fresh token and builds the storage link from it,
// the way the page's own script does.
func filesterSign(ctx context.Context, s *Share, base, slug string) (*Target, error) {
	var out struct {
		Success bool   `json:"success"`
		Server  string `json:"server"`
		File    string `json:"file"`
		Name    string `json:"name"`
		Token   string `json:"token"`
		Message string `json:"message"`
	}
	in := map[string]string{"file_slug": slug}
	if err := s.client.PostJSON(ctx, base+filesterTokenPath,
		httpx.RefererOrigin(base+"/d/"+slug, base), in, &out); err != nil {
		return nil, fmt.Errorf("filester: sign %s: %w", slug, err)
	}
	if !out.Success || out.Server == "" || out.File == "" || out.Token == "" {
		return nil, fmt.Errorf("filester: sign %s: %s", slug, cmp.Or(out.Message, "no link returned"))
	}
	return &Target{
		URL:  filesterMediaURL(out.Server, out.File, out.Token),
		Name: cmp.Or(out.Name, out.File, slug),
		Size: -1,
	}, nil
}

// filesterMediaURL builds the storage link. The page adds download=true and
// the name for its own button; the file itself does not need them.
func filesterMediaURL(server, file, token string) string {
	return server + "/v2/" + url.PathEscape(file) + "?token=" + url.QueryEscape(token)
}

// ---------------------------------------------------------------- folders

var filesterCardLinkRE = regexp.MustCompile(`(?:window\.)?location\.href\s*=\s*['"](/d/[A-Za-z0-9_-]+)['"]`)

// The host uses one folder_access_token cookie for all folders. Each folder
// needs its own jar, retained by its files, or another job's unlock would
// replace the credentials needed to sign this one's downloads.
type filesterSession struct {
	s        *Share
	u        *url.URL
	password string
	jar      http.CookieJar
}

func (s *filesterSession) page(ctx context.Context, u *url.URL) (*html.Node, error) {
	ctx = httpx.WithCookieJar(ctx, s.jar)
	doc, final, err := s.s.client.GetPage(ctx, u.String(), httpx.Referer(s.u.String()), nil)
	if err != nil {
		return nil, err
	}
	if !filesterSameFolder(u, final) {
		return nil, fmt.Errorf("filester: folder redirected to another page")
	}
	root, err := parseHTML(doc)
	if err != nil {
		return nil, err
	}
	form := filesterPasswordForm(root)
	if form == nil {
		return root, nil
	}
	if s.password == "" {
		return nil, ErrPasswordRequired
	}
	nonce := findFirst(form, func(n *html.Node) bool {
		return isElem(n, atom.Input) && attr(n, "name") == "nonce"
	})
	if nonce == nil || attr(nonce, "value") == "" {
		return nil, fmt.Errorf("filester: password form has no nonce")
	}
	// This is the page's UTF-8/base64 envelope, not a password hash.
	payload := fmt.Sprintf("%s|%d|%s", s.password, time.Now().UnixMilli(), attr(nonce, "value"))
	values := url.Values{
		"nonce":    {attr(nonce, "value")},
		"password": {base64.StdEncoding.EncodeToString([]byte(payload))},
	}
	req, err := s.s.client.NewRequest(ctx, http.MethodPost, final.String(), []byte(values.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set(httpx.HeaderContentType, httpx.ContentTypeForm)
	httpx.RefererOrigin(final.String(), util.Origin(final)).Apply(req)
	body, err := s.s.client.Bytes(req)
	if err != nil {
		return nil, err
	}
	root, err = parseHTML(string(body))
	if err != nil {
		return nil, err
	}
	if filesterPasswordForm(root) != nil {
		return nil, fmt.Errorf("filester: password was not accepted: %w", ErrPasswordRequired)
	}
	return root, nil
}

func filesterPasswordForm(root *html.Node) *html.Node {
	return findFirst(root, func(n *html.Node) bool {
		return isElem(n, atom.Form) && attr(n, "id") == "password-form"
	})
}

func (s *filesterSession) resolve(ctx context.Context, slug string) (*Target, error) {
	ctx = httpx.WithCookieJar(ctx, s.jar)
	target, err := filesterSign(ctx, s.s, util.Origin(s.u), slug)
	if !httpx.HasStatus(err, http.StatusForbidden) {
		return target, err
	}
	// A long queue can outlive the folder cookie as well as the media token.
	// Revisit the gate on this attempt's route, then sign once more.
	if _, err := s.page(ctx, s.u); err != nil {
		return nil, err
	}
	return filesterSign(ctx, s.s, util.Origin(s.u), slug)
}

// filesterFolder walks a folder share and its subfolders.
func filesterFolder(ctx context.Context, s *Share, u *url.URL, opts Options) (*Result, error) {
	first := filesterFolderURL(u)
	res := &Result{}
	l := &filesterLister{s: s, opts: opts}
	if err := walkTree(ctx, opts, res, filesterFolderKey(first), first, l.list); err != nil {
		return nil, err
	}
	if len(res.Files) == 0 {
		return nil, fmt.Errorf("filester: no files found in this folder")
	}
	return res, nil
}

// filesterFolderURL is a folder's first page: its link without a fragment or
// page number.
func filesterFolderURL(u *url.URL) *url.URL {
	first := *u
	first.Fragment = ""
	query := first.Query()
	query.Del("page")
	first.RawQuery = query.Encode()
	return &first
}

// filesterFolderKey identifies a folder however it was linked.
func filesterFolderKey(u *url.URL) string {
	return util.Origin(u) + strings.TrimRight(u.Path, "/")
}

// filesterLister reads folders for walkTree, a page at a time. The page
// budget is the whole walk's, so it lives here rather than in one folder.
type filesterLister struct {
	s     *Share
	opts  Options
	pages int
}

func (l *filesterLister) list(ctx context.Context, w *treeWalk, first *url.URL, depth int) ([]treeItem[*url.URL], error) {
	budget := config.MaxAlbumPages - l.pages
	if budget <= 0 {
		w.note("page limit")
		return nil, nil
	}
	jar, _ := cookiejar.New(&cookiejar.Options{PublicSuffixList: publicsuffix.List})
	session := &filesterSession{s: l.s, u: first, password: l.opts.Password, jar: jar}
	room := w.room()
	page := first
	seenFiles := make(map[string]bool)
	var (
		subfolders []*html.Node
		locked     error
	)
	walk, err := walkPagesFor(ctx, pageWalk[filesterCard]{
		key:   func(c filesterCard) string { return c.slug },
		limit: room + 1,
		pages: budget,
		fetch: func(ctx context.Context, n int) (pageOf[filesterCard], error) {
			ResolveNote(ctx, fmt.Sprintf("Reading folder page %d", n+1))
			l.pages++
			root, err := session.page(ctx, page)
			if err != nil {
				return pageOf[filesterCard]{}, err
			}
			grid := findFirst(root, func(n *html.Node) bool { return attr(n, "id") == "filesGrid" })
			folders := findFirst(root, func(n *html.Node) bool { return attr(n, "id") == "subfoldersGrid" })
			if grid == nil && folders == nil {
				if n == 0 {
					return pageOf[filesterCard]{}, errFilesterNoListing
				}
				return pageOf[filesterCard]{stop: "partial — folder listing missing from a page"}, nil
			}
			if n == 0 {
				if depth == 0 {
					w.res.Title = cmp.Or(firstText(root, atom.H1), util.PathSegments(first)[1])
				}
				if folders != nil {
					subfolders = findAll(folders, func(n *html.Node) bool {
						return isElem(n, atom.A) && hasClass(n, "subfolder-item")
					})
				}
			}
			var p pageOf[filesterCard]
			if grid != nil {
				for _, card := range findAll(grid, func(n *html.Node) bool { return hasClass(n, "file-item") }) {
					c, ok := filesterCardOf(session, first, card)
					if !ok {
						w.note("unavailable files")
						continue
					}
					seenFiles[c.slug] = true
					p.items = append(p.items, c)
				}
			}
			next, invalid := filesterNextPage(root, page, n+1)
			if invalid {
				p.note = invalidNextNote
			}
			// A first page without a usable next link is the whole folder,
			// and an empty one is a folder of only subfolders, not a repeat.
			p.next, p.end = next != nil, n == 0 && next == nil
			if p.next && len(seenFiles) >= room {
				// Files enough to fill the walk, and more pages: none of
				// theirs could be taken, so the walk says the limit cut it.
				p.next, p.note = false, fileLimitNote
			}
			page = next
			return p, nil
		},
		failed: func(err error) (string, bool) {
			if errors.Is(err, ErrPasswordRequired) {
				locked = err
				return "", true
			}
			return "partial — could not read a folder page", true
		},
	})
	switch {
	case err != nil && ctx.Err() != nil:
		return nil, err
	case errors.Is(err, ErrPasswordRequired):
		return nil, treeStop{err}
	case locked != nil:
		return nil, treeStop{locked}
	case err != nil && depth == 0:
		return nil, err
	case errors.Is(err, errFilesterNoListing):
		w.note("folder listing missing from a page")
		return nil, nil
	case err != nil:
		w.note("could not read a folder page")
		return nil, nil
	}
	w.ended(walk.walkEnd)
	cards := walk.items
	items := make([]treeItem[*url.URL], 0, len(cards)+len(subfolders))
	for _, c := range cards {
		items = append(items, treeItem[*url.URL]{File: c.file})
	}
	if len(cards) > room {
		// One more than fits proves the folder holds more: the walk keeps
		// what fits and says so, and no subfolder's files could fit.
		return items, nil
	}
	for _, folder := range subfolders {
		child := filesterChildURL(first, attr(folder, "href"), "f")
		if child == nil {
			w.note("unavailable subfolders")
			continue
		}
		child = filesterFolderURL(child)
		id := util.PathSegments(child)[1]
		items = append(items, treeItem[*url.URL]{
			Folder:   child,
			Key:      filesterFolderKey(child),
			Name:     util.Unescape(textOfFirst(folder, func(n *html.Node) bool { return hasClass(n, "subfolder-name") })),
			Fallback: id,
		})
	}
	return items, nil
}

var errFilesterNoListing = errors.New("filester: no folder listing on this page")

// filesterCard is one file card of a folder page, keyed by its slug.
type filesterCard struct {
	slug string
	file *File
}

// filesterCardOf reads a file card, signing its file only when resolved;
// false is a card whose link is not a file page on the folder's host.
func filesterCardOf(session *filesterSession, first *url.URL, card *html.Node) (filesterCard, bool) {
	link := attr(card, "href")
	if match := filesterCardLinkRE.FindStringSubmatch(attr(card, "onclick")); match != nil {
		link = match[1]
	}
	fileURL := filesterChildURL(first, link, "d")
	if fileURL == nil {
		return filesterCard{}, false
	}
	slug := util.PathSegments(fileURL)[1]
	return filesterCard{slug: slug, file: &File{
		Name: cmp.Or(attr(card, "data-name"), textOfFirst(card, func(n *html.Node) bool { return hasClass(n, "file-name") }), slug),
		Size: parsedSize(attr(card, "data-size")),
		Resolve: func(ctx context.Context) (*Target, error) {
			return session.resolve(ctx, slug)
		},
	}}, true
}

func filesterSameFolder(a, b *url.URL) bool {
	return a.Scheme == b.Scheme && strings.EqualFold(a.Host, b.Host) && strings.TrimRight(a.Path, "/") == strings.TrimRight(b.Path, "/")
}

func filesterChildURL(base *url.URL, ref, kind string) *url.URL {
	if ref == "" {
		return nil
	}
	u, err := url.Parse(resolveRef(base, ref))
	if err != nil || u.Scheme != base.Scheme || !strings.EqualFold(u.Host, base.Host) {
		return nil
	}
	segs := util.PathSegments(u)
	if len(segs) != 2 || segs[0] != kind {
		return nil
	}
	return u
}

func filesterNextPage(root *html.Node, page *url.URL, current int) (*url.URL, bool) {
	for _, a := range findAll(root, func(n *html.Node) bool { return isElem(n, atom.A) && hasClass(n, "page-link") }) {
		u, err := url.Parse(resolveRef(page, attr(a, "href")))
		if err != nil {
			continue
		}
		number, _ := strconv.Atoi(u.Query().Get("page"))
		if number <= current {
			continue
		}
		if number != current+1 || !filesterSameFolder(page, u) {
			return nil, true
		}
		return u, false
	}
	return nil, false
}
