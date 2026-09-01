package extractor

import (
	"context"
	"fmt"
	"net/url"
	"path"
	"strings"

	"github.com/JohanLindvall/HeapLeach/internal/config"
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
// at all. Another site running this software costs one line in Match.
//
// The label stays "pixeldrain" whichever host answered: it names the software
// both of them run, and a user who pasted a nova.storage link is better
// served by an error that says which program is talking than by one that
// repeats the domain back at them.
type Pixeldrain struct {
	hostSet
	client *httpx.Client
}

// pixeldrainAPI is the API root, relative to whichever origin was asked.
const pixeldrainAPI = "/api"

// Node types in a shared filesystem.
const (
	pixeldrainDirNode  = "dir"
	pixeldrainFileNode = "file"
)

// pixeldrainMaxFiles bounds what one shared link resolves to. A shared
// directory can be somebody's whole drive, and a job of tens of thousands of
// items is not what anyone pasting a link meant.
const pixeldrainMaxFiles = 5000

// NewPixeldrain builds the pixeldrain extractor.
func NewPixeldrain(client *httpx.Client) *Pixeldrain {
	return &Pixeldrain{hostSet: hostSet{"pixeldrain.com", "nova.storage"}, client: client}
}

func (p *Pixeldrain) Name() string { return "pixeldrain" }

// Extract lists a whole album, a shared directory, or a single file.
func (p *Pixeldrain) Extract(ctx context.Context, u *url.URL, _ Options) (*Result, error) {
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
		return p.shared(ctx, origin, segs[1:])
	}
	return p.file(ctx, origin, segs[1])
}

// list resolves an album of files.
func (p *Pixeldrain) list(ctx context.Context, origin, id string) (*Result, error) {
	var out struct {
		Success bool             `json:"success"`
		Title   string           `json:"title"`
		Files   []pixeldrainFile `json:"files"`
		Value   string           `json:"value"`
		Message string           `json:"message"`
	}
	endpoint := origin + pixeldrainAPI + "/list/" + url.PathEscape(id)
	if err := p.client.GetJSON(ctx, endpoint, nil, &out); err != nil {
		return nil, fmt.Errorf("pixeldrain: list %s: %w", id, err)
	}
	if !out.Success {
		return nil, fmt.Errorf("pixeldrain: list %s: %s", id, util.FirstNonEmpty(out.Message, out.Value, "not available"))
	}

	files := make([]File, 0, len(out.Files))
	for _, f := range out.Files {
		files = append(files, p.toFile(origin, f))
	}
	return &Result{Title: util.FirstNonEmpty(out.Title, id), Files: files}, nil
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
	return &Result{Title: util.FirstNonEmpty(f.Name, id), Files: []File{p.toFile(origin, f)}}, nil
}

// toFile converts API metadata into a downloadable entry.
func (p *Pixeldrain) toFile(origin string, f pixeldrainFile) File {
	size := f.Size
	if size == 0 {
		size = -1
	}
	return File{
		Name:    util.FirstNonEmpty(f.Name, f.ID),
		URL:     origin + pixeldrainAPI + "/file/" + url.PathEscape(f.ID) + "?download",
		Size:    size,
		Headers: httpx.Referer(origin + "/"),
	}
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
func (p *Pixeldrain) shared(ctx context.Context, origin string, segs []string) (*Result, error) {
	node, children, err := p.stat(ctx, origin, segs)
	if err != nil {
		return nil, err
	}

	// A share can name a single file just as well as a directory.
	if node.Type == pixeldrainFileNode {
		file, ok := p.toNodeFile(origin, segs, node, "")
		if !ok {
			return nil, fmt.Errorf("pixeldrain: %s cannot be downloaded (%s)",
				pixeldrainPath(segs), pixeldrainWhyNot(node))
		}
		return &Result{Title: file.Name, Files: []File{file}}, nil
	}

	res := &Result{Title: util.FirstNonEmpty(node.Name, segs[0])}
	p.collect(ctx, origin, segs, children, "", 0, res)
	if len(res.Files) == 0 {
		return nil, fmt.Errorf("pixeldrain: %s holds no downloadable files", pixeldrainPath(segs))
	}
	return res, nil
}

// collect appends a directory's files to res and recurses into its
// subdirectories, preserving the tree in File.Dir.
//
// A child is addressed by appending its name to the path this listing was
// asked for, rather than by the path the node itself carries: the request
// path is the one just proved to address this directory, while a node's own
// path is written relative to the filesystem it lives in — which is not
// necessarily the one the share id opened onto.
func (p *Pixeldrain) collect(ctx context.Context, origin string, base []string,
	children []pixeldrainNode, dir string, depth int, res *Result) {

	for _, child := range children {
		if len(res.Files) >= pixeldrainMaxFiles {
			return
		}
		if child.Name == "" {
			continue
		}
		segs := append(append([]string(nil), base...), child.Name)

		if child.Type == pixeldrainDirNode {
			if depth >= config.MaxFolderDepth {
				continue
			}
			_, grandchildren, err := p.stat(ctx, origin, segs)
			if err != nil {
				continue // one unreadable subdirectory must not sink the job
			}
			p.collect(ctx, origin, segs, grandchildren, path.Join(dir, child.Name), depth+1, res)
			continue
		}
		if file, ok := p.toNodeFile(origin, segs, &child, dir); ok {
			res.Files = append(res.Files, file)
		}
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
func (p *Pixeldrain) toNodeFile(origin string, segs []string, n *pixeldrainNode, dir string) (File, bool) {
	// A reported node is refused by the API itself, so it is left out rather
	// than queued to fail.
	if n.AbuseType != "" {
		return File{}, false
	}
	size := n.FileSize
	if size <= 0 {
		size = -1
	}
	return File{
		Name: util.FirstNonEmpty(n.Name, segs[len(segs)-1]),
		// ?attach asks for the bytes; the bare path serves the viewer for
		// anything the site can render.
		URL:     pixeldrainFSURL(origin, segs) + "?attach",
		Size:    size,
		Dir:     dir,
		Headers: httpx.Referer(origin + "/"),
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
	FileType string `json:"file_type"`
	// AbuseType is present only on a node that has been reported, and such a
	// node is refused to every caller.
	AbuseType string `json:"abuse_type"`
}
