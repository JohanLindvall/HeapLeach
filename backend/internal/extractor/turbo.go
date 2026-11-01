// SPDX-License-Identifier: MIT

package extractor

import (
	"cmp"
	"context"
	"fmt"
	"net/url"

	"github.com/JohanLindvall/HeapLeach/internal/httpx"
	"github.com/JohanLindvall/HeapLeach/internal/util"
)

// turbo resolves turbo.cr video links (/embed/<id>, /d/<id>, /f/<id>).
//
// The player asks /api/sign for a URL carrying a token and an `exp`
// timestamp roughly twenty minutes out, so signing is deferred to download
// time via Resolve.

// NewTurbo builds the turbo.cr extractor.
func NewTurbo(client *httpx.Client) *Share { return newShare(client, &turboSite) }

// turboShape is a video page: the player (/embed/<id>), the download page
// (/d/<id>, /f/<id>) or the watch page (/v/<id>).
var turboShape = pathShape(`^/(embed|d|f|v)/[A-Za-z0-9_-]+/?$`)

// turboSignPath is the signing endpoint, which the player and the download
// page call with the id.
const turboSignPath = "/api/sign?v="

// turboSite takes a video page that uses the signing endpoint extract asks:
// the player and the download page call it from their script, and the watch
// page frames the player for the same id. Each link is one lazily-signed
// entry.
var turboSite = shareSite{
	name: "turbo",
	recognises: "video pages (`/embed/<id>`, `/d/<id>`, `/v/<id>`) whose player or download page " +
		"calls the `/api/sign?v=` signing endpoint, or that frame that player",
	forms: []shareForm{{turboShape, func(p *Page) bool {
		id := p.Segments()[1]
		return p.Has(turboSignPath) || p.frames(func(src *url.URL) bool { return src.Path == "/embed/"+id })
	}}},
	// A page the probe was kept from, by the signing endpoint at its origin
	// answering for the id: what extract reads.
	withheld: func(ctx context.Context, s *Share, p *Page) bool {
		segs := p.Segments()
		if len(segs) < 2 {
			return false
		}
		base, id := util.Origin(p.Final), segs[len(segs)-1]
		return withheldAsked(ctx, p, "turbo "+base+" "+id, func(ctx context.Context) bool {
			_, err := turboSign(ctx, s, base, id)
			return err == nil
		})
	},
	extract: func(ctx context.Context, s *Share, u *url.URL, _ Options) (*Result, error) {
		segs := util.PathSegments(u)
		if len(segs) == 0 {
			return nil, fmt.Errorf("turbo: no video id in %s", u.Redacted())
		}
		id, base := segs[len(segs)-1], util.Origin(u)
		return signedResult(ctx, func(ctx context.Context) (*Target, error) { return turboSign(ctx, s, base, id) })
	},
}

// turboSign fetches a fresh signed URL for a video id.
func turboSign(ctx context.Context, s *Share, base, id string) (*Target, error) {
	var out struct {
		Success          bool   `json:"success"`
		URL              string `json:"url"`
		Filename         string `json:"filename"`
		OriginalFilename string `json:"original_filename"`
		Message          string `json:"message"`
	}
	endpoint := base + turboSignPath + url.QueryEscape(id)
	if err := s.client.GetJSON(ctx, endpoint,
		httpx.RefererOrigin(base+"/embed/"+id, base), &out); err != nil {
		return nil, fmt.Errorf("turbo: sign %s: %w", id, err)
	}
	if !out.Success || out.URL == "" {
		return nil, fmt.Errorf("turbo: sign %s: %s", id, cmp.Or(out.Message, "no url returned"))
	}
	return &Target{
		URL:     out.URL,
		Name:    cmp.Or(out.OriginalFilename, out.Filename, util.NameFromURL(out.URL), id),
		Size:    -1,
		Headers: httpx.SiteOrigin(base),
	}, nil
}
