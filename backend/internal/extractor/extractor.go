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
	"strings"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
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
	// over, so they arrive this way instead of as a URL.
	Segments []string
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
}

// Result is everything an extractor found behind one input URL.
type Result struct {
	// Title names the job and the destination folder.
	Title string
	// Files are the resources to download, in display order.
	Files []File
}

// Extractor resolves one family of URLs.
type Extractor interface {
	// Name is the short host label shown in the UI.
	Name() string
	// Match reports whether this extractor handles the URL.
	Match(u *url.URL) bool
	// Extract resolves the URL into downloadable files. opts may be zero.
	Extract(ctx context.Context, u *url.URL, opts Options) (*Result, error)
}

// Options carries per-request extras supplied by the caller.
type Options struct {
	// Password unlocks protected folders (currently gofile only).
	Password string
}

// ErrPasswordRequired signals that the content is gated behind a password.
var ErrPasswordRequired = errors.New("this link is password protected: supply the password and retry")

// Registry dispatches a URL to the first extractor that matches.
type Registry struct {
	extractors []Extractor
	fallback   Extractor
}

// NewRegistry wires up every supported host.
func NewRegistry(cfg *config.Config, client *httpx.Client) *Registry {
	extractors := []Extractor{
		NewGofile(cfg, client),
		NewMega(client),
		NewBunkr(client),
		NewErome(client),
		NewPixeldrain(client),
		NewTurbo(client),
		NewDropbox(client),
		NewMediafire(client),
		NewGoogleDrive(client),
		NewSVTPlay(client),
		// Public broadcasters, all the same job as SVT Play: an open
		// metadata endpoint, a DRM check, and either a plain rangeable file
		// or a self-contained HLS rendition. The ones whose streams carry
		// audio apart are on the yt-dlp handoff list instead — see
		// handoffs.go, where the reason is stated once for all of them.
		NewRUV(client),
		NewARD(client),
		NewZDF(client),
		NewSRF(client),
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
		NewVimeo(),
		NewYandex(client),
		NewBooru(client),
		NewFourChan(client),
		NewRedGifs(client),
		NewKemono(client),
		NewPornHub(client),
		NewXHamster(client),
		NewTNAFlix(client),
		NewPornOne(client),
		NewEporner(client),
		NewPixhost(client),
		NewImagePond(client),
		NewSuvobox(client),
		NewCyberdrop(client),
		NewImgur(client),
		NewCivitai(client),
		NewBitChute(client),
		NewStreamable(client),
		NewWeTransfer(client),
		NewVidmoly(client),
		NewNRK(client),
		NewPornPics(client),
		NewArchiveOrg(cfg, client),
		NewFeeds(client),
		NewAutoindex(client),
		NewFapello(client),
		NewCoomerFans(client),
		NewOKru(client),
		NewStreamtape(client),
		NewDoodStream(client),
		NewMixDrop(client),
	}
	// Platform families: one extractor covering every install of a piece of
	// software, named and matched per install where a list is worth having
	// and sniffed by shape where it is not. Registered after the
	// hand-written hosts, since a named host is always the better answer.
	extractors = append(extractors,
		NewFediverse(client),
		NewMediaWiki(cfg, client),
	)
	extractors = append(extractors, NewHandoffs(client)...)
	extractors = append(extractors, NewPeerTubeSites(cfg, client)...)
	extractors = append(extractors, NewCheveretoSites(cfg, client)...)
	extractors = append(extractors, NewBandzoogleSites(cfg, client)...)
	extractors = append(extractors, NewFoolFuukaSites(cfg, client)...)
	extractors = append(extractors, NewKVSSites(cfg, client)...)

	// Last of all, the generic capabilities: a bare HLS manifest is a
	// manifest whatever host serves it, and these must never take a URL a
	// named host would have claimed.
	extractors = append(extractors, NewHLSDirect(client))

	reg := &Registry{fallback: NewDirect(client)}
	// The link harvester resolves what it finds through the registry it is
	// part of, so it is handed that registry rather than building one. It
	// claims no host: only an explicit "links:" prefix reaches it.
	//
	// It leads the list because every other Match looks at the host alone.
	// A harvest of a page that happens to live on a supported host would
	// otherwise be routed to that host's extractor, which would try to
	// fetch "links://..." and fail on the scheme.
	reg.extractors = append([]Extractor{NewLinks(client, reg)}, extractors...)
	return reg
}

// Hosts lists the supported host labels, for the startup log.
func (r *Registry) Hosts() []string {
	names := make([]string, 0, len(r.extractors))
	for _, e := range r.extractors {
		names = append(names, e.Name())
	}
	return names
}

// Find returns the extractor for a URL. It never returns nil: unrecognised
// hosts get the direct-link extractor.
func (r *Registry) Find(u *url.URL) Extractor {
	ex, _ := r.Known(u)
	return ex
}

// Known returns the extractor for a URL and whether a host actually claimed
// it, as opposed to the fallback matching it the way it matches everything.
//
// Find cannot answer that, and the link harvester needs it answered: it reads
// a page full of links of which a handful are downloads and the rest are
// navigation, and "is this a host we support" is the only thing separating
// the two.
func (r *Registry) Known(u *url.URL) (Extractor, bool) {
	for _, e := range r.extractors {
		if e.Match(u) {
			return e, true
		}
	}
	return r.fallback, false
}

// Extract parses rawURL and resolves it.
func (r *Registry) Extract(ctx context.Context, rawURL string, opts Options) (*Result, Extractor, error) {
	u, err := ParseURL(rawURL)
	if err != nil {
		return nil, nil, err
	}
	ex := r.Find(u)
	res, err := ex.Extract(ctx, u, opts)
	if err != nil {
		return nil, ex, err
	}
	if res == nil || len(res.Files) == 0 {
		return nil, ex, fmt.Errorf("%s: no downloadable files found at %s", ex.Name(), u.Redacted())
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
