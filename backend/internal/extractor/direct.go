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

// directSniff recognises a shape rather than a host. It returns false to mean
// "not mine", and the URL then falls through to whatever would have happened
// anyway — which is what makes a wrong guess free, and is the contract every
// sniff here keeps.
type directSniff func(context.Context, *httpx.Client, *url.URL, Options) (*Result, bool)

// directSniffs are tried in order, most specific first.
//
// Each one is a platform or a shape with an unbounded tail: far more sites run
// this software, or serve this kind of page, than any list will ever name. A
// sniff is worth a request where a host list is not, because the cost of being
// wrong is one fetch and the cost of being absent is a wrong download — the
// direct fallback would otherwise write a player page, a directory index or a
// manifest to disk and call it a finished file.
//
// Ordering matters where two could match the same URL: the ones that key off
// an unmistakable marker come before the ones that read markup for a guess.
var directSniffs = []directSniff{
	// A manifest is unambiguous — it says #EXTM3U on its first line — and
	// pasting one is common enough that getting it wrong is the defect this
	// list mainly exists to fix.
	func(ctx context.Context, c *httpx.Client, u *url.URL, _ Options) (*Result, bool) {
		if !hlsSniffable(u) {
			return nil, false
		}
		res, err := hlsSniff(ctx, c, u)
		return res, err == nil && res != nil
	},
	// A published API answering to its own version is proof, not a guess.
	func(ctx context.Context, c *httpx.Client, u *url.URL, opts Options) (*Result, bool) {
		res, ok, _ := peerTubeSniff(ctx, c, u, opts)
		return res, ok
	},
	// A generator meta tag naming the software, likewise.
	cheveretoSniff,
	// The KVS path shape: /videos/<id>/<slug>/, which is specific enough to
	// be worth one GET on a host nobody registered.
	func(ctx context.Context, c *httpx.Client, u *url.URL, _ Options) (*Result, bool) {
		return kvsSniff(ctx, c, u)
	},
	// An autoindex announces itself in its title.
	func(ctx context.Context, c *httpx.Client, u *url.URL, _ Options) (*Result, bool) {
		return autoindexSniff(ctx, c, u)
	},
	// Last, and least certain: an ordinary page that happens to carry a
	// video in its markup or its metadata.
	func(ctx context.Context, c *httpx.Client, u *url.URL, _ Options) (*Result, bool) {
		return mediaPageSniff(ctx, c, u)
	},
}

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
func (d *Direct) Extract(ctx context.Context, u *url.URL, opts Options) (*Result, error) {
	for _, sniff := range directSniffs {
		if res, ok := sniff(ctx, d.client, u, opts); ok {
			return res, nil
		}
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
