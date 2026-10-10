// SPDX-License-Identifier: MIT

// Package download runs the worker pool: it turns submitted URLs into jobs,
// schedules their files across a configurable number of parallel transfers,
// and publishes live progress to subscribers.
package download

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/extractor"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
	"github.com/JohanLindvall/HeapLeach/internal/proxy"
	"github.com/JohanLindvall/HeapLeach/internal/tools"
	"github.com/JohanLindvall/HeapLeach/internal/util"
)

// Manager owns all jobs and the worker pool.
//
// Locking rule: every Job and Item field except the atomic downloaded and
// streams counters is guarded by mu. Workers take mu to publish transitions,
// never while blocked on I/O.
type Manager struct {
	cfg    *config.Config
	reg    *extractor.Registry
	client *httpx.Client
	log    *slog.Logger
	// settingsMu serializes preparation/publication with shutdown. The pool
	// is opened lazily and owned until Close, even when routing is disabled.
	settingsMu   sync.Mutex
	proxies      *proxy.Pool // guarded by mu
	proxyConfig  proxy.Configuration
	proxyEnabled bool
	proxyRunning int

	mu      sync.Mutex
	jobs    map[string]*Job
	order   []string
	queue   []*Item
	running int
	limit   int
	streams int
	started bool
	closing bool // guarded by mu; no goroutines may be added once set
	// hostActive counts transfers in flight per host, for the few hosts
	// that ask to be approached gently (extractor.Pace). Only paced items
	// consult it, but every dispatched item is counted, so a mixed queue
	// still sees the truth.
	hostActive map[string]int
	// hostGate is the global admission queue for a host that has asked to
	// be given less. It is keyed on the host that actually answers, which
	// is known only once a link has resolved, so it lives apart from the
	// dispatch-time accounting above and has a lock of its own — see
	// hostgate.go.
	hostGate *hostGate

	wake chan struct{}
	// urgent asks the broadcaster for a frame now. A user's own action —
	// adding, stopping, clearing — is the one change that must not wait for
	// the once-a-second beat: the row a person just clicked on sitting
	// unchanged for most of a second reads as the click not having taken.
	urgent chan struct{}
	ctx    context.Context
	stop   context.CancelFunc
	wg     sync.WaitGroup

	// timings are the segment supervisor's intervals, kept here so tests
	// can shorten them without waiting out the production values.
	timings downloadTimings

	// throttle holds every transfer back to the configured rate, and parks
	// them all when paused.
	throttle *throttle

	// parts records the .part files transfers are currently writing, so a
	// URL-derived name can never be claimed by two items at once.
	partsMu sync.Mutex
	parts   map[string]struct{}

	// hosts bounds the extra connections opened to any one remote, shared
	// across every job so eight files never become eighty connections.
	hosts *hostLimiter

	// dir is where finished files go. It lives here rather than being read
	// from cfg because the UI can move it while transfers are running, and
	// a field two goroutines write and read needs a lock of its own — a
	// small one, since a transfer reads it once at the start rather than
	// per byte.
	dirMu sync.RWMutex
	dir   string

	// Free space at that directory, and the size of the filesystem holding
	// it. Sampled by the broadcaster alone, which is why the timestamp is a
	// plain field; the figures are atomic because /api/state reads them from
	// whatever goroutine is serving the request.
	diskSampled time.Time
	diskDir     string // directory represented by diskSampled
	diskFree    atomic.Int64
	diskTotal   atomic.Int64

	subsMu sync.Mutex
	// subs records whether each stream needs a complete snapshot.
	subs      map[chan []byte]*subscriber
	closed    bool // set under subsMu by Close; a late Subscribe is answered closed
	closeOnce sync.Once

	// hostCount is how many extractors the registry holds, fixed at
	// construction. It rides in every snapshot, and asking the registry to
	// list its names on each progress tick allocated a slice to count and
	// drop.
	hostCount int
	// Source listings have their own bound: each may fan out into many
	// page requests, independently of the transfer pool.
	resolveSlots chan struct{}

	// minFree is how much room must be left at the destination before
	// another transfer is started; zero disables the check. Fixed at
	// construction, so it needs no lock.
	minFree int64

	// version is the build, for the snapshot to report.
	version string

	// Where the queue is written so a restart can pick it up again, and the
	// fingerprint of what was last written — an idle queue is not worth
	// rewriting every interval. The saver and Close both write it, Close
	// while the saver may still be mid-write, so persistMu serialises them
	// and guards statePrint — and makes Close's later picture land last.
	stateFile       string
	persistMu       sync.Mutex
	statePrint      uint64
	legacyStateFile string
	stateReadOnly   bool // a failed restore must not overwrite the original queue
	// legacyState is the uncompressed file the queue was restored from,
	// removed once the compressed one has been written. Guarded by
	// persistMu, and set by Restore before anything else can save.
	legacyState string

	// dirty records a state change worth publishing even while idle. The
	// broadcaster clears it only when it actually builds a frame.
	dirty atomic.Bool
}

// New builds a Manager. Call Start before use.
func New(cfg *config.Config, reg *extractor.Registry, client *httpx.Client, log *slog.Logger) *Manager {
	ctx, stop := context.WithCancel(context.Background())
	hostCount := 0
	if reg != nil {
		hostCount = len(reg.Hosts())
	}
	return &Manager{
		cfg:             cfg,
		proxyConfig:     proxy.Configuration{Endpoints: slices.Clone(cfg.ProxyEndpoints), Feeds: slices.Clone(cfg.ProxyFeeds)},
		reg:             reg,
		hostCount:       hostCount,
		version:         cfg.Version,
		resolveSlots:    make(chan struct{}, config.ResolveConcurrency),
		client:          client.Streaming(),
		log:             log,
		jobs:            make(map[string]*Job),
		parts:           make(map[string]struct{}),
		hostActive:      make(map[string]int),
		hostGate:        newHostGate(),
		dir:             cfg.DownloadDir,
		stateFile:       cfg.StateFile,
		legacyStateFile: cfg.LegacyStateFile,
		minFree:         cfg.MinFreeDisk,
		throttle:        newThrottle(cfg.SpeedLimit),
		limit:           cfg.Concurrency,
		streams:         cfg.Streams,
		timings:         defaultDownloadTimings(),
		hosts:           newHostLimiter(config.MaxConnectionsPerHost),
		wake:            make(chan struct{}, 1),
		urgent:          make(chan struct{}, 1),
		ctx:             ctx,
		stop:            stop,
		subs:            make(map[chan []byte]*subscriber),
	}
}

// Start launches the dispatcher and the progress broadcaster.
func (m *Manager) Start() {
	m.mu.Lock()
	if m.started || m.closing {
		m.mu.Unlock()
		return
	}
	m.started = true
	m.wg.Add(3)
	m.mu.Unlock()
	// Measure once up front: the first snapshot a browser is sent is built
	// before the broadcaster has ticked, and a zero there would draw as a
	// full disk until the first sample landed.
	m.sampleDisk(time.Now())

	go m.dispatch()
	go m.broadcast()
	go m.saver()
}

// Close cancels every in-flight transfer, waits for the workers to exit and
// releases every subscriber. It is safe to call more than once, so callers
// can both defer it and call it explicitly at the right moment.
func (m *Manager) Close() {
	m.closeOnce.Do(func() {
		m.settingsMu.Lock()
		// Before the cancellation, not after it. Stopping cancels every
		// transfer in flight, and a worker winding down marks its item
		// canceled — which is indistinguishable, once written, from an item
		// the user cancelled on purpose, and would restore as nothing left
		// to do. Recorded here they are still running, which the state file
		// writes down as queued: the honest description of a transfer the
		// process did not live to finish.
		//
		// The cost is that a file completing during the wind-down is
		// recorded as queued instead of done. That resolves itself — the
		// next run finds it whole on disk and skips it — where the other way
		// round silently abandons an unfinished download.
		// Serialize the final snapshot with the saver and stop accepting new
		// work before taking it. A saver already waiting for this lock must
		// not overwrite it with workers' shutdown cancellations afterwards.
		m.persistMu.Lock()
		m.mu.Lock()
		m.closing = true
		st := m.stateLocked()
		m.mu.Unlock()
		m.persistState(st)
		m.stop()
		m.persistMu.Unlock()
		m.settingsMu.Unlock()
		m.wg.Wait()
		if m.proxies != nil {
			if err := m.proxies.Close(); err != nil {
				m.log.Warn("close proxy database", "err", err)
			}
		}

		m.subsMu.Lock()
		m.closed = true
		for ch := range m.subs {
			close(ch)
			delete(m.subs, ch)
		}
		m.subsMu.Unlock()
	})
}

// Add registers a URL and starts resolving it in the background, so the UI
// can show the job immediately rather than blocking on a scrape.
func (m *Manager) Add(rawURL, password string) (string, error) {
	// Something the user did, so they see it at once. See nudge.
	defer m.nudge()
	u, err := extractor.ParseURL(rawURL)
	if err != nil {
		return "", err
	}

	job := &Job{
		ID:        newID(),
		Source:    u.String(),
		Title:     u.Hostname() + u.Path,
		Host:      m.reg.Find(u).Name(),
		Password:  password,
		CreatedAt: time.Now(),
	}

	m.mu.Lock()
	if m.closing {
		m.mu.Unlock()
		return "", ErrClosed
	}
	m.jobs[job.ID] = job
	m.order = append(m.order, job.ID)
	m.rereadLocked(job)
	m.mu.Unlock()
	m.markDirty()
	return job.ID, nil
}

// resolve scrapes the source page and enqueues the files it found.
func (m *Manager) resolve(ctx context.Context, job *Job, generation uint64) {
	defer m.wg.Done()

	res, ex, err := m.extractSource(ctx, job)

	m.mu.Lock()
	if m.jobs[job.ID] != job || job.resolveID != generation {
		m.mu.Unlock()
		return
	}
	job.resolving = false
	job.cancel = nil
	switch {
	case job.canceled || (err != nil && ctx.Err() != nil):
		job.canceled = true
	case err != nil:
		job.Err = err.Error()
		m.log.Warn("resolve failed", "job", job.ID, "url", job.Source, "err", err)
	default:
		m.applyResultLocked(job, ex.Name(), res)
	}
	m.mu.Unlock()

	m.markDirty()
	m.signal()
}

func (m *Manager) extractSource(ctx context.Context, job *Job) (*extractor.Result, extractor.Extractor, error) {
	if m.resolveSlots != nil {
		select {
		case m.resolveSlots <- struct{}{}:
			defer func() { <-m.resolveSlots }()
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	return m.reg.Extract(ctx, job.Source, extractor.Options{Password: job.Password})
}

// applyResultLocked turns a resolved source into the job's items. Caller
// holds mu.
func (m *Manager) applyResultLocked(job *Job, host string, res *extractor.Result) {
	job.Host = host
	if res.Title != "" {
		// What the job is shown as carries the extractor's note — a listing
		// cut short, and how far — beside its name.
		job.Title = res.Label()
	}
	// Only fan a job out into its own folder when it holds more than one
	// file; a lone file would otherwise get a pointless directory.
	//
	// Named after the title alone, never the note: a note changes when a cap
	// is raised or a rate limit lifts, and a folder that changed with it
	// filed the next run somewhere new and downloaded every file again.
	folder := ""
	if len(res.Files) > 1 {
		folder = SafeName(util.FirstNonEmpty(res.Title, job.Title))
	}
	items := make([]*Item, 0, len(res.Files))
	for i, f := range res.Files {
		items = append(items, m.newItem(job, f, folder, i))
	}
	separateNames(items)
	job.Items = append(job.Items, items...)
	m.queue = append(m.queue, items...)
	m.log.Info("resolved", "job", job.ID, "host", host, "title", job.Title, "files", len(res.Files))
}

// newItem converts an extractor result into a queued item.
func (m *Manager) newItem(job *Job, f extractor.File, folder string, index int) *Item {
	name := f.Name
	if name == "" {
		name = util.FirstNonEmpty(util.NameFromURL(f.URL), fmt.Sprintf("file-%03d", index+1))
	}
	size := f.Size
	if size == 0 {
		size = -1
	}
	return &Item{
		ID:         newID(),
		JobID:      job.ID,
		Name:       name,
		Dir:        filepath.Join(folder, SafeRelPath(f.Dir)),
		URL:        f.URL,
		Headers:    f.Headers,
		Segments:   f.Segments,
		SegmentKey: f.SegmentKey,
		External:   f.External,
		Size:       size,
		SizeApprox: f.SizeApprox,
		Status:     StatusQueued,
		resolve:    f.Resolve,
		cipher:     f.Cipher,
		pace:       f.Pace,
		reject:     f.Reject,
	}
}

// dispatch starts queued items whenever a worker slot frees up.
func (m *Manager) dispatch() {
	defer m.wg.Done()
	// Timers in the persistent pool and refreshed feeds can make a route
	// available without a worker completing. Settings can enable a pool at
	// any point after this dispatcher starts.
	ticker := time.NewTicker(config.ProxyDispatchTick)
	defer ticker.Stop()
	for {
		select {
		case <-m.ctx.Done():
			return
		case <-m.wake:
		case <-ticker.C:
			m.mu.Lock()
			enabled := m.proxyEnabled
			m.mu.Unlock()
			if !enabled {
				continue
			}
		}

		m.mu.Lock()
		m.updateProxyDemandLocked()
		m.mu.Unlock()
		for {
			m.mu.Lock()
			it := m.nextLocked()
			if it == nil {
				m.mu.Unlock()
				break
			}
			m.running++
			it.inFlight = true
			// Charged here and released in runItem, so the reservation
			// spans exactly the worker's ownership of the item.
			it.hostKey = m.hostKeyLocked(it)
			m.hostActive[it.hostKey]++
			if it.route != nil {
				m.proxyRunning++
			}
			if itemHeld != nil {
				itemHeld(it, true)
			}
			it.Status = StatusRunning
			// A note written while the item waited — a stall deferral's,
			// say — describes a state that has just ended.
			it.Note = ""
			it.waitingFor = ""
			it.notBefore = time.Time{}
			it.startedAt = time.Now()
			it.lastSample = it.startedAt
			it.lastBytes = it.downloaded.Load()
			ctx, cancel := context.WithCancel(m.ctx)
			it.cancel = cancel
			m.mu.Unlock()

			m.markDirty()
			m.wg.Add(1)
			go m.runItem(ctx, cancel, it)
		}
	}
}

// nextLocked takes the next runnable item out of the queue. Caller holds mu.
//
// Four things can disqualify the item at the front. It may have been
// cancelled while it sat there, in which case it is dropped. A worker may
// still own it, which inFlight reports and which must never be handed out
// twice. Or its host may already be running as many transfers as it will
// tolerate, or have named a time before which it will not serve this file —
// and those are a *skip*, not a drop: the item keeps its place and a later
// one is started instead, so one gentle host cannot idle the whole pool
// behind it.
func (m *Manager) nextLocked() *Item {
	// A paused queue starts nothing new. Transfers already running park
	// inside their reads instead, so they keep their place in the file.
	if m.closing || m.throttle.isPaused() || m.running >= m.limit {
		return nil
	}
	// Nor does one with no room to write into. Transfers already running are
	// left alone: their bytes are on disk either way, and abandoning one
	// near its end would throw away more room than it recovered.
	if m.lowOnSpace() {
		return nil
	}
	// The ordinary queue is FIFO. Advancing its head costs nothing; copying
	// every remaining item on each dispatch makes a large album quadratic.
	now := time.Now()
	unavailableRoutes := make(map[routeGroup]bool)
	for len(m.queue) > 0 {
		it := m.queue[0]
		if it.Status == StatusQueued && !it.inFlight && (it.notBefore.After(now) || m.hostFullLocked(it) || !m.leaseRouteLocked(it, unavailableRoutes)) {
			break
		}
		m.queue[0] = nil
		m.queue = m.queue[1:]
		if it.Status == StatusQueued && !it.inFlight {
			return it
		}
	}

	var chosen *Item
	kept := m.queue[:0]
	for _, it := range m.queue {
		switch {
		case chosen != nil:
			kept = append(kept, it)
		case it.Status != StatusQueued || it.inFlight:
			// Cancelled where it stood, or still owned: forget it.
		case it.notBefore.After(now), m.hostFullLocked(it), !m.leaseRouteLocked(it, unavailableRoutes):
			kept = append(kept, it)
		default:
			chosen = it
		}
	}
	clear(m.queue[len(kept):])
	m.queue = kept
	return chosen
}

// lowOnSpace reports whether the destination has too little room left to
// start another transfer.
//
// It reads the figure the broadcaster samples rather than asking the
// filesystem, so it costs nothing on a path the dispatcher walks constantly
// and never blocks the lock it is called under. That figure is at most one
// DiskSampleInterval old, which a floor measured in gigabytes can absorb.
//
// A destination that could not be measured reports zero for both, and is
// never treated as full: refusing to download because the check itself
// failed would be a worse failure than the one it guards against.
func (m *Manager) lowOnSpace() bool {
	if m.minFree <= 0 || m.diskTotal.Load() <= 0 {
		return false
	}
	return m.diskFree.Load() < m.minFree
}

// hostFullLocked reports whether an item's host is already running as many
// transfers as its pace allows.
//
// This is the cap an extractor declares, which it can do because those hosts
// are known by the URL alone. A cap *learned* from a host refusing work
// cannot be applied here: the host that answers is the one a signed link
// resolves to, and nothing has resolved yet at dispatch. That one is a
// queue the transfer itself waits in — see hostGate. Caller holds mu.
func (m *Manager) hostFullLocked(it *Item) bool {
	if m.usesProxies(it) {
		// A transfer started before enabling proxies has no lease. Let it
		// finish before the pool can lease the same direct address. When
		// disabling, the normal group cap below drains all leased transfers
		// before returning to the ordinary connection.
		return m.hostActive[m.hostKeyLocked(it)] > m.proxyRunning
	}
	if it.pace != nil && it.pace.Files > 0 &&
		m.hostActive[m.hostKeyLocked(it)] >= it.pace.Files {
		return true
	}
	// And the cap a host has asked for itself. An item that has resolved
	// once knows which host that is even while it sits in the queue, so
	// its turn is waited for here rather than in a worker — which is what
	// leaves the workers free for the hosts that are coping.
	return m.hostGate.full(hostOf(it.URL))
}

// hostKeyLocked names the remote an item will be charged against.
//
// An explicit pacing group covers aliases and resolved storage URLs alike.
// Otherwise, an item with a resolver has no URL yet, so the job's source stands in.
// That is the right answer rather than a fallback: a paced host's items come
// from that host's own listing, and the storage server a resolver eventually
// picks belongs to it either way. Caller holds mu.
func (m *Manager) hostKeyLocked(it *Item) string {
	if it.pace != nil && it.pace.Group != "" {
		return "group:" + it.pace.Group
	}
	if it.URL != "" {
		return hostOf(it.URL)
	}
	if job, ok := m.jobs[it.JobID]; ok && job.Source != "" {
		return hostOf(job.Source)
	}
	return ""
}

// itemHostLocked names the host an item is actually talking to, which is
// where a signed link resolved rather than where the job came from. Caller
// holds mu.
func (m *Manager) itemHostLocked(it *Item) string { return hostOf(it.URL) }

// itemHost is itemHostLocked for callers that hold nothing.
func (m *Manager) itemHost(it *Item) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.itemHostLocked(it)
}

// itemHeld, when set, brackets the span in which a worker owns an item —
// exactly the span inFlight covers, and reported from under the same lock.
// Bracketing the goroutine instead would overstate it: once ownership is
// released the next worker may legitimately start while the previous one is
// still unwinding. Only tests set this.
var itemHeld func(it *Item, held bool)

// runItem performs one transfer and records the outcome.
func (m *Manager) runItem(ctx context.Context, cancel context.CancelFunc, it *Item) {
	defer m.wg.Done()
	defer cancel()

	err := m.transferRouted(ctx, it)

	m.mu.Lock()
	it.finishedAt = time.Now()
	it.cancel = nil
	it.speed = 0
	it.Note = ""
	it.waitingFor = ""
	it.inFlight = false
	if itemHeld != nil {
		itemHeld(it, false)
	}
	switch {
	case err == nil:
		it.Status = StatusDone
		it.Err = ""
		if it.Size <= 0 {
			it.Size = it.downloaded.Load()
		}
		// A host that saw this through is coping with what it is being
		// given, which is the only evidence that a cap put on it earlier
		// can safely be relaxed. The item's own patience starts over with
		// it: the turns it spent waiting were this host's bad spell, not a
		// property of the file.
		it.overloadWaits = 0
		m.hostGate.eased(m.itemHostLocked(it), m.limit)
		m.logFinishedLocked(it, "download complete")
	case ctx.Err() != nil:
		it.Status = StatusCanceled
		it.Err = ""
		m.logFinishedLocked(it, "download canceled")
	case m.deferHostQueuedLocked(it, err):
		// Its host is full and the item has gone back to the queue; the
		// dispatcher will pick it up when that host has room. Not a
		// failure, and nothing more to record here.
	case m.deferWaitLocked(it, err):
		// Its host named a time to come back, and the item is back in the
		// queue until then — see deferWaitLocked.
	case m.deferStalledLocked(it, err):
		// The stall watchdog gave up on this attempt, and the item has just
		// been sent to the back of the queue with its part file intact —
		// see deferStalledLocked. Nothing more to record here: the deferral
		// wrote the item's state itself.
	default:
		it.Status = StatusFailed
		it.Err = err.Error()
		m.log.Warn("download failed", "item", it.ID, "name", it.Name, "err", err)
	}
	// A retry asked for while this worker was still finishing takes effect
	// now that the item is free again.
	if it.retryPending {
		it.retryPending = false
		m.enqueueLocked(it)
	}
	m.running--
	if it.route != nil {
		m.proxyRunning--
		it.route = nil
	}
	if m.hostActive[it.hostKey]--; m.hostActive[it.hostKey] <= 0 {
		delete(m.hostActive, it.hostKey)
	}
	it.hostKey = ""
	m.mu.Unlock()

	m.markDirty()
	m.signal()
}

// logFinishedLocked records one finished transfer, so a backend log holds a
// line for every download rather than only for the ones that went wrong. A
// skipped item moved no bytes, and saying so is more use than a size with a
// zero elapsed time beside it. Caller holds mu.
func (m *Manager) logFinishedLocked(it *Item, msg string) {
	if it.Skipped {
		m.log.Info("already downloaded", "item", it.ID, "name", it.Name, "path", it.Path)
		return
	}
	args := []any{"item", it.ID, "name", it.Name, "path", it.Path, "bytes", it.downloaded.Load()}
	if !it.startedAt.IsZero() {
		args = append(args, "took", it.finishedAt.Sub(it.startedAt).Round(time.Millisecond))
	}
	m.log.Info(msg, args...)
}

// CancelJob stops a job and everything under it.
func (m *Manager) CancelJob(id string) error {
	// Something the user did, so they see it at once. See nudge.
	defer m.nudge()
	m.mu.Lock()
	job, ok := m.jobs[id]
	if !ok {
		m.mu.Unlock()
		return ErrNotFound
	}
	job.canceled = true
	job.restored = false
	if job.cancel != nil {
		job.cancel()
	}
	for _, it := range job.Items {
		cancelItemLocked(it)
	}
	m.pruneQueueLocked()
	m.mu.Unlock()

	m.markDirty()
	m.signal()
	return nil
}

// CancelItem stops one file, leaving the rest of its job alone.
func (m *Manager) CancelItem(jobID, itemID string) error {
	// Something the user did, so they see it at once. See nudge.
	defer m.nudge()
	m.mu.Lock()
	it, ok := m.findItemLocked(jobID, itemID)
	if !ok {
		m.mu.Unlock()
		return ErrNotFound
	}
	cancelItemLocked(it)
	if job := m.jobs[jobID]; job != nil && !jobHasWorkLeft(job) {
		job.restored = false
	}
	m.pruneQueueLocked()
	m.mu.Unlock()

	m.markDirty()
	m.signal()
	return nil
}

// cancelItemLocked marks an item cancelled and interrupts it if running.
// Caller holds mu.
func cancelItemLocked(it *Item) {
	// A cancel always wins over a retry that has not started yet.
	it.retryPending = false
	if it.Status.Terminal() {
		return
	}
	it.Status = StatusCanceled
	it.Err = ""
	if it.cancel != nil {
		it.cancel()
	}
}

// Canceled items must be released even when pause or the disk floor keeps
// the dispatcher asleep. Otherwise a cleared queue retains its items and
// Busy keeps reporting work that will never run.
func (m *Manager) pruneQueueLocked() {
	m.queue = slices.DeleteFunc(m.queue, func(it *Item) bool { return it.Status != StatusQueued })
}

// RetryJob requeues every failed or cancelled item, re-resolving the source
// first when the job never produced any items.
func (m *Manager) RetryJob(id string) error {
	// Something the user did, so they see it at once. See nudge.
	defer m.nudge()
	recheckTools()

	m.mu.Lock()
	if m.closing {
		m.mu.Unlock()
		return ErrClosed
	}
	job, ok := m.jobs[id]
	if !ok {
		m.mu.Unlock()
		return ErrNotFound
	}
	if job.resolving && !job.canceled {
		m.mu.Unlock()
		return errors.New("job is already resolving")
	}
	job.canceled = false

	// Two jobs are re-read rather than re-queued. A restored one, for the
	// same reason resuming the whole queue re-reads it: its items are last
	// run's record, not somewhere the files can be fetched from. And one
	// with no items at all, where the extractor itself is what failed.
	if job.restored || job.unfetchable || len(job.Items) == 0 {
		m.rereadLocked(job)
		m.mu.Unlock()

		m.markDirty()
		return nil
	}

	job.Err = ""
	for _, it := range job.Items {
		if it.Status == StatusFailed || it.Status == StatusCanceled {
			m.enqueueLocked(it)
		}
	}
	m.mu.Unlock()

	m.markDirty()
	m.signal()
	return nil
}

// RetryItem requeues a single file.
func (m *Manager) RetryItem(jobID, itemID string) error {
	// Something the user did, so they see it at once. See nudge.
	defer m.nudge()
	recheckTools()

	m.mu.Lock()
	if m.closing {
		m.mu.Unlock()
		return ErrClosed
	}
	it, ok := m.findItemLocked(jobID, itemID)
	if !ok {
		m.mu.Unlock()
		return ErrNotFound
	}
	job := m.jobs[jobID]
	unfetchable := job != nil && (job.restored || job.unfetchable)
	if !unfetchable && (it.Status == StatusRunning || it.Status == StatusQueued) {
		m.mu.Unlock()
		return errors.New("item is already in progress")
	}
	if job != nil {
		job.canceled = false
	}

	// A restored job's items carry no URL — none is stored, because the
	// signed links half these hosts hand out would be stale by the next run
	// — so there is nothing to fetch one of them from and re-queueing it on
	// its own fails with "no download URL". The job is re-read instead,
	// which is what the item's own note asks for and what the job-level
	// retry does. Everything already downloaded is recognised on disk and
	// skipped, so this costs the listing and nothing more.
	if unfetchable {
		m.rereadLocked(job)
		m.mu.Unlock()

		m.markDirty()
		return nil
	}

	m.enqueueLocked(it)
	m.mu.Unlock()

	m.markDirty()
	m.signal()
	return nil
}

// rereadLocked puts a job back to being resolved from its source, dropping
// whatever items it holds. Registering the goroutine under mu prevents it
// from racing Close's Wait. A generation keeps a canceled extractor's late
// result from replacing the result of a subsequent retry. Caller holds mu.
func (m *Manager) rereadLocked(job *Job) {
	if job.cancel != nil {
		job.cancel()
	}
	job.restored = false
	job.unfetchable = false
	job.canceled = false
	job.Items = nil
	job.Err = ""
	job.resolving = true
	ctx, cancel := context.WithCancel(m.ctx)
	job.cancel = cancel
	job.resolveID++
	generation := job.resolveID
	m.wg.Add(1)
	go func() {
		defer cancel()
		m.resolve(ctx, job, generation)
	}()
}

// recheckTools is tools.Recheck, kept behind a variable so tests can observe
// it. A retry is a user saying "try that again", and the commonest reason a
// job failed outright is a helper binary that was not installed — so it is
// also the moment to stop believing that it still is not. Called before the
// lock: the tools package guards itself, and there is no reason to hold mu
// across it.
var recheckTools = tools.Recheck

// enqueueLocked returns an item to the queue for a fresh attempt. An item a
// worker still owns is only marked: that worker re-queues it as it exits, so
// the same file is never downloaded by two goroutines at once.
// Caller holds mu.
func (m *Manager) enqueueLocked(it *Item) {
	if it.Status.Terminal() {
		it.proxyRetries = 0
	}
	// Whatever was true of the destination last time is re-established by
	// the worker, not carried over.
	it.Skipped = false
	if it.inFlight {
		it.retryPending = true
		return
	}
	it.Status = StatusQueued
	it.Err = ""
	it.Note = ""
	it.waitingFor = ""
	it.notBefore = time.Time{}
	it.speed = 0
	it.stallDefers = 0
	// A retry starts the host's patience over. Carried across, a file that
	// had run out of it failed on the first refusal after being retried,
	// without waiting at all. deferHostQueuedLocked puts both back for the
	// one caller that is continuing a wait rather than starting one.
	it.overloadWaits = 0
	it.refusedAt = time.Time{}
	it.startedAt = time.Time{}
	it.finishedAt = time.Time{}
	it.downloaded.Store(0)
	it.lastBytes = 0
	m.queue = append(m.queue, it)
}

// itemNoteLocked renders what an item has to say for itself right now.
//
// An item waiting for a host is the one case where the stored note cannot
// be trusted: what the host is taking changes while the item waits, so a
// note written when it was turned away goes on stating whatever was true
// then. A queue full of rows waiting on one host then shows two or three
// different numbers for it at once. This reads the host's current answer
// instead, so every row waiting on it says the same thing. Caller holds mu.
//
// An item held back until a time is the same in a different way: the time
// left is worked out here, rounded up to the minute so the row changes once
// a minute rather than in every frame, and the note goes once the time has
// come — the item is then only waiting for a worker like any other.
func (m *Manager) itemNoteLocked(it *Item) string {
	if !it.notBefore.IsZero() {
		left := time.Until(it.notBefore)
		if left <= 0 {
			return ""
		}
		return fmt.Sprintf("%s — %s left", it.Note, minutesLeft(left))
	}
	if it.waitingFor == "" {
		return it.Note
	}
	limit, _ := m.hostGate.waiting(it.waitingFor)
	return waitingNote(it.waitingFor, limit)
}

// minutesLeft reads a wait as whole minutes, rounded up: "1h4m", "12m".
func minutesLeft(d time.Duration) string {
	d = (d + time.Minute - 1).Truncate(time.Minute)
	return strings.TrimSuffix(d.String(), "0s")
}

// deferHostQueuedLocked puts an item back in the queue because its host is
// already giving out every slot it has.
//
// Nothing has gone wrong and nothing is counted against the item: it simply
// has not reached the front of that host's queue, and the dispatcher will
// pass over it until there is room. Unlike a stall this carries no budget,
// because there is no attempt to run out of — an item can wait its turn all
// day without that meaning anything is failing.
func (m *Manager) deferHostQueuedLocked(it *Item, err error) bool {
	queued, ok := errors.AsType[*hostQueuedError](err)
	if !ok {
		return false
	}
	// A host being left alone frees nothing and finishes nothing, so
	// without this the dispatcher would have no reason to look at the queue
	// again until some other transfer happened to end — and if every host
	// is quiet, none will. That holds for the rest of the queue behind this
	// host even when this item is taken off by a pending retry.
	if queued.wait > 0 {
		time.AfterFunc(queued.wait, m.signal)
	}
	if it.retryPending {
		return false
	}
	turns, refused := it.overloadWaits, it.refusedAt
	m.enqueueLocked(it) // clears the note along with the rest; say why after
	it.overloadWaits, it.refusedAt = turns, refused
	// The host, not the sentence: what it is taking is read afresh for every
	// snapshot, so this row and the hundred others behind the same host
	// never disagree about it.
	it.waitingFor = queued.host
	it.Note = waitingNote(queued.host, queued.limit)
	return true
}

// deferWaitLocked puts an item back in the queue until a time its host
// named, and reports whether it did. Caller holds mu.
//
// Keep2Share makes an address wait the best part of an hour between free
// downloads. Sat out in the worker, that read as "Downloading" for an hour
// in which nothing moved, and kept a slot from everything else in the queue.
// Back in the queue it reads as waiting, the dispatcher passes over it until
// the time comes, and a timer wakes the dispatcher then, since nothing else
// might. Like a full host, this is not an attempt that failed, so it costs
// the item nothing.
func (m *Manager) deferWaitLocked(it *Item, err error) bool {
	wait, ok := errors.AsType[*extractor.WaitError](err)
	if !ok || it.retryPending {
		return false
	}
	if it.route == nil && m.usesProxies(it) {
		// Proxies may have been enabled while this ordinary direct attempt
		// was still resolving. Its timer belongs to that address alone.
		wait = &extractor.WaitError{Until: time.Now(), Reason: "Waiting for an available download route"}
	}
	m.enqueueLocked(it) // clears the note along with the rest; say why after
	it.notBefore = wait.Until
	it.Note = wait.Reason
	time.AfterFunc(time.Until(wait.Until), m.signal)
	m.log.Info("host asked for a wait; deferred until then",
		"item", it.ID, "name", it.Name, "until", wait.Until.Format(time.DateTime))
	return true
}

// deferStalledLocked sends a stalled item to the back of the queue instead
// of failing it, and reports whether it did. Caller holds mu.
//
// A stall is usually the host's condition rather than the item's — bunkr's
// CDN, measured, serves an address at full speed for a few hundred megabytes
// and then throttles it to a trickle for everything, on fresh connections
// included. Retrying such an item in place pins a worker for StallTimeout a
// time while the whole queue waits behind it; sending it to the back frees
// the slot at once, lets everything that can move take its turn, and comes
// back to this item after the widest interval the queue can offer — with
// the part file and sidecar intact, so its next turn resumes rather than
// restarts.
//
// The patience is the same retry budget the in-place retries used to spend,
// paid at the back of the queue instead of at the front: past MaxRetries
// deferrals the next stall is a failure, so a host that never resumes still
// terminates the item — and the headless run with it. A retry the user
// asked for meanwhile takes precedence and starts the item over with a
// clean slate.
func (m *Manager) deferStalledLocked(it *Item, err error) bool {
	stall, ok := errors.AsType[*stalledError](err)
	if !ok || it.retryPending {
		return false
	}
	limit := 0
	if m.cfg != nil {
		limit = m.cfg.MaxRetries
	}
	// A stall that came after bytes had landed is the connection's failure
	// rather than the transfer's, exactly as a dropped connection is to
	// transfer: the host served the file a moment ago and the part file is
	// further along than it was. The budget counts stalls in a row that
	// moved nothing, so this one starts the count over.
	if stall.moved {
		it.stallDefers = 0
	}
	if it.stallDefers >= limit {
		return false
	}

	defers := it.stallDefers + 1
	m.enqueueLocked(it) // clears the counter along with the rest; restore it
	it.stallDefers = defers
	it.Note = fmt.Sprintf("stalled — no data for %s; sent to the back of the queue (%d of %d)",
		stall.after, defers, limit)
	m.log.Info("transfer stalled; deferred to the back of the queue",
		"item", it.ID, "name", it.Name, "defers", defers, "limit", limit)
	return true
}

// RemoveJob cancels a job and forgets it. Files already on disk stay.
func (m *Manager) RemoveJob(id string) error {
	// Something the user did, so they see it at once. See nudge.
	defer m.nudge()
	m.mu.Lock()
	job, ok := m.jobs[id]
	if !ok {
		m.mu.Unlock()
		return ErrNotFound
	}
	job.canceled = true
	if job.cancel != nil {
		job.cancel()
	}
	for _, it := range job.Items {
		cancelItemLocked(it)
	}
	delete(m.jobs, id)
	m.order = util.Remove(m.order, id)
	m.pruneQueueLocked()
	m.mu.Unlock()

	m.markDirty()
	m.signal()
	return nil
}

// ClearFinished forgets every job that has nothing left to do.
func (m *Manager) ClearFinished() int {
	// Something the user did, so they see it at once. See nudge.
	defer m.nudge()
	m.mu.Lock()
	var removed int
	kept := m.order[:0]
	for _, id := range m.order {
		job := m.jobs[id]
		if job == nil || !job.status().Terminal() {
			kept = append(kept, id)
			continue
		}
		job.canceled = true
		if job.cancel != nil {
			job.cancel()
		}
		for _, it := range job.Items {
			cancelItemLocked(it)
		}
		delete(m.jobs, id)
		removed++
	}
	clear(m.order[len(kept):])
	m.order = kept
	m.pruneQueueLocked()
	m.mu.Unlock()

	if removed > 0 {
		m.markDirty()
	}
	return removed
}

// SetConcurrency resizes the pool without interrupting running transfers.
func (m *Manager) SetConcurrency(n int) error {
	return m.ApplySettings(Settings{Concurrency: &n})
}

// SetStreams changes the connection ceiling for new transfers.
func (m *Manager) SetStreams(n int) error {
	return m.ApplySettings(Settings{Streams: &n})
}

// ErrNotFound is returned for an unknown job or item id.
var ErrNotFound = errors.New("not found")

// ErrClosed is returned when work is submitted during or after shutdown.
var ErrClosed = errors.New("download manager is closed")

// DownloadDir is where finished files are being written.
func (m *Manager) DownloadDir() string {
	m.dirMu.RLock()
	defer m.dirMu.RUnlock()
	return m.dir
}

// SetDownloadDir moves the destination, creating the directory and proving it
// writable first — the same checks startup makes, since a directory named
// from the UI is no more trustworthy than one named on the command line.
//
// Transfers already running keep the destination they started with: their
// path was settled when the worker took the item, and moving a part file
// mid-flight would break the resume that part file exists for. Everything
// still queued goes to the new place. That is worth knowing rather than
// hiding, so the API says it back to the caller.
func (m *Manager) SetDownloadDir(path string) error {
	return m.ApplySettings(Settings{DownloadDir: &path})
}

// SetPaused stops or resumes the whole queue.
//
// Pausing holds the connections open rather than dropping them: a running
// transfer parks inside its read, and the dispatcher stops handing out work.
// A long pause may still cost a connection to a server that times it out,
// which the usual retry and resume handle.
func (m *Manager) SetPaused(paused bool) {
	_ = m.ApplySettings(Settings{Paused: &paused})
}

// resumeRestored sets going every job that came back from the state file
// with work left in it.
//
// Their items are dropped rather than queued. What was read back describes
// what the last run found, which is worth showing and useless to fetch from:
// the links are expired or were never written down. Resolution builds the
// real list, and the files already on disk are skipped as it goes — so the
// job picks up where it left off without this having to work out where that
// was.
func (m *Manager) resumeRestored() {
	m.mu.Lock()
	if m.closing {
		m.mu.Unlock()
		return
	}
	starting := 0
	for _, id := range m.order {
		job, ok := m.jobs[id]
		if !ok || !job.restored {
			continue
		}
		m.rereadLocked(job)
		starting++
	}
	m.mu.Unlock()

	if starting == 0 {
		return
	}
	m.log.Info("resuming restored jobs", "jobs", starting)
	m.markDirty()
}

// Paused reports whether the queue is held.
func (m *Manager) Paused() bool { return m.throttle.isPaused() }

// Busy reports whether anything is still going to happen: a transfer
// running, an item waiting for a slot, or a source still being scraped.
//
// A job that is only resolving counts, and has to: it has no items yet, so
// the queue looks empty at exactly the moment work is about to arrive.
func (m *Manager) Busy() bool {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.running > 0 || len(m.queue) > 0 {
		return true
	}
	for _, job := range m.jobs {
		if job.resolving {
			return true
		}
	}
	return false
}

// SetSpeedLimit caps total throughput in bytes per second. Zero is
// unlimited. The cap is shared: it bounds everything moving at once, not
// each transfer separately.
func (m *Manager) SetSpeedLimit(bytesPerSecond int64) error {
	return m.ApplySettings(Settings{SpeedLimit: &bytesPerSecond})
}

// SpeedLimit reports the current cap in bytes per second; 0 is unlimited.
func (m *Manager) SpeedLimit() int64 { return m.throttle.currentLimit() }

// findItemLocked looks up an item within a job. Caller holds mu.
func (m *Manager) findItemLocked(jobID, itemID string) (*Item, bool) {
	job, ok := m.jobs[jobID]
	if !ok {
		return nil, false
	}
	for _, it := range job.Items {
		if it.ID == itemID {
			return it, true
		}
	}
	return nil, false
}

// signal nudges the dispatcher without ever blocking the caller.
func (m *Manager) signal() {
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

// markDirty asks the broadcaster to publish, for a change no running
// transfer would otherwise carry to the browser.
func (m *Manager) markDirty() { m.dirty.Store(true) }
