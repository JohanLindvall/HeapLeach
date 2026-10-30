// SPDX-License-Identifier: MIT

// Package extractor turns a page URL into the concrete list of files behind
// it. Each supported host gets one Extractor; unknown hosts fall through to
// a direct-link extractor.
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
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
	"github.com/JohanLindvall/HeapLeach/internal/util"
)

// File is a single downloadable resource.
type File struct {
	// Name is the destination filename, before sanitising. When Resolve is
	// set this is only a display hint until the item actually starts.
	Name string
	// URL is the direct link to fetch. Empty when Resolve supplies it.
	URL string
	// Size is the length in bytes, or -1 when the host did not report one.
	//
	// Exact means exact: the downloader skips a file whose length already
	// matches what is on disk, so a Size that is merely close is worse than
	// no Size at all. Two rules follow.
	//
	// A length read off a listing page and rounded for display sets
	// SizeApprox as well. And an extractor that REWRITES a URL must drop
	// the size the host gave it — imgur publishes the length of a GIF and
	// serves an MP4 of the same image up to fifteen times smaller, so
	// carrying that number over would have the skip check compare against a
	// length the file can never reach. Use -1, not SizeApprox, which would
	// still claim the right order of magnitude.
	Size int64
	// SizeApprox marks a Size that was read off a listing page and rounded
	// for display ("57.80 MB"). It is good enough to total a job with, but
	// never exact enough to decide that a file on disk is already this
	// one.
	SizeApprox bool
	// Headers are extra request headers this host requires (Referer,
	// Cookie, ...). They are re-applied on every retry.
	Headers httpx.Header
	// Dir is an optional relative subdirectory, used for nested folders.
	Dir string
	// Segments, when set, is an ordered list of parts to fetch and join
	// into one file. Adaptive streams (HLS) have no single file to range
	// over, so they arrive this way instead of as a URL. A part that is a
	// byte range of a larger file carries the range in its fragment
	// (SegmentRange).
	Segments []string
	// SegmentKey, when set, is the AES-128 key the Segments are encrypted
	// under. The downloader fetches it and decrypts each part as it lands,
	// so the file on disk is the plain stream.
	SegmentKey *SegmentKey
	// Audio, when set, is the soundtrack of a playlist that keeps it apart
	// from the video, as CMAF does: one Track per rendition, every
	// language, the default first. The downloader fetches each as it does
	// the Segments and joins them all into one MP4, by stream copy with
	// ffmpeg.
	Audio []Track
	// Subtitles are WebVTT tracks of a playlist's video, each a Track whose
	// Segments are its WebVTT files in order. The downloader joins them
	// into the MP4 as text tracks, leaving out any it cannot fetch.
	Subtitles []Track
	// External, when set, names a page that an external downloader handles
	// end to end, because reaching the media at all needs machinery well
	// beyond fetching a URL.
	External string
	// Cipher, when set, means the host serves this file encrypted and the
	// downloader must decrypt it on the way to disk.
	Cipher *StreamCipher
	// Resolve, when non-nil, produces the direct link at download time
	// instead of extraction time. Hosts that mint short-lived signed URLs
	// (bunkr, turbo) use this: a link signed while the item sat in the
	// queue would already have expired by the time its turn came.
	Resolve func(ctx context.Context) (*Target, error)
	// Refresh re-reads this file's source on a new route before resolving
	// its media. The registry supplies it for challenge recovery; ordinary
	// attempts keep the host's cheaper Resolve path.
	Refresh func(ctx context.Context) (*Target, error)
	// Check, when set, asks about the file itself before a route is spent
	// on it, over the caller's own connection: whether the host still offers
	// it free, say. An error fails the file; a question that cannot be
	// answered must return nil, and leave the deciding to Resolve.
	Check func(ctx context.Context) error
	// ProxyFallback opts this file into proxy recovery after a temporary
	// failure on the normal connection. Permanent file errors still fail.
	ProxyFallback bool
	// Pace, when set, holds the downloader back from this host. Nil is the
	// normal case: go as fast as the queue's own settings allow.
	Pace *Pace
	// Reject, when set, is given the response a transfer is about to write
	// — the URL redirects actually landed on, and the headers — and may
	// refuse it.
	//
	// The downloader already refuses a web page served in place of a file,
	// because a host that does that reports a length that agrees with the
	// page and the result records as a complete download. Some hosts have
	// the same failure in a form no general rule can catch: imgur answers a
	// deleted image with a redirect to a real 503-byte PNG, correct content
	// type, correct length, and 206 to a ranged request. Nothing about the
	// response is wrong except that it is not the file.
	//
	// Only the extractor knows what its host's dead end looks like, so this
	// is where that knowledge goes. A rejection is final: it means the
	// resource is gone, not that the attempt went badly, so it is never
	// retried.
	Reject func(finalURL string, header http.Header) error
	// Refused, when set, is given the status and headers a transfer was
	// refused with, and the request's context, which names the route it went
	// through. It may say what the refusal means in a resolver's terms: a
	// WaitError for an allowance the address has used up, say, which is how
	// mega's storage answers once an address's transfer quota is spent. Nil,
	// or a nil answer, leaves the refusal an ordinary failure.
	Refused func(ctx context.Context, status int, header http.Header) error
}

// Pace is how an extractor says "fetch this gently".
//
// Most hosts want nothing of the sort, and the queue's own concurrency and
// stream settings are the right answer. A few actively punish parallelism
// instead of refusing it — archive.org serves one connection at 7.6 MB/s and
// three at 21 kB/s each, per address, and goes on doing so for minutes after
// the connections are gone.
//
// That is invisible to every defence the downloader already has. Nothing is
// refused, so `hostLimiter.penalise` never fires; the bytes keep moving, so
// `watchForStall` is satisfied; and the transfer looks *slow*, which is
// precisely the condition that makes the supervisor open more connections.
// The engine would otherwise talk itself into the worst case and stay there.
//
// So a host that behaves this way declares it, and the extractor is where
// that knowledge belongs: the downloader cannot infer from a clean 206 that
// the host resents being asked twice.
type Pace struct {
	// Streams caps connections opened for one file. 1 disables splitting.
	// Zero means the queue's own ceiling applies.
	Streams int
	// Files caps transfers to this host running at once, across every job.
	// Zero means the queue's own concurrency applies. Capping Streams alone
	// is not enough: several files at one connection each is still several
	// connections to the same host.
	Files int
	// Group shares the file limit across aliases and storage servers. Empty
	// uses the item's host. Set it when the limit belongs to the service,
	// independently of which domain serves a file or whether it resolved yet.
	Group string
	// PerRoute allows a configured proxy pool to apply Files to each egress
	// address instead of the whole service. Group must name that service;
	// a route is leased before Resolve and held through the file transfer.
	PerRoute bool
	// DirectFirst leaves a PerRoute file on the normal connection, unpaced,
	// until the host makes that connection wait (a WaitError from Resolve or
	// from Refused), and only then moves it onto the group's routes, where
	// the rest of this Pace applies. It is for a host that counts a quota
	// per address, as mega does: while the normal connection has quota left
	// it is faster than any proxy, and once it has none only another address
	// can go on. Without proxies the file waits for the quota instead.
	DirectFirst bool
	// WAF applies separate challenge history when choosing a route, while
	// preserving the service's own per-address file limit.
	WAF bool
}

// StreamCipher describes payload that arrives encrypted.
//
// AES-CTR is the only mode here, and deliberately so. It is a stream cipher:
// the keystream at any byte offset is computable without touching a single
// byte before it, so every range decrypts on its own. That is what lets an
// encrypted host — mega, today — keep the ordinary transfer machinery
// instead of needing a serial path of its own: several connections at once,
// segments finishing out of order, and resuming a part file across runs all
// stay exactly as they are.
//
// The key never changes between attempts, so unlike a URL it belongs to the
// File rather than to a resolved Target.
type StreamCipher struct {
	// Key is the AES key: 16 bytes for AES-128.
	Key []byte
	// Nonce is the high half of the initial counter block; the low half is
	// the block index, which is what makes an arbitrary offset addressable.
	Nonce []byte
}

// Target is a freshly resolved direct link.
type Target struct {
	// URL is the direct link to fetch.
	URL string
	// Headers replace the File's headers when non-empty.
	Headers httpx.Header
	// Size is the length in bytes, or 0/-1 when unknown.
	Size int64
	// Name overrides the File's name when non-empty.
	Name string
	// Segments and SegmentKey replace the File's when set: a playlist whose
	// links are signed is read again at transfer time, key and all, or a
	// queue longer than the signature would fail partway down.
	Segments   []string
	SegmentKey *SegmentKey
	// Audio and Subtitles replace the File's when set, for the same reason.
	Audio     []Track
	Subtitles []Track
	// Discard is set by a resolver whose links are bound to the address
	// that obtained them, and drops what it keeps of this one so that the
	// next resolve starts afresh. The downloader calls it when the host
	// refuses the link because another address is using it: one route can
	// present several, since a new connection may leave by the other
	// address family or by another of a provider's exits.
	Discard func()
}

// file is the File a resolved Target describes, in the directory dir: what
// an extractor that resolves eagerly lists, where one deferring resolution
// would return the Target from Resolve.
func (t *Target) file(dir string) File {
	return File{
		Name: t.Name, URL: t.URL, Size: t.Size, Dir: dir, Headers: t.Headers,
		Segments: t.Segments, SegmentKey: t.SegmentKey, Audio: t.Audio, Subtitles: t.Subtitles,
	}
}

// WaitError is a resolver's way of saying a file cannot be fetched yet,
// rather than that it failed: Keep2Share, say, makes an address wait the
// best part of an hour between free downloads. The downloader puts the item
// back in the queue until Until, as waiting rather than downloading, and the
// worker it held goes on to something else meanwhile. Resolve is called
// again once the time has come.
type WaitError struct {
	Until time.Time
	// Reason is shown while the item waits, followed by the time left.
	Reason string
}

func (e *WaitError) Error() string {
	return fmt.Sprintf("%s until %s", e.Reason, e.Until.Format(time.TimeOnly))
}

// RefusedError is a resolver's way of saying a host refused the address it
// was asked from, not the file. Keep2Share answers "Download is not
// available", and nothing more, to free downloads from some addresses while
// handing the same file to the next one. Through a proxy route the
// downloader holds that route back from the host for a while and tries the
// file through another; without one there is nowhere else to ask, and it is
// an ordinary failure.
type RefusedError struct{ Err error }

func (e *RefusedError) Error() string { return e.Err.Error() }
func (e *RefusedError) Unwrap() error { return e.Err }

// TransientError marks a resolver's failure that the next attempt may well
// not meet, though nothing below HTTP saw it fail: a run of CAPTCHAs that
// could not be read in time, say. Through a proxy route the downloader
// tries the file again on the best route free; without one, resolution
// retries with backoff up to the normal retry budget.
type TransientError struct{ Err error }

func (e *TransientError) Error() string { return e.Err.Error() }
func (e *TransientError) Unwrap() error { return e.Err }

// Track is one audio rendition of a playlist that keeps its audio apart from
// its video: its own segments, in order, and what it is. A Track with no
// Segments is one the playlist names as carried in the video's own segments,
// which is how the main language and an alternate's playlist can sit side by
// side: it is listed so that the joined file labels it and keeps the order.
type Track struct {
	Segments []string
	Key      *SegmentKey
	// Language is the rendition's tag as the playlist gives it ("sv",
	// "sv-x-tydligaretal"), Name its label ("Svenska", "Tydligare tal").
	Language string
	Name     string
	// Default and Forced are the playlist's own flags. A subtitle marked
	// default is meant to show unasked; one forced, only the parts in
	// another language from the soundtrack.
	Default, Forced bool
}

// SegmentKey is how a playlist's segments are encrypted: HLS's
// METHOD=AES-128, which is AES-128 in CBC mode applied to each segment on
// its own, PKCS#7-padded, under one key fetched from a URL.
type SegmentKey struct {
	// URI is where the sixteen-byte key is fetched from. Often signed like
	// the segments, so it is read at transfer time, never stored.
	URI string
	// IV is the initialisation vector the playlist names, or nil when it
	// names none, in which case each segment's IV is its media sequence
	// number — Sequence for the first, counting up.
	IV       []byte
	Sequence int64
	// Clear is how many segments at the start are in the clear: 1 for a
	// fragmented MP4's initialisation segment named before the key. They
	// are neither decrypted nor counted in the media sequence.
	Clear int
}

// Result is everything an extractor found behind one input URL.
type Result struct {
	// Title names the job and the destination folder.
	Title string
	// Note is what the job's name adds for whoever is looking at it — that
	// a listing was cut short by a cap or a rate limit, and how far. It is
	// shown beside the title and never becomes part of the folder: the
	// folder is the title's alone, so a job read again with a higher cap,
	// or once the rate limit has lifted, finds everything it already
	// downloaded where it left it. A note in the title itself moved the
	// whole job to a new folder the moment the note changed.
	Note string
	// Files are the resources to download, in display order.
	Files []File
}

// Label is the title with its note, for display.
func (r *Result) Label() string {
	if r.Note == "" {
		return r.Title
	}
	return r.Title + " (" + r.Note + ")"
}

// Extractor resolves one family of pages: it recognises them (Recogniser),
// says which for the inventory (Describer), and extracts them.
type Extractor interface {
	Recogniser
	Describer
	// Name is the short label shown in the UI.
	Name() string
	// Extract resolves the URL into downloadable files. opts may be zero.
	Extract(ctx context.Context, u *url.URL, opts Options) (*Result, error)
}

// Recogniser is how an extractor claims a URL without naming its host.
//
// Nothing in either method may depend on the URL's host. The registry
// fetches the page first (probePage) and gives it to each extractor's
// Recognise in turn; the first to claim it extracts it. So an extractor is
// chosen by what the site serves — its name, its player, its bundle, its API
// answering — and a site that moves domain, or an install nobody listed,
// still reaches the right code.
type Recogniser interface {
	// Shape reports whether the URL has a form this extractor reads, by its
	// path, query and fragment. It is all the link scanner has to go on
	// before fetching, so it decides which links on a page are worth a
	// probe; a link of the right form that turns out to be someone else's
	// page is dropped after its probe. Report false for forms too common to
	// single out — the site root, a bare slug — rather than turn every
	// navigation link into a request.
	Shape(u *url.URL) bool
	// Recognise reports whether the probed page is one this extractor
	// reads. It may make a bounded request of its own (platformJSON) when
	// the page cannot say — a challenge stood in for it, say — but only once
	// the URL's form has made the answer likely: every unrecognised page is
	// offered to every extractor.
	Recognise(ctx context.Context, p *Page) bool
}

// ProxyFallback opts a source into the manager's temporary-failure recovery.
// It still starts on the normal connection; the manager owns error
// classification, proxy availability and the shared route retry policy.
type ProxyFallback interface {
	ProxyFallback() bool
}

// Options carries per-request extras supplied by the caller.
type Options struct {
	// Password unlocks protected folders.
	Password string

	// limits is stamped by Registry.Extract from the configuration, so it
	// reaches every extractor — including one reached through another, an
	// album found by a search. Unexported, so only this package can set it:
	// a caller outside gets what the configuration says. Read through
	// maxSources and maxFiles.
	limits Limits
	// ancestors is local to one expansion path, so independent sources can
	// resolve concurrently without sharing a mutable visited set.
	ancestors []string
	// strict marks a source followed for the form of its link: a page that
	// no extractor recognises is navigation, and is reported as
	// errUnrecognised rather than handed to the fallback. It applies to that
	// one URL, never to what its extractor goes on to resolve.
	strict bool
}

// Limits bound what one submitted URL may expand to. Zero in either means
// the built-in default.
type Limits struct {
	// Sources is how many of a page's links are followed: the albums of an
	// index search, the links of a harvested thread.
	Sources int
	// Files is how many files one submitted URL may resolve to.
	Files int
}

// maxSources is how many of a page's links may be followed.
func (o Options) maxSources() int {
	if o.limits.Sources > 0 {
		return o.limits.Sources
	}
	return config.MaxExpandedSources
}

// maxFiles is how many files one submitted URL may resolve to.
func (o Options) maxFiles() int {
	if o.limits.Files > 0 {
		return o.limits.Files
	}
	return config.MaxListingFiles
}

// ErrPasswordRequired signals that the content is gated behind a password.
var ErrPasswordRequired = errors.New("this link is password protected: supply the password and retry")

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

// NewRegistry wires up every supported host.
func NewRegistry(cfg *config.Config, client *httpx.Client) *Registry {
	extractors := []Extractor{
		NewGofile(cfg, client),
		NewMega(client),
		NewBunkr(client),
		NewErome(client),
		NewRajce(client),
		NewPixeldrain(client),
		NewKeep2Share(client),
		NewFileBoom(client),
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
		NewYouTube(),
		NewVimeo(client),
		NewNiconico(client),
		NewYandex(client),
		NewBooru(client),
		NewFourChan(client),
		NewRedGifs(client),
		NewKemono(client),
		NewPlayer(client, &pornhubPlayer),
		NewPlayer(client, &xhamsterPlayer),
		NewPlayer(client, &tnaflixPlayer),
		NewPlayer(client, &drtuberPlayer),
		NewPlayer(client, &pornonePlayer),
		NewEporner(client),
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
		NewOKru(client),
		NewPlayer(client, &streamtapePlayer),
		NewDoodStream(client),
		NewPlayer(client, &mixdropPlayer),
		NewPlayer(client, &luluPlayer),
		NewPlayer(client, &vidsPlayer),
		NewPlayer(client, &sendvidPlayer),
		NewKnuthing(client),
		NewNoodleMagazine(client),
	}
	// Platform families: one extractor covering every install of a piece of
	// software, named and matched per install where a list is worth having
	// and sniffed by shape where it is not. Registered after the
	// hand-written hosts, since a named host is always the better answer.
	extractors = append(extractors, NewMediaWiki(client))
	extractors = append(extractors, NewHandoffs(client)...)
	extractors = append(extractors, NewPeerTube(client))
	extractors = append(extractors, NewChevereto(client))
	extractors = append(extractors, NewBandzoogle(client))
	extractors = append(extractors, NewFoolFuuka(client))
	extractors = append(extractors, NewKVS(client))
	// The fediverse is matched by path alone, on any host, and its /c/<name>
	// is also Rumble's and PeerTube's channel shape. So it comes after every
	// family that claims hosts by name, or it takes their URLs from them.
	extractors = append(extractors, NewFediverse(client))

	// Last of all, the generic capabilities: a bare HLS manifest is a
	// manifest whatever host serves it, and these must never take a URL a
	// named host would have claimed.
	extractors = append(extractors, NewHLSDirect(client))

	direct := NewDirect(client)
	reg := &Registry{
		client:   client,
		fallback: direct,
		limits:   Limits{Sources: cfg.MaxSources, Files: cfg.MaxFiles},
	}
	direct.registry = reg
	// The link harvester resolves what it finds through the registry it is
	// part of, so it is handed that registry rather than building one. It
	// claims no host: only an explicit "links:" prefix reaches it.
	//
	// It leads the list because every other Match looks at the host alone.
	// A harvest of a page that happens to live on a supported host would
	// otherwise be routed to that host's extractor, which would try to
	// fetch "links://..." and fail on the scheme.
	reg.extractors = append([]Extractor{NewLinks(client, reg)}, extractors...)
	// The aggregator hosts nothing either, and resolves the frame it finds
	// through the registry for the same reason, so it is wired the same way
	// once the registry exists. It claims one host, so its position among
	// the rest does not matter.
	reg.extractors = append(reg.extractors, NewAlohaTube(client, reg))
	// The album index hosts nothing either, and resolves each result the
	// same way, so it is wired once the registry exists for the same reason.
	reg.extractors = append(reg.extractors, NewBalbums(client, reg))
	reg.extractors = append(reg.extractors, NewVoyeurKing(client, reg))
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
	fallback, optedIn := ex.(ProxyFallback)
	if err != nil && httpx.RouteID(ctx) == "" && (challenged || (optedIn && fallback.ProxyFallback())) {
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
