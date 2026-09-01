package extractor

import (
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"

	"github.com/JohanLindvall/HeapLeach/internal/util"
)

// parseHTML parses a document with the standard tolerant HTML5 parser, so
// the unclosed tags and stray markup these sites ship do not derail us the
// way ad-hoc regexes would.
func parseHTML(doc string) (*html.Node, error) {
	root, err := html.Parse(strings.NewReader(doc))
	if err != nil {
		return nil, fmt.Errorf("parse html: %w", err)
	}
	return root, nil
}

// walk visits n and every descendant in document order.
func walk(n *html.Node, fn func(*html.Node)) {
	fn(n)
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		walk(c, fn)
	}
}

// attr returns the value of an attribute, or "" when absent.
func attr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}

// hasAttr reports whether an attribute is present, whatever its value.
// Valueless HTML attributes ("allowfullscreen") parse to an empty string,
// which attr cannot tell apart from an absent one.
func hasAttr(n *html.Node, key string) bool {
	for _, a := range n.Attr {
		if a.Key == key {
			return true
		}
	}
	return false
}

// hasClass reports whether the node carries a class token.
func hasClass(n *html.Node, class string) bool {
	return strings.Contains(" "+attr(n, "class")+" ", " "+class+" ")
}

// isElem reports whether n is an element with the given tag.
func isElem(n *html.Node, a atom.Atom) bool {
	return n.Type == html.ElementNode && n.DataAtom == a
}

// findAll collects every node matching pred, in document order.
func findAll(root *html.Node, pred func(*html.Node) bool) []*html.Node {
	var out []*html.Node
	walk(root, func(n *html.Node) {
		if pred(n) {
			out = append(out, n)
		}
	})
	return out
}

// findFirst returns the first node matching pred, or nil.
func findFirst(root *html.Node, pred func(*html.Node) bool) *html.Node {
	var found *html.Node
	walk(root, func(n *html.Node) {
		if found == nil && pred(n) {
			found = n
		}
	})
	return found
}

// textOf returns the collapsed text content of a subtree.
func textOf(n *html.Node) string {
	var b strings.Builder
	walk(n, func(c *html.Node) {
		if c.Type == html.TextNode {
			b.WriteString(c.Data)
		}
	})
	return strings.Join(strings.Fields(b.String()), " ")
}

// firstText returns the text of the first element with the given tag.
func firstText(root *html.Node, a atom.Atom) string {
	if n := findFirst(root, func(n *html.Node) bool { return isElem(n, a) }); n != nil {
		return textOf(n)
	}
	return ""
}

// metaContent reads <meta property=... content=...> (or name=...).
func metaContent(root *html.Node, key string) string {
	n := findFirst(root, func(n *html.Node) bool {
		if !isElem(n, atom.Meta) {
			return false
		}
		return attr(n, "property") == key || attr(n, "name") == key
	})
	if n == nil {
		return ""
	}
	return strings.TrimSpace(attr(n, "content"))
}

// resolveRef turns a possibly relative reference into an absolute URL.
func resolveRef(base *url.URL, ref string) string {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return ""
	}
	u, err := url.Parse(ref)
	if err != nil {
		return ""
	}
	return base.ResolveReference(u).String()
}

// atomTitle is the <title> element, named locally so callers outside this
// file need not import the atom package.
var atomTitle = atom.Title

// trimSiteSuffix drops a trailing site name from a document title, covering
// both the "Album | Site" and "Album - Site" conventions these hosts use.
func trimSiteSuffix(title string) string {
	return util.TrimTitleSuffix(title, " | ", " - ")
}

// folderName trims a title down to a readable directory name. The
// downloader sanitises every path component anyway; this only keeps a
// rambling title from becoming a rambling folder.
func folderName(title string) string {
	if runes := []rune(strings.TrimSpace(title)); len(runes) > 80 {
		return strings.TrimSpace(string(runes[:80]))
	}
	return strings.TrimSpace(title)
}

// humanSize matches the "57.80 MB" shape listings print next to a file.
var humanSize = regexp.MustCompile(`(?i)([0-9]+(?:\.[0-9]+)?)\s*(B|KB|MB|GB|TB|KIB|MIB|GIB|TIB)\b`)

// parseHumanSize converts a printed size to bytes, or -1 when there is none
// to read. Listings round these, so the result is a hint for the UI, not a
// length to hold a transfer to.
func parseHumanSize(text string) int64 {
	m := humanSize.FindStringSubmatch(text)
	if m == nil {
		return -1
	}
	value, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return -1
	}
	scale := map[string]float64{
		"B": 1, "KB": 1 << 10, "MB": 1 << 20, "GB": 1 << 30, "TB": 1 << 40,
		"KIB": 1 << 10, "MIB": 1 << 20, "GIB": 1 << 30, "TIB": 1 << 40,
	}[strings.ToUpper(m[2])]
	if scale == 0 {
		return -1
	}
	return int64(value * scale)
}

// firstTitleOf reads the document <title>, for pages whose own metadata
// omits a name.
func firstTitleOf(doc string) string {
	root, err := parseHTML(doc)
	if err != nil {
		return ""
	}
	return firstText(root, atomTitle)
}
