// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
	"github.com/JohanLindvall/HeapLeach/internal/util"
	"golang.org/x/net/html"
)

// Emload reads the free download control on a file share. The page carries
// a storage server and a per-visit token; read both on the worker's route.
// Restricted pages can carry those attributes too, so the token alone does
// not establish that the site offers a free download.
type Emload struct {
	client *httpx.Client
}

func NewEmload(client *httpx.Client) *Emload {
	return &Emload{client: client.WithTimeout(config.EmloadRequestTimeout)}
}

func (e *Emload) Name() string { return "emload" }

// emloadShape is a file share, /v2/file/<id>[/<name>] or the older
// /file/<id>[/<name>].
var emloadShape = pathShape(`^(/v2)?/file/[A-Za-z0-9_=+%-]+(/[^/]*)?/?$`)

// Recognises is the inventory's description.
func (e *Emload) Recognises() string {
	return "file shares (`/v2/file/<id>/<name>`) whose page carries the `dl-file` download panel, " +
		"or its removed-file or password notice"
}

// Shape reports a file share.
func (e *Emload) Shape(u *url.URL) bool { return emloadShape(u) }

// Recognise takes a share page by the panel the resolver reads, the
// dl-file block holding the name and the free-download control, or by the
// notices the resolver reports in its place.
func (e *Emload) Recognise(_ context.Context, p *Page) bool {
	if !emloadShape(p.Final) {
		return false
	}
	root := p.HTML()
	return root != nil && findFirst(root, func(n *html.Node) bool {
		return hasClass(n, "dl-file") || hasClass(n, "nofile") || hasClass(n, "_filepassw")
	}) != nil
}

func (e *Emload) Extract(_ context.Context, u *url.URL, _ Options) (*Result, error) {
	parts := util.PathSegments(u)
	if len(parts) > 0 && parts[0] == "v2" {
		parts = parts[1:]
	}
	if len(parts) < 2 || len(parts) > 3 || parts[0] != "file" {
		return nil, fmt.Errorf("emload: expected a file share (/v2/file/<id> or /file/<id>)")
	}
	page := *u
	page.Fragment = ""
	// Discovery must not visit the page before the dispatcher leases a
	// route. The pasted filename is a hint; Resolve supplies the real name
	// and the transfer response supplies an exact size.
	name := parts[len(parts)-1]
	return &Result{Title: name, Files: []File{{
		Name: name, Size: -1,
		Pace: &Pace{Group: e.Name(), Files: 1, Streams: 1, PerRoute: true},
		Resolve: func(ctx context.Context) (*Target, error) {
			fresh, err := e.page(ctx, page.String())
			if err != nil {
				return nil, err
			}
			if fresh.limited {
				return nil, &WaitError{Until: time.Now().Add(config.EmloadDailyWait), Reason: "emload: daily free-download limit reached"}
			}
			// The page's rounded display size is not an exact target size.
			return &Target{URL: fresh.link, Name: fresh.name, Size: -1, Headers: httpx.Referer(fresh.referer)}, nil
		},
	}}}, nil
}

type emloadPage struct {
	name, link, referer string
	limited             bool
}

func (e *Emload) page(ctx context.Context, raw string) (*emloadPage, error) {
	doc, final, err := e.client.GetPage(ctx, raw, nil, nil)
	if err != nil {
		return nil, fmt.Errorf("emload: fetch share: %w", err)
	}
	root, err := parseHTML(doc)
	if err != nil {
		return nil, err
	}
	if findFirst(root, func(n *html.Node) bool { return hasClass(n, "nofile") }) != nil {
		return nil, fmt.Errorf("emload: this file is no longer available")
	}
	if findFirst(root, func(n *html.Node) bool { return hasClass(n, "_filepassw") }) != nil {
		return nil, fmt.Errorf("emload: password-protected files are not supported")
	}
	if findFirst(root, func(n *html.Node) bool {
		return hasClass(n, "maintenance") || (hasClass(n, "download") && hasClass(n, "sv4"))
	}) != nil {
		return nil, fmt.Errorf("emload: download server is down for maintenance")
	}
	file := findFirst(root, func(n *html.Node) bool { return hasClass(n, "dl-file") })
	if file == nil {
		return nil, fmt.Errorf("emload: no file download on this page")
	}
	name := textOfFirst(file, func(n *html.Node) bool { return hasClass(n, "filename") })
	if name == "" {
		parts := util.PathSegments(final)
		if len(parts) > 0 {
			name = parts[len(parts)-1]
		}
	}
	if name == "" {
		return nil, fmt.Errorf("emload: missing filename")
	}
	info := &emloadPage{name: name, referer: final.String()}
	download := findFirst(file, func(n *html.Node) bool { return hasClass(n, "free-downloader") })
	server, token := "", ""
	if download != nil {
		server, token = strings.TrimSpace(attr(download, "data-srv")), strings.TrimSpace(attr(download, "data-dl"))
	}
	// These are states, not storage hostnames. In particular, a small file
	// may still be premium-only; the free-size limit is a separate check.
	switch server {
	case "dlsize":
		return nil, fmt.Errorf("emload: this file exceeds the free-download size limit and requires a Premium account")
	case "dlreached":
		// Resolve returns a WaitError so the pool cools this route and
		// another address may take the file without holding a worker.
		info.limited = true
		return info, nil
	case "serverblocked":
		return nil, fmt.Errorf("emload: download server is down for maintenance")
	}
	button := findFirst(file, func(n *html.Node) bool {
		return hasClass(n, "init-free-download") && !hasAttr(n, "disabled") && attr(n, "aria-disabled") != "true"
	})
	if button == nil {
		message := strings.ToLower(textOf(file))
		if strings.Contains(message, "this link is for premium only") || strings.Contains(message, "this link only for premium") {
			return nil, fmt.Errorf("emload: this file requires a Premium account; the site offers no free download")
		}
		return nil, fmt.Errorf("emload: no free download button (the file may be restricted or unavailable)")
	}
	// decodeURIComponent preserves a literal '+', unlike form decoding.
	server, err = url.PathUnescape(server)
	if err != nil {
		return nil, fmt.Errorf("emload: invalid download server")
	}
	base, err := url.Parse(server)
	if err != nil || (base.Scheme != "https" && base.Scheme != "http") || base.Host == "" ||
		base.User != nil || base.RawQuery != "" || base.Fragment != "" || token == "" {
		return nil, fmt.Errorf("emload: missing or invalid free download token/server")
	}
	base.Path = strings.TrimRight(base.Path, "/") + "/download.php"
	base.RawPath = ""
	base.RawQuery = url.Values{"accesstoken": {token}}.Encode()
	info.link = base.String()
	return info, nil
}
