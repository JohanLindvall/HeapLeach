package extractor

import (
	"context"
	"net/url"

	"github.com/JohanLindvall/HeapLeach/internal/httpx"
	"github.com/JohanLindvall/HeapLeach/internal/util"
)

// Direct is the fallback for hosts with no dedicated extractor: it treats
// the URL as a file and lets the downloader learn the name and size from the
// response headers.
type Direct struct {
	client *httpx.Client
}

// NewDirect builds the fallback extractor.
func NewDirect(client *httpx.Client) *Direct { return &Direct{client: client} }

func (d *Direct) Name() string { return "direct" }

func (d *Direct) Match(*url.URL) bool { return true }

// Extract wraps the URL as a single file.
//
// One shape is looked at more closely first. A URL laid out like a Kernel
// Video Sharing video page is very likely to be one — the platform runs on
// far more hosts than any list will name — and treating such a page as a
// file would download the HTML shell instead of the video. A page that turns
// out not to be one falls through to the handling below, which is what would
// have happened anyway.
func (d *Direct) Extract(ctx context.Context, u *url.URL, _ Options) (*Result, error) {
	if res, ok := kvsSniff(ctx, d.client, u); ok {
		return res, nil
	}

	name := util.FirstNonEmpty(util.NameFromURL(u.String()), u.Hostname())
	return &Result{
		Title: name,
		Files: []File{{
			Name:    name,
			URL:     u.String(),
			Size:    -1,
			Headers: httpx.Referer(util.Origin(u) + "/"),
		}},
	}, nil
}
