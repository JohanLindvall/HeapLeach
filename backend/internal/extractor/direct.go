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

// directSniff recognises a shape rather than a host, and answers one of three
// ways.
//
//	nil, nil    not mine — fall through to whatever would have happened
//	            anyway, which is what makes a wrong guess free
//	res, nil    recognised and resolved
//	nil, err    recognised, and then it went wrong
//
// That third case is why this returns an error at all. Once a response has
// identified itself — a body opening #EXTM3U, a generator tag naming the
// software — falling through would hand the document back to the direct
// fallback, which would save the manifest or the page shell to disk and
// report a finished download. That is the exact failure these sniffs exist to
// prevent, so a sniff that has committed says what went wrong instead of
// pretending it never looked.
type directSniff func(context.Context, *httpx.Client, *url.URL, Options) (*Result, error)

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
	// The KVS path shape: /videos/<id>/<slug>/. It leads because for a URL
	// laid out that way the guess is very likely right, so it should not
	// pay for anyone else's probe first.
	func(ctx context.Context, c *httpx.Client, u *url.URL, _ Options) (*Result, error) {
		res, _ := kvsSniff(ctx, c, u)
		return res, nil
	},
	// A manifest is unambiguous — it says #EXTM3U on its first line — and
	// pasting one is common enough that getting it wrong is the defect this
	// list mainly exists to fix. It is also the one sniff that reports
	// rather than falls through, for the reason on directSniff.
	func(ctx context.Context, c *httpx.Client, u *url.URL, _ Options) (*Result, error) {
		return hlsSniff(ctx, c, u)
	},
	// A published API answering with its own version is proof, not a guess.
	func(ctx context.Context, c *httpx.Client, u *url.URL, opts Options) (*Result, error) {
		res, ok, err := peerTubeSniff(ctx, c, u, opts)
		if !ok {
			return nil, nil
		}
		return res, err
	},
	// A generator meta tag naming the software, likewise.
	func(ctx context.Context, c *httpx.Client, u *url.URL, opts Options) (*Result, error) {
		res, _ := cheveretoSniff(ctx, c, u, opts)
		return res, nil
	},
	// A band-site player labels its own track anchors, which is as good as a
	// generator tag and costs the same one fetch.
	func(ctx context.Context, c *httpx.Client, u *url.URL, _ Options) (*Result, error) {
		return bandzoogleSniff(ctx, c, u)
	},
	// An autoindex announces itself in its title.
	func(ctx context.Context, c *httpx.Client, u *url.URL, _ Options) (*Result, error) {
		res, _ := autoindexSniff(ctx, c, u)
		return res, nil
	},
	// Last, and least certain: an ordinary page that happens to carry a
	// video in its markup or its metadata.
	func(ctx context.Context, c *httpx.Client, u *url.URL, _ Options) (*Result, error) {
		res, _ := mediaPageSniff(ctx, c, u)
		return res, nil
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
		switch res, err := sniff(ctx, d.client, u, opts); {
		case err != nil:
			return nil, err
		case res != nil:
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
