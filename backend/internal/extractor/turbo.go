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
func NewTurbo(client *httpx.Client) *Turbo { return &Turbo{client: client} }

func (t *Turbo) Name() string { return "turbo" }

func (t *Turbo) Match(u *url.URL) bool {
	return util.HostMatches(u.Host, "turbo.cr") || util.HostMatches(u.Host, "turbocdn.st")
}

// Extract produces a single lazily-signed entry.
func (t *Turbo) Extract(ctx context.Context, u *url.URL, _ Options) (*Result, error) {
	segs := util.PathSegments(u)
	if len(segs) == 0 {
		return nil, fmt.Errorf("turbo: no video id in %s", u.Redacted())
	}
	id := segs[len(segs)-1]
	base := util.Origin(u)

	// Sign once now so the job fails fast on a dead link and so the UI can
	// show the real filename while the item waits its turn.
	signed, err := t.sign(ctx, base, id)
	if err != nil {
		return nil, err
	}
	return &Result{
		Title: signed.Name,
		Files: []File{{
			Name:    signed.Name,
			URL:     signed.URL,
			Size:    -1,
			Headers: signed.Headers,
			Resolve: func(ctx context.Context) (*Target, error) { return t.sign(ctx, base, id) },
		}},
	}, nil
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
	endpoint := base + "/api/sign?v=" + url.QueryEscape(id)
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
