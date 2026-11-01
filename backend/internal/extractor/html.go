// SPDX-License-Identifier: MIT

package extractor

import (
	"encoding/json"
	"fmt"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"

	"github.com/JohanLindvall/HeapLeach/internal/config"
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

// findFirst returns the first node matching pred, or nil. It stops at the
// first match: recognisers ask this of every unrecognised page, and most of
// what they look for sits in the head.
func findFirst(root *html.Node, pred func(*html.Node) bool) *html.Node {
	if pred(root) {
		return root
	}
	for c := root.FirstChild; c != nil; c = c.NextSibling {
		if found := findFirst(c, pred); found != nil {
			return found
		}
	}
	return nil
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

// textOfFirst returns the text of the first node matching pred, or "" when
// nothing does — the shape every title lookup has, and one that otherwise
// needs a nil guard at each site.
func textOfFirst(root *html.Node, pred func(*html.Node) bool) string {
	if n := findFirst(root, pred); n != nil {
		return textOf(n)
	}
	return ""
}

// firstText returns the text of the first element with the given tag.
func firstText(root *html.Node, a atom.Atom) string {
	return textOfFirst(root, func(n *html.Node) bool { return isElem(n, a) })
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

// folderName trims a title down to a readable directory name, as one
// folder: its separators turned as sourceFolder turns them, since a slash
// in a post's title would otherwise nest it. The downloader sanitises every
// path component anyway; this keeps a rambling title from becoming a
// rambling folder. A label that names a folder untruncated — a season, a
// programme — goes through sourceFolder alone, which keeps the names
// existing downloads are filed under.
func folderName(title string) string {
	title = sourceFolder(title)
	if runes := []rune(title); len(runes) > config.MaxFolderNameRunes {
		return strings.TrimSpace(string(runes[:config.MaxFolderNameRunes]))
	}
	return title
}

// linkExt is the extension the name a link ends in carries, lowercased, or
// fallback where it names none a container would have — a dot and one to
// four characters. Many a host's file says what it is in its link and
// nowhere else, so this reads the link rather than writing the answer down;
// the fallback is what the host has always served there. The name is the
// path's last element, so a link ending in a slash still names its file.
func linkExt(link, fallback string) string {
	if ext := strings.ToLower(path.Ext(util.NameFromURL(link))); len(ext) > 1 && len(ext) <= 5 {
		return ext
	}
	return fallback
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

// matchedIDs is the first group of every match of each pattern in text, in
// turn, each id kept once where it first appears and empty ones dropped.
func matchedIDs(text string, res ...*regexp.Regexp) []string {
	var ids []string
	for _, re := range res {
		for _, m := range re.FindAllStringSubmatch(text, -1) {
			ids = append(ids, m[1])
		}
	}
	return util.Dedupe(ids)
}

// hasAnyClass reports whether n carries any of classes.
func hasAnyClass(n *html.Node, classes ...string) bool {
	return slices.ContainsFunc(classes, func(class string) bool { return hasClass(n, class) })
}

// rawText is the text an element holds, exactly as written: the payload of
// a script, whose JSON strings may hold names whose spacing is their own.
// textOf is for rendered display text and collapses whitespace, which
// inside a payload would change the names and paths read out of it.
func rawText(n *html.Node) string {
	var text strings.Builder
	for node := n.FirstChild; node != nil; node = node.NextSibling {
		if node.Type == html.TextNode {
			text.WriteString(node.Data)
		}
	}
	return text.String()
}

// scriptTexts is the raw text (rawText) of each script element under root.
func scriptTexts(root *html.Node) []string {
	var texts []string
	for _, script := range findAll(root, func(n *html.Node) bool { return isElem(n, atom.Script) }) {
		texts = append(texts, rawText(script))
	}
	return texts
}

// linkedData is the raw payload of each JSON-LD script under root, in the
// document's order. Schemas, required fields and which object counts stay
// with each reader: a page also carries breadcrumbs, recommendations and
// image objects in the same form.
func linkedData(root *html.Node) []string {
	var docs []string
	for _, script := range findAll(root, func(n *html.Node) bool {
		return isElem(n, atom.Script) && strings.Contains(strings.ToLower(attr(n, "type")), "ld+json")
	}) {
		docs = append(docs, jsonControlsEscaped(rawText(script)))
	}
	return docs
}

// jsonControlsEscaped is s with every control character inside a JSON
// string literal escaped, and any other whitespace outside one made a
// space. JSON forbids controls in a string, but page templates write a
// description's line breaks and tabs into JSON-LD as they are, and every
// browser and search engine reads such a block; a strict decoder would
// skip the whole of it, media URL and all. Outside a string JSON allows a
// space, tab, line break or carriage return, which are left alone; a
// template that indents with no-break spaces or leaves a form feed between
// tokens has those read as plain spaces, as a browser reads them.
func jsonControlsEscaped(s string) string {
	if !strings.ContainsFunc(s, func(r rune) bool { return r < 0x20 || r > 0x7f && unicode.IsSpace(r) }) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	inString, escaped := false, false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case !inString:
			if r, size := utf8.DecodeRuneInString(s[i:]); unicode.IsSpace(r) && !jsonSpace(r) {
				b.WriteByte(' ')
				i += size - 1
				continue
			}
			inString = c == '"'
		case escaped:
			escaped = false
		case c == '\\':
			escaped = true
		case c == '"':
			inString = false
		case c < 0x20:
			switch c {
			case '\n':
				b.WriteString(`\n`)
			case '\r':
				b.WriteString(`\r`)
			case '\t':
				b.WriteString(`\t`)
			default:
				fmt.Fprintf(&b, `\u%04x`, c)
			}
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}

// jsonSpace reports whether r is whitespace JSON allows between tokens.
func jsonSpace(r rune) bool { return r == ' ' || r == '\t' || r == '\n' || r == '\r' }

// jsonAttr decodes the JSON held in the first element under root carrying
// the attribute name — an Inertia app's data-page, say — into out. found is
// false when no element carries it; err is the decoding's. The parser has
// already decoded the attribute's quoting and entities.
func jsonAttr(root *html.Node, name string, out any) (found bool, err error) {
	node := findFirst(root, func(n *html.Node) bool { return hasAttr(n, name) })
	if node == nil {
		return false, nil
	}
	return true, json.Unmarshal([]byte(attr(node, name)), out)
}
