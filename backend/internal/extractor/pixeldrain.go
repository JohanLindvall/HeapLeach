// SPDX-License-Identifier: MIT

package extractor

import (
	"cmp"
	"context"
	"fmt"
	"net/url"
	"strings"

	"github.com/JohanLindvall/HeapLeach/internal/httpx"
	"github.com/JohanLindvall/HeapLeach/internal/util"
)

// Pixeldrain resolves lists (/l/<id>), single files (/u/<id>, /f/<id>) and
// shared directories (/d/<id>) through the documented public API. Links are
// stable, so everything resolves up front.
//
// nova.storage runs the same software — not something similar, the same: both
// hosts answer byte for byte, down to the JSON body of a not_found. So there
// is nothing here to fork. The one thing that differs between them is which
// host the link named, and that is read off the URL at extraction time rather
// than compiled in, which is why no request or Referer below mentions a host
// at all, and why another site running this software needs nothing here: it
// is recognised by the software's viewer, or by its API answering.
//
// The label stays "pixeldrain" whichever host answered: it names the software
// both of them run, and a user who pasted a nova.storage link is better
// served by an error that says which program is talking than by one that
// repeats the domain back at them.
type Pixeldrain struct {
	about
	client *httpx.Client
}

// pixeldrainAPI is the API root, relative to whichever origin was asked.
const pixeldrainAPI = "/api"

// Node types in a shared filesystem.
const (
	pixeldrainDirNode  = "dir"
	pixeldrainFileNode = "file"
)

// pixeldrainPace asks for one connection per file, because this host counts
// connections rather than files.
//
// An anonymous caller is allowed only so many open downloads at once, and
// the API says so outright — 403 with "max_concurrent_downloads" where the
// bytes would be. What matters is that the budget is spent per *connection*:
// the segmented engine's eight-way split turns one modest download into
// eight of that allowance, so a single file can exhaust it on its own and a
// queue of four is refused before it starts.
//
// One connection each is not the loss it would be elsewhere. Free downloads
// here are speed-capped per account rather than per connection, so splitting
// buys nothing anyway, and what it costs is the whole queue: at one
// connection per file the number of connections is the number of transfers,
// which is a figure the user set and can see.
//
// The number the host actually allows is not published, so nothing here
// guesses at it. Files is deliberately left alone: the downloader learns the
// rest by being refused — see refusedForConnectionCount — rather than by a
// constant here that would be wrong for somebody's paid account.
var pixeldrainPace = Pace{Streams: 1}

// NewPixeldrain builds the pixeldrain extractor.
func NewPixeldrain(client *httpx.Client) *Pixeldrain {
	return &Pixeldrain{client: client, about: about{name: "pixeldrain", shape: pixeldrainLinkShape,
		recognises: "pixeldrain files, lists and shared directories (`/u/<id>`, `/l/<id>`, `/d/<id>`) " +
			"served by the software's viewer, which is handed its API root inline, or whose origin's " +
			"API describes exactly that file, list or directory"}}
}

// pixeldrainShape is a file (/u/<id>, /f/<id>), a list (/l/<id>) or a
// shared filesystem path (/d/<id>[/...]).
var pixeldrainShape = pathShape(`^/[uf]/[A-Za-z0-9]+/?$`, `^/l/[A-Za-z0-9]+/?$`, `^/d/[^/]+(/.*)?$`)

// pixeldrainLinkShape is the narrower form a link alone is followed for: the
// software's eight-character file and list ids. /f/<id> and /d/<id> are other
// hosts' forms as well, and are recognised only once fetched.
var pixeldrainLinkShape = pathShape(`^/[ul]/[A-Za-z0-9]{8}/?$`)

// The viewer's own structure: the bundle path its pages load their viewer
// from, and the API root the page hands that viewer inline.
const (
	pixeldrainViewerPath = "/res/svelte/"
	pixeldrainAPIGlobal  = "window.api_endpoint"
)

// Recognise takes a link of those forms served by the software's viewer. A
// page that cannot say is settled by the metadata the API publishes for the
// very id the link names; the short paths alone are common on unrelated
// sites, so a page that does say, and says something else, is not asked.
func (p *Pixeldrain) Recognise(ctx context.Context, page *Page) bool {
	if !pixeldrainShape(page.Final) {
		return false
	}
	if page.Script(pixeldrainViewerPath) && page.Has(pixeldrainAPIGlobal) {
		return true
	}
	return page.Silent() && pixeldrainAnswers(ctx, page, p.client)
}

// pixeldrainAnswers recognises a compatible install by its file, list or
// filesystem metadata for the id the page's URL names.
func pixeldrainAnswers(ctx context.Context, page *Page, client *httpx.Client) bool {
	segs := page.Segments()
	if len(segs) < 2 {
		return false
	}
	origin := util.Origin(page.Final)
	validFile := func(f pixeldrainFile) bool {
		return f.ID != "" && f.Name != "" && f.MimeType != ""
	}
	switch {
	case len(segs) == 2 && (segs[0] == "u" || segs[0] == "f"):
		var f pixeldrainFile
		return pageJSON(ctx, page, client, origin+pixeldrainAPI+"/file/"+url.PathEscape(segs[1])+"/info", nil, &f) &&
			f.ID == segs[1] && validFile(f)
	case len(segs) == 2 && segs[0] == "l":
		var list pixeldrainList
		if pageJSON(ctx, page, client, origin+pixeldrainAPI+"/list/"+url.PathEscape(segs[1]), nil, &list) && list.Success {
			for _, f := range list.Files {
				if validFile(f) {
					return true
				}
			}
		}
	case segs[0] == "d":
		var stat struct {
			Path      []pixeldrainNode `json:"path"`
			BaseIndex *int             `json:"base_index"`
		}
		if pageJSON(ctx, page, client, pixeldrainFSURL(origin, segs[1:])+"?stat", nil, &stat) &&
			stat.BaseIndex != nil && *stat.BaseIndex >= 0 && *stat.BaseIndex < len(stat.Path) {
			node := stat.Path[*stat.BaseIndex]
			return node.Name != "" && (node.Type == pixeldrainDirNode || node.Type == pixeldrainFileNode)
		}
	}
	return false
}

// Extract lists a whole album, a shared directory, or a single file.
func (p *Pixeldrain) Extract(ctx context.Context, u *url.URL, opts Options) (*Result, error) {
	origin := util.Origin(u)
	segs := util.PathSegments(u)
	if len(segs) < 2 {
		return nil, fmt.Errorf("pixeldrain: no id in %s", u.Redacted())
	}

	switch segs[0] {
	case "l":
		return p.list(ctx, origin, segs[1])
	case "d":
		// A share link may point into the tree rather than at its root, and
		// everything after the id is part of the address.
		return p.shared(ctx, origin, segs[1:], opts)
	}
	return p.file(ctx, origin, segs[1])
}

// list resolves an album of files.
func (p *Pixeldrain) list(ctx context.Context, origin, id string) (*Result, error) {
	var out pixeldrainList
	endpoint := origin + pixeldrainAPI + "/list/" + url.PathEscape(id)
	if err := p.client.GetJSON(ctx, endpoint, nil, &out); err != nil {
		return nil, fmt.Errorf("pixeldrain: list %s: %w", id, err)
	}
	if !out.Success {
		return nil, fmt.Errorf("pixeldrain: list %s: %s", id, cmp.Or(out.Message, out.Value, "not available"))
	}

	files := make([]File, 0, len(out.Files))
	for _, f := range out.Files {
		files = append(files, p.toFile(origin, f))
	}
	return &Result{Title: cmp.Or(out.Title, id), Files: files}, nil
}

// file resolves a single upload.
func (p *Pixeldrain) file(ctx context.Context, origin, id string) (*Result, error) {
	var f pixeldrainFile
	endpoint := origin + pixeldrainAPI + "/file/" + url.PathEscape(id) + "/info"
	if err := p.client.GetJSON(ctx, endpoint, nil, &f); err != nil {
		return nil, fmt.Errorf("pixeldrain: file %s: %w", id, err)
	}
	if f.ID == "" {
		f.ID = id
	}
	return &Result{Title: cmp.Or(f.Name, id), Files: []File{p.toFile(origin, f)}}, nil
}

// toFile converts API metadata into a downloadable entry.
func (p *Pixeldrain) toFile(origin string, f pixeldrainFile) File {
	return File{
		Name:    cmp.Or(f.Name, f.ID),
		URL:     origin + pixeldrainAPI + "/file/" + url.PathEscape(f.ID) + "?download",
		Size:    knownSize(f.Size),
		Headers: httpx.Referer(origin + "/"),
		Pace:    &pixeldrainPace,
	}
}

// pixeldrainList is a list's metadata: its files, or why there are none.
type pixeldrainList struct {
	Success bool             `json:"success"`
	Title   string           `json:"title"`
	Files   []pixeldrainFile `json:"files"`
	Value   string           `json:"value"`
	Message string           `json:"message"`
}

// pixeldrainFile is the subset of the file metadata we need.
type pixeldrainFile struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Size     int64  `json:"size"`
	MimeType string `json:"mime_type"`
}

// ------------------------------------------------- the shared filesystem

// shared resolves a /d/<id> link.
//
// This is a different thing from a list: the id addresses a directory in
// somebody's filesystem, so what is behind it is a tree, and the tree is what
// the job should be. Walking it costs one request per directory — nothing per
// file, since a stat lists its children in full — which is why it is done
// here rather than by asking for the bulk-download archive: that arrives as
// one opaque item, cannot resume per file, and has to be unpacked afterwards.
func (p *Pixeldrain) shared(ctx context.Context, origin string, segs []string, opts Options) (*Result, error) {
	node, children, err := p.stat(ctx, origin, segs)
	if err != nil {
		return nil, err
	}

	// A share can name a single file just as well as a directory.
	if node.Type == pixeldrainFileNode {
		file, ok := p.toNodeFile(origin, segs, node)
		if !ok {
			return nil, fmt.Errorf("pixeldrain: %s cannot be downloaded (%s)",
				pixeldrainPath(segs), pixeldrainWhyNot(node))
		}
		return &Result{Title: file.Name, Files: []File{file}}, nil
	}

	res := &Result{Title: cmp.Or(node.Name, segs[0])}
	root := pixeldrainDir{segs: segs, children: children}
	if err := walkTree(ctx, opts, res, pixeldrainPath(segs), root, p.lister(origin)); err != nil {
		return nil, err
	}
	if len(res.Files) == 0 {
		return nil, fmt.Errorf("pixeldrain: %s holds no downloadable files", pixeldrainPath(segs))
	}
	return res, nil
}

// pixeldrainDir is one directory of a shared filesystem, as walkTree carries
// it: its path, and its children once they have been read.
type pixeldrainDir struct {
	segs     []string
	children []pixeldrainNode
}

// lister reads one directory for walkTree. The root's children arrive with
// it, since its stat is what said it was a directory.
//
// A child is addressed by appending its name to the path this listing was
// asked for, rather than by the path the node itself carries: the request
// path is the one just proved to address this directory, while a node's own
// path is written relative to the filesystem it lives in — which is not
// necessarily the one the share id opened onto.
func (p *Pixeldrain) lister(origin string) treeList[pixeldrainDir] {
	return func(ctx context.Context, _ *treeWalk, d pixeldrainDir, depth int) ([]treeItem[pixeldrainDir], error) {
		if depth > 0 {
			var err error
			if _, d.children, err = p.stat(ctx, origin, d.segs); err != nil {
				return nil, err
			}
		}
		var items []treeItem[pixeldrainDir]
		for _, child := range d.children {
			if child.Name == "" {
				continue
			}
			segs := append(append([]string(nil), d.segs...), child.Name)
			if child.Type == pixeldrainDirNode {
				items = append(items, treeItem[pixeldrainDir]{
					Folder: pixeldrainDir{segs: segs}, Key: pixeldrainPath(segs), Name: child.Name,
				})
				continue
			}
			if file, ok := p.toNodeFile(origin, segs, &child); ok {
				items = append(items, treeItem[pixeldrainDir]{File: &file})
			}
		}
		return items, nil
	}
}

// stat reads one path of a shared filesystem: what it is, and what is in it.
func (p *Pixeldrain) stat(ctx context.Context, origin string, segs []string) (*pixeldrainNode, []pixeldrainNode, error) {
	var out pixeldrainStat
	endpoint := pixeldrainFSURL(origin, segs) + "?stat"
	if err := p.client.GetJSON(ctx, endpoint, httpx.Referer(origin+"/"), &out); err != nil {
		return nil, nil, fmt.Errorf("pixeldrain: %s: %w", pixeldrainPath(segs), err)
	}
	node := out.node()
	if node == nil {
		return nil, nil, fmt.Errorf("pixeldrain: %s: the answer described no file or directory",
			pixeldrainPath(segs))
	}
	return node, out.Children, nil
}

// toNodeFile turns one file node into a downloadable entry.
func (p *Pixeldrain) toNodeFile(origin string, segs []string, n *pixeldrainNode) (File, bool) {
	// A reported node is refused by the API itself, so it is left out rather
	// than queued to fail.
	if n.AbuseType != "" {
		return File{}, false
	}
	return File{
		Name: cmp.Or(n.Name, segs[len(segs)-1]),
		// ?attach asks for the bytes; the bare path serves the viewer for
		// anything the site can render.
		URL:     pixeldrainFSURL(origin, segs) + "?attach",
		Size:    knownSize(n.FileSize),
		Headers: httpx.Referer(origin + "/"),
		Pace:    &pixeldrainPace,
	}, true
}

// pixeldrainWhyNot explains a node the API will not serve.
func pixeldrainWhyNot(n *pixeldrainNode) string {
	if n.AbuseType != "" {
		return "it was reported as " + n.AbuseType
	}
	return "the host does not offer it"
}

// pixeldrainFSURL addresses one path of a shared filesystem.
//
// Each component is escaped on its own, because these are names from a real
// filesystem: a file called "a/b" is one component, and escaping the whole
// path in one go would turn it into two.
func pixeldrainFSURL(origin string, segs []string) string {
	var b strings.Builder
	b.WriteString(origin + pixeldrainAPI + "/filesystem")
	for _, seg := range segs {
		b.WriteString("/")
		b.WriteString(url.PathEscape(seg))
	}
	return b.String()
}

// pixeldrainPath renders a filesystem path for an error message.
func pixeldrainPath(segs []string) string { return strings.Join(segs, "/") }

// pixeldrainStat is the answer to a stat: the breadcrumb from the share's
// root down to the node asked about, and, when that is a directory, what is
// inside it.
type pixeldrainStat struct {
	Path      []pixeldrainNode `json:"path"`
	BaseIndex int              `json:"base_index"`
	Children  []pixeldrainNode `json:"children"`
}

// node is the entry that was asked about. It is not simply the last of the
// breadcrumb — base_index is what says which one it is, and the array can
// carry entries either side of it.
func (s *pixeldrainStat) node() *pixeldrainNode {
	if s.BaseIndex >= 0 && s.BaseIndex < len(s.Path) {
		return &s.Path[s.BaseIndex]
	}
	if len(s.Path) > 0 {
		return &s.Path[len(s.Path)-1]
	}
	return nil
}

// pixeldrainNode is one file or directory of a shared filesystem.
type pixeldrainNode struct {
	Type     string `json:"type"`
	Name     string `json:"name"`
	FileSize int64  `json:"file_size"`
	// AbuseType is present only on a node that has been reported, and such a
	// node is refused to every caller.
	AbuseType string `json:"abuse_type"`
}
