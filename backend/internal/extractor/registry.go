// SPDX-License-Identifier: MIT

package extractor

import (
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
		NewBooru(client),
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
	}
	return names
}

// Guess names the extractor a URL's form points to, before anything is
// fetched: a label for a job not yet read. Extraction decides for real.
func (r *Registry) Guess(u *url.URL) string {
	if ex, ok := r.Known(u); ok {
		return ex.Name()
	}
	return r.fallback.Name()
}

// Known returns the first extractor whose URL form the link has, and
// whether there was one, as opposed to the fallback that takes everything.
//
// The link harvester and the page scanner need that answered without a
// fetch: a page full of links of which a handful are downloads and the rest
// navigation, and the form of the link is all there is to separate them.
// The answer is a candidate, not a verdict; Extract probes the page and may
// find it is no extractor's.
func (r *Registry) Known(u *url.URL) (Extractor, bool) {
	for _, e := range r.extractors {
		if e.Shape(u) {
			return e, true
		}
	}
	return r.fallback, false
}

// identify probes the URL and returns the first extractor that recognises
// the page, or the fallback.
func (r *Registry) identify(ctx context.Context, u *url.URL) (Extractor, *Page) {
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
	return r.fallback, p
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
	label := r.Guess(u)
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
	read := func(ctx context.Context) (Extractor, *Result, error) {
		// The probe's own challenge is not the extraction's: a recogniser
		// may claim a challenged page by its form and an open API, and the
		// extractor it chose then goes on without the page. Only what that
		// extractor meets counts. A challenged page nobody claimed is the
		// read's answer as it stands, rather than one for the fallback to
		// fetch into again.
		ex, page := r.identify(ctx, u)
		if ex == r.fallback && page.Challenged {
			return ex, nil, page.Err
		}
		if strict && ex == r.fallback {
			// A failed probe says nothing about whether the page is supported.
			// Keep its error for source retries and route recovery; only a
			// successfully read, unrecognised page or a missing link is
			// navigation.
			if page.Err != nil {
				return ex, nil, page.Err
			}
			if page.Status >= http.StatusBadRequest && page.Status != http.StatusNotFound && page.Status != http.StatusGone {
				return ex, nil, &httpx.StatusError{
					Code: page.Status, Status: fmt.Sprintf("%d %s", page.Status, http.StatusText(page.Status)),
					URL: u.Redacted(), Header: page.Header,
					Body: strings.Clone(page.Body[:min(len(page.Body), config.ErrorBodySample)]),
				}
			}
			return ex, nil, fmt.Errorf("%s: %w", u.Redacted(), errUnrecognised)
		}
		ctx, challenge := httpx.CaptureChallenges(ctx)
		res, err := ex.Extract(withPage(ctx, page), u, opts)
		if ctx.Err() == nil && challenge() != nil {
			return ex, nil, challenge()
		}
		return ex, res, err
	}
	reread := func(ctx context.Context) (*Result, error) {
		_, res, err := read(ctx)
		return res, err
	}
	ex, res, err := read(ctx)
	recovered := false
	_, challenged := errors.AsType[*httpx.ChallengeError](err)
	// A player row may opt its sources into the manager's temporary-failure
	// recovery. It still starts on the normal connection; the manager owns
	// error classification, proxy availability and the route retry policy.
	player, isPlayer := ex.(*Player)
	if err != nil && httpx.RouteID(ctx) == "" && (challenged || (isPlayer && player.ProxyFallback())) {
		if recover, ok := ctx.Value(challengeRecoveryKey{}).(ChallengeRecovery); ok {
			res, err = recover(ctx, func(ctx context.Context) (*Result, error) {
				got, res, err := read(ctx)
				if err == nil {
					ex = got
				}
				return res, err
			}, err)
			recovered = err == nil
		}
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
