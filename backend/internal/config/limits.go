package config

import "time"

// Tunables shared across packages. They live here rather than next to their
// single use so the whole system's behaviour can be read, and adjusted, in
// one place.
const (
	// MaxConcurrency caps the worker pool. Past this the bottleneck is the
	// remote host, and hammering it invites rate limiting.
	MaxConcurrency = 32

	// DefaultConcurrency is the starting number of parallel transfers.
	DefaultConcurrency = 4

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

	// CopyBufferSize is the transfer chunk size: large enough that the
	// progress counter is not bumped per packet, small enough to stay
	// responsive to cancellation.
	CopyBufferSize = 256 << 10

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

	// RequestRetryBase and RequestRetryMax bound the backoff between
	// retries of a single HTTP request.
	RequestRetryBase = 400 * time.Millisecond
	RequestRetryMax  = 15 * time.Second

	// TransferRetryBase and TransferRetryMax bound the backoff between
	// retries of a whole file transfer, which are costlier than a request.
	TransferRetryBase = 1 * time.Second
	TransferRetryMax  = 20 * time.Second

	// BusyRetryBase and BusyRetryMax bound the wait between attempts at a
	// host that is merely busy. Such a host is not failing, it is asking us
	// to come back later, so these attempts are unlimited in number and
	// only the interval is capped.
	BusyRetryBase = 2 * time.Second
	BusyRetryMax  = 60 * time.Second

	// MaxRetryAfter caps how long a server's Retry-After can park a worker.
	MaxRetryAfter = 2 * time.Minute
)

// Server tuning.
const (
	// MaxRequestBytes bounds an inbound API request body.
	MaxRequestBytes = 1 << 20

	// SSEHeartbeat keeps idle event streams alive through proxies that
	// would otherwise time them out.
	SSEHeartbeat = 25 * time.Second

	// ReadHeaderTimeout bounds how long a client may take to send headers.
	ReadHeaderTimeout = 10 * time.Second

	// IdleTimeout bounds a kept-alive connection between requests.
	IdleTimeout = 120 * time.Second

	// ShutdownTimeout bounds graceful shutdown.
	ShutdownTimeout = 10 * time.Second
)

// Extraction tuning.
const (
	// MaxFolderDepth bounds recursion into nested remote folders.
	MaxFolderDepth = 8

	// FolderPageSize is how many children to request per API page.
	FolderPageSize = 100

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
