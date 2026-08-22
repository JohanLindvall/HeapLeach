package extractor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
	"github.com/JohanLindvall/HeapLeach/internal/util"
	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// FoolFuuka installs: the 4chan archives.
//
// A thread on 4chan falls off its board within hours and takes its images
// with it, which is the one thing fourchan.go can do nothing about. The
// archives are where the threads worth keeping persist, and they all run the
// same software — FoolFuuka, whose JSON API is deliberately modelled on
// 4chan's own. So one core covers every install and adding one costs a line
// in a table, exactly as kvssites.go does for a tube platform.
//
// Reading the API rather than the pages is not a preference here, it is the
// only way in. Desuarchive — the largest of them, and the one most links
// point at — answers an ordinary client with a Cloudflare interstitial on
// every board page, thread page and search page it has, while leaving /_/api/
// open to anyone. A scraper would fail on precisely the archive most worth
// having.
//
// What the API hands back is better than 4chan's own. Every attachment
// carries the absolute link to the stored file, the poster's original
// filename, and an exact byte length: nothing has to be rebuilt from a
// timestamp, and nothing is rounded, so Size is exact and SizeApprox stays
// false. That was checked rather than assumed — wherever the media host sends
// a Content-Length at all, it agrees with media_size to the byte.
//
// Three things about the responses have to be read carefully:
//
//   - An error arrives with a 200 status. Both "Thread not found." and "No
//     results found." come back as {"error":"..."} over a perfectly ordinary
//     200; only an unknown board manages a 422. A request that succeeded at
//     the HTTP level has therefore said nothing yet, and treating the decode
//     as the answer would report a missing thread as "no downloadable files",
//     which is both wrong and leaves the user nothing to act on.
//
//   - A thread's replies arrive as a JSON *object* keyed by post number, not
//     as a list. Decoded into a Go map and ranged over, a thread would list
//     its files in a different order on every run, so they are sorted — by
//     the number's value rather than its text, because a board's post numbers
//     gain digits over its lifetime and "9999" sorts after "10000" otherwise.
//
//   - The top-level object's keys carry the board's own ordering, which a map
//     would throw away just as thoroughly. It is walked with a token decoder
//     instead. See foolFuukaDecode.
type FoolFuuka struct {
	client *httpx.Client
	site   foolFuukaSite
}

// foolFuukaSite is one install.
type foolFuukaSite struct {
	// name is the UI label and the prefix on every error this raises.
	name string
	// root is the origin every API call is addressed to, which is not
	// necessarily the host the user pasted — see the rbt.asia entry.
	root string
	// domains are the hosts routed here.
	domains []string
}

// foolFuukaSites are the installs this build ships knowing about.
//
// The list is short, and it is short for a reason worth stating plainly: on
// most of the archives that still exist, the endpoint this needs is the one
// that is shut. Asking whether a host answers is not the same question as
// asking whether it works, and the two come apart here in three separate
// ways, so each candidate was taken all the way through — thread listed,
// file transferred — before it earned a line.
var foolFuukaSites = []foolFuukaSite{
	// rbt.asia is no longer an install of its own. Every path on it, the API
	// included, 302s to desuarchive — so it is listed as another domain of
	// that install rather than as an archive in its own right. The link
	// someone saved years ago still resolves, and the job is named after the
	// archive that actually holds the posts.
	{name: "desuarchive", root: "https://desuarchive.org", domains: []string{"desuarchive.org", "rbt.asia"}},
	{name: "palanq", root: "https://archive.palanq.win", domains: []string{"archive.palanq.win"}},
}

// Deliberately absent. Each is a live host that answers, which is exactly
// what makes listing one a trap:
//
//   - thebarchive.com and archiveofsins.com serve /_/api/chan/index/ to
//     anyone and answer /_/api/chan/thread/ and /_/api/chan/search/ with a
//     Cloudflare challenge. So the endpoint that lists a board is open and
//     the endpoint that opens a thread is not, which is the wrong way round
//     for a downloader: what a person pastes is a thread. Their HTML thread
//     pages are still served, so a scraper could reach them — but it would
//     be reading rounded sizes off a page instead of exact ones out of JSON,
//     which is most of what this file is for.
//
//   - arch.b4k.dev has the opposite shape. Its API is entirely open; it is
//     the media host, arch-img.b4k.dev, that sits behind a bot rule. A few
//     files transfer and then every request answers with the challenge page,
//     and it does not lapse — six requests spaced twenty seconds apart, five
//     minutes after the last one, were all refused. A queue of a hundred
//     images would fetch three, which reads as a broken downloader rather
//     than as a host that turned us away.
//
//   - boards.fireden.net is not merely frozen at 2022, which would be fine
//     for old threads. Of its boards only /sci/ still answers the API at
//     all; the rest report "No board selected." or 500. And every media_link
//     on the posts it does return is a 404, because img-lb.fireden.net
//     serves nothing any more — while the API goes on reporting media_status
//     "normal" and an exact size for each of them. Nothing short of fetching
//     one reveals that the file is gone.
//
//   - archive.4plebs.org and archived.moe answer 403 to a client that cannot
//     pass a challenge, on every path. Left out the way booru.go leaves out
//     danbooru.donmai.us: a name in this list is a promise.
//
// HEAPLEACH_FOOLFUUKA_HOSTS adds any of them back for whoever can reach them,
// which is the same escape kvssites.go offers and exists for the same reason:
// what these hosts do is decided by their operators, not by a release.

// FoolFuuka's read-only API. The paths are fixed across installs — that is
// what makes one core enough — and only the origin changes.
const (
	foolFuukaThreadAPI = "/_/api/chan/thread/"
	foolFuukaIndexAPI  = "/_/api/chan/index/"
	foolFuukaSearchAPI = "/_/api/chan/search/"
	foolFuukaPostAPI   = "/_/api/chan/post/"
)

// foolFuukaNormal is the only media_status worth queueing. A row marked
// anything else — a banned image, a purged one — has a name and a size in the
// database and nothing behind them.
//
// The converse does not hold, and it is worth not expecting it to: "normal"
// is what the archive recorded when it took the post, not a claim about what
// is on its disk today. Desuarchive answers for /wsg/ videos with exact sizes
// and a normal status and then 404s every one of them, while the images
// beside them fetch. So the status is a filter, never a guarantee — a file
// that turns out to be gone fails visibly at transfer time, which is the only
// place the truth is available.
const foolFuukaNormal = "normal"

// foolFuukaMaxBoardLen bounds what a first path segment may be and still be
// taken for a board. 4chan's longest is "trash", at five; the bound is what
// keeps the site's own furniture ("foolfuuka", "gallery") from being sent to
// the API as though it named a board.
const foolFuukaMaxBoardLen = 5

// NewFoolFuukaSites builds one extractor per install, so each is matched on
// its own hosts and named for itself in the UI, exactly as a hand-written
// host would be.
//
// HEAPLEACH_FOOLFUUKA_HOSTS appends installs at runtime. An archive named that
// way is addressed at its own origin and labelled by its first domain label,
// which is all the table itself carries for the ones compiled in.
func NewFoolFuukaSites(cfg *config.Config, client *httpx.Client) []Extractor {
	sites := foolFuukaSites
	if cfg != nil && len(cfg.ExtraHostsFor(config.FamilyFoolFuuka)) > 0 {
		sites = append([]foolFuukaSite(nil), sites...)
		for _, host := range util.Dedupe(cfg.ExtraHostsFor(config.FamilyFoolFuuka)) {
			host = strings.ToLower(strings.TrimSpace(host))
			// A host already covered is left alone rather than registered a
			// second time under a label of its own, which would give two
			// extractors one name and make a failure impossible to attribute.
			if host == "" || foolFuukaKnown(sites, host) {
				continue
			}
			sites = append(sites, foolFuukaSite{
				name:    foolFuukaLabel(host),
				root:    "https://" + host,
				domains: []string{host},
			})
		}
	}

	out := make([]Extractor, 0, len(sites))
	for _, site := range sites {
		out = append(out, &FoolFuuka{client: client, site: site})
	}
	return out
}

// foolFuukaKnown reports whether a host is already covered, so naming one of
// the compiled-in archives in the environment does not register it twice
// under a second label.
func foolFuukaKnown(sites []foolFuukaSite, host string) bool {
	for _, site := range sites {
		for _, domain := range site.domains {
			if util.HostMatches(host, domain) {
				return true
			}
		}
	}
	return false
}

// foolFuukaLabel names a runtime host by its first label, the way kvssites.go
// names a tube install. It is the part that distinguishes one archive from
// another, whether the host is a bare domain or a subdomain of one.
func foolFuukaLabel(host string) string {
	label, _, _ := strings.Cut(host, ".")
	return label
}

func (f *FoolFuuka) Name() string { return f.site.name }

func (f *FoolFuuka) Match(u *url.URL) bool {
	for _, domain := range f.site.domains {
		if util.HostMatches(u.Host, domain) {
			return true
		}
	}
	return false
}

// Extract resolves a thread, a single post, a board index page or a search.
func (f *FoolFuuka) Extract(ctx context.Context, u *url.URL, _ Options) (*Result, error) {
	target, err := foolFuukaParse(u)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", f.site.name, err)
	}
	switch target.kind {
	case foolFuukaThreadTarget:
		return f.thread(ctx, target)
	case foolFuukaPostTarget:
		return f.post(ctx, target)
	case foolFuukaSearchTarget:
		return f.search(ctx, target)
	default:
		return f.index(ctx, target)
	}
}

// ------------------------------------------------------------------ targets

// The four things a FoolFuuka URL can name.
const (
	foolFuukaIndexTarget = iota
	foolFuukaThreadTarget
	foolFuukaPostTarget
	foolFuukaSearchTarget
)

// foolFuukaTarget is a parsed input URL.
type foolFuukaTarget struct {
	kind  int
	board string
	// num is the thread or post number, for the two targets that name one.
	num string
	// page is the index or search page, one-based as the site writes it.
	page int
	// search is the query the API is handed, built from the path.
	search url.Values
	// label describes the search in the job title.
	label string
}

// foolFuukaParse reads an input URL by its shape alone.
//
// By shape alone because the page behind it cannot be fetched to check: the
// installs that matter answer their own HTML with a challenge. Everything
// here is therefore decided from the path, and a path that fits none of the
// forms is reported with the forms spelled out rather than guessed at.
func foolFuukaParse(u *url.URL) (foolFuukaTarget, error) {
	var t foolFuukaTarget
	segs := util.PathSegments(u)
	if len(segs) == 0 || !foolFuukaIsBoard(segs[0]) {
		return t, fmt.Errorf("%s names no board — expected /<board>/thread/<number>/, "+
			"/<board>/post/<number>/, /<board>/search/text/<query>/ or /<board>/", u.Redacted())
	}
	t.board, t.page = segs[0], 1
	rest := segs[1:]

	switch {
	case len(rest) == 0:
		t.kind = foolFuukaIndexTarget
	case rest[0] == "thread" && len(rest) >= 2:
		t.kind, t.num = foolFuukaThreadTarget, rest[1]
	case rest[0] == "post" && len(rest) >= 2:
		t.kind, t.num = foolFuukaPostTarget, rest[1]
	case rest[0] == "page" && len(rest) >= 2:
		t.kind, t.page = foolFuukaIndexTarget, foolFuukaPageNumber(rest[1])
	case rest[0] == "search":
		t.kind = foolFuukaSearchTarget
		t.search, t.page, t.label = foolFuukaSearchQuery(rest[1:])
		if len(t.search) == 0 {
			return t, fmt.Errorf("%s is a search with no terms in it", u.Redacted())
		}
	default:
		return t, fmt.Errorf("%s is not a thread, post, search or board page on this archive", u.Redacted())
	}
	return t, nil
}

// foolFuukaIsBoard reports whether a path segment is shaped like a board
// name, which on 4chan is a short run of lowercase letters and digits.
func foolFuukaIsBoard(seg string) bool {
	if seg == "" || len(seg) > foolFuukaMaxBoardLen {
		return false
	}
	for _, r := range seg {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') {
			return false
		}
	}
	return true
}

// foolFuukaSearchQuery turns a search path into the API's own parameters.
//
// FoolFuuka writes a search as alternating name and value segments —
// /search/text/<query>/page/2/ — and the search endpoint takes those same
// names as query parameters, so they are passed through rather than mapped.
// That is what keeps the less-used ones (image, filename, subject, deleted,
// start, end) working without this file having to know they exist.
//
// A trailing name with no value is dropped: it is what a half-typed URL looks
// like, and sending it would ask the archive to search for nothing.
func foolFuukaSearchQuery(segs []string) (url.Values, int, string) {
	query := url.Values{}
	page := 1
	var label []string
	for i := 0; i+1 < len(segs); i += 2 {
		name, value := segs[i], segs[i+1]
		if name == "page" {
			page = foolFuukaPageNumber(value)
			continue
		}
		if name == "" || value == "" {
			continue
		}
		query.Set(name, value)
		label = append(label, value)
	}
	return query, page, strings.Join(label, " ")
}

// foolFuukaPageNumber reads a page from a path segment, treating anything
// unreadable as the first page rather than as an error: a stray segment is a
// worse reason to refuse a board than it is to start at the beginning.
func foolFuukaPageNumber(seg string) int {
	if n, err := strconv.Atoi(seg); err == nil && n > 0 {
		return n
	}
	return 1
}

// ----------------------------------------------------------------- routes

// thread lists every attachment in one thread, in the order it was posted.
func (f *FoolFuuka) thread(ctx context.Context, t foolFuukaTarget) (*Result, error) {
	entries, err := f.call(ctx, foolFuukaThreadAPI, url.Values{
		"board": {t.board}, "num": {t.num},
	})
	if err != nil {
		return nil, fmt.Errorf("%s: /%s/ thread %s: %w", f.site.name, t.board, t.num, err)
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("%s: /%s/ thread %s is not in this archive", f.site.name, t.board, t.num)
	}

	posts := entries[0].posts()
	files := f.files(posts, "")
	if len(files) == 0 {
		return nil, fmt.Errorf("%s: /%s/ thread %s has no attachments left "+
			"(%d posts, none of them holding a file this can fetch)",
			f.site.name, t.board, t.num, len(posts))
	}

	title := ""
	if op := entries[0].Thread.OP; op != nil {
		title = strings.TrimSpace(op.Title)
	}
	return &Result{Title: util.FirstNonEmpty(title, t.board+"-"+t.num), Files: files}, nil
}

// post resolves the single attachment a /post/<number>/ link names.
//
// The post endpoint answers with the post itself rather than with the
// keyed object every other endpoint uses, so it is decoded on its own.
func (f *FoolFuuka) post(ctx context.Context, t foolFuukaTarget) (*Result, error) {
	link := f.site.root + foolFuukaPostAPI + "?" + url.Values{
		"board": {t.board}, "num": {t.num},
	}.Encode()

	var payload struct {
		Error string `json:"error"`
		foolFuukaPost
	}
	if err := f.client.GetJSON(ctx, link, f.headers(), &payload); err != nil {
		return nil, fmt.Errorf("%s: /%s/ post %s: %w", f.site.name, t.board, t.num, err)
	}
	if payload.Error != "" {
		return nil, fmt.Errorf("%s: /%s/ post %s: %s", f.site.name, t.board, t.num,
			foolFuukaReason(payload.Error))
	}

	files := f.files([]foolFuukaPost{payload.foolFuukaPost}, "")
	if len(files) == 0 {
		return nil, fmt.Errorf("%s: /%s/ post %s holds no attachment", f.site.name, t.board, t.num)
	}
	return &Result{Title: t.board + "-" + t.num, Files: files}, nil
}

// index resolves one page of a board.
//
// The index endpoint returns each thread's opening post and nothing else, so
// taking it at face value would collect one image per thread and quietly
// leave the rest — which on these boards is most of them. Every thread on the
// page is therefore opened, several at a time, the way coomerfans.go expands
// a creator's posts and for the same reason: doing it one after another
// leaves a board resolving for a minute before a byte moves.
func (f *FoolFuuka) index(ctx context.Context, t foolFuukaTarget) (*Result, error) {
	entries, err := f.call(ctx, foolFuukaIndexAPI, url.Values{
		"board": {t.board}, "page": {strconv.Itoa(t.page)},
	})
	if err != nil {
		return nil, fmt.Errorf("%s: /%s/ page %d: %w", f.site.name, t.board, t.page, err)
	}

	threads := make([]string, 0, len(entries))
	for _, entry := range entries {
		if num := entry.number(); num != "" {
			threads = append(threads, num)
		}
	}
	if len(threads) == 0 {
		return nil, fmt.Errorf("%s: /%s/ has no threads on page %d", f.site.name, t.board, t.page)
	}

	files, firstErr := f.expand(ctx, t.board, threads)
	if len(files) == 0 {
		// Which of these it was matters: a board whose threads are all text
		// is a fact about the board, while a board none of whose threads
		// would open is a fact about the archive, and only one of them is
		// worth trying again later.
		if firstErr != nil {
			return nil, fmt.Errorf("%s: /%s/ page %d: none of its threads would open: %w",
				f.site.name, t.board, t.page, firstErr)
		}
		return nil, fmt.Errorf("%s: /%s/ page %d: none of its %d threads holds an attachment",
			f.site.name, t.board, t.page, len(threads))
	}
	return &Result{
		Title: fmt.Sprintf("%s-%s-page-%d", f.site.name, t.board, t.page),
		Files: files,
	}, nil
}

// expand reads every thread on an index page, several at a time.
//
// Results are collected by index rather than appended as they arrive, so the
// job lists its files in the board's own order however the requests
// interleave. A thread that will not load is skipped rather than failing the
// page: the other fourteen are still worth having. What comes back alongside
// them is the first failure, kept for the case where there are no other
// fourteen — see index.
func (f *FoolFuuka) expand(ctx context.Context, board string, threads []string) ([]File, error) {
	groups := make([][]File, len(threads))
	failures := make([]error, len(threads))

	var wg sync.WaitGroup
	slots := make(chan struct{}, config.PageFetchConcurrency)
	for i, num := range threads {
		wg.Add(1)
		go func(i int, num string) {
			defer wg.Done()
			select {
			case slots <- struct{}{}:
				defer func() { <-slots }()
			case <-ctx.Done():
				failures[i] = ctx.Err()
				return
			}
			entries, err := f.call(ctx, foolFuukaThreadAPI, url.Values{
				"board": {board}, "num": {num},
			})
			if err != nil {
				failures[i] = err
				return
			}
			if len(entries) == 0 {
				return
			}
			// A folder per thread, because a board page is fifteen
			// conversations rather than one and several hundred files in a
			// single directory lose which was which.
			groups[i] = f.files(entries[0].posts(), num)
		}(i, num)
	}
	wg.Wait()

	var files []File
	for _, group := range groups {
		files = append(files, group...)
	}
	for _, err := range failures {
		if err != nil {
			return files, err
		}
	}
	return files, nil
}

// search walks a board's search results.
//
// Paging stops at the archive's own answer rather than at a page count: past
// the last page the endpoint reports "No results found.", which is the end of
// the walk and not a failure — but on the *first* page it means the search
// matched nothing, which is worth saying.
//
// It also stops at a page that adds no post it has not already seen, for the
// two reasons coomerfans.go pages that way: a listing run past its end tends
// to answer with the last page again rather than with nothing, and an archive
// taking new posts while the walk is in progress shifts everything down a
// page, which without this would queue the same file twice.
func (f *FoolFuuka) search(ctx context.Context, t foolFuukaTarget) (*Result, error) {
	query := url.Values{"board": {t.board}}
	for name, values := range t.search {
		query[name] = values
	}

	var files []File
	seen := make(map[string]bool)
	for page := t.page; len(files) < config.MaxSearchResults && page-t.page < config.MaxAlbumPages; page++ {
		query.Set("page", strconv.Itoa(page))
		entries, err := f.call(ctx, foolFuukaSearchAPI, query)
		if err != nil {
			if page == t.page {
				return nil, fmt.Errorf("%s: /%s/ search: %w", f.site.name, t.board, err)
			}
			break
		}
		var posts []foolFuukaPost
		for _, entry := range entries {
			for _, post := range entry.posts() {
				if num := post.Num.String(); num == "" || !seen[num] {
					seen[num] = true
					posts = append(posts, post)
				}
			}
		}
		if len(posts) == 0 {
			break
		}
		files = append(files, f.files(posts, "")...)
	}

	if len(files) == 0 {
		return nil, fmt.Errorf("%s: nothing on /%s/ matching %q has an attachment",
			f.site.name, t.board, t.label)
	}
	// The loop stops once the ceiling is reached, which is a page later than
	// the ceiling itself. Trimming keeps the number meaning what it says.
	if len(files) > config.MaxSearchResults {
		files = files[:config.MaxSearchResults]
	}
	return &Result{Title: t.board + " " + t.label, Files: files}, nil
}

// ------------------------------------------------------------------- files

// files turns posts into downloadable attachments.
func (f *FoolFuuka) files(posts []foolFuukaPost, dir string) []File {
	out := make([]File, 0, len(posts))
	for _, post := range posts {
		media := post.Media
		// A row that is not "normal" describes a file the archive no longer
		// holds. Note that a spoilered image is perfectly normal — the
		// spoiler flag hides the thumbnail on the page and says nothing
		// about the file — so it is media_status alone that decides.
		if media == nil || media.Status != foolFuukaNormal {
			continue
		}
		// The stored name keeps a thread's files in the order they were
		// posted and unique within it, which the poster's own name is not:
		// three people uploading "image.png" is the ordinary case. The
		// original is kept as the fallback, for the rare row that has one
		// and no stored name.
		name := util.FirstNonEmpty(media.Media, media.Filename)
		if name == "" {
			continue
		}

		file := File{
			Name: name,
			// Exact, not read off a listing: this is the length the media
			// host reports for the same file, so a part-finished download
			// can be recognised before a connection is opened.
			Size:    media.size(),
			Dir:     dir,
			Headers: f.headers(),
		}
		switch {
		case media.Link != "":
			file.URL = media.Link
		case media.RemoteLink != "":
			// No local copy: what is left is a link the archive recorded
			// rather than one it serves, and it may not be the file. See
			// resolveRemote.
			file.Resolve = f.remoteResolver(media.RemoteLink, name, media.size())
		default:
			continue
		}
		out = append(out, file)
	}
	return out
}

// headers are what every request for this install carries.
func (f *FoolFuuka) headers() httpx.Header { return httpx.Referer(f.site.root + "/") }

// remoteResolver defers a fallback link to the moment its transfer starts.
func (f *FoolFuuka) remoteResolver(link, name string, size int64) func(context.Context) (*Target, error) {
	return func(ctx context.Context) (*Target, error) {
		return f.resolveRemote(ctx, link, name, size)
	}
}

// resolveRemote follows the link an archive recorded for a file it never kept.
//
// remote_media_link is not reliably the file. Some installs put FoolFuuka's
// own redirect route there, which answers 200 with three hundred bytes of
// HTML whose <meta http-equiv="refresh"> carries the real location. Saved as
// it stands that is a page of markup under the image's name, with a
// Content-Length that agrees with it, so nothing further down would notice.
// Others record the original 4chan link, which for an old post is long gone.
//
// The URL cannot be read to tell which: these installs serve their media and
// their pages from one host. So the server is asked, and asked with HEAD,
// which settles it for the price of no body at all; the page is fetched only
// when the answer was HTML. A host that will not do HEAD is taken at its word
// and the link used as it is, since refusing the method says nothing about
// what is behind it.
//
// This is a resolver rather than work done during extraction because it is a
// request per file and only a small minority of posts need one — paying for
// them as their turn comes keeps a thread of five hundred from spending the
// wait in "resolving".
func (f *FoolFuuka) resolveRemote(ctx context.Context, link, name string, size int64) (*Target, error) {
	target := &Target{URL: link, Name: name, Size: size, Headers: f.headers()}

	req, err := f.client.NewRequest(ctx, http.MethodHead, link, nil)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", f.site.name, err)
	}
	req.Header.Set(httpx.HeaderReferer, f.site.root+"/")
	resp, err := f.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s: %s: %w", f.site.name, name, err)
	}
	status := resp.StatusCode
	contentType := resp.Header.Get(httpx.HeaderContentType)
	_ = resp.Body.Close()

	switch {
	case status == http.StatusMethodNotAllowed, status == http.StatusNotImplemented:
		return target, nil
	case status >= 400:
		return nil, fmt.Errorf("%s: %s was never mirrored here and the link the archive "+
			"recorded for it answers %d", f.site.name, name, status)
	case !foolFuukaIsHTML(contentType):
		return target, nil
	}

	doc, err := f.client.GetString(ctx, link, f.headers())
	if err != nil {
		return nil, fmt.Errorf("%s: %s: %w", f.site.name, name, err)
	}
	next := foolFuukaRefreshTarget(doc, link)
	if next == "" {
		return nil, fmt.Errorf("%s: the archive answered for %s with a page rather than "+
			"the file, and the page points nowhere", f.site.name, name)
	}
	target.URL = next
	return target, nil
}

// foolFuukaIsHTML reports whether a content type is a web page.
func foolFuukaIsHTML(contentType string) bool {
	return strings.Contains(strings.ToLower(contentType), "text/html")
}

// foolFuukaRefreshValue picks the location out of a refresh directive, which
// is written "0; url=<where>" with the quoting and spacing left to taste.
var foolFuukaRefreshValue = regexp.MustCompile(`(?i)\burl\s*=\s*['"]?([^'";]+)`)

// foolFuukaRefreshTarget reads where a page redirects itself to.
//
// A refresh with no url= is not a redirect at all — Cloudflare's own
// interstitial carries content="360", meaning reload this page in six
// minutes — so the url is what is looked for rather than the element.
func foolFuukaRefreshTarget(doc, base string) string {
	root, err := parseHTML(doc)
	if err != nil {
		return ""
	}
	from, err := url.Parse(base)
	if err != nil {
		return ""
	}
	for _, meta := range findAll(root, func(n *html.Node) bool {
		return isElem(n, atom.Meta) && strings.EqualFold(attr(n, "http-equiv"), "refresh")
	}) {
		match := foolFuukaRefreshValue.FindStringSubmatch(attr(meta, "content"))
		if match == nil {
			continue
		}
		if next := resolveRef(from, strings.TrimSpace(match[1])); next != "" {
			return next
		}
	}
	return ""
}

// ----------------------------------------------------------------- the API

// call fetches one endpoint and decodes it.
func (f *FoolFuuka) call(ctx context.Context, endpoint string, query url.Values) ([]foolFuukaEntry, error) {
	link := f.site.root + endpoint + "?" + query.Encode()

	var raw json.RawMessage
	if err := f.client.GetJSON(ctx, link, f.headers(), &raw); err != nil {
		return nil, err
	}
	return foolFuukaDecode(raw)
}

// foolFuukaEntry is one top-level entry of an API response: a thread, keyed
// by its number, or — from the search endpoint — a bare page of results
// keyed by its index.
type foolFuukaEntry struct {
	Key    string
	Thread foolFuukaThread
}

// number is the thread's own number, preferring what the opening post says
// over the key it was filed under.
func (e foolFuukaEntry) number() string {
	if e.Thread.OP != nil {
		if num := e.Thread.OP.Num.String(); num != "" {
			return num
		}
	}
	return e.Key
}

// posts is the entry's posts in order, the opening one first.
func (e foolFuukaEntry) posts() []foolFuukaPost {
	out := make([]foolFuukaPost, 0, len(e.Thread.Posts)+1)
	if e.Thread.OP != nil {
		out = append(out, *e.Thread.OP)
	}
	return append(out, e.Thread.Posts...)
}

// foolFuukaThread is one thread as every endpoint returns it: the opening
// post apart from the replies.
type foolFuukaThread struct {
	OP    *foolFuukaPost `json:"op"`
	Posts foolFuukaPosts `json:"posts"`
}

// foolFuukaDecode reads the API's top-level object.
//
// It walks the document with a token decoder rather than unmarshalling into a
// map, because the keys carry an ordering — a board index is in the board's
// bump order, which is neither the numeric order of the thread numbers nor
// anything a Go map preserves.
//
// It is also where {"error":"..."} is caught, which is the whole reason this
// is not a plain Unmarshal: that document arrives under a 200, and decoded
// into anything shaped like a thread it comes out empty rather than wrong.
func foolFuukaDecode(raw []byte) ([]foolFuukaEntry, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	tok, err := dec.Token()
	if err != nil {
		return nil, fmt.Errorf("unexpected response: %w", err)
	}
	if delim, ok := tok.(json.Delim); !ok || delim != '{' {
		return nil, errors.New("unexpected response: not a JSON object")
	}

	var out []foolFuukaEntry
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return nil, fmt.Errorf("unexpected response: %w", err)
		}
		key, _ := keyTok.(string)

		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return nil, fmt.Errorf("unexpected response: %w", err)
		}
		switch key {
		case "error":
			var message string
			if err := json.Unmarshal(value, &message); err == nil && message != "" {
				return nil, errors.New(foolFuukaReason(message))
			}
			return nil, errors.New("the archive reported an error it did not describe")
		case "meta":
			// Search responses carry a result count alongside the page.
			continue
		}

		var thread foolFuukaThread
		if err := json.Unmarshal(value, &thread); err != nil {
			// An entry shaped unlike the rest is skipped rather than taken
			// for a failure: an install that adds a block of its own beside
			// the threads should cost nothing.
			continue
		}
		out = append(out, foolFuukaEntry{Key: key, Thread: thread})
	}
	return out, nil
}

// foolFuukaReason tidies the archive's own message for use inside one of
// ours, where its trailing full stop would land mid-sentence.
func foolFuukaReason(message string) string {
	return strings.TrimRight(strings.TrimSpace(message), ".")
}

// foolFuukaPosts holds a thread's replies.
//
// They arrive in whichever of two shapes the endpoint uses: the thread
// endpoint keys them by post number, while search returns a flat array. PHP
// renders an empty set of either as [], so a thread with no replies at all
// has to decode through the array branch even on the endpoint that keys them.
type foolFuukaPosts []foolFuukaPost

func (p *foolFuukaPosts) UnmarshalJSON(raw []byte) error {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		*p = nil
		return nil
	}
	if raw[0] == '[' {
		var list []foolFuukaPost
		if err := json.Unmarshal(raw, &list); err != nil {
			return err
		}
		*p = list
		return nil
	}

	var keyed map[string]foolFuukaPost
	if err := json.Unmarshal(raw, &keyed); err != nil {
		return err
	}
	list := make([]foolFuukaPost, 0, len(keyed))
	for key, post := range keyed {
		// The key a post is filed under is its number, so a row that omits
		// its own still has one to be ordered by.
		if post.Num.String() == "" {
			post.Num = flexValue(key)
		}
		list = append(list, post)
	}
	// Ranging over that map threw the thread's order away, and the post
	// number is what restores it — compared as a number, because a board's
	// numbering gains digits over the years and "9999" sorts after "10000"
	// as text. The text is then the tie-break, and it has to be one: a
	// stable sort would be no help when the order going in is a map's, which
	// is to say different on every run.
	sort.Slice(list, func(i, j int) bool {
		a, b := foolFuukaNumber(list[i].Num), foolFuukaNumber(list[j].Num)
		if a != b {
			return a < b
		}
		return list[i].Num.String() < list[j].Num.String()
	})
	*p = list
	return nil
}

// foolFuukaNumber reads a post number for ordering. Anything unreadable
// sorts first, where it is at least in a fixed place.
func foolFuukaNumber(v flexValue) int64 {
	n, err := strconv.ParseInt(strings.TrimSpace(v.String()), 10, 64)
	if err != nil {
		return 0
	}
	return n
}

// foolFuukaPost is the part of a post this needs.
type foolFuukaPost struct {
	Num flexValue `json:"num"`
	// Title is the thread subject, carried by the opening post and null on
	// almost every reply.
	Title string          `json:"title"`
	Media *foolFuukaMedia `json:"media"`
}

// foolFuukaMedia is a post's attachment.
type foolFuukaMedia struct {
	// Media is the name the archive stored the file under, which is what it
	// is saved as here.
	Media string `json:"media"`
	// Filename is the name the poster uploaded it with.
	Filename string `json:"media_filename"`
	// Link is the absolute link to the archive's own copy, and empty when
	// there is not one.
	Link string `json:"media_link"`
	// RemoteLink is what the archive recorded instead — sometimes the
	// original 4chan link, sometimes its own redirect page.
	RemoteLink string `json:"remote_media_link"`
	// Size is exact, and arrives as a string on every install seen. flexValue
	// covers the one that sends it as a number.
	Size flexValue `json:"media_size"`
	// Status is "normal" for a file that is still there.
	Status string `json:"media_status"`
}

// size is the attachment's exact length, or -1 when it cannot be read.
func (m *foolFuukaMedia) size() int64 {
	n, err := strconv.ParseInt(strings.TrimSpace(m.Size.String()), 10, 64)
	if err != nil || n <= 0 {
		return -1
	}
	return n
}
