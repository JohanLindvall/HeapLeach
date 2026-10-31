// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"path"
	"strconv"
	"strings"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
	"github.com/JohanLindvall/HeapLeach/internal/util"
)

// Fediverse covers the ActivityPub servers — Mastodon and its Pleroma,
// Akkoma and GoToSocial relatives, Pixelfed, Lemmy, and the Misskey family —
// without naming a single instance.
//
// It can do that because these servers publish what they are. nodeinfo is a
// specification rather than a convention: /.well-known/nodeinfo answers with a
// list of links, and the document one of those points at carries
// software.name. That is what separates this from booru.go, which has to
// carry a list of hosts that starts rotting the day it is written, and from
// the KVS sniff, which guesses from the shape of a page. Here the instance
// states its own software and is believed.
//
// A list does remain, but it is of API families rather than of hosts, and it
// names only the servers that speak something other than the Mastodon client
// API. Everything else — including forks nobody here has heard of — is asked
// in Mastodon's dialect, which is the fediverse's lingua franca; a name that
// is not listed and then fails says so in the error rather than being refused
// up front. So that list grows when a new server *implementation* appears,
// which is rare, and not when a new instance does, which is daily.
//
// A page is recognised by its URL's shape — /@name, /c/name and the post
// paths, which are the fediverse's own conventions rather than any one
// server's — together with the server's nodeinfo naming its software, and
// nodeinfo again decides how to ask.
type Fediverse struct {
	client *httpx.Client

	// software remembers what each host answered nodeinfo with. Discovery
	// costs two requests, and no instance changes which server it runs
	// between one paste and the next.
	software hostMemo[string]
}

const (
	// fediWellKnown is where the discovery document lives. It is the only
	// path here fixed by the specification: the document it returns names
	// where the real one is, and that is *not* at a predictable place —
	// mastodon serves /nodeinfo/2.0, pleroma /nodeinfo/2.0.json and pixelfed
	// /api/nodeinfo/2.0.json. Guessing would work on two hosts out of three,
	// which is the worst kind of working.
	fediWellKnown = "/.well-known/nodeinfo"

	// fediSchemaRel prefixes the rel of every nodeinfo link. Instances
	// publish other rels beside them, so this is what picks the right ones.
	fediSchemaRel = "http://nodeinfo.diaspora.software/ns/schema/"
)

// Page strides. Each is the largest its API accepts, so a timeline costs as
// few requests as the host allows — and asking for more than a host allows is
// not a smaller page but a refusal, which is why pixelfed has its own: it
// answers a limit above 24 with 422 rather than with 24 posts.
const (
	fediMastodonPageSize = 40
	fediPixelfedPageSize = 24
	fediLemmyPageSize    = 50
	fediMisskeyPageSize  = 100
)

// fediDialect is one API family, as the rows the timeline engine (read)
// asks. Each dialect's file holds its row and the calls the row names.
type fediDialect struct {
	// api is the base a Mastodon-family dialect reads statuses from.
	api string
	// stride is the page size asked for; see the page strides above.
	stride int
	// communities says the dialect has them: only Lemmy does.
	communities bool
	// post reads the single post the link names.
	post func(ctx context.Context, s *fediSession) (*Result, error)
	// owner reads whose timeline the link names: the id its pages are asked
	// by and the job's title.
	owner func(ctx context.Context, s *fediSession) (id, title string, err error)
	// page reads one page of the owner's posts: those preceding until, the
	// last id of the page before ("" for the first), or page n for an API
	// that pages by number. An empty page ends the timeline.
	page func(ctx context.Context, s *fediSession, until string, n int) ([]fediEntry, error)
	// empty is the error for a timeline with nothing to fetch, given the
	// title and how many posts were looked through.
	empty string
}

// fediDialects maps nodeinfo's software.name onto the API family it speaks.
// Names are lower-cased before the lookup, since the field is free text.
// Software that is not listed is asked in Mastodon's dialect, the
// fediverse's lingua franca.
var fediDialects = map[string]*fediDialect{
	// The Mastodon API proper, and the servers that reimplement it. Listed
	// so a failure on one of them is reported as what it is rather than as
	// the consequence of a guessed dialect.
	"mastodon":   &fediMastodon,
	"pleroma":    &fediMastodon,
	"akkoma":     &fediMastodon,
	"gotosocial": &fediMastodon,
	"hometown":   &fediMastodon,

	"pixelfed": &fediPixelfed,
	"lemmy":    &fediLemmy,

	// The Misskey family: each of these is a fork of it and carries the same
	// POST /api/users/notes.
	"misskey":    &fediMisskey,
	"sharkey":    &fediMisskey,
	"firefish":   &fediMisskey,
	"calckey":    &fediMisskey,
	"foundkey":   &fediMisskey,
	"iceshrimp":  &fediMisskey,
	"cherrypick": &fediMisskey,
}

// fediSession is one extraction: the instance, the link's target, the
// dialect asked, and what the asking has settled so far.
type fediSession struct {
	*Fediverse
	d    *fediDialect
	u    *url.URL
	t    *fediTarget
	root string
	// owner and title are the timeline's, once the dialect's owner read them.
	owner, title string
	// api is the Lemmy API version that answered, once one has.
	api string
}

// host is the instance's name, as handles are qualified with it.
func (s *fediSession) host() string { return s.u.Hostname() }

// NewFediverse builds the fediverse extractor.
func NewFediverse(client *httpx.Client) *Fediverse {
	return &Fediverse{client: client}
}

func (f *Fediverse) Name() string { return "fediverse" }

// Recognises is the inventory's description.
func (f *Fediverse) Recognises() string {
	return "fediverse accounts, communities and posts (`/@name`, `/users/<name>`, `/u/<name>`, " +
		"`/c/<name>`, `/notes/<id>`, `/notice/<id>`, `/post/<n>`, `/p/<user>/<id>`) on any server " +
		"whose nodeinfo names its software"
}

// Shape reports the fediverse's own path conventions.
func (f *Fediverse) Shape(u *url.URL) bool { return fediTargetOf(u) != nil }

// Recognise takes a page of one of those forms on a server whose nodeinfo
// names the software it runs, which is the document this extractor's
// choice of API depends on and which every fediverse server publishes by
// specification. The page itself is not read: these are application
// shells, often behind a challenge, and the specification is the better
// witness. The answer is kept on the probed page, so the extraction that
// follows asks nothing again.
func (f *Fediverse) Recognise(ctx context.Context, p *Page) bool {
	if fediTargetOf(p.Final) == nil {
		return false
	}
	_, err := f.nodeInfo(p.Final, f.probedJSON(ctx, p, util.Origin(p.Final)))
	return err == nil
}

// Extract discovers what the instance runs, then asks it in its own dialect.
func (f *Fediverse) Extract(ctx context.Context, u *url.URL, opts Options) (*Result, error) {
	target := fediTargetOf(u)
	if target == nil {
		return nil, fmt.Errorf("fediverse: %s is not an account (/@name), a community "+
			"(/c/name) or a single post", u.Redacted())
	}

	software, err := f.softwareName(ctx, u)
	if err != nil {
		return nil, err
	}
	d, known := fediDialects[software]
	if !known {
		d = &fediMastodon
	}
	s := &fediSession{Fediverse: f, d: d, u: u, t: target, root: util.Origin(u)}
	res, err := s.read(ctx, opts)
	if err != nil && !known {
		// The dialect was an assumption, so a failure has two possible
		// causes and the user is owed both.
		return nil, fmt.Errorf("%w (%s runs %q, which this asked in mastodon's dialect for "+
			"want of anything better)", err, u.Hostname(), software)
	}
	return res, err
}

// read is the timeline engine every dialect is a row of: a single post is
// the dialect's to read, and a timeline is the owner's pages walked back
// from the newest.
//
// An id cursor is exclusive and the listing newest first, so each page asks
// for what precedes the last id of the one before; a dialect paging by
// number reads n instead. An empty page ends the walk, as does a page that
// adds nothing — a Lemmy 1.0 instance ignoring the page number it no longer
// knows hands back the first page again — and the timeline page cap, which
// is what stops an account with a decade of posts resolving for minutes. A
// page after the first that fails is not fatal: what has already been
// collected is still worth queueing. The walk stops sooner at the user's
// file cap, counted in files, since one post may carry several and another
// none.
func (s *fediSession) read(ctx context.Context, opts Options) (*Result, error) {
	switch {
	case s.t.kind == fediCommunity && !s.d.communities:
		return nil, fmt.Errorf("fediverse: communities are a lemmy thing, and %s does not run "+
			"it (%s)", s.host(), s.u.Redacted())
	case s.t.kind == fediPost:
		return s.d.post(ctx, s)
	}
	var err error
	if s.owner, s.title, err = s.d.owner(ctx, s); err != nil {
		return nil, err
	}

	until := ""
	posts, note, err := walkPages(ctx, pageWalk[fediEntry]{
		key:   func(post fediEntry) string { return post.id },
		limit: opts.maxFiles(), limitNote: fileLimitNote, pages: config.MaxTimelinePages,
		weigh: func(posts []fediEntry) int { return len(fediFiles(posts)) },
		fetch: func(ctx context.Context, n int) (pageOf[fediEntry], error) {
			batch, err := s.d.page(ctx, s, until, n)
			if err != nil {
				return pageOf[fediEntry]{}, err
			}
			if len(batch) == 0 {
				return pageOf[fediEntry]{end: true}, nil
			}
			until = batch[len(batch)-1].id
			return pageOf[fediEntry]{items: batch, next: until != ""}, nil
		},
	})
	if err != nil {
		return nil, err
	}
	files := fediFiles(posts)
	if len(files) == 0 {
		return nil, fmt.Errorf("fediverse: "+s.d.empty, s.title, config.MaxTimelinePages*s.d.stride)
	}
	return &Result{Title: s.title, Files: files, Note: note}, nil
}

// fediEntry is one post as the timeline engine walks it: the id it is
// listed, and paged past, by, and the files it carries.
type fediEntry struct {
	id    string
	files []File
}

// fediFiles flattens posts into their files, each link once.
func fediFiles(posts []fediEntry) []File {
	seen := make(map[string]bool)
	var files []File
	for _, post := range posts {
		for _, file := range post.files {
			if !seen[file.URL] {
				seen[file.URL] = true
				files = append(files, file)
			}
		}
	}
	return files
}

// ----------------------------------------------------------------- discovery

// errNoAnswer is a bounded probe's silence, which Recognise reads as no.
var errNoAnswer = errors.New("no answer")

// probedJSON asks through the probed page's memo, so each endpoint is asked
// once whether a recogniser or the extraction asks first.
func (f *Fediverse) probedJSON(ctx context.Context, p *Page, root string) func(string, any) error {
	return func(endpoint string, out any) error {
		if !pageJSON(ctx, p, f.client, endpoint, fediHeaders(root), out) {
			return errNoAnswer
		}
		return nil
	}
}

// softwareName reads software.name out of the instance's nodeinfo: through
// the probed page's memo when there is one, and otherwise, or when that
// came to nothing, with requests of its own.
func (f *Fediverse) softwareName(ctx context.Context, u *url.URL) (string, error) {
	return f.software.lookup(u.Host, func() (string, error) {
		root := util.Origin(u)
		if p := probed(ctx, u); p != nil {
			if name, err := f.nodeInfo(u, f.probedJSON(ctx, p, root)); err == nil {
				return name, nil
			}
		}
		return f.nodeInfo(u, func(endpoint string, out any) error {
			return f.client.GetJSON(ctx, endpoint, fediHeaders(root), out)
		})
	})
}

// nodeInfo reads software.name with get. Two requests, because the
// specification says so: the well-known path carries links, and the
// document behind one of them carries the software.
func (f *Fediverse) nodeInfo(u *url.URL, get func(endpoint string, out any) error) (string, error) {
	var index struct {
		Links []fediNodeLink `json:"links"`
	}
	if err := get(util.Origin(u)+fediWellKnown, &index); err != nil {
		return "", fmt.Errorf("fediverse: %s publishes no nodeinfo, so it is not a fediverse "+
			"instance this can read: %w", u.Hostname(), err)
	}

	href := fediNodeInfoHref(index.Links, u)
	if href == "" {
		return "", fmt.Errorf("fediverse: %s answered nodeinfo with no link to a document of "+
			"its own", u.Hostname())
	}

	var doc struct {
		Software struct {
			Name string `json:"name"`
		} `json:"software"`
	}
	if err := get(href, &doc); err != nil {
		return "", fmt.Errorf("fediverse: %s: read nodeinfo: %w", u.Hostname(), err)
	}
	name := strings.ToLower(strings.TrimSpace(doc.Software.Name))
	if name == "" {
		return "", fmt.Errorf("fediverse: %s published a nodeinfo document naming no software",
			u.Hostname())
	}
	return name, nil
}

// fediNodeLink is one entry of the discovery document.
type fediNodeLink struct {
	Rel  string `json:"rel"`
	Href string `json:"href"`
}

// fediNodeInfoHref picks which link to follow.
//
// Every schema version carries software.name, so any of them would answer the
// question; the newest is taken because that leaves the choice deterministic
// on the instances that offer several, which is most of them. A link pointing
// somewhere other than the instance itself is ignored — the document is a
// host describing itself, and following it off-host would let any page send
// us to fetch a URL of its choosing.
func fediNodeInfoHref(links []fediNodeLink, u *url.URL) string {
	var best, bestVersion string
	for _, link := range links {
		if !strings.HasPrefix(link.Rel, fediSchemaRel) {
			continue
		}
		href, err := url.Parse(link.Href)
		if err != nil || (href.Scheme != "http" && href.Scheme != "https") {
			continue
		}
		if !util.HostMatches(href.Host, u.Hostname()) {
			continue
		}
		version := strings.TrimPrefix(link.Rel, fediSchemaRel)
		if best == "" || fediNewerSchema(version, bestVersion) {
			best, bestVersion = link.Href, version
		}
	}
	return best
}

// fediNewerSchema compares two nodeinfo schema versions ("2.0", "2.1").
func fediNewerSchema(a, b string) bool {
	amajor, aminor := fediVersionParts(a)
	bmajor, bminor := fediVersionParts(b)
	return amajor > bmajor || (amajor == bmajor && aminor > bminor)
}

// fediVersionParts splits "2.1" into its two numbers, reading anything
// unparseable as zero so a malformed rel simply never wins.
func fediVersionParts(v string) (major, minor int) {
	head, tail, _ := strings.Cut(v, ".")
	major, _ = strconv.Atoi(head)
	minor, _ = strconv.Atoi(tail)
	return major, minor
}

// -------------------------------------------------------------------- shapes

// fediKind is what a URL points at.
type fediKind int

const (
	fediAccount fediKind = iota
	fediCommunity
	fediPost
)

// fediTarget is a parsed link.
type fediTarget struct {
	kind fediKind
	// handle is the local name, or "name@host" for an account or community
	// this instance is merely showing on another's behalf.
	handle string
	// id identifies a single post.
	id string
}

// fediTargetOf reads a link's shape, or returns nil.
//
// These shapes are the fediverse's own conventions rather than any one
// server's: /@name is a profile on Mastodon, Pleroma, Pixelfed and every
// Misskey fork alike, and /c/name is a Lemmy community. Which server is
// behind them is nodeinfo's business and not this function's — so a shape is
// accepted here even where the dialect that turns out to be running cannot
// serve it, and the refusal then comes with the host's name attached.
func fediTargetOf(u *url.URL) *fediTarget {
	segs := util.PathSegments(u)
	// Mastodon's web client prefixes the profile path with its own route.
	if len(segs) > 1 && (segs[0] == "web" || segs[0] == "deck") {
		segs = segs[1:]
	}

	switch {
	case len(segs) == 1 && strings.HasPrefix(segs[0], "@"):
		return fediAccountTarget(segs[0])

	// A status under the profile that wrote it: mastodon and pleroma both.
	case len(segs) == 2 && strings.HasPrefix(segs[0], "@") && fediIsID(segs[1]):
		if t := fediAccountTarget(segs[0]); t != nil {
			t.kind, t.id = fediPost, segs[1]
			return t
		}

	// The ActivityPub actor path, and lemmy's person path.
	case len(segs) == 2 && (segs[0] == "users" || segs[0] == "u"):
		return fediAccountTarget(segs[1])

	case len(segs) == 2 && segs[0] == "c":
		if t := fediAccountTarget(strings.TrimPrefix(segs[1], "!")); t != nil {
			t.kind = fediCommunity
			return t
		}

	// notes: misskey. notice: pleroma. post: lemmy, whose ids are numeric.
	case len(segs) == 2 && (segs[0] == "notes" || segs[0] == "notice") && fediIsID(segs[1]):
		return &fediTarget{kind: fediPost, id: segs[1]}
	case len(segs) == 2 && segs[0] == "post" && fediIsNumber(segs[1]):
		return &fediTarget{kind: fediPost, id: segs[1]}

	// Pixelfed writes a post as /p/<user>/<id>, and the user is needed: see
	// mastodonOne for why the id alone will not fetch it.
	case len(segs) == 3 && segs[0] == "p" && fediIsID(segs[2]):
		if t := fediAccountTarget(segs[1]); t != nil {
			t.kind, t.id = fediPost, segs[2]
			return t
		}
	}
	return nil
}

// fediAccountTarget validates a handle, with or without its leading sigil and
// with or without the "@host" that makes it somebody else's account.
func fediAccountTarget(seg string) *fediTarget {
	handle, err := url.PathUnescape(strings.TrimPrefix(seg, "@"))
	if err != nil {
		return nil
	}
	name, host, qualified := strings.Cut(handle, "@")
	if !fediIsName(name) {
		return nil
	}
	// A qualified handle names another instance, so what follows the second
	// @ has to look like a domain rather than be a stray character.
	if qualified && !strings.Contains(strings.TrimSuffix(host, "."), ".") {
		return nil
	}
	return &fediTarget{kind: fediAccount, handle: handle}
}

// fediIsName reports whether a path segment can be an account name. The
// fediverse's own rule is letters, digits and underscores, with dots and
// dashes tolerated; anything else is some other site's URL that happens to
// begin with an @.
func fediIsName(name string) bool { return fediToken(name, 64, fediAlnum+"_-.") }

// fediIsID reports whether a segment can be a post id: mastodon numbers them,
// pleroma and misskey use short alphanumeric ones.
func fediIsID(id string) bool { return fediToken(id, 64, fediAlnum) }

// fediIsNumber reports whether a segment is all digits.
func fediIsNumber(id string) bool { return fediToken(id, 20, fediDigits) }

const (
	fediDigits = "0123456789"
	fediAlnum  = fediDigits + "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"
)

// fediToken reports whether seg is one to most bytes of the characters in
// set and nothing else.
func fediToken(seg string, most int, set string) bool {
	return seg != "" && len(seg) <= most && strings.Trim(seg, set) == ""
}

// fediHandle renders a handle the way the fediverse writes one, qualifying a
// bare name with the instance it was read from. Two instances' "alice" then
// become two folders rather than one, which is the whole point.
func fediHandle(handle, host string) string {
	if handle == "" {
		return host
	}
	if strings.Contains(handle, "@") {
		return handle
	}
	return handle + "@" + host
}

// fediHeaders are what every request in this file carries.
func fediHeaders(root string) httpx.Header { return httpx.Referer(root + "/") }

// fediExt is the extension a URL's path ends in, ignoring the query string —
// misskey marks some media links with one.
func fediExt(link string) string {
	ext := path.Ext(util.NameFromURL(link))
	if len(ext) > 6 {
		return "" // a dot inside the name rather than an extension
	}
	return ext
}

// fediIsMediaURL guesses from the path alone, for the one caller with nothing
// better to go on: a lemmy instance too old to record what it fetched.
func fediIsMediaURL(link string) bool {
	switch strings.ToLower(fediExt(link)) {
	case ".jpg", ".jpeg", ".png", ".gif", ".webp", ".avif", ".bmp",
		".mp4", ".m4v", ".webm", ".mov", ".mkv", ".mp3", ".m4a", ".ogg", ".opus":
		return true
	}
	return false
}
