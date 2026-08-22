package extractor

import (
	"context"
	"fmt"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
	"github.com/JohanLindvall/HeapLeach/internal/util"
	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// Open directories: the listing a web server generates for a directory that
// has no index page of its own.
//
// This is the one extractor that needs to know nothing about any host, and
// that is the point of it. A server publishing a directory describes it
// completely — every name, every size, and which entries are directories —
// and while the markup is not standardised it varies narrowly: Apache,
// nginx, lighttpd and the rest all print one link per entry with the size
// beside it, inside either a <pre> block or a table row. Pasted in without
// this, such a URL downloads the index page itself: a few kilobytes of HTML
// named after the directory.
//
// Recognising one is the only real decision here, because being wrong is
// expensive in one direction — a page mistaken for a listing is a file the
// user asked for and did not get. The <title> is the reliable marker and
// nothing else on the page is. Every generator writes the directory into it
// ("Index of /pub" from Apache and nginx verbatim, "Directory listing for
// /pub" from Python's server, "<item> directory listing" from archive.org),
// while the bodies share no element, class or attribute at all. So the title
// decides, tolerantly, and the structure is only read once it has said yes.
//
// Which links belong to the listing is decided on the *resolved* URL rather
// than on the text of the href: a child is a URL on the same host whose path
// is this directory's path plus exactly one more segment. That is a single
// rule for a relative href ("clip.mp4"), for a listing that writes its links
// absolutely, and for every spelling of the parent — "../", "/", "/pub/" —
// which all resolve outside the directory and are dropped without ever being
// named. Apache's column-sort links (?C=N;O=D) carry a query and go the same
// way. The rule is also the guarantee that a crawl cannot wander off the
// directory it was pointed at, whatever the page asks for.
//
// The crawl is bounded, because a directory tree is not, and which bound
// catches what is worth being clear about. A symlink pointing at its own
// parent is published as an infinitely deep tree that no listing admits to,
// and the depth cap is the only thing that stops it: every traversal of such
// a loop has a fresh, longer URL, so nothing that remembers where it has been
// can recognise it. The file and directory caps bound the other runaway, the
// tree that is merely enormous. The visited set is kept alongside them for
// what it states rather than what it catches — a directory is read once,
// however many listings name it — since the child rule above already makes
// every descendant's URL longer than its parent's.
//
// Two other things were expected to come along for free, and only one does.
// archive.org's /download/<id> is a genuine autoindex — a table of relative
// links carrying Apache's own abbreviated sizes — so it is claimed on that
// path shape. An IPFS gateway is not: kubo's directory template writes its
// links root-relative to whatever base the gateway rewrites to, and puts the
// size in a sibling of the link rather than beside it, so a gateway that
// rewrites yields nothing here. One that does not, and titles the page after
// the directory it is listing, is picked up by the title rule below. Anything
// beyond that belongs in an extractor of its own.
type Autoindex struct {
	client *httpx.Client
}

// NewAutoindex builds the open-directory extractor.
func NewAutoindex(client *httpx.Client) *Autoindex { return &Autoindex{client: client} }

func (a *Autoindex) Name() string { return "autoindex" }

// Match claims the URL shapes that are an autoindex by construction. Nothing
// else can be settled from a URL alone, which is what autoindexSniff is for.
func (a *Autoindex) Match(u *url.URL) bool { return autoindexArchiveOrg(u) }

// Extract crawls the directory the URL names.
func (a *Autoindex) Extract(ctx context.Context, u *url.URL, _ Options) (*Result, error) {
	return a.crawl(ctx, u)
}

// autoindexArchiveOrg reports whether a URL is an archive.org item listing.
//
// An item's /download/<id> is served as an ordinary autoindex, with or
// without a trailing slash. Deeper than that the shape stops being reliable:
// /download/<id>/<name> is a file inside the item, and claiming it would
// replace a working download with a page that is not a listing. So a deeper
// path is only taken when it ends in a slash and therefore says it is a
// directory.
func autoindexArchiveOrg(u *url.URL) bool {
	if !util.HostMatches(u.Host, "archive.org") {
		return false
	}
	segs := util.PathSegments(u)
	if len(segs) < 2 || segs[0] != "download" {
		return false
	}
	return len(segs) == 2 || strings.HasSuffix(u.Path, "/")
}

// autoindexSniff resolves a URL that turns out to be an open directory.
//
// It belongs to the fallback for the same reason kvsSniff does: nothing about
// an open directory says which host it is on, so there is no list to be on
// and no Match that could recognise one. A page that is not a listing falls
// straight through to the direct-link handling that would have run anyway, so
// being wrong costs one GET.
//
// Only a path ending in a slash is looked at, and that restraint is
// deliberate. A trailing slash means a directory everywhere, and a URL
// without one may well be a signed link that is only good once — spending a
// speculative GET on it to ask a question the URL already answered would be a
// poor trade.
func autoindexSniff(ctx context.Context, client *httpx.Client, u *url.URL) (*Result, bool) {
	if !strings.HasSuffix(u.Path, "/") && u.Path != "" {
		return nil, false
	}
	res, err := (&Autoindex{client: client}).crawl(ctx, u)
	if err != nil || res == nil || len(res.Files) == 0 {
		return nil, false
	}
	return res, true
}

// ------------------------------------------------------------------ crawl

// autoindexNode is one directory of the tree being read.
type autoindexNode struct {
	// base is the directory URL, always ending in a slash, which is what the
	// listing's links resolve against.
	base *url.URL
	// rel is the path from the directory the user asked for, and becomes the
	// File's Dir so the tree lands on disk the way it is published.
	rel string
	// files are this directory's own entries, in listing order.
	files []File
	// subs are the subdirectories actually descended into, in listing order,
	// and pending those named before the budget had its say.
	subs    []*autoindexNode
	pending []autoindexEntry
}

// autoindexEntry is one line of a listing.
type autoindexEntry struct {
	name   string
	url    *url.URL
	dir    bool
	size   int64
	approx bool
}

// crawl reads the directory and everything under it.
//
// The tree is read a level at a time rather than depth-first, for two
// reasons. A level's directories are independent, so they are fetched
// several at a time; and the budget is then applied between levels, where
// the counts are settled, so which files a truncated crawl returns does not
// depend on how the requests happened to interleave.
func (a *Autoindex) crawl(ctx context.Context, u *url.URL) (*Result, error) {
	root := &autoindexNode{base: autoindexBase(u)}
	if err := a.read(ctx, root); err != nil {
		return nil, err
	}

	// Keyed on the URL that would actually be fetched, so two spellings of
	// one directory count as one whatever the listing wrote. What it cannot
	// see is a symlinked loop, whose every traversal has a longer URL than
	// the last; the depth cap is what ends that one.
	visited := map[string]bool{root.base.String(): true}
	budget := autoindexBudget{dirs: 1, files: len(root.files)}

	level := []*autoindexNode{root}
	for depth := 1; depth <= config.MaxAutoindexDepth; depth++ {
		next := autoindexDescend(level, visited, &budget)
		if len(next) == 0 {
			break
		}
		a.readAll(ctx, next)
		for _, node := range next {
			budget.files += len(node.files)
		}
		level = next
	}

	files := autoindexFlatten(root, nil)
	if len(files) > config.MaxAutoindexFiles {
		files = files[:config.MaxAutoindexFiles]
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("autoindex: %s lists no files", root.base.Redacted())
	}
	return &Result{Title: autoindexTitle(root.base), Files: files}, nil
}

// autoindexBudget is what stops a crawl that would not stop on its own.
type autoindexBudget struct {
	dirs  int
	files int
}

// spent reports whether the crawl has read as much as it is allowed to.
func (b *autoindexBudget) spent() bool {
	return b.dirs >= config.MaxAutoindexDirs || b.files >= config.MaxAutoindexFiles
}

// autoindexDescend turns the subdirectories a level named into the next
// level, in listing order, and is the only place the budget and the cycle
// guard are consulted.
func autoindexDescend(level []*autoindexNode, visited map[string]bool, budget *autoindexBudget) []*autoindexNode {
	var next []*autoindexNode
	for _, node := range level {
		for _, entry := range node.pending {
			if budget.spent() {
				return next
			}
			key := entry.url.String()
			if visited[key] {
				continue
			}
			visited[key] = true
			budget.dirs++

			child := &autoindexNode{base: entry.url, rel: path.Join(node.rel, entry.name)}
			node.subs = append(node.subs, child)
			next = append(next, child)
		}
		node.pending = nil
	}
	return next
}

// readAll reads a level's directories several at a time.
//
// A subdirectory that will not load, or that answers with something that is
// not a listing, is skipped rather than failing the crawl: on a tree of
// hundreds the rest is still worth having, and the root — the only page the
// user actually named — was already read on its own, where its failure is
// reported.
func (a *Autoindex) readAll(ctx context.Context, nodes []*autoindexNode) {
	var wg sync.WaitGroup
	slots := make(chan struct{}, config.PageFetchConcurrency)
	for _, node := range nodes {
		wg.Add(1)
		go func(node *autoindexNode) {
			defer wg.Done()
			select {
			case slots <- struct{}{}:
				defer func() { <-slots }()
			case <-ctx.Done():
				return
			}
			_ = a.read(ctx, node)
		}(node)
	}
	wg.Wait()
}

// read fetches one directory and fills in its files and subdirectories.
func (a *Autoindex) read(ctx context.Context, node *autoindexNode) error {
	page := node.base.String()
	doc, err := a.client.GetString(ctx, page, httpx.Referer(util.Origin(node.base)+"/"))
	if err != nil {
		return fmt.Errorf("autoindex: fetch %s: %w", node.base.Redacted(), err)
	}
	root, err := parseHTML(doc)
	if err != nil {
		return fmt.Errorf("autoindex: parse %s: %w", node.base.Redacted(), err)
	}
	if !autoindexIsListing(root, node.base) {
		return fmt.Errorf("autoindex: %s is a web page, not a directory listing", node.base.Redacted())
	}

	for _, entry := range autoindexEntries(root, node.base) {
		if entry.dir {
			node.pending = append(node.pending, entry)
			continue
		}
		node.files = append(node.files, File{
			Name:       entry.name,
			URL:        entry.url.String(),
			Size:       entry.size,
			SizeApprox: entry.approx,
			Dir:        node.rel,
			Headers:    httpx.Referer(page),
		})
	}
	return nil
}

// autoindexFlatten collects the tree in listing order: a directory's own
// files, then each subdirectory in the order the listing named it.
func autoindexFlatten(node *autoindexNode, out []File) []File {
	out = append(out, node.files...)
	for _, sub := range node.subs {
		out = autoindexFlatten(sub, out)
	}
	return out
}

// autoindexTitle names the job after the directory, falling back to the host
// for a listing served at the root.
func autoindexTitle(base *url.URL) string {
	segs := util.PathSegments(base)
	if len(segs) > 0 {
		return segs[len(segs)-1]
	}
	return base.Hostname()
}

// ---------------------------------------------------------------- parsing

// autoindexBase is the directory a listing's links resolve against: the
// requested path with a trailing slash, and no query or fragment.
//
// The slash is added rather than assumed because a server serves the same
// listing at /pub and /pub/ (archive.org does, for one) while a relative link
// on the page means the same thing either way. Resolving "clip.mp4" against
// the un-slashed form would put it in the parent directory.
func autoindexBase(u *url.URL) *url.URL {
	base := *u
	base.RawQuery, base.ForceQuery = "", false
	base.Fragment, base.RawFragment = "", ""
	if !strings.HasSuffix(base.Path, "/") {
		base.Path += "/"
		if base.RawPath != "" {
			base.RawPath += "/"
		}
	}
	return &base
}

// autoindexMarkers are the ways a generated listing names itself in its
// title. They are matched case-insensitively and anywhere in the string,
// because the generators wrap the same words differently: Apache and nginx
// lead with them, archive.org trails them after the item's name.
var autoindexMarkers = []string{"index of", "directory listing", "directory index"}

// autoindexIsListing reports whether a document is a generated listing.
//
// The title is the whole test. Every other candidate marker fails on some
// generator: the heading is absent from some templates and rewritten by
// anyone who themed theirs, the <pre> block is a table elsewhere, and the
// entries themselves cannot decide the question — a page of links to its own
// subpaths is what most sites are.
func autoindexIsListing(root *html.Node, base *url.URL) bool {
	title := strings.TrimSpace(firstText(root, atomTitle))

	// A gateway-generated index names the page after the directory it lists
	// and says nothing else. Only the full path counts: a page titled after
	// the last segment alone would be every second page on the web.
	if title != "" && (title == base.Path || title == strings.TrimSuffix(base.Path, "/")) {
		return true
	}

	// The heading is worth a look after the title, for a template that put
	// the words there and the bare directory name in the title.
	for _, text := range []string{title, firstText(root, atom.H1)} {
		lower := strings.ToLower(text)
		for _, marker := range autoindexMarkers {
			if strings.Contains(lower, marker) {
				return true
			}
		}
	}
	return false
}

// autoindexEntries reads every entry a listing names, in document order.
func autoindexEntries(root *html.Node, base *url.URL) []autoindexEntry {
	var out []autoindexEntry
	seen := make(map[string]bool)
	for _, a := range findAll(root, func(n *html.Node) bool { return isElem(n, atom.A) }) {
		name, target, ok := autoindexChild(base, attr(a, "href"))
		if !ok || seen[target.String()] {
			continue
		}
		seen[target.String()] = true

		entry := autoindexEntry{
			name: name,
			url:  target,
			dir:  strings.HasSuffix(target.Path, "/"),
			size: -1,
		}
		if !entry.dir {
			entry.size, entry.approx = autoindexEntrySize(a)
		}
		out = append(out, entry)
	}
	return out
}

// autoindexChild resolves an href and reports whether it names an entry of
// this directory — a URL on the same host, one path segment below the base.
//
// Everything a listing carries that is not an entry falls out of that one
// test: the parent link however it is spelled, the sort links (which carry a
// query), the site's own navigation, and any link off the host entirely.
func autoindexChild(base *url.URL, href string) (string, *url.URL, bool) {
	href = strings.TrimSpace(href)
	if href == "" {
		return "", nil, false
	}
	ref, err := url.Parse(href)
	if err != nil {
		return "", nil, false
	}
	if ref.RawQuery != "" || ref.ForceQuery || ref.Fragment != "" {
		return "", nil, false
	}

	target := base.ResolveReference(ref)
	if target.Scheme != base.Scheme || target.Host != base.Host {
		return "", nil, false
	}
	rest, ok := strings.CutPrefix(target.Path, base.Path)
	if !ok {
		return "", nil, false
	}
	// The comparison is on the decoded path, so a name that encodes a
	// separator (%2F) is rejected here rather than escaping the directory
	// later. Nothing below the download root may be written outside it.
	name := strings.TrimSuffix(rest, "/")
	if name == "" || name == "." || name == ".." || strings.Contains(name, "/") {
		return "", nil, false
	}
	return name, target, true
}

// autoindexEntrySize reads the size a listing prints beside an entry.
//
// The two layouts need different reaching. In a table the size is a cell of
// its own, and which cell varies — Apache puts a description after it,
// lighttpd a MIME type — so every cell but the entry's own is tried and the
// last one that yields a size wins, which is order-independent and rejects
// the date column on its own. In a <pre> block the size is simply the rest of
// the line.
func autoindexEntrySize(a *html.Node) (int64, bool) {
	row := autoindexRow(a)
	if row == nil {
		return autoindexSize(autoindexTail(a))
	}
	size, approx := int64(-1), false
	for cell := row.FirstChild; cell != nil; cell = cell.NextSibling {
		if !isElem(cell, atom.Td) && !isElem(cell, atom.Th) {
			continue
		}
		if autoindexContains(cell, a) {
			continue
		}
		if n, ap := autoindexSize(textOf(cell)); n >= 0 {
			size, approx = n, ap
		}
	}
	return size, approx
}

// autoindexRow finds the table row an entry sits in, or nil when it is not
// in a table at all.
func autoindexRow(n *html.Node) *html.Node {
	for p := n.Parent; p != nil; p = p.Parent {
		switch {
		case isElem(p, atom.Tr):
			return p
		case isElem(p, atom.Table), isElem(p, atom.Body):
			return nil
		}
	}
	return nil
}

// autoindexContains reports whether target is inside root.
func autoindexContains(root, target *html.Node) bool {
	for p := target; p != nil; p = p.Parent {
		if p == root {
			return true
		}
	}
	return false
}

// autoindexTail is the text a <pre> listing prints after a link, up to the
// end of that line.
//
// Stopping at the newline is what keeps one entry's size from being read off
// the next entry's row, since the whole block is one run of text with the
// links embedded in it. Stopping at the next link matters for the same
// reason in templates that put no newline between entries.
func autoindexTail(a *html.Node) string {
	var b strings.Builder
	for n := a.NextSibling; n != nil; n = n.NextSibling {
		if isElem(n, atom.A) || isElem(n, atom.Br) {
			break
		}
		if n.Type != html.TextNode {
			b.WriteString(" " + textOf(n))
			continue
		}
		if i := strings.IndexByte(n.Data, '\n'); i >= 0 {
			b.WriteString(n.Data[:i])
			break
		}
		b.WriteString(n.Data)
	}
	return b.String()
}

// autoindexShortSize matches the abbreviation Apache prints, where the unit
// is a bare letter: "14M", "404K", "1.4G".
var autoindexShortSize = regexp.MustCompile(`(?i)^([0-9]+(?:\.[0-9]+)?)([KMGTP])$`)

// autoindexScale is the multiplier for those letters. Every generator here
// abbreviates in binary units however it spells them.
var autoindexScale = map[string]float64{
	"K": 1 << 10, "M": 1 << 20, "G": 1 << 30, "T": 1 << 40, "P": 1 << 50,
}

// autoindexSize reads a printed size, reporting -1 when there is none and
// whether what it read had been rounded.
//
// That second answer is the reason this is not simply parseHumanSize. A
// number with no unit has not been rounded — it is the byte count itself, and
// nginx prints every size that way while Apache does so for anything under a
// kilobyte — which is an exact length the downloader may use to conclude that
// a file already on disk is this one. A number with a unit has been cut to
// three digits and can only ever be a hint. A directory's "-" is neither and
// reads as no size at all.
func autoindexSize(text string) (int64, bool) {
	fields := strings.Fields(text)
	if len(fields) == 0 {
		return -1, false
	}
	last := fields[len(fields)-1]

	if n, err := strconv.ParseInt(last, 10, 64); err == nil {
		return n, false
	}
	// The shared parser does not recognise a lone letter, and should not:
	// loose in a page "14M" is as likely to be a view count as a size. At the
	// end of a listing row it is unambiguous.
	if m := autoindexShortSize.FindStringSubmatch(last); m != nil {
		if value, err := strconv.ParseFloat(m[1], 64); err == nil {
			if scale := autoindexScale[strings.ToUpper(m[2])]; scale > 0 {
				return int64(value * scale), true
			}
		}
	}
	if n := parseHumanSize(text); n >= 0 {
		return n, true
	}
	return -1, false
}
