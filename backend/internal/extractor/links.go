package extractor

import (
	"context"
	"fmt"
	"net/url"
	"path"
	"regexp"
	"strings"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
	"github.com/JohanLindvall/HeapLeach/internal/util"
	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// Links reads a page for the file-host links it carries and resolves each one
// through the registry it belongs to.
//
// Every other extractor here answers "what is behind this one link". That is
// rarely the question: the hosts this build supports are precisely the ones
// people paste into a thread, and the thread is where they paste two hundred
// of them. This turns the thirty single-host extractors into one bulk
// operation without any of them knowing about it.
//
// It is deliberately not matched by host, and that is the whole of its
// interface design. A harvester that recognised pages on its own would quietly
// change what pasting an ordinary URL means — a video page that happens to
// list a hundred related videos would start queueing all of them — so it is
// reached only through an explicit prefix on the input:
//
//	links:https://board.example.test/threads/a-thread
//
// Only links this build has an extractor of its own for survive. Everything
// else is dropped rather than handed to the direct fallback, because the
// fallback matches everything: kept, a page's links would queue its navigation,
// its avatars and its stylesheets alongside the files. That is what
// Registry.Known answers and Registry.Find cannot.
//
// Links on the page's own host are kept along with the rest. When the page is
// an ordinary forum its own links are unsupported and vanish in that filter
// anyway, and when it is not — the index page of a supported tube, say — its
// own links are exactly what was being asked for. The price of the second case
// is that the site's navigation is supported too, and gets tried: harvesting
// one tube's front page found a hundred and fifty video pages and a dozen
// sign-in and sort-order pages, which fail, are skipped, and cost a request
// each. A failed request is the cheapest thing here.
//
// Be plain about the reach: this reads whatever a page serves an anonymous
// fetch, and nothing else. That is a great many pages, the big aggregator
// forums among them — both that were checked while this was written answered a
// logged-out client, one from behind a challenge the browser-shaped transport
// already passes. But a board is free to keep any of it back, and the boards
// worth harvesting are the likeliest to. What comes back then is the login
// page, whose links are all the board's own and so all unsupported, and that
// is reported as a page with nothing on it rather than as an empty thread.
//
// Nothing is resolved here beyond the page itself. Each File arrives from its
// own extractor exactly as that extractor built it — only the directory it
// lands in is changed — so it keeps its own Resolve closure, and that is the
// reason this composes at all: bunkr's twenty-minute signed links are still
// minted at transfer time, gofile's mirrors still rotate on each attempt, and
// mega's file key still travels with the file. A harvester that flattened
// results into plain URLs would break every one of those on a queue long
// enough to matter, which is the only kind of queue it produces.
type Links struct {
	client   *httpx.Client
	registry *Registry
}

const (
	// linksScheme is the prefix that asks for a harvest. It is a URL scheme
	// only in shape: ParseURL recognises it and wraps the rest as the page to
	// read.
	linksScheme = "links"
	linksPrefix = linksScheme + ":"

	// linksMaxSources bounds how many of a page's links are followed. Each one
	// is a full extraction, several requests at some hosts, and a real page
	// carries more of them than one might guess — a tube's own front page
	// measured just under two hundred. So this is set where following the lot
	// stops plausibly being what anybody asked for.
	linksMaxSources = 500
)

// NewLinks builds the harvester. It is handed the registry it is part of
// rather than building one, because what it resolves links with must be the
// same set of extractors the user already has.
func NewLinks(client *httpx.Client, registry *Registry) *Links {
	return &Links{client: client, registry: registry}
}

func (l *Links) Name() string { return linksScheme }

// Match accepts the harvest prefix and nothing else. ParseURL turns
// "links:<page>" into a URL with this scheme, so a host is never enough to
// reach the harvester by accident.
func (l *Links) Match(u *url.URL) bool { return u.Scheme == linksScheme }

// Extract harvests the page and merges what its links resolve to.
func (l *Links) Extract(ctx context.Context, u *url.URL, opts Options) (*Result, error) {
	page, err := ParseURL(linksTargetOf(u))
	if err != nil {
		return nil, fmt.Errorf("links: %w", err)
	}
	if page.Scheme == linksScheme {
		return nil, fmt.Errorf("links: %s stacks the prefix; one is all it takes", u.Redacted())
	}

	doc, err := l.client.GetString(ctx, page.String(), httpx.Referer(util.Origin(page)+"/"))
	if err != nil {
		return nil, fmt.Errorf("links: fetch %s: %w", page.Redacted(), err)
	}
	root, err := parseHTML(doc)
	if err != nil {
		return nil, fmt.Errorf("links: parse %s: %w", page.Redacted(), err)
	}

	candidates := linksCandidates(root, page)
	sources := l.supported(candidates)
	if len(sources) == 0 {
		return nil, fmt.Errorf("links: none of the %d links on %s point at a host this build "+
			"knows (the page may be showing a login rather than its content)",
			len(candidates), page.Redacted())
	}

	found := len(sources)
	if len(sources) > linksMaxSources {
		sources = sources[:linksMaxSources]
	}

	files := l.expand(ctx, sources, opts)
	if len(files) == 0 {
		return nil, fmt.Errorf("links: none of the %d supported links on %s resolved to a file "+
			"(they may all have expired)", len(sources), page.Redacted())
	}

	resolved := len(files)
	if len(files) > config.MaxListingFiles {
		files = files[:config.MaxListingFiles]
	}

	title := util.FirstNonEmpty(trimSiteSuffix(firstText(root, atomTitle)), page.Hostname()+page.Path)
	return &Result{
		Title: linksTitle(title, len(sources), found, len(files), resolved),
		Files: files,
	}, nil
}

// ------------------------------------------------------------- the prefix

// linksInput strips the harvest prefix from raw input, reporting whether it
// was there.
//
// Both "links:<page>" and "links://<page>" are accepted, because a prefix
// shaped like a scheme invites being typed like one, and a user who has just
// been told to write "links:" in front of a URL that itself begins "https://"
// is entitled to guess wrong about the slashes.
func linksInput(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	if len(raw) < len(linksPrefix) || !strings.EqualFold(raw[:len(linksPrefix)], linksPrefix) {
		return "", false
	}
	return strings.TrimPrefix(strings.TrimSpace(raw[len(linksPrefix):]), "//"), true
}

// IsHarvest reports whether a raw input asks for a harvest rather than naming
// a host of its own.
//
// The command line needs this before anything is parsed: a positional argument
// is either a source or the download directory, and told apart by its scheme.
// Without it "links:<page>" reads as a directory and the run creates one.
func IsHarvest(raw string) bool {
	_, ok := linksInput(raw)
	return ok
}

// linksURL validates the page a harvest names and wraps it as the URL the
// registry dispatches on. Called from ParseURL, which is the only place a raw
// input becomes a URL.
//
// The page is parsed first so a typo is refused where the job is submitted,
// with the same message any other bad URL gets, rather than minutes later in
// the background resolve.
func linksURL(raw string) (*url.URL, error) {
	page, err := ParseURL(raw)
	if err != nil {
		return nil, fmt.Errorf("links: %w", err)
	}

	// An https page is carried in the ordinary fields, so the harvest URL
	// answers Hostname() and Path like every other job's does and the UI has
	// something to show for it before the harvest returns. http is the
	// exception and is kept opaque instead: the ordinary form has nowhere to
	// record the scheme, and would silently promote the page to https on the
	// way back out — the manager stores a job's source as the URL's own
	// String(), so this has to survive the round trip exactly.
	if page.Scheme == "https" {
		harvest := *page
		harvest.Scheme = linksScheme
		return &harvest, nil
	}
	return &url.URL{Scheme: linksScheme, Opaque: page.String()}, nil
}

// linksTargetOf recovers the page from a harvest URL, undoing linksURL.
func linksTargetOf(u *url.URL) string {
	if u.Opaque != "" {
		return u.Opaque
	}
	page := *u
	page.Scheme = "https"
	return page.String()
}

// ------------------------------------------------------------ the harvest

// bareLink matches a URL printed as text rather than linked. Whitespace,
// quotes and brackets end it, which costs the occasional link that genuinely
// contains a bracket and saves every link that is merely followed by one.
var bareLink = regexp.MustCompile(`(?i)https?://[^\s"'<>()\[\]{}|^]+`)

// linksTrailing is the punctuation a sentence leaves stuck to a pasted URL.
const linksTrailing = `.,;:!?"'`

// linksCandidates collects the URLs a page offers as links, in the order the
// page gives them and without repeats.
//
// Three sources, and that set is narrower than "every href and src" for a
// measured reason. An extractor matches on the host, so a page's own furniture
// counts as a supported link and gets followed as though it were content: a
// stylesheet, a favicon, an avatar served from a host that happens to be
// supported. Sweeping every attribute of a supported tube's own front page
// turned up a hundred and ninety-three "sources", nearly all of them its CSS.
// What is left is:
//
//   - anchors, which are the links a reader can follow, and the only thing on
//     a forum page that a post actually put there;
//   - embedded frames, because a player framed in from ok.ru or streamtape is
//     a video somebody wants and appears nowhere else in the markup;
//   - text, because a board that will not linkify a post still prints the link
//     where a reader can see it, and a board that routes its anchors through a
//     redirector of its own puts the real URL in the anchor's text while the
//     href points at the redirector. Reading only anchors would harvest the
//     redirector.
//
// Script bodies are text like any other and are read as such: a page that
// keeps its posts in a JSON blob still names the hosts there, and a URL that
// is not a supported host is dropped a step later, so a generous sweep of the
// text costs nothing where a generous sweep of the attributes cost plenty.
//
// Fragments are kept. It is tempting to read "#..." as a page anchor and strip
// it for a tidier dedupe, but mega puts the decryption key there: a mega link
// without its fragment is a file nobody can open.
func linksCandidates(root *html.Node, base *url.URL) []string {
	var out []string
	seen := make(map[string]bool)
	add := func(ref string) {
		link := resolveRef(base, ref)
		if link == "" || seen[link] {
			return
		}
		// resolveRef resolves anything, including the mailto: and javascript:
		// references a page is full of, so the scheme is checked after it
		// rather than before.
		u, err := url.Parse(link)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			return
		}
		seen[link] = true
		out = append(out, link)
	}

	walk(root, func(n *html.Node) {
		switch {
		case n.Type == html.TextNode:
			for _, match := range bareLink.FindAllString(n.Data, -1) {
				add(strings.TrimRight(match, linksTrailing))
			}
		case isElem(n, atom.A), isElem(n, atom.Area):
			add(attr(n, "href"))
		case isElem(n, atom.Iframe), isElem(n, atom.Embed):
			add(attr(n, "src"))
		}
	})
	return out
}

// supported keeps the candidates that reach a real extractor.
//
// Registry.Find never returns nil, so asking it whether a link is supported
// answers yes for every link on the page; Known is the question that has an
// answer worth acting on. The harvester excludes itself on top of that: a
// harvest that harvested would walk the web from wherever it started.
func (l *Links) supported(candidates []string) []string {
	out := make([]string, 0, len(candidates))
	for _, link := range candidates {
		u, err := ParseURL(link)
		if err != nil {
			continue
		}
		ex, known := l.registry.Known(u)
		if !known {
			continue
		}
		if _, self := ex.(*Links); self {
			continue
		}
		out = append(out, link)
	}
	return out
}

// expand resolves every harvested link, several at a time.
//
// The bound is the one a listing expanded page-by-page uses, and for the same
// reason rather than by coincidence: a thread's two hundred links are usually
// two hundred links to the same host, so this is a burst at one host however
// many hosts the page names.
//
// Results are collected by index rather than appended as they arrive, so the
// job lists its files in the order the page did however the requests
// interleave. A link that will not resolve is skipped rather than failing the
// job: every thread of any age has dead links in it, and the live ones are
// still worth having.
func (l *Links) expand(ctx context.Context, sources []string, opts Options) []File {
	return FanOut(ctx, sources, func(ctx context.Context, link string) ([]File, error) {
		res, _, err := l.registry.Extract(ctx, link, opts)
		if err != nil {
			return nil, err
		}
		return linksFiles(res), nil
	})
}

// linksFiles takes one source's result into the harvest.
//
// Each File is copied through untouched but for its directory — resolver,
// cipher, headers and size all as its own extractor left them — because those
// are what make the file downloadable at all, and a harvest that rewrote them
// would be a harvest that only worked on hosts serving plain links.
//
// Every source gets a folder, even one holding a single file. The manager
// takes the opposite view for a job, and is right to: one file does not need a
// directory. A harvest is the case that inverts it, because the job root would
// otherwise be a heap of hundreds of files from unrelated sources with nothing
// but their names to say which came from where.
func linksFiles(res *Result) []File {
	folder := linksFolder(res.Title)
	files := make([]File, 0, len(res.Files))
	for _, f := range res.Files {
		f.Dir = path.Join(folder, f.Dir)
		files = append(files, f)
	}
	return files
}

// linksFolder reduces a source's title to one directory component.
//
// SafeName, which sanitises these properly, lives in the download package and
// the dependency runs the other way, so this does only the part that has to
// happen before the name is handed over: a title carrying a separator would be
// split by the manager's own SafeRelPath, and "A Creator / An Album" would
// arrive as two nested folders rather than one. What the name may contain
// otherwise is the manager's business, not an extractor's.
func linksFolder(title string) string {
	return strings.Map(func(r rune) rune {
		if r == '/' || r == '\\' {
			return '_'
		}
		return r
	}, strings.TrimSpace(title))
}

// linksTitle names the job, admitting anything the caps left out.
//
// An extractor has no logger and a Result carries nothing but a title and its
// files, so the title is the only place a partial answer can be declared — and
// it is a good one, being what names the job in the UI and the folder on disk,
// which is exactly where somebody comparing a thread against what they got
// will look. Silently returning the first thousand of three thousand files
// would be indistinguishable from the thread having a thousand.
func linksTitle(title string, sources, found, files, resolved int) string {
	var notes []string
	if sources < found {
		notes = append(notes, fmt.Sprintf("%d of %d links", sources, found))
	}
	if files < resolved {
		notes = append(notes, fmt.Sprintf("%d of %d files", files, resolved))
	}
	if len(notes) == 0 {
		return title
	}
	return title + " (" + strings.Join(notes, ", ") + ")"
}
