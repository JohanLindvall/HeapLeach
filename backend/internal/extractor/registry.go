// SPDX-License-Identifier: MIT

package extractor

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strings"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
	"github.com/JohanLindvall/HeapLeach/internal/util"
)

// Registry dispatches a URL to the first extractor that recognises its page.
type Registry struct {
	extractors []Extractor
	fallback   Extractor
	// client probes pages for identification, and pages keeps what it got.
	client *httpx.Client
	pages  pageCache
	// limits is what every extraction through this registry is bounded by.
	limits Limits
	// hostAliases adds hosts to siteHosts, by extractor name, for what the
	// link scanner follows; claimAliases for what is claimed by its host
	// (hostClaims). A test's reserved host standing in for a site's own;
	// nil outside tests.
	hostAliases, claimAliases map[string][]string
	// families are each platform family's installs (familyHosts).
	families map[string][]string
}

// NewRegistry wires up every supported extractor, in the order a probed page
// is offered to them: the first whose Recognise takes the page extracts it.
// Several link forms overlap, so the order is part of the behaviour, and
// this list is the one place it is written down.
//
// The registry is allocated first, so the extractors that resolve what
// they find through the registry they are part of — the link harvester,
// the pointer pages, the indexes and the fallback — are each handed it as
// they are built, rather than wired afterwards or given one of their own.
func NewRegistry(cfg *config.Config, client *httpx.Client) *Registry {
	direct := NewDirect(client)
	reg := &Registry{
		client:   client,
		fallback: direct,
		limits:   Limits{Sources: cfg.MaxSources, Files: cfg.MaxFiles},
		families: familyHosts(cfg),
	}
	direct.registry = reg
	reg.extractors = slices.Concat([]Extractor{
		// The link harvester claims no host: only an explicit "links:"
		// prefix reaches it. It leads the list because the prefix, not the
		// page, decides: a harvest of a page another extractor recognises
		// would otherwise be routed there, which would try to fetch
		// "links://..." and fail on the scheme.
		NewLinks(client, reg),
		NewGofile(cfg, client),
		NewMega(client),
		NewBunkr(client),
		NewErome(client),
		NewRajce(client),
		NewPixeldrain(client),
		NewKeep2Share(client),
		NewFileJump(client),
		NewEmload(client),
		NewTurbo(client),
		NewDropbox(client),
		NewMediafire(client),
		NewGoogleDrive(client),
		NewSVTPlay(client),
		// Public broadcasters, all the same job as SVT Play: an open
		// metadata endpoint, a DRM check, and either a plain rangeable file
		// or an HLS stream, its audio and subtitles joined to it where they
		// are kept apart. Those still on the yt-dlp handoff list are there
		// for the reasons handoffs.go states.
		NewRUV(client),
		NewARD(client),
		NewZDF(client),
		NewSRF(client),
		NewArte(client),
		NewORF(client),
		NewYle(client),
		NewDRTV(client),
		NewRTBF(client),
		NewFranceTV(client),
		NewRadioCanada(client),
		NewVRTMax(client),
		NewNPOStart(client),
		NewRaiPlay(client),
		NewRTVE(client),
		NewRTPPlay(client),
		NewPBS(client),
		NewNPR(client),
		NewABCListen(client),
		NewBBCSounds(client),
		NewVimeo(client),
		NewNiconico(client),
		NewPointer(client, reg, "yandex"),
		NewFourChan(client),
		NewRedGifs(client),
		NewKemono(client),
		NewPlayer(client, &pornhubPlayer),
		NewPlayer(client, &xhamsterPlayer),
		NewPlayer(client, &tnaflixPlayer),
		NewPlayer(client, &drtuberPlayer),
		NewPlayer(client, &pornonePlayer),
		NewPlayer(client, &epornerPlayer),
		NewPixhost(client),
		NewImagePond(client),
		NewSuvobox(client),
		NewFilester(client),
		NewBandcamp(client),
		NewCyberdrop(client),
		NewImgur(client),
		NewCivitai(client),
		NewBitChute(client),
		NewStreamable(client),
		NewWeTransfer(client),
		NewPlayer(client, &vidmolyPlayer),
		NewNRK(client),
		NewPornPics(client),
		NewArchiveOrg(cfg, client),
		NewFeeds(client),
		NewAutoindex(client),
		NewFapello(client),
		NewCoomerFans(client),
		NewCeleb(client),
		NewCum(client),
		NewPlayer(client, &okruPlayer),
		NewPlayer(client, &streamtapePlayer),
		NewPlayer(client, &doodPlayer),
		NewPlayer(client, &mixdropPlayer),
		NewPlayer(client, &luluPlayer),
		NewPlayer(client, &vidsPlayer),
		NewPlayer(client, &sendvidPlayer),
		NewPlayer(client, &knuthingPlayer),
		NewPlayer(client, &noodlePlayer),

		// Platform families: one extractor covering every install of a
		// piece of software, recognised by what the install serves — its
		// API, its bundle, its markup — rather than by a list of hosts.
		// Registered after the single-site extractors, whose recognisers
		// are narrower and the better answer for a page both would take.
		NewMediaWiki(client),
		// The image boards are recognised by their API where the page
		// cannot say, and their forms (/posts/<n>, /images/<n>, ?tags=)
		// are common ones, so every single-site recogniser is offered a
		// page first: a page one of them takes costs no guessed API
		// request on its origin.
		NewBooru(client),
	}, NewHandoffs(client), []Extractor{
		NewPeerTube(client),
		NewChevereto(client),
		NewBandzoogle(client),
		NewFoolFuuka(client),
		NewKVS(client),
		// The fediverse's paths are its conventions rather than any one
		// server's, and its /c/<name> is also Rumble's and PeerTube's
		// channel shape. So it comes after every family whose recogniser
		// is narrower, or a page both would take goes to it.
		NewFediverse(client),
		// Then the generic capabilities: a bare HLS manifest is a manifest
		// whatever serves it, and this must never take a page a more
		// specific extractor would have recognised.
		NewHLSDirect(client),
		// Last, the pages that host nothing themselves: one that only
		// points at a video elsewhere, and the album indexes, each
		// resolving what it finds through the registry.
		NewPointer(client, reg, "alohatube"),
		NewBalbums(client, reg),
		NewVoyeurKing(client, reg),
	})
	return reg
}

// Hosts lists the supported labels, for the startup log.
func (r *Registry) Hosts() []string {
	names := make([]string, 0, len(r.extractors))
	for _, e := range r.extractors {
		names = append(names, e.Name())
		if l, ok := e.(siteLabeller); ok {
			for _, label := range l.siteLabels() {
				if !slices.Contains(names, label) {
					names = append(names, label)
				}
			}
		}
	}
	return names
}

// Guess names the extractor a URL's form points to, before anything is
// fetched: the first whose form claims it, for a log line. Extraction
// decides for real; a label shown to the user is Label's, which names no
// claimant where several forms claim the link.
func (r *Registry) Guess(u *url.URL) string {
	if ex, ok := r.Known(u); ok {
		return ex.Name()
	}
	return r.fallback.Name()
}

// Label names a job not yet read: the extractor whose hosts claim the link
// (hostClaims, by install for a family); else the extractor its link's form
// points to when exactly one form claims it, else the host it is on. A
// single site's form counts only on that site's hosts (formClaimsHere).
// Forms overlap —
// a player's /e/<id> is a share's too — and the first of several is a
// guess wrong as often as right, so an ambiguous link is shown by where it
// is rather than by a name it may never get. Extraction decides for real.
func (r *Registry) Label(u *url.URL) string {
	if e := r.hostClaim(u, claimFirst, claimFirstForm, claimFallback, claimLast); e != nil {
		return r.JobLabel(e, u)
	}
	if only, ok := r.claimant(u); ok {
		return only.Name()
	}
	return cmp.Or(u.Hostname(), r.fallback.Name())
}

// claimant is the one extractor whose form claims the link, and whether
// exactly one does.
func (r *Registry) claimant(u *url.URL) (Extractor, bool) {
	var only Extractor
	for _, e := range r.extractors {
		if r.formClaimsHere(e, u) {
			if only != nil {
				return nil, false
			}
			only = e
		}
	}
	return only, only != nil
}

// claimsHere reports whether any extractor's form claims the link where
// it is (formClaimsHere): a single site's path on an unrelated host is that
// host's own page, not a supported link.
func (r *Registry) claimsHere(u *url.URL) bool {
	return slices.ContainsFunc(r.extractors, func(e Extractor) bool { return r.formClaimsHere(e, u) })
}

// formClaimsHere is formClaims held to the hosts the form can lead to an
// item on: a single site's form on that site's hosts (siteHosts), or one
// whose id names the service anywhere (offHostForms); a platform's or
// capability's form on any host.
func (r *Registry) formClaimsHere(e Extractor, u *url.URL) bool {
	if !formClaims(e, u) {
		return false
	}
	hosts, single := siteHosts[e.Name()]
	if !single || onSiteHosts(u, hosts) || onSiteHosts(u, r.hostAliases[e.Name()]) || onSiteHosts(u, r.claimAliases[e.Name()]) {
		return true
	}
	off := offHostForms[e.Name()]
	return off != nil && off(u)
}

// formClaims reports whether the link has e's form: its Shape, or a link
// the scanner follows to it (linkForms).
func formClaims(e Extractor, u *url.URL) bool {
	if e.Shape(u) {
		return true
	}
	follow := linkForms[e.Name()].follow
	return follow != nil && follow(u)
}

// JobLabel names a job ex read from u. A platform family many operators
// run names the install, by its host, as each was named while installs
// were listed one by one; every other extractor names itself. It is a
// label only: the folder is the result's title.
func (r *Registry) JobLabel(ex Extractor, u *url.URL) string {
	if ex == nil {
		return ""
	}
	if u != nil {
		if label := r.installLabel(ex.Name(), u.Hostname()); label != "" {
			return label
		}
	}
	if named, ok := ex.(installNamer); ok && u != nil {
		if label := named.installName(u.Hostname()); label != "" {
			return label
		}
	}
	return ex.Name()
}

// installNamer is a platform family whose jobs are labelled by install.
type installNamer interface {
	installName(host string) string
}

// Identified reports whether ex is an extractor that recognised a page, as
// opposed to nothing or the fallback that takes everything.
func (r *Registry) Identified(ex Extractor) bool { return ex != nil && ex != r.fallback }

// Known returns the first extractor whose URL form the link has, and
// whether there was one, as opposed to the fallback that takes everything.
//
// The link harvester and the page scanner need that answered without a
// fetch: a page full of links of which a handful are downloads and the rest
// navigation, and the form of the link is all there is to separate them.
// The answer is a candidate, not a verdict; Extract probes the page and may
// find it is no extractor's. Besides each Shape, the links the scanner
// follows by the host that hands them out count (linkForms). A scanner
// that knows the page a link was found on asks Follows.
func (r *Registry) Known(u *url.URL) (Extractor, bool) {
	for _, e := range r.extractors {
		if formClaims(e, u) {
			return e, true
		}
	}
	return r.fallback, false
}

// AliasHosts makes hosts stand in for the extractor name's own (siteHosts)
// in what the link scanner follows. It is for tests, whose fixtures serve a
// site from a reserved host or a local address and may not carry the real
// site's item links; nothing else calls it, and it must be called before the
// registry is shared.
func (r *Registry) AliasHosts(name string, hosts ...string) {
	if r.hostAliases == nil {
		r.hostAliases = map[string][]string{}
	}
	r.hostAliases[name] = append(r.hostAliases[name], hosts...)
}

// Follows is Known for a link found on the page at from, held to the hosts
// a link can lead to an item on. Explicit links: harvesting also admits
// known installs' item routes through harvests.
//
// An extractor that serves one site (siteHosts) follows only links on its
// own hosts: its Shape names paths other sites use for their own pages, and
// a page's navigation would otherwise be probed link by link. On those
// hosts, a link found on another site's page is followed whatever its path,
// unless it names a file; a link found on the site's own page only when its
// Shape claims it, since there a bare path is as often an account or genre
// page as an item. A platform's or capability's Shape stays valid on any
// host, and so is a single-site extractor's form whose id names its service
// (offHostForms). Both kinds of linkForms are followed only to another site: a form a
// site's own pages use for navigation (linkForm.local), and a link named by
// its host (linkForm.follow).
func (r *Registry) Follows(u, from *url.URL) (Extractor, bool) {
	home := oneSite(u, from)
	for _, e := range r.extractors {
		form := linkForms[e.Name()]
		if !home && form.follow != nil && form.follow(u) {
			return e, true
		}
		hosts, single := siteHosts[e.Name()]
		if single && !onSiteHosts(u, hosts) && !onSiteHosts(u, r.hostAliases[e.Name()]) {
			if off := offHostForms[e.Name()]; off != nil && off(u) {
				return e, true
			}
			continue
		}
		if e.Shape(u) && (!home || form.local == nil || !form.local(u)) {
			return e, true
		}
		if single && !home && probeWorthy(u) {
			return e, true
		}
	}
	return r.fallback, false
}

// harvests adds an explicit harvest's same-site items to normal discovery.
// A platform's Shape alone is not enough: it often includes profiles and
// listings, and the same paths on an unrelated site can be navigation.
// Only a known install and the platform's item predicate lift that filter.
func (r *Registry) harvests(u, from *url.URL) (Extractor, bool) {
	if ex, ok := r.Follows(u, from); ok {
		return ex, true
	}
	if !oneSite(u, from) {
		return r.fallback, false
	}
	for _, e := range r.extractors {
		item := linkForms[e.Name()].harvest
		if item == nil {
			continue
		}
		hosts, _ := r.claimHosts(e)
		if (onSiteHosts(u, hosts) || onSiteHosts(u, r.hostAliases[e.Name()])) && item(u) {
			return e, true
		}
	}
	return r.fallback, false
}

// identify probes the URL and returns the first extractor that recognises
// the page, or the fallback.
//
// A link on a host whose extractor claims it first (hostClaims) is that
// extractor's without a probe, and no page is attached; one on a host whose
// extractor claims it as a fallback is that extractor's when no recogniser
// took the page, with the page as the probe found it.
func (r *Registry) identify(ctx context.Context, u *url.URL) (Extractor, *Page) {
	if e := r.hostClaim(u, claimFirst, claimFirstForm); e != nil {
		return e, nil
	}
	if !probeWorthy(u) && !r.claims(u) {
		return r.fallback, &Page{URL: u, Final: u}
	}
	var p *Page
	if r.client != nil {
		p = r.pages.get(ctx, r.client, u)
	} else {
		p = &Page{URL: u, Final: u, Err: errNoProbe}
	}
	for _, e := range r.extractors {
		if e.Recognise(ctx, p) {
			return e, p
		}
	}
	if e := r.hostClaim(u, claimFallback); e != nil {
		return e, p
	}
	return r.fallback, p
}

// proxyFallbackForm reports whether the URL has the form of a player row
// that opted into proxy recovery. Only a read whose probe failed asks it:
// the form is a candidate, and recovery re-reads through another route,
// where the page must then be recognised as the row's (sameRead).
func (r *Registry) proxyFallbackForm(u *url.URL) bool {
	return slices.ContainsFunc(r.extractors, func(e Extractor) bool {
		player, ok := e.(*Player)
		return ok && player.ProxyFallback() && player.Shape(u)
	})
}

// claims reports whether any extractor's form claims the URL.
func (r *Registry) claims(u *url.URL) bool {
	_, ok := r.Known(u)
	return ok
}

// errNoProbe is the page of a registry built without a client, as tests
// build one.
var errNoProbe = errors.New("no client to probe with")

// probeWorthy reports whether a URL could be a page, by the extension its
// path ends in: none, or one a page is served under. A link that names a
// file of some other kind, and that no extractor's form claims, is the
// fallback's without a fetch; the probe would only download the start of
// the very file the fallback is about to download.
func probeWorthy(u *url.URL) bool {
	if u.Scheme != "http" && u.Scheme != "https" {
		return true
	}
	if strings.Trim(u.Path, "/") == "" || strings.HasSuffix(u.Path, "/") {
		return true
	}
	ext := strings.ToLower(path.Ext(u.Path))
	switch ext {
	case "", ".html", ".htm", ".shtml", ".xhtml", ".php", ".asp", ".aspx", ".jsp", ".cgi", ".pl":
		return true
	}
	// Only something that reads as a file type counts: a slug such as
	// "part-2.0" or "v1.5" names a page whose title has a dot in it.
	return !fileExtension.MatchString(ext)
}

// fileExtension is an extension that names a file type.
var fileExtension = regexp.MustCompile(`^\.[a-z][a-z0-9]{0,4}$`)

// follow extracts a link chosen for the form of its link — a source on a
// page, a frame, a file a listing names — strictly: a page that turns out to
// be no extractor's is errUnrecognised, never handed to the fallback to be
// saved as a page.
func (r *Registry) follow(ctx context.Context, link string, opts Options) (*Result, Extractor, error) {
	opts.strict = true
	return r.Extract(ctx, link, opts)
}

// probeFailure is the error a probe that did not read its page stands for:
// its transport or body failure, or the status it was answered with. nil
// when the page was read.
func probeFailure(u *url.URL, page *Page) error {
	if page == nil {
		return nil
	}
	if page.Err != nil {
		return page.Err
	}
	if page.Status >= http.StatusBadRequest {
		return &httpx.StatusError{
			Code: page.Status, Status: fmt.Sprintf("%d %s", page.Status, http.StatusText(page.Status)),
			URL: u.Redacted(), Header: page.Header,
			Body: strings.Clone(page.Body[:min(len(page.Body), config.ErrorBodySample)]),
		}
	}
	return nil
}

// refusedPage reports whether a failed probe stands for a page rather than
// a file: a refusal whose body is text — an error page — or a failure no
// moment later will change (cancellation, a refused route). A refusal
// served as media or a binary type is left to the fallback, whose download
// asks with the Referer the probe sends only after a 401 or 403. So is a
// failure that may pass (transientProbe): no answer, a timeout, a rate
// limit or a server error, which the transfer engine's retries, stall
// handling and route recovery outlast where a refused job would not.
func refusedPage(ctx context.Context, page *Page) bool {
	if page == nil || transientProbe(ctx, page) {
		return false
	}
	if page.Status == 0 {
		return page.Err != nil
	}
	kind := page.MediaType()
	return probeReadsType(kind) || probeSniffsType(kind)
}

// transientProbe reports whether a probe failed in a way a moment later may
// not repeat: nothing answered, or it timed out, or it was answered with a
// rate limit or a server error (probeTransient).
func transientProbe(ctx context.Context, page *Page) bool {
	switch {
	case page == nil || page.Challenged:
		return false
	case page.Status == 0:
		return probeTransient(ctx, page.Err)
	}
	return page.Status == http.StatusTooManyRequests || page.Status == http.StatusRequestTimeout ||
		page.Status >= http.StatusInternalServerError
}

// errUnrecognised reports a source that had a supported form but whose page
// no extractor recognised: navigation that merely looked like a download.
var errUnrecognised = errors.New("no extractor recognises this page")

// Extract parses rawURL and resolves it.
func (r *Registry) Extract(ctx context.Context, rawURL string, opts Options) (*Result, Extractor, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	// One admission per top-level extraction, shared by every nested one
	// through ctx, so nested listings are held to one request bound
	// between them rather than multiplying their own (httpx.Admission).
	if httpx.AdmissionOf(ctx) == nil {
		ctx = httpx.WithAdmission(ctx, httpx.NewAdmission(config.DiscoveryRequests))
	}
	u, err := ParseURL(rawURL)
	if err != nil {
		return nil, nil, err
	}
	label := r.Label(u)
	if slices.Contains(opts.ancestors, u.String()) {
		return nil, nil, fmt.Errorf("%s: recursive source link", label)
	}
	if len(opts.ancestors) >= config.MaxExtractionDepth {
		return nil, nil, fmt.Errorf("%s: source nesting exceeds %d levels", label, config.MaxExtractionDepth)
	}
	// A source followed from another page is never the submitted URL, even
	// when the page it was found on was read without the registry.
	submitted := len(opts.ancestors) == 0 && !opts.strict
	opts.ancestors = append(slices.Clone(opts.ancestors), u.String())
	if opts.limits == (Limits{}) {
		opts.limits = r.limits
	}
	strict := opts.strict
	opts.strict = false
	// The page is identified afresh on every read, so that a read recovered
	// through another route, or a file refreshed at transfer time, is judged
	// by what that route was served rather than by the challenge the first
	// one met.
	read := func(ctx context.Context) (Extractor, *Page, *Result, error) {
		// The probe's own challenge is not the extraction's: a recogniser
		// may claim a challenged page by its form and an open API, and the
		// extractor it chose then goes on without the page. Only what that
		// extractor meets counts. A challenged page nobody claimed is the
		// read's answer as it stands, rather than one for the fallback to
		// fetch into again. The probe is made outside any enclosing
		// extraction's challenge capture: when this URL is a side link of
		// a page being read, its challenge is its own answer, and must not
		// become the enclosing page's (httpx.WithoutChallengeCapture).
		ex, page := r.identify(httpx.WithoutChallengeCapture(ctx), u)
		if ex == r.fallback {
			if page.Challenged {
				return ex, page, nil, page.Err
			}
			failed := probeFailure(u, page)
			// A failed probe says nothing about whether the page is
			// supported. A followed link keeps its error for source
			// retries and route recovery. Submitted links keep refusals
			// of claimed pages too, but a transient failure on an unrelated
			// host can reach the transfer's retries: a single site's URL
			// shape alone does not establish that the link is its page.
			// A page too large to read whole is the exception for a
			// submitted URL: the fallback sniffs its prefix.
			if failed != nil {
				claimed := r.claims(u)
				if submitted && ctx.Err() == nil && transientProbe(ctx, page) {
					claimed = r.claimsHere(u) ||
						r.hostClaim(u, claimFirst, claimFirstForm, claimFallback, claimLast) != nil
					if !claimed && page.Final != nil {
						// A redirect can establish the site association.
						// Keep even a known site's non-item page as its
						// failure when that host claims the page.
						claimed = r.claimsHere(page.Final) ||
							r.hostClaim(page.Final, claimFirst, claimFirstForm, claimFallback, claimLast) != nil
					}
				}
				if strict || ((claimed || refusedPage(ctx, page)) &&
					!httpx.IsTooLarge(failed) && !errors.Is(failed, errNoProbe)) {
					return ex, page, nil, failed
				}
			}
			if strict {
				// Only a successfully read, unrecognised page is
				// navigation.
				return ex, page, nil, fmt.Errorf("%s: %w", u.Redacted(), errUnrecognised)
			}
		}
		ctx, challenge := httpx.CaptureChallenges(ctx)
		res, err := ex.Extract(withPage(ctx, page), u, opts)
		if ex == r.fallback && errors.Is(err, errPlainPage) {
			// A page of a site whose extractor explains its other pages
			// (claimLast), which the fallback found nothing on.
			if last := r.hostClaim(u, claimLast); last != nil {
				ex = last
				res, err = last.Extract(withPage(ctx, page), u, opts)
			}
		}
		if ctx.Err() == nil && challenge() != nil {
			return ex, page, nil, challenge()
		}
		return ex, page, res, err
	}
	// pinned is the extractor the first successful read chose. A later
	// read — a refresh at transfer time, a read recovered through another
	// route — that lands anywhere else has been served something else
	// (a refusal, a block page, a redesign), never another answer for the
	// same file: taken, it would swap the item for the fallback's copy of
	// the page under a new name.
	var pinned Extractor
	sameRead := func(got Extractor, page *Page) error {
		if got == r.fallback {
			// The fallback's answer for a page that would not load is
			// that page, never the file.
			if failed := probeFailure(u, page); failed != nil && !errors.Is(failed, errNoProbe) {
				return failed
			}
		}
		if pinned != nil && got != pinned {
			return fmt.Errorf("%s: %s is no longer recognised as its page (it now reads as %s)",
				pinned.Name(), u.Redacted(), got.Name())
		}
		return nil
	}
	reread := func(ctx context.Context) (*Result, error) {
		got, page, res, err := read(ctx)
		if err != nil {
			return nil, err
		}
		if err := sameRead(got, page); err != nil {
			return nil, err
		}
		return res, nil
	}
	ex, _, res, err := read(ctx)
	recovered := false
	_, challenged := errors.AsType[*httpx.ChallengeError](err)
	// A player row may opt its sources into the manager's temporary-failure
	// recovery. It still starts on the normal connection; the manager owns
	// error classification, proxy availability and the route retry policy.
	player, isPlayer := ex.(*Player)
	optedIn := isPlayer && player.ProxyFallback()
	if err != nil && ex == r.fallback && !errors.Is(err, errUnrecognised) {
		// A probe refused, failed or answered with an error is no page to
		// recognise a player by: a link of an opted-in row's form keeps
		// its recovery, which is for exactly an address the host refuses.
		optedIn = optedIn || r.proxyFallbackForm(u)
	}
	if err != nil && httpx.RouteID(ctx) == "" && (challenged || optedIn) {
		if recover, ok := ctx.Value(challengeRecoveryKey{}).(ChallengeRecovery); ok {
			res, err = recover(ctx, func(ctx context.Context) (*Result, error) {
				got, page, res, err := read(ctx)
				if err == nil {
					// A route that was refused, or served a page nobody
					// recognises in place of the one that was blocked, has
					// not recovered the read: the recovery moves on.
					err = sameRead(got, page)
				}
				if err != nil {
					return nil, err
				}
				ex = got
				return res, nil
			}, err)
			recovered = err == nil
		}
	}
	if err == nil {
		pinned = ex
	}
	if err != nil {
		return nil, ex, err
	}
	if res == nil || len(res.Files) == 0 {
		return nil, ex, fmt.Errorf("%s: no downloadable files found at %s", ex.Name(), u.Redacted())
	}
	// The file cap holds whatever an extractor did about it: a submitted
	// URL that listed more than the setting allows is cut here, and says
	// so. A nested source is left whole for the expansion holding it, which
	// applies the cap a source at a time and counts what it cut.
	if limit := opts.maxFiles(); submitted && len(res.Files) > limit {
		res.Files = res.Files[:limit]
		res.Note = joinNotes(res.Note, fileLimitNote)
	}
	// The title names the job and, for a source expanded into folders, the
	// folder too; see util.Unescape for why it may still carry entities.
	res.Title = util.TitleName(util.Unescape(res.Title))
	res.Note = util.Unescape(res.Note)
	for i := range res.Files {
		res.Files[i].Name = util.TitleName(util.Unescape(res.Files[i].Name))
		res.Files[i].Dir = util.Unescape(res.Files[i].Dir)
		if res.Files[i].Refresh == nil {
			res.Files[i].Refresh = refreshFile(reread, res.Files[i], len(res.Files) == 1)
		}
		if recovered {
			res.Files[i].Resolve = res.Files[i].Refresh
		}
	}
	if res.Title == "" {
		res.Title = strings.Trim(u.Path, "/")
	}
	return res, ex, nil
}

// ParseURL normalises user input into an absolute http(s) URL.
func ParseURL(raw string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, errors.New("empty URL")
	}
	// A "links:" prefix asks for the page to be read for the links it
	// carries rather than downloaded. It is recognised here because this is
	// where raw input becomes a URL, and Find only ever sees one.
	if page, ok := linksInput(raw); ok {
		return linksURL(page)
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("invalid URL %q: %w", raw, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("unsupported scheme %q (only http and https)", u.Scheme)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("invalid URL %q: no host", raw)
	}
	return u, nil
}
