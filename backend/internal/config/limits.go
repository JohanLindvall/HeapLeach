// SPDX-License-Identifier: MIT

package config

import "time"

// Application proxy configuration and scheduling. Pool health and discovery
// defaults live in github.com/JohanLindvall/proxytron/lease.
const (
	DefaultProxyFeed    = "https://raw.githubusercontent.com/proxifly/free-proxy-list/refs/heads/main/proxies/all/data.txt"
	ProxyDispatchTick   = time.Second
	ProxyMaxPageSize    = 200
	DefaultProxyRetries = 20

	// RouteWantedRecheck is how long the answer to whether a service has a
	// file due to start is kept before the queue is scanned again.
	RouteWantedRecheck = time.Second
	// RouteExtraAcquisitions bounds how many extra streams the whole queue
	// sets up at once. It is not a bound on how many run, which is files at
	// once times streams per file. Setting one up is a hunt through mostly
	// dead routes, then a CAPTCHA and the host's ticket timer, and the
	// CAPTCHA reader takes a few at a time (CaptchaConcurrency), which the
	// files' own routes need first. Unbounded, 256 running files would all set one up together the
	// moment they were let, as after every restart.
	RouteExtraAcquisitions = 64
	// RouteExtraBackoffMax caps the wait between a transfer's attempts at
	// an extra stream after attempts that got nowhere, each of which cost
	// the host a CAPTCHA.
	RouteExtraBackoffMax = 10 * time.Minute

	// ProxyDialTimeout bounds the connection to a proxy itself. Most routes
	// in a public feed are dead, and most dead ones never answer at all, so
	// this is what one costs a file to find out. A live proxy answers in
	// well under a second; at the 15 seconds every other dial is allowed,
	// files spent nearly all their time waiting on routes that never would.
	ProxyDialTimeout = 5 * time.Second

	// RouteSummaryInterval is how often the outcome of routed attempts is
	// logged, per service.
	RouteSummaryInterval = time.Minute

	// ProxyExitProbeURL answers a request with the public address it came
	// from, as plain text. A per-route host counts free downloads by the
	// address it sees, and a proxy's exit need not be the address it listens
	// on, so each route is asked it before the host sees the route. The
	// answer is also how a route shows it works. ProxyExitProbeFallbackURL is
	// asked when the first gives no address.
	ProxyExitProbeURL         = "https://api.ipify.org"
	ProxyExitProbeFallbackURL = "https://checkip.amazonaws.com"
	// ProxyEchoRecheck is how long the echo services' own health, asked
	// over the normal connection before blaming a route, is gone by.
	ProxyEchoRecheck = time.Minute
	// ProxyExitProbeTimeout bounds that question.
	ProxyExitProbeTimeout = 10 * time.Second
	// ProxyExitTTL is how long a route's exit, once found, is gone by.
	ProxyExitTTL = time.Hour

	// ProxyAddressRefusedWait is how long a route, and its exit, is held back
	// after the host refuses free downloads from its address outright. With
	// the minute-long refusal backoff, a host refusing nearly every address
	// was asked again by each of them every few minutes, each time after a
	// CAPTCHA, about a thousand times a minute in all.
	ProxyAddressRefusedWait = time.Hour

	// ProxyHealthEpoch numbers the rules routes are judged by. A database
	// written under another number has every route's health forgotten when
	// it is opened, keeping the inventory. Raise it when a change means the
	// verdicts already recorded were reached wrongly, so that no route stays
	// shelved, cooling or favoured on their account. Epoch 1 forgot what was
	// judged while downloads could share an exit address, and while every
	// restart started the inventory over. Epoch 2 forgot a day of refusals
	// that were the files' and not the addresses': files the site offers to
	// premium users alone, which every address is refused, set aside each
	// route that was tried on one for an hour.
	ProxyHealthEpoch = 2
)

// Tunables shared across packages. They live here rather than next to their
// single use so the whole system's behaviour can be read, and adjusted, in
// one place.
const (
	// Service logs are kept only for the current run. Bound both ordinary
	// bursts and unusually large diagnostic records before exposing them.
	ServiceLogEntries    = 1000
	ServiceLogBytes      = 4 << 20
	ServiceLogEntryBytes = 16 << 10

	// DefaultMaxConcurrency is the default ceiling on parallel transfers, the
	// most the concurrency setting may be raised to at startup or live. It is
	// a setting (HEAPLEACH_MAX_CONCURRENCY, -max-concurrency): with proxy
	// routes each address carries its own Keep2Share transfer, so a pool of
	// addresses can use far more than one host could stand.
	DefaultMaxConcurrency = 256

	// DefaultMinFreeDisk is how much room must be left at the destination
	// before another transfer starts. A bulk downloader fills a disk faster
	// than anyone watches it, and the failure when it does is not a clean
	// one: the transfer in flight dies partway with "no space left on
	// device", and so does every one behind it.
	//
	// Ten gibibytes rather than a percentage: what makes the difference is
	// having room for the next file and for whatever else on the machine
	// needs to write, and neither scales with how large the disk is.
	DefaultMinFreeDisk = 10 << 30

	// DefaultConcurrency is the starting number of parallel transfers.
	DefaultConcurrency = 4

	// ResolveConcurrency bounds source listings across jobs. Per-listing
	// fan-out alone would let a large paste launch thousands of requests.
	ResolveConcurrency = 4

	// MaxExtractionDepth bounds chains of index sites resolving through
	// one another, including loops whose URLs change on every hop.
	MaxExtractionDepth = 16

	// DefaultMaxRetries is how many times a failed request or transfer is
	// repeated before it is reported as failed.
	DefaultMaxRetries = 3

	// DefaultTimeout bounds a single non-streaming request. Body transfers
	// clear it and rely on context cancellation instead.
	DefaultTimeout = 60 * time.Second
)

// Transfer tuning.
const (
	// ProgressTick is how often progress is sampled and pushed to browsers.
	// Fast enough to feel live, slow enough to stay cheap with many items.
	ProgressTick = 400 * time.Millisecond

	// FrameInterval is how often a snapshot is pushed to browsers while
	// something is actually moving.
	//
	// Slower than ProgressTick, which is a sampling rate: rates are
	// measured often so they are smooth, and sent once a second because
	// that is as often as a number on a screen is worth redrawing. The
	// difference is most of what a browser was being asked to receive.
	FrameInterval = time.Second

	// NudgeCoalesce is how long the broadcaster waits after a user action
	// for others arriving with it, so that pasting a list of links — an add
	// per link — is one frame rather than one per link. Short enough to read
	// as immediate.
	NudgeCoalesce = 50 * time.Millisecond

	// IdleFrameInterval is how often a snapshot is pushed to browsers while
	// transfers are running but no byte counter is moving.
	//
	// A queue held behind a host taking one download at a time spends most
	// of its life in that state, and a full snapshot every ProgressTick to
	// report that nothing happened is most of what such a queue costs.
	// Slower than the tick but not silent: the rates decay towards zero
	// while nothing moves, and a viewer should see that rather than a
	// frozen number.
	IdleFrameInterval = 2 * time.Second

	// StateSaveInterval is how often the queue is written out, and it is
	// written on shutdown besides (Manager.Close). Slow on purpose, and
	// slower than it once was: the file carries a line for every file of
	// every job, so a queue of twenty thousand files was five megabytes
	// rewritten every ten seconds whenever any one of them changed status —
	// tens of gigabytes a day onto the disk for a record nobody reads until
	// the next start.
	//
	// What a crash costs is bounded and small. The file records what each
	// item is and how it ended, never how many bytes it has moved; the part
	// files on disk are what make a transfer resumable, and they are not
	// written by this at all. A job added and not yet saved is the one thing
	// a crash can lose outright, and a clean stop — a deploy, a restart —
	// saves on the way out.
	StateSaveInterval = 10 * time.Minute

	// MaxStateBytes bounds what a queue file may decompress to: far above
	// any real queue (a quarter of a million files is under a hundred
	// megabytes of JSON), and low enough that a corrupt file fails cheaply.
	MaxStateBytes = 1 << 30

	// DiskSampleInterval is how often the destination's free space is
	// measured. Far slower than the progress tick on purpose: it is a
	// syscall against a filesystem that may well be a network mount, and
	// free space does not move fast enough to be worth asking oftener.
	DiskSampleInterval = 5 * time.Second

	// CopyBufferSize is the transfer chunk size: large enough that the
	// progress counter is not bumped per packet, small enough to stay
	// responsive to cancellation.
	CopyBufferSize = 256 << 10

	// WriteBufferSize is how much a connection accumulates before it
	// touches the file.
	//
	// A read returns whatever the socket has, which is tens of kilobytes on
	// an ordinary connection, and writing each one straight through meant a
	// write syscall per read: several hundred a second across a busy queue,
	// every one of them an inotify event for whatever is watching the
	// download directory. A media server, a sync client or a backup tool
	// pointed at that directory sees a storm and rescans through it. The
	// bytes are the same either way; only the number of writes changes.
	//
	// Three things bound this. Memory, first: a buffer is held per
	// connection, so the worst case is the file concurrency times the
	// stream ceiling times this — at the defaults 32 MiB, and rather more
	// on a queue tuned wider. Second, an attempt that dies loses whatever
	// its connections were still holding, which is bytes re-fetched rather
	// than bytes lost, since nothing is recorded as on disk until it is
	// there. Third, the retry budget judges an attempt by what it left on
	// disk, so a buffer much larger than what a flaky host manages between
	// drops would make productive attempts look unproductive.
	//
	// It deliberately matches MinSegmentSize: a range worth splitting off
	// is exactly one buffer's worth of work.
	//
	// A connection starts on WriteBufferStart and grows to this only once
	// it has moved WriteBufferSize at an average of WriteBufferGrowRate a
	// second or more. Most of a queue of free downloads is capped at a few
	// tens of kilobytes a second, where a full buffer takes the better part
	// of half a minute to fill. That held a mebibyte apiece for nothing, a
	// quarter of a gibibyte across 256 transfers. And it bit the third bound
	// above: a connection dropped before its buffer filled had written
	// nothing, so its attempt looked unproductive. It is the average over a
	// mebibyte that is judged, because those downloads arrive in bursts: a
	// single read fills the starting buffer at once on a connection averaging
	// 42 kB/s, and judged by how fast one buffer filled, most of them grew.
	WriteBufferSize     = 1 << 20
	WriteBufferStart    = 64 << 10
	WriteBufferGrowRate = 64 << 10

	// SpeedSmoothing weights the previous rate against the latest sample.
	// Higher values give steadier, slower-reacting numbers.
	SpeedSmoothing = 0.6

	// DefaultStreams is the ceiling on connections opened for one file.
	DefaultStreams = 8

	// MaxStreams bounds what the setting may be raised to.
	MaxStreams = 16

	// DefaultSlowSpeed is the throughput, in bytes per second, below which a
	// transfer is considered slow enough to be worth another connection.
	DefaultSlowSpeed = 2_000_000

	// MinSegmentSize is the smallest useful piece of work. A segment is only
	// split when both halves would be at least this large, which also keeps
	// the split point comfortably ahead of the writer: a reader advances by
	// at most CopyBufferSize between the split decision and its next write,
	// and that is far smaller than this.
	MinSegmentSize = 1 << 20

	// MinSplitFileSize is the smallest file worth splitting at all.
	MinSplitFileSize = 8 << 20

	// RouteRangeSize is the most one request of a multi-route transfer asks
	// for. Its connections read every response to the end and ask for the
	// next range on the same connection, since the link allows one, so a
	// split can take only what has not been asked for. The last range of a
	// file is read by one route alone: at a capped free link's 42 kB/s,
	// 8 MiB is about three minutes.
	RouteRangeSize = 8 << 20

	// SegmentPollInterval is how often the supervisor checks whether the
	// transfer has finished. It is short so completion is not delayed by
	// the much longer throughput-probe interval.
	SegmentPollInterval = 250 * time.Millisecond

	// StreamProbeInterval is how often throughput is re-measured to decide
	// whether another connection would help.
	StreamProbeInterval = 2 * time.Second

	// SpeedWindow is how many probe samples are averaged before the reading
	// is trusted. A single interval swings far too much to act on.
	SpeedWindow = 3

	// MaxConnectionsPerHost caps the *extra* connections opened to any one
	// host across every download. Primary connections are never counted, so
	// this can never starve a transfer — it only limits going faster.
	MaxConnectionsPerHost = 8

	// StreamAddCooldown is the settling time after opening a connection,
	// so eight are not opened at once on a brief dip.
	StreamAddCooldown = 3 * time.Second

	// ThroughputRegressionRatio is how much of its previous rate a transfer
	// must keep after a connection is added for that connection to count as
	// harmless. Below it, the host is being made slower by parallelism and
	// no more connections are opened.
	//
	// Well under 1 on purpose: throughput is noisy, and the only case worth
	// acting on is the unmistakable one. Archive.org drops by two orders of
	// magnitude, so a generous threshold still catches it while leaving an
	// ordinary fluctuation alone.
	ThroughputRegressionRatio = 0.7

	// SegmentStateInterval is how often segment progress is written to the
	// sidecar file that makes a part-finished transfer resumable.
	SegmentStateInterval = 5 * time.Second

	// StallTimeout aborts a transfer whose byte counter has not moved for
	// this long. Without it a server that accepts the connection and then
	// sends nothing would pin a worker forever, since body reads have no
	// deadline of their own. The aborted attempt is retried and resumes
	// from what is already on disk.
	//
	// Generous on purpose: some hosts pace streaming media at a few hundred
	// kB/s, and a slow transfer is not a stalled one.
	StallTimeout = 90 * time.Second
)

// HTTP client tuning.
const (
	// MaxRedirects bounds a redirect chain.
	MaxRedirects = 10

	// MaxResponseBytes caps a buffered JSON or HTML body held in memory.
	MaxResponseBytes = 8 << 20

	// PlatformProbeTimeout bounds a guessed API endpoint on an unknown host.
	PlatformProbeTimeout = 5 * time.Second

	// PageCacheTTL is how long a probed page is reused. Long enough for one
	// submission's identification, extraction and the sources it expands
	// to share a fetch; short enough that a page re-read later is fetched
	// again, since a re-read is usually asking because the page changed.
	PageCacheTTL = time.Minute
	// PageCacheBytes bounds the bodies of the pages kept, oldest dropped
	// first. Parsed documents ride along with their pages, at several times
	// a body's size, which is why this is a fraction of what one job's
	// listing might otherwise hold.
	PageCacheBytes = 32 << 20

	// MaxSegmentBytes caps one playlist part, which is assembled in memory
	// before being appended in order. Set far above any real part — those
	// are seconds of video — so it never truncates a genuine one; it is a
	// bound against a server that misreports a length or never stops
	// sending, which would otherwise be unbounded allocation. Reaching it
	// is an error rather than a short read: silently joining a truncated
	// part would produce a corrupt file that looks finished.
	MaxSegmentBytes = 256 << 20

	// RequestRetryBase and RequestRetryMax bound the backoff between
	// retries of a single HTTP request.
	RequestRetryBase = 400 * time.Millisecond
	RequestRetryMax  = 15 * time.Second

	// TransferRetryBase and TransferRetryMax bound the backoff between
	// retries of a whole file transfer, which are costlier than a request.
	TransferRetryBase = 1 * time.Second
	TransferRetryMax  = 20 * time.Second

	// ProgressRetryDelay is how long a transfer waits before resuming after
	// a connection dropped on an attempt that was moving bytes. Such an
	// attempt is not counted against the retry budget — see Manager.transfer
	// — and the wait is flat rather than backed off, because backoff exists
	// to stop hammering a host that is failing, and a host that served part
	// of the file a moment ago is not failing. Long enough for a CDN node to
	// come back or a half-open connection to time out of a table somewhere;
	// short enough that a worker is not idled for nothing.
	ProgressRetryDelay = 30 * time.Second

	// BusyRetryBase and BusyRetryMax bound the wait between attempts at a
	// host that is merely busy. Such a host is not failing, it is asking us
	// to come back later, so for items that can re-resolve — rotating to a
	// mirror, or minting a fresh signed link — these attempts are unlimited
	// in number and only the interval is capped.
	BusyRetryBase = 2 * time.Second
	BusyRetryMax  = 60 * time.Second

	// BusyRetryLimit caps those attempts for an item with no resolver,
	// where every retry asks the same URL the same way. A host that keeps
	// answering such a request with a web page is not busy — the URL is a
	// web page, usually one no extractor recognised — and retrying forever
	// would sit in the queue as "host busy" without end.
	BusyRetryLimit = 5

	// OverloadRetries is how many times a transfer waits out a host that
	// answered 503.
	//
	// Counted apart from the ordinary retry budget, and generous for the
	// same reason the connection-limit count is: what clears the condition
	// is partly our own doing. A storage backend that falls over under load
	// recovers once less is being asked of it, and the cap that comes down
	// alongside these waits is what stops asking. It is still capped,
	// because a host can also be down in earnest, and a queue that waited
	// on that forever would never finish.
	OverloadRetries = 10

	// ConnectionLimitRetries is how many times a transfer waits out a host
	// that refused it for having too many connections open at once.
	//
	// It is counted apart from the ordinary retry budget and is generous,
	// because of what clears the condition: our own other transfers
	// finishing. Waiting through a sibling download is the whole point, and
	// that takes as long as that download takes. On the busy-host schedule
	// this is around six minutes of patience.
	//
	// It is capped rather than unlimited because the same refusal arrives
	// when the connections belong to somebody else on this address — another
	// program, another browser tab — and then nothing here can make it clear
	// and a headless run would never terminate.
	ConnectionLimitRetries = 10

	// RateLimitRetryBase and RateLimitRetryMax bound the wait between
	// attempts at a host that answered 429.
	//
	// A rate limit is the same thing one layer down as a busy host is to the
	// downloader: not a failure but a request to come back, and answered the
	// same way rather than with the backoff that exists for things going
	// wrong. That one starts at 400ms, which is right for a connection that
	// dropped and useless against a limiter — three attempts spend under
	// three seconds and ask again well inside whatever window the host is
	// counting, so the whole budget is gone before the limit has begun to
	// lift. These are the busy-host intervals, for the same reason.
	RateLimitRetryBase = 2 * time.Second
	RateLimitRetryMax  = 60 * time.Second

	// RateLimitRetries is how many times a 429 is waited out. It is counted
	// apart from the ordinary retry budget, which a rate limit must not
	// spend: a request that waits out a limiter and then meets a dropped
	// connection should still have its own retries left. Four doublings
	// from the base is around half a minute of patience, or whatever longer
	// interval the host asked for in Retry-After.
	RateLimitRetries = 4

	// MaxRetryAfter caps how long a server's Retry-After can park a worker.
	MaxRetryAfter = 2 * time.Minute

	// ErrorBodySample is how much of a failed response's body is kept for the
	// error message, which is usually where these hosts put the real reason.
	ErrorBodySample = 4 << 10

	// PlaylistStateEvery is how often a playlist transfer checkpoints its
	// progress, in segments joined.
	PlaylistStateEvery = 20

	// SubtitleFetchTimeout bounds fetching a playlist's subtitles, all
	// tracks together, and SubtitleFetchConcurrency how many of a track's
	// WebVTT files are asked for at once. Subtitles are a bonus to the
	// video: a host that will not serve them in this time is left without
	// them rather than holding the video up.
	SubtitleFetchTimeout     = 2 * time.Minute
	SubtitleFetchConcurrency = 4

	// RemuxTimeout bounds the lossless rewrap of a finished transport stream.
	// Copying streams is fast even for a long programme, so a stuck ffmpeg
	// should not hold a worker indefinitely.
	RemuxTimeout = 30 * time.Minute

	// Metadata probes must finish even if a helper stops responding.
	ProbeTimeout    = 2 * time.Minute
	HelperWaitDelay = 5 * time.Second
)

// Server tuning.
const (
	// MaxRequestBytes bounds an inbound API request body.
	MaxRequestBytes = 1 << 20

	// SSEHeartbeat keeps idle event streams alive through proxies that
	// would otherwise time them out.
	SSEHeartbeat = 25 * time.Second

	// SSEWriteTimeout releases a stream whose browser stopped reading.
	SSEWriteTimeout = 10 * time.Second

	// ReadHeaderTimeout bounds how long a client may take to send headers.
	ReadHeaderTimeout = 10 * time.Second

	// RequestTimeout bounds API request bodies and ordinary replies. The
	// event stream replaces the write deadline with a per-frame deadline.
	RequestTimeout = 30 * time.Second

	// IdleTimeout bounds a kept-alive connection between requests.
	IdleTimeout = 120 * time.Second

	// ShutdownTimeout bounds graceful shutdown.
	ShutdownTimeout = 10 * time.Second

	// IdleExitAfter is how long a bare run keeps going once it has nothing
	// to download and nothing is watching it. Long enough to survive a
	// reload or a browser restart, short enough that closing the tab is a
	// way of quitting.
	IdleExitAfter = time.Minute

	// IdleCheckInterval is how often that is tested. Coarse on purpose:
	// the deadline is a minute, so a few seconds either way is noise, and
	// this runs for the whole life of the process.
	IdleCheckInterval = 5 * time.Second
)

// Extraction tuning.
const (
	// Keep2Share's free tier requires a locally read image CAPTCHA and a
	// wait. Bound both independently of transport retries; an unreadable
	// challenge must not turn into a stream of new challenges. A wrong
	// answer leaves the challenge open, so each image is tried with up to
	// Keep2ShareCaptchaGuesses of the reader's readings, most likely first.
	Keep2ShareCaptchaAttempts = 3
	Keep2ShareCaptchaGuesses  = 3
	Keep2ShareWaits           = 5
	Keep2ShareMaxWait         = 2 * time.Hour
	Keep2ShareExpiryMargin    = 5 * time.Second
	Keep2ShareOCRTimeout      = 20 * time.Second
	Keep2ShareCaptchaBytes    = 1 << 20
	Keep2ShareCaptchaPixels   = 128 << 10

	// The pinned CAPTCHA model and its constrained CTC decoder. The width
	// bound limits inference work even for an image with an extreme aspect ratio.
	CaptchaModelAlphabet = 8210
	CaptchaModelHeight   = 64
	CaptchaModelMaxWidth = 1024
	CaptchaLength        = 6
	CaptchaReadings      = 5
	CaptchaBeamWidth     = 32
	CaptchaProposals     = 8
	CaptchaBeamFloor     = -9.0
	CaptchaThreads       = 2

	// Keep2ShareRequestTimeout bounds the small API and CAPTCHA-image
	// requests. Waiting a minute on each silent public proxy pins workers
	// long before a file can start. OCR and the host's ticket timer have
	// their own budgets; media bodies use the transfer stall watchdog. Leave
	// room for the 15-second dial timeout to identify an unreachable proxy
	// before the request deadline would turn it into a generic timeout.
	Keep2ShareRequestTimeout = 20 * time.Second

	// DefaultCaptchaConcurrency is how many CAPTCHAs are read at once, each
	// in a model session of its own (54 MB of model, two runtime threads).
	// It is a setting (HEAPLEACH_CAPTCHA_CONCURRENCY, -captcha-concurrency),
	// held under MaxCaptchaConcurrency. Two kept up with one file at a time.
	// Hundreds running, each with extra streams on routes of their own, each
	// wanting a CAPTCHA, queued behind two.
	DefaultCaptchaConcurrency = 4
	MaxCaptchaConcurrency     = 64

	// Reuse the loaded model between challenges, releasing it after inactivity.
	Keep2ShareOCRIdle = 5 * time.Minute

	// Keep2ShareAddressLimitWait is how long an address whose free traffic or
	// download count is used up is left alone. The host does not say when
	// the limit lifts; this is long enough not to spend a CAPTCHA on such a
	// route every few minutes, short of the day such limits usually run to.
	Keep2ShareAddressLimitWait = 6 * time.Hour

	// Keep2ShareInPlaceWait is the longest ticket timer sat out in the
	// worker. The usual one is half a minute; past this it is the wait
	// between free downloads, which the file spends back in the queue.
	Keep2ShareInPlaceWait = time.Minute

	// Keep2ShareFreeCheck is how long the site's word that a file can be had
	// free is trusted before it is asked again. A file it offers to premium
	// users alone fails at once, so only a free one is ever asked twice.
	Keep2ShareFreeCheck = 10 * time.Minute

	// Emload offers one free download per day without stating a reset time.
	// Cool the limited route while other addresses may take the file.
	EmloadDailyWait = 24 * time.Hour
	// Keep silent download proxies from holding a worker for the general
	// request timeout. Set it on the client so route scoring sees timeouts.
	EmloadRequestTimeout = 20 * time.Second

	// MegaQuotaWait is how long an address whose mega transfer quota is used
	// up waits when mega does not say how long is left. It usually says, in
	// seconds; MEGA's own client assumes an hour when it does not.
	MegaQuotaWait = time.Hour
	// MegaRequestTimeout bounds a request for a mega download link, which
	// goes through a proxy route once the normal connection's quota is used
	// up. Set on the client so route scoring sees the timeouts.
	MegaRequestTimeout = 20 * time.Second

	// MaxFolderDepth bounds recursion into nested remote folders.
	MaxFolderDepth = 8

	// FolderPageSize is how many children to request per API page.
	FolderPageSize = 100

	// CumPageSize matches the creator site's own post-list request size.
	CumPageSize = 50

	// ThrottleMinWait keeps a rate-limited reader from spinning through
	// sub-millisecond sleeps when the limit is very low.
	ThrottleMinWait = 5 * time.Millisecond

	// ThrottleWindow is the period the rate limiter averages its own
	// throughput over before deciding whether it is the bottleneck.
	ThrottleWindow = time.Second

	// ThrottleWindowStale is how old that measurement may be and still
	// describe the present.
	ThrottleWindowStale = 5 * time.Second

	// ThrottleSaturation is the fraction of the ceiling that counts as
	// running into it. Below this the transfers are slow for their own
	// reasons and splitting them further may genuinely help.
	ThrottleSaturation = 0.85

	// ExtractRetries is how many times an extractor repeats a request that
	// answered without the data it should have carried. Some hosts serve
	// their ordinary page under load but with the media list empty, and
	// reporting that as "this offers nothing to download" would be both
	// wrong and unactionable.
	ExtractRetries = 3

	// PageFetchConcurrency bounds how many secondary pages one listing is
	// expanded with at once. A site that lists posts but keeps the media on
	// each post's own page needs a request per post, and doing those one
	// after another leaves a large profile resolving for minutes before a
	// byte moves. Deliberately far below the transfer concurrency: this is
	// a burst of small requests at a single host, not a download.
	PageFetchConcurrency = 6

	// MaxAlbumPages bounds how far a paginated album listing is followed.
	// Listings also stop early once a page adds nothing new, so this is a
	// backstop against a site that keeps answering with the last page.
	MaxAlbumPages = 200

	// MaxListingFiles bounds how many files one submitted URL may resolve
	// to. Several hosts will happily describe more than anyone meant to ask
	// for — a maxed category on a gallery site is thousands of albums, an
	// archive collection is unbounded, a directory tree can be walked
	// forever — and a job of a hundred thousand files is a mistake being
	// carried out efficiently rather than a download. An extractor that
	// reaches this should say what it dropped, the way a truncated listing
	// is worse than a refused one.
	//
	// It is the default, not a ceiling: HEAPLEACH_MAX_FILES (-max-files)
	// sets it per process, for somebody who did mean the whole index. See
	// Config.MaxFiles, and extractor.Options, which carries it to every
	// extractor that walks a listing.
	MaxListingFiles = 20000

	// MaxExpandedSources is how many of a page's links are followed, by the
	// harvester and by an index search. Each one is a full extraction,
	// several requests at some hosts, and a page carries more of them than
	// one might guess — a tube's own front page measured just under two
	// hundred, and a search over an index of six hundred thousand albums is
	// unbounded in principle. Like MaxListingFiles it is the default, and
	// HEAPLEACH_MAX_SOURCES (-max-sources) sets it.
	MaxExpandedSources = 500

	// MaxDirectoryDepth bounds recursion into a directory listing that
	// links to itself, directly or through a symlink. A visited set catches
	// the simple loop; this catches the rest.
	MaxDirectoryDepth = 12

	// MaxAutoindexDepth and MaxAutoindexDirs bound a walk of an open
	// directory listing. Depth is the same guard as MaxDirectoryDepth; the
	// directory count is the one that matters in practice, since a shallow
	// tree can still be very wide.
	// What a walk may return is the listing cap, whatever it is set to.
	MaxAutoindexDepth = MaxDirectoryDepth
	MaxAutoindexDirs  = 500

	// MaxResolvedEpisodes bounds how many episodes of a broadcaster's
	// series are resolved while it is listed. Each costs a request or
	// several to the broadcaster's player before a byte moves, and a daily
	// strand lists thousands. It is not the user's file cap, which the
	// registry applies to what resolved: a refused episode yields no file,
	// so asking only as many as files are wanted could come back empty.
	MaxResolvedEpisodes = 1000

	// MaxResolvedEpisodesNPO is MaxResolvedEpisodes for a broadcaster
	// whose edge stops answering at around two hundred requests and then
	// refuses everything for minutes, each episode costing one. It leaves
	// room to be asked again: an expansion that spent the whole budget
	// would leave the next link pasted failing. A longer series is offered
	// a season at a time, which is how the site divides it too.
	MaxResolvedEpisodesNPO = 120

	// MaxSeasonsNPO bounds how many season listings of that broadcaster one
	// series URL reads, each a request against the same budget. Seasons
	// arrive newest first and the episodes behind them are cut to
	// MaxResolvedEpisodesNPO anyway, so all reading further buys is spending
	// the requests the episodes need. The longest-running strand looked at
	// had twenty-three.
	MaxSeasonsNPO = 30

	// GeoProbesDR is how many of a listing's programmes shown in Denmark
	// alone are asked for before it is taken that this address cannot tell
	// whether they are refused here.
	GeoProbesDR = 3

	// MaxPagesRadioCanada is the most pages of a list, or seasons of a
	// programme, that broadcaster's own loader asks for.
	MaxPagesRadioCanada = 200

	// MaxResolvedReleases bounds how many releases of a band's discography
	// are read while it is listed, each a page of its own. A label's
	// catalogue can run to hundreds; past this the listing says what it
	// left out.
	MaxResolvedReleases = 1000

	// MaxTimelinePages bounds how many pages of a social timeline are
	// walked. Unlike an album, a timeline has no end.
	MaxTimelinePages = 40

	// ManifestSniffBytes is how much of a response is read to decide
	// whether it is an HLS manifest. A manifest announces itself on the
	// first line, so this only has to survive a byte-order mark and some
	// whitespace.
	ManifestSniffBytes = 1 << 10
)

// Terminal display tuning, for runs that download straight to disk.
const (
	// CLIFrameInterval is how often the animated display repaints. Fast
	// enough that the spinner reads as motion, slow enough to stay cheap.
	CLIFrameInterval = 100 * time.Millisecond

	// CLIPlainInterval is how often progress is printed when the output is
	// not a terminal, where every line is permanent.
	CLIPlainInterval = 5 * time.Second

	// CLIMaxRows bounds how many transfers the display lists at once; the
	// rest are summarised on one line.
	CLIMaxRows = 12

	// CLIBarWidth is the width of a progress bar in columns.
	CLIBarWidth = 22

	// The stats to the right of each bar are fixed-width columns, so the
	// bars stay in one line down the screen instead of shifting every time
	// an ETA gains a digit.
	CLISizeWidth   = 17
	CLISpeedWidth  = 10
	CLIStreamWidth = 3
	CLIETAWidth    = 6

	// CLIDefaultWidth is assumed when the terminal will not report a size,
	// which is the case for pipes and dumb terminals.
	CLIDefaultWidth = 100
)

// What a remaining time may be projected from. The browser mirrors both of
// these in frontend/src/format.ts, the way the byte formatters mirror each
// other: the same transfer must not be given a finish time in one display
// and none in the other.
const (
	// ETAMinRate is the slowest rate a remaining time may be divided out
	// of, in bytes per second.
	//
	// A rate here is a windowed average, so a transfer whose connection
	// drops does not report zero — it decays towards it, through 12.9, 7.7,
	// 4.6 and on down. Both displays truncate a rate to whole bytes, so
	// anything under one already renders as "0 B/s", and dividing by it
	// produced the finish time this exists to prevent: a stalled file
	// sitting at 87% and claiming eleven thousand days. Whatever else is
	// true, the two numbers on one line must not contradict each other.
	ETAMinRate = 1.0

	// ETAHorizon is the furthest ahead a remaining time is worth stating.
	//
	// The floor above is not enough on its own: a decaying rate passes
	// through several honest-looking bytes per second on its way down, and
	// four of them divide a large remainder into centuries just as well.
	// Past a week the number has stopped being a finish time and become a
	// statement about a rate that is nearly zero — which the note beside it
	// already makes, in words, and better. The cost is that a genuinely
	// enormous queue crawling along loses its projection too; that is the
	// right trade, since the projection was worth nothing to wait on in
	// either case.
	ETAHorizon = 7 * 24 * time.Hour
)
