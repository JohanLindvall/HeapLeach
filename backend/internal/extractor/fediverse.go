package extractor

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"sync"

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

// ------------------------------------------------------------------ mastodon

// mastodonAccount is the part of an account this needs, which is mostly its
// id: every other route is keyed by that rather than by the name.
type mastodonAccount struct {
	ID       string `json:"id"`
	Username string `json:"username"`
	Acct     string `json:"acct"`
}

// mastodonStatus is one post.
type mastodonStatus struct {
	ID      string          `json:"id"`
	Account mastodonAccount `json:"account"`
	Media   []mastodonMedia `json:"media_attachments"`
	// Reblog is the post this one boosts, if it is a boost at all.
	Reblog *mastodonStatus `json:"reblog"`
}

// mastodonMedia is one attachment.
//
// preview_url is deliberately absent. It is the same file under /small/ — a
// thumbnail — and a struct that does not carry it cannot reach for it by
// mistake when url is empty. meta is absent for a sharper reason:
// meta.original carries a field named "size" whose value is "2225x3356", the
// pixel dimensions. Read as a byte count it would be wrong by three orders of
// magnitude, and it would be wrong in the direction that tells the downloader
// a part-finished file is already complete.
type mastodonMedia struct {
	URL string `json:"url"`
	// RemoteURL is the origin instance's copy, set when this instance is
	// showing somebody else's media. It stands in when the local cache has
	// not been filled yet, which is what a null url means.
	RemoteURL string `json:"remote_url"`
}

// mastodon resolves an account or a single status through the Mastodon
// client API.
//
// Nothing here goes near /api/v1/timelines/public: since Mastodon 4.4 it
// answers an anonymous caller 422 "This method requires an authenticated
// user". The per-account statuses route is the one that stayed open, and it
// is what a pasted profile is asking about anyway.
func (f *Fediverse) mastodon(ctx context.Context, u *url.URL, t *fediTarget, pixelfed bool) (*Result, error) {
	if t.kind == fediCommunity {
		return nil, fmt.Errorf("fediverse: communities are a lemmy thing, and %s does not run "+
			"it (%s)", u.Hostname(), u.Redacted())
	}
	root := util.Origin(u)

	if t.kind == fediPost {
		status, err := f.mastodonOne(ctx, root, t, pixelfed)
		if err != nil {
			return nil, err
		}
		files := mastodonFiles([]mastodonStatus{*status})
		if len(files) == 0 {
			return nil, fmt.Errorf("fediverse: post %s on %s carries no media", t.id, u.Hostname())
		}
		handle := fediHandle(util.FirstNonEmpty(status.Account.Acct, t.handle), u.Hostname())
		return &Result{Title: "@" + handle + " " + t.id, Files: files}, nil
	}

	account, err := f.mastodonLookup(ctx, root, t.handle)
	if err != nil {
		return nil, err
	}
	title := "@" + fediHandle(util.FirstNonEmpty(account.Acct, account.Username, t.handle), u.Hostname())

	statuses, err := f.mastodonTimeline(ctx, root, account.ID, "", pixelfed)
	if err != nil {
		return nil, err
	}
	files := mastodonFiles(statuses)
	if len(files) == 0 {
		_, stride := mastodonPaging(pixelfed)
		return nil, fmt.Errorf("fediverse: %s has attached no media to its most recent %d posts",
			title, config.MaxTimelinePages*stride)
	}
	return &Result{Title: title, Files: files}, nil
}

// mastodonPaging picks the base and the page stride for the dialect in hand.
//
// Pixelfed answers /api/v1/accounts/<id>/statuses with a 302 to its login
// page, and following that redirect yields HTML where JSON was expected. Its
// own prefix serves the identical Mastodon-shaped statuses to anyone. The
// account lookup is the other way round — only /api/v1 has it — so the two
// bases cannot be collapsed into one. The stride travels with the base
// because pixelfed caps a page at 24 and refuses anything larger.
func mastodonPaging(pixelfed bool) (api string, pageSize int) {
	if pixelfed {
		return fediPixelfedAPI, fediPixelfedPageSize
	}
	return fediMastodonAPI, fediMastodonPageSize
}

// mastodonLookup turns a handle into the account, and above all into its id.
//
// lookup is asked first and /accounts/<nickname> second, in that order for a
// reason: Pleroma predates accounts/lookup and answers 404 for it while
// accepting a nickname where Mastodon insists on a numeric id — and Mastodon
// answers 404 for *that*. Either way round only one host pays the second
// request, and only when the first found nothing.
func (f *Fediverse) mastodonLookup(ctx context.Context, root, handle string) (*mastodonAccount, error) {
	if handle == "" {
		return nil, fmt.Errorf("fediverse: the link names no account")
	}

	var account mastodonAccount
	lookup := root + fediMastodonAPI + "/accounts/lookup?acct=" + url.QueryEscape(handle)
	err := f.client.GetJSON(ctx, lookup, fediHeaders(root), &account)
	if err == nil && account.ID != "" {
		return &account, nil
	}

	byName := root + fediMastodonAPI + "/accounts/" + url.PathEscape(handle)
	if nickErr := f.client.GetJSON(ctx, byName, fediHeaders(root), &account); nickErr == nil && account.ID != "" {
		return &account, nil
	}
	if err == nil {
		err = fmt.Errorf("the lookup returned no account")
	}
	return nil, fmt.Errorf("fediverse: no account %q on this instance: %w", handle, err)
}

// mastodonTimeline pages back through an account's media posts.
//
// max_id is exclusive and the listing is newest first, so each page asks for
// what precedes the last id of the one before. A page that comes back short,
// or that hands back the id it was given, ends the walk — as does the cap,
// which is what stops an account with a decade of posts resolving for
// minutes. only_media is what keeps that cap spent on posts there is
// something to fetch from rather than on a year of conversation. A page after
// the first that fails is not fatal: what has already been collected is still
// worth queueing.
func (f *Fediverse) mastodonTimeline(ctx context.Context, root, accountID, until string, pixelfed bool) ([]mastodonStatus, error) {
	api, pageSize := mastodonPaging(pixelfed)

	var all []mastodonStatus
	for page := 0; page < config.MaxTimelinePages; page++ {
		query := url.Values{}
		query.Set("limit", strconv.Itoa(pageSize))
		query.Set("only_media", "true")
		if until != "" {
			query.Set("max_id", until)
		}
		endpoint := fmt.Sprintf("%s%s/accounts/%s/statuses?%s",
			root, api, url.PathEscape(accountID), query.Encode())

		var batch []mastodonStatus
		if err := f.client.GetJSON(ctx, endpoint, fediHeaders(root), &batch); err != nil {
			if page == 0 {
				return nil, fmt.Errorf("fediverse: read the account's posts: %w", err)
			}
			break
		}
		if len(batch) == 0 {
			break
		}
		all = append(all, batch...)

		next := batch[len(batch)-1].ID
		if next == "" || next == until || len(batch) < pageSize {
			break
		}
		until = next
	}
	return all, nil
}

// mastodonOne fetches a single post.
//
// Pixelfed serves neither of its single-status routes to a logged-out caller
// — /api/v1 redirects to the login page and its own prefix answers 403 — so
// there the post is found on the account's timeline instead, and in one
// request rather than by walking it: ids are ordered and max_id is exclusive,
// so asking for what precedes id+1 puts the wanted post first.
func (f *Fediverse) mastodonOne(ctx context.Context, root string, t *fediTarget, pixelfed bool) (*mastodonStatus, error) {
	if pixelfed {
		account, err := f.mastodonLookup(ctx, root, t.handle)
		if err != nil {
			return nil, err
		}
		id, err := strconv.ParseUint(t.id, 10, 64)
		if err != nil || id == ^uint64(0) {
			return nil, fmt.Errorf("fediverse: %q is not a pixelfed post id", t.id)
		}
		statuses, err := f.mastodonTimeline(ctx, root, account.ID, strconv.FormatUint(id+1, 10), true)
		if err != nil {
			return nil, err
		}
		for i := range statuses {
			if statuses[i].ID == t.id {
				return &statuses[i], nil
			}
		}
		return nil, fmt.Errorf("fediverse: post %s is not among %s's recent posts, and pixelfed "+
			"serves a single post only to signed-in callers", t.id, t.handle)
	}

	var status mastodonStatus
	endpoint := root + fediMastodonAPI + "/statuses/" + url.PathEscape(t.id)
	if err := f.client.GetJSON(ctx, endpoint, fediHeaders(root), &status); err != nil {
		return nil, fmt.Errorf("fediverse: read post %s: %w", t.id, err)
	}
	// A link to a boost was pasted because of what the boost shows, so this
	// one follows it. A timeline does the opposite; see mastodonFiles.
	if status.Reblog != nil {
		return status.Reblog, nil
	}
	return &status, nil
}

// mastodonFiles flattens statuses into downloadable attachments.
//
// A boost is skipped rather than followed, and that is not merely a
// preference about whose posts belong in the folder: Pleroma and Akkoma fill
// a boost's own media_attachments with the *original author's* files, where
// Mastodon leaves it empty. Taking them would quietly mix somebody else's
// media into a job that named this account, and on a Pleroma instance that is
// the common case rather than the odd one.
//
// No size is reported at all, because nothing in this API states a byte
// length — see mastodonMedia for the field that looks like one and is not.
func mastodonFiles(statuses []mastodonStatus) []File {
	seen := make(map[string]bool)
	var files []File

	for _, status := range statuses {
		if status.Reblog != nil {
			continue
		}
		for i, media := range status.Media {
			link := util.FirstNonEmpty(media.URL, media.RemoteURL)
			if link == "" || seen[link] {
				continue
			}
			seen[link] = true
			files = append(files, File{
				Name: fmt.Sprintf("%s_%d%s", status.ID, i+1, fediExt(link)),
				URL:  link,
				Size: -1,
			})
		}
	}
	return files
}

// --------------------------------------------------------------------- lemmy

// lemmyAPIs are the API roots to try, in the order a probe should ask.
//
// v3 comes first because it is what every instance in the wild answers today;
// v4 arrives with Lemmy 1.0. Which one an instance speaks is not something to
// assume in either direction, so it is discovered — once per job, on the
// first request, rather than as a request of its own.
var lemmyAPIs = []string{"/api/v3", "/api/v4"}

// lemmyClient remembers which version answered, so the probe is not repeated
// for every page of a listing.
type lemmyClient struct {
	f    *Fediverse
	root string
	api  string
}

// get performs one API call, resolving the version on the way if it is not
// settled yet.
//
// A 404 is what the wrong version answers — and, awkwardly, also what an
// unknown community answers. That ambiguity is why a total failure reports
// the *first* version's error rather than the last: on every instance
// deployed today that is the one whose message names what was really missing.
func (l *lemmyClient) get(ctx context.Context, route string, query url.Values, out any) error {
	if l.api != "" {
		return l.f.client.GetJSON(ctx, l.endpoint(l.api, route, query), fediHeaders(l.root), out)
	}

	var first error
	for _, api := range lemmyAPIs {
		err := l.f.client.GetJSON(ctx, l.endpoint(api, route, query), fediHeaders(l.root), out)
		if err == nil {
			l.api = api
			return nil
		}
		if first == nil {
			first = err
		}
		if !httpx.HasStatus(err, http.StatusNotFound) {
			return err
		}
	}
	return first
}

func (l *lemmyClient) endpoint(api, route string, query url.Values) string {
	return l.root + api + route + "?" + query.Encode()
}

// lemmyPost is one submission.
//
// thumbnail_url is deliberately absent: it is the instance's own scaled copy
// of whatever the post points at, served from its pict-rs no matter where the
// original lives, and taking it would fill a folder with previews.
type lemmyPost struct {
	ID      int64  `json:"id"`
	Name    string `json:"name"`
	URL     string `json:"url"`
	Deleted bool   `json:"deleted"`
	Removed bool   `json:"removed"`
	// ContentType is what the instance saw when it fetched the link. It is
	// the only honest way to tell an image submission from a link to an
	// article, and versions before 0.19 do not send it.
	ContentType string `json:"url_content_type"`
}

// lemmyPostView wraps a post with the context the API attaches to it.
type lemmyPostView struct {
	Post lemmyPost `json:"post"`
}

// lemmy resolves a community, a person's submissions, or a single post.
func (f *Fediverse) lemmy(ctx context.Context, u *url.URL, t *fediTarget) (*Result, error) {
	client := &lemmyClient{f: f, root: util.Origin(u)}

	if t.kind == fediPost {
		var payload struct {
			PostView lemmyPostView `json:"post_view"`
		}
		if err := client.get(ctx, "/post", url.Values{"id": {t.id}}, &payload); err != nil {
			return nil, fmt.Errorf("fediverse: read post %s: %w", t.id, err)
		}
		files := lemmyFiles([]lemmyPostView{payload.PostView})
		if len(files) == 0 {
			return nil, fmt.Errorf("fediverse: post %s links to no media — it is a text post, "+
				"or a link to a page rather than to a file", t.id)
		}
		title := util.FirstNonEmpty(payload.PostView.Post.Name, "post "+t.id)
		return &Result{Title: folderName(title), Files: files}, nil
	}

	route, key := "/post/list", "community_name"
	title := "!" + fediHandle(t.handle, u.Hostname())
	if t.kind == fediAccount {
		// A person's page is the same listing keyed differently. The response
		// carries their comments as well, which hold nothing to download.
		route, key = "/user", "username"
		title = "@" + fediHandle(t.handle, u.Hostname())
	}

	var views []lemmyPostView
	for page := 1; page <= config.MaxTimelinePages; page++ {
		query := url.Values{}
		query.Set(key, t.handle)
		query.Set("limit", strconv.Itoa(fediLemmyPageSize))
		query.Set("page", strconv.Itoa(page))
		query.Set("sort", "New")

		var payload struct {
			Posts []lemmyPostView `json:"posts"`
		}
		if err := client.get(ctx, route, query, &payload); err != nil {
			if page == 1 {
				return nil, fmt.Errorf("fediverse: read %s: %w", title, err)
			}
			break
		}
		// Lemmy 1.0 replaces page numbers with a cursor, and an instance that
		// ignores a parameter it no longer knows would hand back the first
		// page for ever. Stopping once a page adds nothing new is what makes
		// that one wasted request instead of an endless loop.
		before := len(views)
		lemmyAppend(&views, payload.Posts)
		if len(views) == before || len(payload.Posts) < fediLemmyPageSize {
			break
		}
	}

	files := lemmyFiles(views)
	if len(files) == 0 {
		return nil, fmt.Errorf("fediverse: %s has no image or video submissions among its most "+
			"recent %d posts", title, config.MaxTimelinePages*fediLemmyPageSize)
	}
	return &Result{Title: title, Files: files}, nil
}

// lemmyAppend adds the posts of a page that are not held already.
func lemmyAppend(views *[]lemmyPostView, page []lemmyPostView) {
	seen := make(map[int64]bool, len(*views))
	for _, view := range *views {
		seen[view.Post.ID] = true
	}
	for _, view := range page {
		if !seen[view.Post.ID] {
			seen[view.Post.ID] = true
			*views = append(*views, view)
		}
	}
}

// lemmyFiles keeps the submissions that are a file rather than a page.
//
// A post's url is whatever was submitted: for an image post it is the pict-rs
// object — often on another instance, since a federated community carries
// everyone's — but for a link post it is a news article, and queueing that
// would save an HTML page under a picture's name. The saved name pairs the
// post id with its title, because pict-rs names everything after a UUID and
// two posts can share a title.
func lemmyFiles(views []lemmyPostView) []File {
	seen := make(map[string]bool)
	var files []File

	for _, view := range views {
		post := view.Post
		if post.URL == "" || post.Deleted || post.Removed || seen[post.URL] {
			continue
		}
		if !lemmyIsMedia(&post) {
			continue
		}
		seen[post.URL] = true

		name := strconv.FormatInt(post.ID, 10)
		if title := folderName(post.Name); title != "" {
			name += " " + title
		}
		files = append(files, File{
			Name: name + fediExt(post.URL),
			URL:  post.URL,
			Size: -1,
		})
	}
	return files
}

// lemmyIsMedia reports whether a submission points at a file worth fetching.
//
// The instance's own reading of the link is trusted where it says something:
// media is media, and text/html is a page whatever its path suggests. What it
// records for a link it could not identify is neither — a video site's watch
// page comes back as "application/binary" — so those fall through to the path,
// which is also all that an instance too old to record a type ever leaves.
func lemmyIsMedia(post *lemmyPost) bool {
	switch kind, _, _ := strings.Cut(post.ContentType, "/"); kind {
	case "image", "video", "audio":
		return true
	case "text":
		return false
	}
	return fediIsMediaURL(post.URL)
}

// ------------------------------------------------------------------- misskey

// misskeyUser is the part of a user this needs.
type misskeyUser struct {
	ID       string `json:"id"`
	Username string `json:"username"`
	Host     string `json:"host"`
}

// misskeyNote is one post.
type misskeyNote struct {
	ID    string        `json:"id"`
	Text  string        `json:"text"`
	User  misskeyUser   `json:"user"`
	Files []misskeyFile `json:"files"`
}

// misskeyFile is one attachment.
//
// Unlike the Mastodon family this states an exact byte length — but it states
// the *original upload's*, and url does not always serve that file. See
// misskeyFiles. thumbnailUrl is absent for the usual reason: it is a
// thumbnail.
type misskeyFile struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Size int64  `json:"size"`
	URL  string `json:"url"`
}

// misskeyWebPublic prefixes the filename of an instance's re-encoded copy.
const misskeyWebPublic = "webpublic-"

// misskey resolves a user's notes, or a single note. Every call is a POST
// carrying JSON, including the ones that only read.
func (f *Fediverse) misskey(ctx context.Context, u *url.URL, t *fediTarget) (*Result, error) {
	if t.kind == fediCommunity {
		return nil, fmt.Errorf("fediverse: communities are a lemmy thing, and %s does not run "+
			"it (%s)", u.Hostname(), u.Redacted())
	}
	root := util.Origin(u)

	if t.kind == fediPost {
		var note misskeyNote
		body := struct {
			NoteID string `json:"noteId"`
		}{NoteID: t.id}
		if err := f.client.PostJSON(ctx, root+"/api/notes/show", fediHeaders(root), body, &note); err != nil {
			return nil, fmt.Errorf("fediverse: read note %s: %w", t.id, err)
		}
		files := misskeyFiles([]misskeyNote{note})
		if len(files) == 0 {
			return nil, fmt.Errorf("fediverse: note %s carries no files", t.id)
		}
		return &Result{Title: misskeyNoteTitle(&note, u.Hostname()), Files: files}, nil
	}

	user, err := f.misskeyLookup(ctx, root, t.handle)
	if err != nil {
		return nil, err
	}
	title := "@" + fediHandle(t.handle, u.Hostname())

	notes, err := f.misskeyTimeline(ctx, root, user.ID)
	if err != nil {
		return nil, err
	}
	files := misskeyFiles(notes)
	if len(files) == 0 {
		return nil, fmt.Errorf("fediverse: %s has attached no files to its most recent %d notes",
			title, config.MaxTimelinePages*fediMisskeyPageSize)
	}
	return &Result{Title: title, Files: files}, nil
}

// misskeyLookup turns a handle into the user, and above all into their id.
//
// host is null for a local account and the domain for one this instance is
// merely showing, and it has to be *sent* either way: it is how the API tells
// "the alice here" from "the alice over there".
func (f *Fediverse) misskeyLookup(ctx context.Context, root, handle string) (*misskeyUser, error) {
	name, host, _ := strings.Cut(handle, "@")
	if name == "" {
		return nil, fmt.Errorf("fediverse: the link names no account")
	}
	body := struct {
		Username string  `json:"username"`
		Host     *string `json:"host"`
	}{Username: name}
	if host != "" {
		body.Host = &host
	}

	var user misskeyUser
	if err := f.client.PostJSON(ctx, root+"/api/users/show", fediHeaders(root), body, &user); err != nil {
		return nil, fmt.Errorf("fediverse: no account %q on this instance: %w", handle, err)
	}
	if user.ID == "" {
		return nil, fmt.Errorf("fediverse: no account %q on this instance", handle)
	}
	return &user, nil
}

// misskeyTimeline pages back through a user's notes that carry files.
//
// untilId is this API's max_id: exclusive, newest first, each page asking for
// what precedes the last id of the one before. The same three stopping
// conditions apply as to a Mastodon timeline — a short page, a page that
// makes no progress, and the cap.
func (f *Fediverse) misskeyTimeline(ctx context.Context, root, userID string) ([]misskeyNote, error) {
	var all []misskeyNote
	until := ""

	for page := 0; page < config.MaxTimelinePages; page++ {
		body := struct {
			UserID    string `json:"userId"`
			Limit     int    `json:"limit"`
			WithFiles bool   `json:"withFiles"`
			UntilID   string `json:"untilId,omitempty"`
		}{UserID: userID, Limit: fediMisskeyPageSize, WithFiles: true, UntilID: until}

		var batch []misskeyNote
		if err := f.client.PostJSON(ctx, root+"/api/users/notes", fediHeaders(root), body, &batch); err != nil {
			if page == 0 {
				return nil, fmt.Errorf("fediverse: read the account's notes: %w", err)
			}
			break
		}
		if len(batch) == 0 {
			break
		}
		all = append(all, batch...)

		next := batch[len(batch)-1].ID
		if next == "" || next == until || len(batch) < fediMisskeyPageSize {
			break
		}
		until = next
	}
	return all, nil
}

// misskeyFiles flattens notes into downloadable attachments.
//
// A renote needs none of the care a Mastodon boost does: a note's files are
// always its own, and a plain renote carries none, so asking for notes with
// files never returns somebody else's.
//
// The size is where care is needed. It is exact — but it describes the file
// as uploaded, and url does not always serve that file: an instance that
// re-encodes images for the web publishes the copy under a "webpublic-" name,
// and a measured example was 4.8 MB by this field and 1.1 MB on the wire. The
// original is not addressable, its object name being a different UUID
// entirely, so the copy is what gets fetched and the size is dropped rather
// than reported. A length describing a different file is worse than none:
// this one is exact enough to be believed, and the downloader would use it to
// conclude that a file already on disk was this one.
func misskeyFiles(notes []misskeyNote) []File {
	seen := make(map[string]bool)
	var files []File

	for _, note := range notes {
		for _, file := range note.Files {
			if file.URL == "" || seen[file.URL] {
				continue
			}
			seen[file.URL] = true

			size := file.Size
			if size <= 0 || misskeyIsDerived(file.URL) {
				size = -1
			}
			files = append(files, File{
				Name: misskeyName(&file),
				URL:  file.URL,
				Size: size,
			})
		}
	}
	return files
}

// misskeyIsDerived reports whether a URL serves the instance's own re-encoded
// copy rather than what was uploaded.
func misskeyIsDerived(link string) bool {
	return strings.HasPrefix(util.NameFromURL(link), misskeyWebPublic)
}

// misskeyName keeps the uploader's filename but corrects its extension to
// what the URL actually serves, so a re-encoded copy is not saved under the
// format it used to be.
func misskeyName(file *misskeyFile) string {
	name := util.FirstNonEmpty(file.Name, util.NameFromURL(file.URL), file.ID)
	served := fediExt(file.URL)
	if served != "" && !strings.EqualFold(path.Ext(name), served) {
		name = strings.TrimSuffix(name, path.Ext(name)) + served
	}
	return name
}

// misskeyNoteTitle names a single-note job after what was written, falling
// back to the handle and id for a note that is media and nothing else.
func misskeyNoteTitle(note *misskeyNote, host string) string {
	if text := folderName(note.Text); text != "" {
		return text
	}
	// The user's own host is set only for an account this instance is
	// showing on another's behalf; for a local one it is null.
	author := note.User.Username
	if note.User.Host != "" {
		author += "@" + note.User.Host
	}
	return "@" + fediHandle(author, host) + " " + note.ID
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
