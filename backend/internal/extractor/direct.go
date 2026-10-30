// SPDX-License-Identifier: MIT

package extractor

import (
	"cmp"
	"context"
	"net/url"

	"github.com/JohanLindvall/HeapLeach/internal/httpx"
	"github.com/JohanLindvall/HeapLeach/internal/util"
)

// Direct is the fallback for pages no extractor recognised: it reads an
// ordinary page for the downloads it links or the media it plays, and
// otherwise treats the URL as a file and lets the downloader learn the name
// and size from the response headers.
//
// It identifies no software. Every platform — a tube script, an image host,
// an archive, a wiki, a manifest, an open directory — is recognised from the
// probed page by its own extractor in the registry, once, before this is
// reached; what is left here is generic page work.
type Direct struct {
	client   *httpx.Client
	registry *Registry
}

// NewDirect builds the fallback extractor.
func NewDirect(client *httpx.Client) *Direct { return &Direct{client: client} }

func (d *Direct) Name() string { return "direct" }

// Shape claims no link: the fallback is what nothing else recognised, never
// a reason to follow one.
func (d *Direct) Shape(*url.URL) bool { return false }

// Recognise takes any page; the registry asks it last.
func (d *Direct) Recognise(context.Context, *Page) bool { return true }

// Recognises is the inventory's description.
func (d *Direct) Recognises() string {
	return "anything else: a page's supported links or embedded player, or the URL as a plain file"
}

// Extract reads an ordinary page for download links or media before
// treating the URL as a file itself.
func (d *Direct) Extract(ctx context.Context, u *url.URL, opts Options) (*Result, error) {
	if res, err := d.pageSniff(ctx, u, opts); res != nil || err != nil {
		return res, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	name := cmp.Or(util.NameFromURL(u.String()), u.Hostname())
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
