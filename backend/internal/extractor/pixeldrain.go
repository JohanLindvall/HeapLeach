package extractor

import (
	"context"
	"fmt"
	"net/url"

	"github.com/JohanLindvall/HeapLeach/internal/httpx"
	"github.com/JohanLindvall/HeapLeach/internal/util"
)

// Pixeldrain resolves pixeldrain lists (/l/<id>) and single files
// (/u/<id>, /f/<id>) through its documented public API. Links are stable,
// so everything resolves up front.
type Pixeldrain struct {
	client *httpx.Client
}

const (
	pixeldrainRoot = "https://pixeldrain.com"
	pixeldrainAPI  = pixeldrainRoot + "/api"
)

// NewPixeldrain builds the pixeldrain extractor.
func NewPixeldrain(client *httpx.Client) *Pixeldrain { return &Pixeldrain{client: client} }

func (p *Pixeldrain) Name() string { return "pixeldrain" }

func (p *Pixeldrain) Match(u *url.URL) bool { return util.HostMatches(u.Host, "pixeldrain.com") }

// Extract lists a whole album or a single file.
func (p *Pixeldrain) Extract(ctx context.Context, u *url.URL, _ Options) (*Result, error) {
	segs := util.PathSegments(u)
	if len(segs) < 2 {
		return nil, fmt.Errorf("pixeldrain: no id in %s", u.Redacted())
	}
	kind, id := segs[0], segs[1]

	if kind == "l" {
		return p.list(ctx, id)
	}
	return p.file(ctx, id)
}

// list resolves an album of files.
func (p *Pixeldrain) list(ctx context.Context, id string) (*Result, error) {
	var out struct {
		Success bool             `json:"success"`
		Title   string           `json:"title"`
		Files   []pixeldrainFile `json:"files"`
		Value   string           `json:"value"`
		Message string           `json:"message"`
	}
	if err := p.client.GetJSON(ctx, pixeldrainAPI+"/list/"+url.PathEscape(id), nil, &out); err != nil {
		return nil, fmt.Errorf("pixeldrain: list %s: %w", id, err)
	}
	if !out.Success {
		return nil, fmt.Errorf("pixeldrain: list %s: %s", id, util.FirstNonEmpty(out.Message, out.Value, "not available"))
	}

	files := make([]File, 0, len(out.Files))
	for _, f := range out.Files {
		files = append(files, p.toFile(f))
	}
	return &Result{Title: util.FirstNonEmpty(out.Title, id), Files: files}, nil
}

// file resolves a single upload.
func (p *Pixeldrain) file(ctx context.Context, id string) (*Result, error) {
	var f pixeldrainFile
	if err := p.client.GetJSON(ctx, pixeldrainAPI+"/file/"+url.PathEscape(id)+"/info", nil, &f); err != nil {
		return nil, fmt.Errorf("pixeldrain: file %s: %w", id, err)
	}
	if f.ID == "" {
		f.ID = id
	}
	return &Result{Title: util.FirstNonEmpty(f.Name, id), Files: []File{p.toFile(f)}}, nil
}

// toFile converts API metadata into a downloadable entry.
func (p *Pixeldrain) toFile(f pixeldrainFile) File {
	size := f.Size
	if size == 0 {
		size = -1
	}
	return File{
		Name:    util.FirstNonEmpty(f.Name, f.ID),
		URL:     pixeldrainAPI + "/file/" + url.PathEscape(f.ID) + "?download",
		Size:    size,
		Headers: httpx.Referer(pixeldrainRoot + "/"),
	}
}

// pixeldrainFile is the subset of the file metadata we need.
type pixeldrainFile struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Size     int64  `json:"size"`
	MimeType string `json:"mime_type"`
}
