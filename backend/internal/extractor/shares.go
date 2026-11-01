// SPDX-License-Identifier: MIT

package extractor

import (
	"cmp"
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/httpx"
	"github.com/JohanLindvall/HeapLeach/internal/util"
	"golang.org/x/net/html"
)

// Share pages: hosts whose links are a page standing for a file, an album
// or a folder of uploads. What sets one host apart is how its page is read,
// and that is the host's own code; what they share — a name, an inventory
// line, a few link forms each recognised by what its page carries, and the
// reading of a recognised link — is a row of a table (shareSite), and this
// is the one extractor that serves every row.
type Share struct {
	about
	client *httpx.Client
	site   *shareSite
	// api is the row's API base, a field so a test can serve it.
	api string
}

// shareSite is one host's row.
type shareSite struct {
	// about names the host and describes its forms for the inventory; its
	// shape is left unset, since the extractor's is any of the forms'.
	about
	// forms are the host's link forms, in the order a page is offered to
	// them: the first whose shape the page's final URL has decides.
	forms []shareForm
	// pageForms are offered a page after forms, and are left out of the
	// row's Shape: forms another row's links already have, recognised as
	// this row's by the page alone, so the link scanner's label for them
	// stays the other row's.
	pageForms []shareForm
	// extract reads a link the registry recognised, or one handed over
	// directly, which it checks for itself.
	extract func(ctx context.Context, s *Share, u *url.URL, opts Options) (*Result, error)
	// timeout caps every request to the host, when set.
	timeout time.Duration
	// api is the base of the host's API, for a row whose test serves one.
	api string
	// withheld, when set, takes a page of one of forms that the probe was
	// kept from (probeWithheld) by one bounded question to the host's API
	// (withheldAsked), for a row whose extract reads that API rather than
	// the page.
	withheld func(ctx context.Context, s *Share, p *Page) bool
	// left, when set, takes a page whose submitted URL has one of forms but
	// whose final URL has none: a route that redirects off the site, as a
	// download button does to signed storage, judged by one bounded
	// question about the share it belongs to.
	left func(ctx context.Context, s *Share, p *Page) bool
}

// shareForm is one link form: shape is worth a probe for the link scanner,
// and recognise takes a probed page whose final URL has that shape by what
// extract goes on to read there. A form without recognise is a link that
// redirects to another form, followed for that and never recognised in its
// own right.
type shareForm struct {
	shape     func(*url.URL) bool
	recognise func(*Page) bool
}

// newShare builds the extractor for one row.
func newShare(client *httpx.Client, site *shareSite) *Share {
	if site.timeout > 0 {
		client = client.WithTimeout(site.timeout)
	}
	var shapes []func(*url.URL) bool
	for _, f := range site.forms {
		shapes = append(shapes, f.shape)
	}
	about := site.about
	about.shape = eitherShape(shapes...)
	return &Share{about: about, client: client, site: site, api: site.api}
}

// Recognise offers the page to the form its final URL has, or a withheld
// page of the row's forms to its withheld question.
func (s *Share) Recognise(ctx context.Context, p *Page) bool {
	if s.site.withheld != nil && probeWithheld(p) && s.Shape(p.Final) {
		return s.client != nil && s.site.withheld(ctx, s, p)
	}
	if s.site.left != nil && !s.Shape(p.Final) && s.Shape(p.URL) {
		return s.client != nil && s.site.left(ctx, s, p)
	}
	for _, f := range s.site.forms {
		if f.shape(p.Final) {
			return f.recognise != nil && f.recognise(p)
		}
	}
	for _, f := range s.site.pageForms {
		if f.shape(p.Final) {
			return f.recognise != nil && f.recognise(p)
		}
	}
	return false
}

// Extract reads the link as the row does.
func (s *Share) Extract(ctx context.Context, u *url.URL, opts Options) (*Result, error) {
	return s.site.extract(ctx, s, u, opts)
}

// root fetches a page the row reads, the probe's own when it is the one
// asked for, with the referer the host expects; a failure names the host.
func (s *Share) root(ctx context.Context, u *url.URL, referer string) (*html.Node, error) {
	root, _, err := fetchRoot(ctx, s.client, u, httpx.Referer(referer))
	if err != nil {
		return nil, fmt.Errorf("%s: fetch %s: %w", s.name, u.Redacted(), err)
	}
	return root, nil
}

// albumTitle names a job after the album a page lists: its metadata title,
// which a template does not truncate to fit a box, then the document's
// title without the site's name, then the link's own path.
func albumTitle(root *html.Node, u *url.URL) string {
	return cmp.Or(
		metaContent(root, "og:title"),
		trimSiteSuffix(firstText(root, atomTitle)),
		strings.Trim(u.Path, "/"),
	)
}

// enabledControl reports a control the page offers for use: neither
// disabled nor marked so for assistive technology, the two ways a share
// page withholds its download button.
func enabledControl(n *html.Node) bool {
	return !hasAttr(n, "disabled") && attr(n, "aria-disabled") != "true"
}

// linkPathID reads the id out of a link of the form /<kind>/<id> for any of
// kinds, ignoring its query, and yields "" for anything else a listing
// links to.
func linkPathID(link string, kinds ...string) string {
	u, err := url.Parse(link)
	if err != nil {
		return ""
	}
	segs := util.PathSegments(u)
	for _, kind := range kinds {
		if len(segs) == 2 && segs[0] == kind {
			return segs[1]
		}
	}
	return ""
}
