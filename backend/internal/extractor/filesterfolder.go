// SPDX-License-Identifier: MIT

package extractor

import (
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

var filesterCardLinkRE = regexp.MustCompile(`(?:window\.)?location\.href\s*=\s*['"](/d/[A-Za-z0-9_-]+)['"]`)

// The host uses one folder_access_token cookie for all folders. Each folder
// needs its own jar, retained by its files, or another job's unlock would
// replace the credentials needed to sign this one's downloads.
type filesterSession struct {
	f        *Filester
	u        *url.URL
	password string
	jar      http.CookieJar
}

func (s *filesterSession) page(ctx context.Context, u *url.URL) (*html.Node, error) {
	ctx = httpx.WithCookieJar(ctx, s.jar)
	doc, final, err := s.f.client.GetPage(ctx, u.String(), httpx.Referer(s.u.String()), nil)
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
	req, err := s.f.client.NewRequest(ctx, http.MethodPost, final.String(), []byte(values.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set(httpx.HeaderContentType, httpx.ContentTypeForm)
	for key, value := range httpx.RefererOrigin(final.String(), util.Origin(final)) {
		req.Header.Set(key, value)
	}
	body, err := s.f.client.Bytes(req)
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
	target, err := s.f.sign(ctx, util.Origin(s.u), slug)
	if !httpx.HasStatus(err, http.StatusForbidden) {
		return target, err
	}
	// A long queue can outlive the folder cookie as well as the media token.
	// Revisit the gate on this attempt's route, then sign once more.
	if _, err := s.page(ctx, s.u); err != nil {
		return nil, err
	}
	return s.f.sign(ctx, util.Origin(s.u), slug)
}

func (f *Filester) folder(ctx context.Context, u *url.URL, opts Options) (*Result, error) {
	first := filesterFolderURL(u)
	res := &Result{}
	l := &filesterLister{f: f, opts: opts}
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
	f     *Filester
	opts  Options
	pages int
}

func (l *filesterLister) list(ctx context.Context, w *treeWalk, first *url.URL, depth int) ([]treeItem[*url.URL], error) {
	jar, _ := cookiejar.New(&cookiejar.Options{PublicSuffixList: publicsuffix.List})
	session := &filesterSession{f: l.f, u: first, password: l.opts.Password, jar: jar}
	page := first
	seenFiles := make(map[string]bool)
	var (
		items      []treeItem[*url.URL]
		subfolders []*html.Node
	)
	for index := 0; ; index++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if l.pages >= config.MaxAlbumPages {
			w.note("page limit")
			break
		}
		ResolveNote(ctx, fmt.Sprintf("Reading folder page %d", index+1))
		l.pages++
		root, err := session.page(ctx, page)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if errors.Is(err, ErrPasswordRequired) {
				return nil, treeStop{err}
			}
			if depth == 0 && index == 0 {
				return nil, err
			}
			w.note("could not read a folder page")
			break
		}
		grid := findFirst(root, func(n *html.Node) bool { return attr(n, "id") == "filesGrid" })
		folders := findFirst(root, func(n *html.Node) bool { return attr(n, "id") == "subfoldersGrid" })
		if grid == nil && folders == nil {
			if depth == 0 && index == 0 {
				return nil, fmt.Errorf("filester: no folder listing on this page")
			}
			w.note("folder listing missing from a page")
			break
		}
		if index == 0 {
			if depth == 0 {
				w.res.Title = util.FirstNonEmpty(firstText(root, atom.H1), util.PathSegments(first)[1])
			}
			if folders != nil {
				subfolders = findAll(folders, func(n *html.Node) bool {
					return isElem(n, atom.A) && hasClass(n, "subfolder-item")
				})
			}
		}
		before := len(items)
		if grid != nil {
			for _, card := range findAll(grid, func(n *html.Node) bool { return hasClass(n, "file-item") }) {
				link := attr(card, "href")
				if match := filesterCardLinkRE.FindStringSubmatch(attr(card, "onclick")); match != nil {
					link = match[1]
				}
				fileURL := filesterChildURL(first, link, "d")
				if fileURL == nil {
					w.note("unavailable files")
					continue
				}
				slug := util.PathSegments(fileURL)[1]
				if seenFiles[slug] {
					continue
				}
				seenFiles[slug] = true
				items = append(items, treeItem[*url.URL]{File: &File{
					Name: util.FirstNonEmpty(attr(card, "data-name"), textOfFirst(card, func(n *html.Node) bool { return hasClass(n, "file-name") }), slug),
					Size: parsedSize(attr(card, "data-size")),
					Resolve: func(ctx context.Context) (*Target, error) {
						return session.resolve(ctx, slug)
					},
				}})
				if len(items) > w.room() {
					// One more than fits proves the folder holds more: the
					// walk keeps what fits and says so.
					return items, nil
				}
			}
		}
		next, invalid := filesterNextPage(root, page, index+1)
		if invalid {
			w.note("invalid next page")
		}
		if index > 0 && len(items) == before {
			w.note("repeated or empty page")
			break
		}
		if next == nil {
			break
		}
		if len(items) >= w.room() {
			w.note("file limit")
			return items, nil
		}
		page = next
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
