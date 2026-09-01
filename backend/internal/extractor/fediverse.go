package extractor

import (
	"context"
	"fmt"
	"net/url"
	"path"
	"strconv"
	"strings"
	"sync"

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
// Match cannot fetch anything, so it is the URL's shape that routes here —
// /@name, /c/name and the post paths, which are the fediverse's own
// conventions rather than any one server's — and nodeinfo that decides how to
// ask. A shape that turns out to belong to something else entirely fails
// naming the host, which is more use than the alternative: saving somebody's
// front page to disk under the name of a picture.
type Fediverse struct {
	client *httpx.Client

	// mu guards software, which remembers what each host answered nodeinfo
	// with. Discovery costs two requests, and no instance changes which
	// server it runs between one paste and the next.
	mu       sync.Mutex
	software map[string]string
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

	// fediMastodonAPI is the Mastodon client API's base.
	fediMastodonAPI = "/api/v1"

	// fediPixelfedAPI is where pixelfed keeps its statuses route; see
	// mastodonTimeline for why it is not the one above.
	fediPixelfedAPI = "/api/pixelfed/v1"
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

// fediDialect is one API family.
type fediDialect int

const (
	// fediMastodon is the Mastodon client API, which Pleroma, Akkoma,
	// GoToSocial, Hometown and most forks implement as well. It is the zero
	// value because it is also what software nobody here has heard of is
	// assumed to speak.
	fediMastodon fediDialect = iota
	// fediPixelfed is that same API with its statuses route moved.
	fediPixelfed
	// fediLemmy is the link aggregator's, which shares nothing with it.
	fediLemmy
	// fediMisskey is the Misskey family's, which is POST-only.
	fediMisskey
)

// fediDialects maps nodeinfo's software.name onto the API family it speaks.
// Names are lower-cased before the lookup, since the field is free text.
var fediDialects = map[string]fediDialect{
	// The Mastodon API proper, and the servers that reimplement it. Listed
	// so a failure on one of them is reported as what it is rather than as
	// the consequence of a guessed dialect.
	"mastodon":   fediMastodon,
	"pleroma":    fediMastodon,
	"akkoma":     fediMastodon,
	"gotosocial": fediMastodon,
	"hometown":   fediMastodon,

	"pixelfed": fediPixelfed,
	"lemmy":    fediLemmy,

	// The Misskey family: each of these is a fork of it and carries the same
	// POST /api/users/notes.
	"misskey":    fediMisskey,
	"sharkey":    fediMisskey,
	"firefish":   fediMisskey,
	"calckey":    fediMisskey,
	"foundkey":   fediMisskey,
	"iceshrimp":  fediMisskey,
	"cherrypick": fediMisskey,
}

// NewFediverse builds the fediverse extractor.
func NewFediverse(client *httpx.Client) *Fediverse {
	return &Fediverse{client: client, software: make(map[string]string)}
}

func (f *Fediverse) Name() string { return "fediverse" }

func (f *Fediverse) Match(u *url.URL) bool { return fediTargetOf(u) != nil }

// Extract discovers what the instance runs, then asks it in its own dialect.
func (f *Fediverse) Extract(ctx context.Context, u *url.URL, _ Options) (*Result, error) {
	target := fediTargetOf(u)
	if target == nil {
		return nil, fmt.Errorf("fediverse: %s is not an account (/@name), a community "+
			"(/c/name) or a single post", u.Redacted())
	}

	software, err := f.softwareName(ctx, u)
	if err != nil {
		return nil, err
	}
	dialect, known := fediDialects[software]

	var res *Result
	switch dialect {
	case fediLemmy:
		res, err = f.lemmy(ctx, u, target)
	case fediMisskey:
		res, err = f.misskey(ctx, u, target)
	default:
		res, err = f.mastodon(ctx, u, target, dialect == fediPixelfed)
	}
	if err != nil && !known {
		// The dialect was an assumption, so a failure has two possible
		// causes and the user is owed both.
		return nil, fmt.Errorf("%w (%s runs %q, which this asked in mastodon's dialect for "+
			"want of anything better)", err, u.Hostname(), software)
	}
	return res, err
}

// ----------------------------------------------------------------- discovery

// softwareName reads software.name out of the instance's nodeinfo.
//
// Two requests, because the specification says so: the well-known path
// carries links, and the document behind one of them carries the software.
func (f *Fediverse) softwareName(ctx context.Context, u *url.URL) (string, error) {
	host := strings.ToLower(u.Host)
	f.mu.Lock()
	cached, ok := f.software[host]
	f.mu.Unlock()
	if ok {
		return cached, nil
	}

	root := util.Origin(u)
	var index struct {
		Links []fediNodeLink `json:"links"`
	}
	if err := f.client.GetJSON(ctx, root+fediWellKnown, fediHeaders(root), &index); err != nil {
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
	if err := f.client.GetJSON(ctx, href, fediHeaders(root), &doc); err != nil {
		return "", fmt.Errorf("fediverse: %s: read nodeinfo: %w", u.Hostname(), err)
	}
	name := strings.ToLower(strings.TrimSpace(doc.Software.Name))
	if name == "" {
		return "", fmt.Errorf("fediverse: %s published a nodeinfo document naming no software",
			u.Hostname())
	}

	f.mu.Lock()
	if f.software == nil {
		f.software = make(map[string]string)
	}
	f.software[host] = name
	f.mu.Unlock()
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
func fediIsName(name string) bool {
	if name == "" || len(name) > 64 {
		return false
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '_', r == '-', r == '.':
		default:
			return false
		}
	}
	return true
}

// fediIsID reports whether a segment can be a post id: mastodon numbers them,
// pleroma and misskey use short alphanumeric ones.
func fediIsID(id string) bool {
	if id == "" || len(id) > 64 {
		return false
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		default:
			return false
		}
	}
	return true
}

// fediIsNumber reports whether a segment is all digits.
func fediIsNumber(id string) bool {
	if id == "" || len(id) > 20 {
		return false
	}
	return strings.IndexFunc(id, func(r rune) bool { return r < '0' || r > '9' }) < 0
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
