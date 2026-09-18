package extractor

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/JohanLindvall/HeapLeach/internal/util"
	"golang.org/x/net/html"
)

// Search results on a Kernel Video Sharing install.
//
// The platform lays a search out as /search/<query>/, with the query kept in
// the path, and pages the results in script: there is no /search/<query>/2/
// to fetch, that path is a 404, so the walk in kvslisting.go asks for the
// block the way the site's own script does. Past the last page the block
// request is a 404 as well, where a member listing serves page one again;
// the walk survives either, but the last page announces itself in its pager
// and the walk reads that rather than finding out. Nothing about the results
// is particular to searching. The tiles link to ordinary video pages, which
// are resolved exactly as a pasted one would be — and a page also lists the
// albums matching the query, in a block with a pager of its own, which is
// why both are picked by name in the walk rather than by coming first.
//
// Like a member's profile this is only offered on the hosts registered as
// KVS: /search/ is every site's URL shape, not this platform's.

// kvsSearchPath reports whether a URL is a search on the install, and returns
// the query and the canonical first page of its results.
//
// A pasted later page, on a theme that pages by path, means the whole search
// the way a member's section means the member, so anything after the query
// is dropped. The query keeps the escaping it arrived with: the site's own
// links write a plus as %2B and a space as a hyphen, and re-encoding a
// decoded segment would hand the platform a literal plus, which its routing
// may well read as a space. The form-style /search/?q=<query> is accepted
// too, since that is what a theme's search box submits before the redirect.
func kvsSearchPath(u *url.URL) (query, listing string, ok bool) {
	var segs []string
	for s := range strings.SplitSeq(u.EscapedPath(), "/") {
		if s != "" {
			segs = append(segs, s)
		}
	}
	if len(segs) == 0 || !strings.EqualFold(segs[0], "search") {
		return "", "", false
	}
	var raw string
	switch {
	case len(segs) >= 2:
		raw = segs[1]
	case u.Query().Get("q") != "":
		raw = url.PathEscape(strings.TrimSpace(u.Query().Get("q")))
	}
	query, err := url.PathUnescape(raw)
	if err != nil || strings.TrimSpace(query) == "" {
		return "", "", false
	}
	return strings.TrimSpace(query), util.Origin(u) + "/search/" + raw + "/", true
}

// kvsListingForSearch describes a search's results to the shared walk. The
// job is named for the query, which is what a folder of its results is
// about; the page's own heading only restates it inside the theme's wording.
func kvsListingForSearch(query, listing string) kvsListing {
	return kvsListing{
		url:   listing,
		title: func(*html.Node) string { return query },
		empty: fmt.Sprintf("no videos match the search at %s", listing),
	}
}
