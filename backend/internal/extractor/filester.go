package extractor

import (
	"context"
	"fmt"
	"net/url"

	"github.com/JohanLindvall/HeapLeach/internal/httpx"
	"github.com/JohanLindvall/HeapLeach/internal/util"
)

// Filester resolves single-file shares (/d/<slug>).
//
// The page itself carries nothing worth parsing: its script asks
// /v2/api/public/download for a token, and the answer names everything —
// the storage server, the stored file, the original name and the token,
// which lasts half an hour. So the link is minted through Resolve at
// download time, and minted once up front only so a dead link fails the job
// at once and the queue shows the real name. The storage server takes no
// referer or cookie and honours ranges.
//
// The authenticated API documents folders; nothing anonymous links to one,
// so only files are handled.
type Filester struct {
	hostSet
	client *httpx.Client
}

// filesterTokenPath is the anonymous token endpoint, relative to whichever
// origin the link named.
const filesterTokenPath = "/v2/api/public/download"

// NewFilester builds the filester extractor.
func NewFilester(client *httpx.Client) *Filester {
	return &Filester{hostSet: hostSet{"filester.si", "filester.me"}, client: client}
}

func (f *Filester) Name() string { return "filester" }

// Extract produces a single lazily-signed entry.
func (f *Filester) Extract(ctx context.Context, u *url.URL, _ Options) (*Result, error) {
	segs := util.PathSegments(u)
	if len(segs) < 2 || segs[0] != "d" {
		return nil, fmt.Errorf("filester: %s is not a file link (/d/<id>)", u.Redacted())
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
