// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"fmt"
	"net/url"

	"github.com/JohanLindvall/HeapLeach/internal/httpx"
	"github.com/JohanLindvall/HeapLeach/internal/util"
	"golang.org/x/net/html"
)

// Filester resolves single-file shares (/d/<slug>) and folders (/f/<id>).
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
// access cookie, also needed when signing their files; see filesterfolder.go.
//
// The site moves between top-level domains, each answering the same API,
// which is why it is known by its pages — the download script on a file
// page, the cards or the password form on a folder — and never by a name.
type Filester struct {
	client *httpx.Client
}

// filesterTokenPath is the anonymous token endpoint, relative to whichever
// origin the link named.
const filesterTokenPath = "/v2/api/public/download"

// NewFilester builds the filester extractor.
func NewFilester(client *httpx.Client) *Filester {
	return &Filester{client: client}
}

// filesterFileShape is a file share, /d/<slug>; filesterFolderShape a
// folder, /f/<id>.
var (
	filesterFileShape   = pathShape(`^/d/[A-Za-z0-9_-]+/?$`)
	filesterFolderShape = pathShape(`^/f/[A-Za-z0-9_-]+/?$`)
)

// filesterDownloadScript is the file page's script, which asks the token
// endpoint for the link.
const filesterDownloadScript = "/js/file_dl.js"

// Recognises is the inventory's description.
func (f *Filester) Recognises() string {
	return "file shares (`/d/<slug>`) loading `file_dl.js` beside a `#downloadButton`, and folders " +
		"(`/f/<id>`) listing file cards or asking for their password"
}

// Shape reports a file share or a folder.
func (f *Filester) Shape(u *url.URL) bool { return filesterFileShape(u) || filesterFolderShape(u) }

// Recognise takes a file page by the script that signs its download, which
// Extract does in its place, and a folder by the file cards the walk reads
// or the password form it fills in.
func (f *Filester) Recognise(_ context.Context, p *Page) bool {
	root := p.HTML()
	if root == nil {
		return false
	}
	switch {
	case filesterFileShape(p.Final):
		return p.Script(filesterDownloadScript) && findFirst(root, func(n *html.Node) bool {
			return attr(n, "id") == "downloadButton"
		}) != nil
	case filesterFolderShape(p.Final):
		return filesterCardLinkRE.MatchString(p.Body) || filesterPasswordForm(root) != nil
	}
	return false
}

func (f *Filester) Name() string { return "filester" }

// Extract lists a folder or produces a single lazily-signed entry.
func (f *Filester) Extract(ctx context.Context, u *url.URL, opts Options) (*Result, error) {
	segs := util.PathSegments(u)
	if len(segs) < 2 || (segs[0] != "d" && segs[0] != "f") {
		return nil, fmt.Errorf("filester: %s is not a file (/d/<id>) or folder (/f/<id>) link", u.Redacted())
	}
	if segs[0] == "f" {
		return f.folder(ctx, u, opts)
	}
	slug, base := segs[1], util.Origin(u)

	signed, err := f.sign(ctx, base, slug)
	if err != nil {
		return nil, err
	}
	return &Result{
		Title: signed.Name,
		Files: []File{{
			Name:    signed.Name,
			URL:     signed.URL,
			Size:    -1,
			Resolve: func(ctx context.Context) (*Target, error) { return f.sign(ctx, base, slug) },
		}},
	}, nil
}

// sign asks for a fresh token and builds the storage link from it, the way
// the page's own script does.
func (f *Filester) sign(ctx context.Context, base, slug string) (*Target, error) {
	var out struct {
		Success bool   `json:"success"`
		Server  string `json:"server"`
		File    string `json:"file"`
		Name    string `json:"name"`
		Token   string `json:"token"`
		Message string `json:"message"`
	}
	in := map[string]string{"file_slug": slug}
	if err := f.client.PostJSON(ctx, base+filesterTokenPath,
		httpx.RefererOrigin(base+"/d/"+slug, base), in, &out); err != nil {
		return nil, fmt.Errorf("filester: sign %s: %w", slug, err)
	}
	if !out.Success || out.Server == "" || out.File == "" || out.Token == "" {
		return nil, fmt.Errorf("filester: sign %s: %s", slug, util.FirstNonEmpty(out.Message, "no link returned"))
	}
	return &Target{
		URL:  filesterMediaURL(out.Server, out.File, out.Token),
		Name: util.FirstNonEmpty(out.Name, out.File, slug),
		Size: -1,
	}, nil
}

// filesterMediaURL builds the storage link. The page adds download=true and
// the name for its own button; the file itself does not need them.
func filesterMediaURL(server, file, token string) string {
	return server + "/v2/" + url.PathEscape(file) + "?token=" + url.QueryEscape(token)
}
