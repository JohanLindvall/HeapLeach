// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"net/http"
	"strconv"
	"strings"

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

// knownSize is the first stated length that is a length at all, as
// File.Size wants it: -1 when none is positive. A host that states no length
// is not stating a zero-length file, and 0 would read as a finished download.
func knownSize(stated ...int64) int64 {
	for _, n := range stated {
		if n > 0 {
			return n
		}
	}
	return -1
}

// parsedSize reads a byte count written out in decimal, as knownSize: -1 when
// it is absent, malformed or not positive.
func parsedSize(text string) int64 {
	n, err := strconv.ParseInt(strings.TrimSpace(text), 10, 64)
	if err != nil {
		return -1
	}
	return knownSize(n)
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

// target is the Target a file's media describes, for a caller holding a File
// that must answer as a resolver: a source read again, or an attachment an
// eager listing already described. It carries the address, headers, name
// and every media field the two share — segments, key, audio and subtitles —
// so a playlist read again replaces all of them downstream rather than
// keeping stale tracks or dropping fresh ones. A size the listing only
// approximated becomes unknown, and a playlist without a URL answers with
// its first segment, which the downloader requires of a resolved target.
//
// The control fields stay with their owners: Resolve, Refresh, Check,
// Cipher, Pace, Reject and the directory belong to the File, and Discard to
// a resolver whose links are bound to an address. Callers that must invoke
// an existing Resolve or refuse an External handoff do so before this.
func (f *File) target() *Target {
	link, size := f.URL, f.Size
	if link == "" && len(f.Segments) > 0 {
		link = f.Segments[0]
	}
	if f.SizeApprox {
		size = -1
	}
	return &Target{
		URL: link, Name: f.Name, Size: size, Headers: f.Headers,
		Segments: f.Segments, SegmentKey: f.SegmentKey, Audio: f.Audio, Subtitles: f.Subtitles,
	}
}

// refreshing is a resolved target as its file, read again by resolve when
// its transfer starts, for a link signed for less time than a queue can
// take. The read again is always fresh (freshRead): a page probed when the
// job was listed is where the expired link came from.
func refreshing(target *Target, resolve func(context.Context) (*Target, error)) File {
	file := target.file("")
	file.Resolve = func(ctx context.Context) (*Target, error) {
		return resolve(httpx.WithoutAdmission(freshRead(ctx)))
	}
	return file
}

// signedResult is the one-file result of a host that signs its link on
// request: signed now, so that a dead link fails the job at once and the
// queue shows the real name, and signed again by the same call when the
// transfer starts. The job is named after the file.
func signedResult(ctx context.Context, sign func(context.Context) (*Target, error)) (*Result, error) {
	target, err := sign(ctx)
	if err != nil {
		return nil, err
	}
	return &Result{Title: target.Name, Files: []File{refreshing(target, sign)}}, nil
}

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
