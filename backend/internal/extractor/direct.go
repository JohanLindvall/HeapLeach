// SPDX-License-Identifier: MIT

package extractor

import (
	"cmp"
	"context"
	"errors"
	"fmt"
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
	about
	client   *httpx.Client
	registry *Registry
}

// NewDirect builds the fallback extractor.
//
// It has no shape: the fallback is what nothing else recognised, never a
// reason to follow a link.
func NewDirect(client *httpx.Client) *Direct {
	return &Direct{client: client, about: about{name: "direct",
		recognises: "anything else: a page's supported links or embedded player, or the URL as a plain file"}}
}

// Recognise takes any page; the registry asks it last.
func (d *Direct) Recognise(context.Context, *Page) bool { return true }

// Extract reads an ordinary page for download links or media before
// treating the URL as a file itself.
//
// A URL that was read as a web page and held nothing is refused rather
// than queued: the downloader would only fetch the same page, reject it as
// not a file, and say no extractor recognised the site, after waiting out
// its retries. Where an extractor's form claims the URL the page is that
// site's own, served without what its extractor reads — an item removed,
// made private or gated, or a redesign — and the error says so.
func (d *Direct) Extract(ctx context.Context, u *url.URL, opts Options) (*Result, error) {
	res, read, err := d.pageSniff(ctx, u, opts)
	if res != nil || err != nil {
		return res, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if read {
		return nil, d.emptyPage(u)
	}

	return directResult(u, httpx.Referer(util.Origin(u)+"/")), nil
}

// directResult leaves the name and size for the transfer to learn, retaining
// the submitted URL rather than a possibly short-lived redirect destination.
func directResult(u *url.URL, headers httpx.Header) *Result {
	name := cmp.Or(util.NameFromURL(u.String()), u.Hostname())
	return &Result{
		Title: name,
		Files: []File{{
			Name:    name,
			URL:     u.String(),
			Size:    -1,
			Headers: headers,
		}},
	}
}

// emptyPage is the error for a web page with nothing on it to download.
func (d *Direct) emptyPage(u *url.URL) error {
	return plainPageError{d.emptyPageText(u)}
}

// errPlainPage matches the fallback's refusal of a web page it found
// nothing on (Direct.emptyPage), whatever its wording.
var errPlainPage = errors.New("a web page with nothing to download")

type plainPageError struct{ error }

func (plainPageError) Is(target error) bool { return target == errPlainPage }

// emptyPageText is emptyPage's error.
func (d *Direct) emptyPageText(u *url.URL) error {
	if d.registry != nil {
		// Only the one claimant is named: of several, the first is a guess
		// (an album link on one site has another's share form).
		if ex, claimed := d.registry.claimant(u); claimed {
			return fmt.Errorf("%s: %s has the form of its links, but its page holds nothing it reads "+
				"(the item may have been removed or made private, sit behind a login or age gate, "+
				"or the site may have changed)", ex.Name(), u.Redacted())
		}
		if d.registry.claimsHere(u) {
			return fmt.Errorf("%s: %s has the form of supported links, but its page holds nothing any "+
				"extractor reads (the item may have been removed or made private, sit behind a login or "+
				"age gate, or the site may have changed)", d.registry.Label(u), u.Redacted())
		}
	}
	return fmt.Errorf("%s: %s is a web page, not a file, and no extractor here recognises it: "+
		"it links no supported host and plays no media this build can find", d.Name(), u.Redacted())
}
