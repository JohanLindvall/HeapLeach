// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"net/url"
	"regexp"
	"strings"

	"github.com/JohanLindvall/HeapLeach/internal/httpx"
	"golang.org/x/net/html"
)

// Helpers the video extractors' recognisers share. Each reads the page for
// something its extractor goes on to depend on — the player a page offers
// for embedding, the configuration it carries — and none of them looks at
// where the page was served from.

// playerMetaKeys are the meta tags a video page names its embeddable player
// in. A site's own player is the steadiest structural fact a watch page
// carries: it is what every other site embeds, so it survives redesigns of
// the page around it.
var playerMetaKeys = []string{"twitter:player", "og:video:url", "og:video:secure_url", "og:video"}

// playerMeta reports whether the page names a player whose path, and
// optionally query, the pattern matches. Only the path and query are read.
func (p *Page) playerMeta(pattern *regexp.Regexp) bool {
	for _, key := range playerMetaKeys {
		content := p.Meta(key)
		if content == "" {
			continue
		}
		u, err := url.Parse(content)
		if err != nil {
			continue
		}
		target := u.EscapedPath()
		if u.RawQuery != "" {
			target += "?" + u.RawQuery
		}
		if pattern.MatchString(target) {
			return true
		}
	}
	return false
}

// namedPrefix reports whether one of the page's names for its site begins
// with the prefix, ignoring case: for a site whose name carries a varying
// tail, as "<Name> ex <old name>" does.
func (p *Page) namedPrefix(prefix string) bool {
	for _, name := range p.Names() {
		name = strings.TrimSpace(name)
		if len(name) >= len(prefix) && strings.EqualFold(name[:len(prefix)], prefix) {
			return true
		}
	}
	return false
}

// fetchPage is fetchDocument for a page held as a string: the probed
// document when it is the one asked for, a fetch otherwise.
func fetchPage(ctx context.Context, client *httpx.Client, page string, headers httpx.Header) (string, error) {
	u, err := url.Parse(page)
	if err != nil {
		return "", err
	}
	doc, _, err := fetchDocument(ctx, client, u, headers)
	return doc, err
}

// hasElement reports whether the document has an element the test accepts.
func (p *Page) hasElement(test func(n *html.Node) bool) bool {
	root := p.HTML()
	return root != nil && findFirst(root, test) != nil
}
