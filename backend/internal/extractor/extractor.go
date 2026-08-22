// Package extractor turns a page URL into the concrete list of files behind
// it. Each supported host gets one Extractor; unknown hosts fall through to
// a direct-link extractor.
package extractor

import (
	"context"
	"errors"
	"fmt"
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
	// Headers replace the File's headers when non-nil.
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
		NewPixhost(client),
		NewFapello(client),
		NewCoomerFans(client),
		NewOKru(client),
		NewStreamtape(client),
		NewDoodStream(client),
		NewMixDrop(client),
	}
	// One extractor per Kernel Video Sharing install, so each is matched
	// and named like any other host. The fallback sniffs for the same
	// player on hosts no list names.
	extractors = append(extractors, NewKVSSites(cfg, client)...)

	return &Registry{extractors: extractors, fallback: NewDirect(client)}
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
	for _, e := range r.extractors {
		if e.Match(u) {
			return e
		}
	}
	return r.fallback
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
