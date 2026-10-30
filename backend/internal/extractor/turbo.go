// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"fmt"
	"net/url"

	"github.com/JohanLindvall/HeapLeach/internal/httpx"
	"github.com/JohanLindvall/HeapLeach/internal/util"
)

// Turbo resolves turbo.cr video links (/embed/<id>, /d/<id>, /f/<id>).
//
// The player asks /api/sign for a URL carrying a token and an `exp`
// timestamp roughly twenty minutes out, so signing is deferred to download
// time via Resolve.
type Turbo struct {
	client *httpx.Client
}

// NewTurbo builds the turbo.cr extractor.
func NewTurbo(client *httpx.Client) *Turbo {
	return &Turbo{client: client}
}

func (t *Turbo) Name() string { return "turbo" }

// turboShape is a video page: the player (/embed/<id>), the download page
// (/d/<id>, /f/<id>) or the watch page (/v/<id>).
var turboShape = pathShape(`^/(embed|d|f|v)/[A-Za-z0-9_-]+/?$`)

// turboSignPath is the signing endpoint, which the player and the download
// page call with the id.
const turboSignPath = "/api/sign?v="

// Recognises is the inventory's description.
func (t *Turbo) Recognises() string {
	return "video pages (`/embed/<id>`, `/d/<id>`, `/v/<id>`) whose player or download page " +
		"calls the `/api/sign?v=` signing endpoint, or that frame that player"
}

// Shape reports a video page.
func (t *Turbo) Shape(u *url.URL) bool { return turboShape(u) }

// Recognise takes a video page that uses the signing endpoint Extract asks:
// the player and the download page call it from their script, and the watch
// page frames the player for the same id.
func (t *Turbo) Recognise(_ context.Context, p *Page) bool {
	segs := util.PathSegments(p.Final)
	if !turboShape(p.Final) || len(segs) < 2 {
		return false
	}
	if p.Has(turboSignPath) {
		return true
	}
	return p.frames(func(src *url.URL) bool { return src.Path == "/embed/"+segs[1] })
}

// Extract produces a single lazily-signed entry.
func (t *Turbo) Extract(ctx context.Context, u *url.URL, _ Options) (*Result, error) {
	segs := util.PathSegments(u)
	if len(segs) == 0 {
		return nil, fmt.Errorf("turbo: no video id in %s", u.Redacted())
	}
	id := segs[len(segs)-1]
	base := util.Origin(u)
	return signedResult(ctx, func(ctx context.Context) (*Target, error) { return t.sign(ctx, base, id) })
}

// sign fetches a fresh signed URL for a video id.
func (t *Turbo) sign(ctx context.Context, base, id string) (*Target, error) {
	var out struct {
		Success          bool   `json:"success"`
		URL              string `json:"url"`
		Filename         string `json:"filename"`
		OriginalFilename string `json:"original_filename"`
		Message          string `json:"message"`
	}
	endpoint := base + turboSignPath + url.QueryEscape(id)
	if err := t.client.GetJSON(ctx, endpoint,
		httpx.RefererOrigin(base+"/embed/"+id, base), &out); err != nil {
		return nil, fmt.Errorf("turbo: sign %s: %w", id, err)
	}
	if !out.Success || out.URL == "" {
		return nil, fmt.Errorf("turbo: sign %s: %s", id, util.FirstNonEmpty(out.Message, "no url returned"))
	}
	return &Target{
		URL:     out.URL,
		Name:    util.FirstNonEmpty(out.OriginalFilename, out.Filename, util.NameFromURL(out.URL), id),
		Size:    -1,
		Headers: httpx.RefererOrigin(base+"/", base),
	}, nil
}
