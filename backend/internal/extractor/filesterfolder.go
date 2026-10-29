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
	"path"
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

type filesterFolderWalk struct {
	f     *Filester
	opts  Options
	res   *Result
	seen  map[string]bool
	pages int
}

func (f *Filester) folder(ctx context.Context, u *url.URL, opts Options) (*Result, error) {
	w := &filesterFolderWalk{f: f, opts: opts, res: &Result{}, seen: make(map[string]bool)}
	if err := w.collect(ctx, u, "", 0); err != nil {
		return nil, err
	}
	if len(w.res.Files) == 0 {
		return nil, fmt.Errorf("filester: no files found in this folder")
	}
	return w.res, nil
}

func (w *filesterFolderWalk) note(reason string) {
	if w.res.Note == "" {
		w.res.Note = "partial — " + reason
	} else if !strings.Contains(w.res.Note, reason) {
		w.res.Note += "; " + reason
	}
}

func (w *filesterFolderWalk) collect(ctx context.Context, u *url.URL, dir string, depth int) error {
	first := *u
	first.Fragment = ""
	query := first.Query()
	query.Del("page")
	first.RawQuery = query.Encode()
	key := util.Origin(&first) + strings.TrimRight(first.Path, "/")
	if w.seen[key] {
		return nil
	}
	if depth >= config.MaxFolderDepth || len(w.seen) >= w.opts.maxSources() {
		w.note("folder limit")
		return nil
	}
	w.seen[key] = true
	jar, _ := cookiejar.New(&cookiejar.Options{PublicSuffixList: publicsuffix.List})
	session := &filesterSession{f: w.f, u: &first, password: w.opts.Password, jar: jar}
	page := &first
	seenFiles := make(map[string]bool)
	var subfolders []*html.Node
	for index := 0; ; index++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		if w.pages >= config.MaxAlbumPages {
			w.note("page limit")
			return nil
		}
		ResolveNote(ctx, fmt.Sprintf("Reading folder page %d", index+1))
		w.pages++
		root, err := session.page(ctx, page)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if errors.Is(err, ErrPasswordRequired) || (depth == 0 && index == 0) {
				return err
			}
			w.note("could not read a folder page")
			break
		}
		grid := findFirst(root, func(n *html.Node) bool { return attr(n, "id") == "filesGrid" })
		folders := findFirst(root, func(n *html.Node) bool { return attr(n, "id") == "subfoldersGrid" })
		if grid == nil && folders == nil {
			if depth == 0 && index == 0 {
				return fmt.Errorf("filester: no folder listing on this page")
			}
			w.note("folder listing missing from a page")
			break
		}
		if index == 0 {
			if depth == 0 {
				w.res.Title = util.FirstNonEmpty(firstText(root, atom.H1), util.PathSegments(u)[1])
			}
			if folders != nil {
				subfolders = findAll(folders, func(n *html.Node) bool {
					return isElem(n, atom.A) && hasClass(n, "subfolder-item")
				})
			}
		}
		before := len(w.res.Files)
		if grid != nil {
			for _, card := range findAll(grid, func(n *html.Node) bool { return hasClass(n, "file-item") }) {
				link := attr(card, "href")
				if match := filesterCardLinkRE.FindStringSubmatch(attr(card, "onclick")); match != nil {
					link = match[1]
				}
				fileURL := filesterChildURL(&first, link, "d")
				if fileURL == nil {
					w.note("unavailable files")
					continue
				}
				slug := util.PathSegments(fileURL)[1]
				if seenFiles[slug] {
					continue
				}
				seenFiles[slug] = true
				if len(w.res.Files) == w.opts.maxFiles() {
					w.note("file limit")
					return nil
				}
				size, err := strconv.ParseInt(attr(card, "data-size"), 10, 64)
				if err != nil || size < 0 {
					size = -1
				}
				w.res.Files = append(w.res.Files, File{
					Name: util.FirstNonEmpty(attr(card, "data-name"), textOfFirst(card, func(n *html.Node) bool { return hasClass(n, "file-name") }), slug),
					Size: size,
					Dir:  dir,
					Resolve: func(ctx context.Context) (*Target, error) {
						return session.resolve(ctx, slug)
					},
				})
			}
		}
		next, invalid := filesterNextPage(root, page, index+1)
		if invalid {
			w.note("invalid next page")
		}
		if index > 0 && len(w.res.Files) == before {
			w.note("repeated or empty page")
			break
		}
		if next == nil {
			break
		}
		if len(w.res.Files) == w.opts.maxFiles() {
			w.note("file limit")
			return nil
		}
		page = next
	}
	for _, folder := range subfolders {
		child := filesterChildURL(&first, attr(folder, "href"), "f")
		if child == nil {
			w.note("unavailable subfolders")
			continue
		}
		if w.seen[util.Origin(child)+strings.TrimRight(child.Path, "/")] {
			continue
		}
		if len(w.res.Files) == w.opts.maxFiles() {
			w.note("file limit")
			break
		}
		name := util.FirstNonEmpty(textOfFirst(folder, func(n *html.Node) bool { return hasClass(n, "subfolder-name") }), util.PathSegments(child)[1])
		name = sourceFolder(util.Unescape(name))
		if name == "" || name == "." || name == ".." {
			name = util.PathSegments(child)[1]
		}
		if err := w.collect(ctx, child, path.Join(dir, name), depth+1); err != nil {
			return err
		}
	}
	return ctx.Err()
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
