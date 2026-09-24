// Package download runs the worker pool: it turns submitted URLs into jobs,
// schedules their files across a configurable number of parallel transfers,
// and publishes live progress to subscribers.
package download

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/extractor"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
	"github.com/JohanLindvall/HeapLeach/internal/tools"
	"github.com/JohanLindvall/HeapLeach/internal/util"
)

// Manager owns all jobs and the worker pool.
//
// Locking rule: every Job and Item field except Item.downloaded is guarded
// by mu. Workers take mu only to publish state transitions, never while
// blocked on I/O.
type Manager struct {
	cfg    *config.Config
	reg    *extractor.Registry
	client *httpx.Client
	log    *slog.Logger

	mu      sync.Mutex
	jobs    map[string]*Job
	order   []string
	queue   []*Item
	running int
	limit   int
	streams int
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
	diskFree    atomic.Int64
	diskTotal   atomic.Int64

	subsMu sync.Mutex
	// subs maps each open stream to the jobs that subscriber renders
	// items for; everything else reaches it slimmed. See
	// snapshotfilter.go.
	subs      map[chan []byte]*subscriber
	closed    bool // set under subsMu by Close; a late Subscribe is answered closed
	closeOnce sync.Once

	// hostCount is how many extractors the registry holds, fixed at
	// construction. It rides in every snapshot, and asking the registry to
	// list its names on each progress tick allocated a slice to count and
	// drop.
	hostCount int

	// minFree is how much room must be left at the destination before
	// another transfer is started; zero disables the check. Fixed at
	// construction, so it needs no lock.
	minFree int64

	// Where the queue is written so a restart can pick it up again, and the
	// fingerprint of what was last written — an idle queue is not worth
	// rewriting every interval. The saver and Close both write it, Close
	// while the saver may still be mid-write, so persistMu serialises them
	// and guards statePrint — and makes Close's later picture land last.
	stateFile  string
	persistMu  sync.Mutex
	statePrint uint64

	// dirty records a state change worth pushing to subscribers even though
	// nothing is transferring. It is taken rather than peeked at and cleared
	// separately, so a change landing while a snapshot is being built sets
	// the flag again and is published on the next tick instead of being lost.
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
		cfg:        cfg,
		reg:        reg,
		hostCount:  hostCount,
		client:     client.Streaming(),
		log:        log,
		jobs:       make(map[string]*Job),
		parts:      make(map[string]struct{}),
		hostActive: make(map[string]int),
		hostGate:   newHostGate(),
		dir:        cfg.DownloadDir,
		stateFile:  cfg.StateFile,
		minFree:    cfg.MinFreeDisk,
		throttle:   newThrottle(cfg.SpeedLimit),
		limit:      cfg.Concurrency,
		streams:    cfg.Streams,
		timings:    defaultDownloadTimings(),
		hosts:      newHostLimiter(config.MaxConnectionsPerHost),
		wake:       make(chan struct{}, 1),
		urgent:     make(chan struct{}, 1),
		ctx:        ctx,
		stop:       stop,
		subs:       make(map[chan []byte]*subscriber),
	}
}

// Start launches the dispatcher and the progress broadcaster.
func (m *Manager) Start() {
	// Measure once up front: the first snapshot a browser is sent is built
	// before the broadcaster has ticked, and a zero there would draw as a
	// full disk until the first sample landed.
	m.sampleDisk(time.Now())

	m.wg.Add(3)
	go m.dispatch()
	go m.broadcast()
	go m.saver()
}

// Close cancels every in-flight transfer, waits for the workers to exit and
// releases every subscriber. It is safe to call more than once, so callers
// can both defer it and call it explicitly at the right moment.
func (m *Manager) Close() {
	m.closeOnce.Do(func() {
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
		m.persist()

		m.stop()
		m.wg.Wait()

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

	ctx, cancel := context.WithCancel(m.ctx)
	job := &Job{
		ID:        newID(),
		Source:    u.String(),
		Title:     u.Hostname() + u.Path,
		Host:      m.reg.Find(u).Name(),
		Password:  password,
		CreatedAt: time.Now(),
		resolving: true,
		cancel:    cancel,
	}

	m.mu.Lock()
	m.jobs[job.ID] = job
	m.order = append(m.order, job.ID)
	m.mu.Unlock()
	m.markDirty()

	m.wg.Add(1)
	go m.resolve(ctx, job)
	return job.ID, nil
}

// resolve scrapes the source page and enqueues the files it found.
func (m *Manager) resolve(ctx context.Context, job *Job) {
	defer m.wg.Done()

	res, ex, err := m.reg.Extract(ctx, job.Source, extractor.Options{Password: job.Password})

	m.mu.Lock()
	job.resolving = false
	switch {
	case job.canceled || (err != nil && ctx.Err() != nil):
		job.canceled = true
	case err != nil:
		job.Err = err.Error()
		m.log.Warn("resolve failed", "job", job.ID, "url", job.Source, "err", err)
	default:
		job.Host = ex.Name()
		if res.Title != "" {
			job.Title = res.Title
		}
		// Only fan a job out into its own folder when it holds more than
		// one file; a lone file would otherwise get a pointless directory.
		folder := ""
		if len(res.Files) > 1 {
			folder = SafeName(job.Title)
		}
		for i, f := range res.Files {
			it := m.newItem(job, f, folder, i)
			job.Items = append(job.Items, it)
			m.queue = append(m.queue, it)
		}
		m.log.Info("resolved", "job", job.ID, "host", ex.Name(), "title", job.Title, "files", len(res.Files))
	}
	m.mu.Unlock()

	m.markDirty()
	m.signal()
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
	for {
		select {
		case <-m.ctx.Done():
			return
		case <-m.wake:
		}

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
			if itemHeld != nil {
				itemHeld(it, true)
			}
			it.Status = StatusRunning
			// A note written while the item waited — a stall deferral's,
			// say — describes a state that has just ended.
			it.Note = ""
			it.waitingFor = ""
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
// Three things can disqualify the item at the front. It may have been
// cancelled while it sat there, in which case it is dropped. A worker may
// still own it, which inFlight reports and which must never be handed out
// twice. Or its host may already be running as many transfers as it will
// tolerate — and that one is a *skip*, not a drop: the item keeps its place
// and a later one is started instead, so one gentle host cannot idle the
// whole pool behind it.
func (m *Manager) nextLocked() *Item {
	// A paused queue starts nothing new. Transfers already running park
	// inside their reads instead, so they keep their place in the file.
	if m.throttle.isPaused() || m.running >= m.limit {
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
	for len(m.queue) > 0 {
		it := m.queue[0]
		if it.Status == StatusQueued && !it.inFlight && m.hostFullLocked(it) {
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
		case m.hostFullLocked(it):
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
// An item with a resolver has no URL yet, so the job's own source stands in.
// That is the right answer rather than a fallback: a paced host's items come
// from that host's own listing, and the storage server a resolver eventually
// picks belongs to it either way. Caller holds mu.
func (m *Manager) hostKeyLocked(it *Item) string {
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

	err := m.transfer(ctx, it)

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
	case ctx.Err() != nil || httpx.IsCanceled(err):
		it.Status = StatusCanceled
		it.Err = ""
		m.logFinishedLocked(it, "download canceled")
	case m.deferHostQueuedLocked(it, err):
		// Its host is full and the item has gone back to the queue; the
		// dispatcher will pick it up when that host has room. Not a
		// failure, and nothing more to record here.
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
	if job.cancel != nil {
		job.cancel()
	}
	for _, it := range job.Items {
		cancelItemLocked(it)
	}
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

// RetryJob requeues every failed or cancelled item, re-resolving the source
// first when the job never produced any items.
func (m *Manager) RetryJob(id string) error {
	// Something the user did, so they see it at once. See nudge.
	defer m.nudge()
	recheckTools()

	m.mu.Lock()
	job, ok := m.jobs[id]
	if !ok {
		m.mu.Unlock()
		return ErrNotFound
	}
	job.canceled = false

	// Two jobs are re-read rather than re-queued. A restored one, for the
	// same reason resuming the whole queue re-reads it: its items are last
	// run's record, not somewhere the files can be fetched from. And one
	// with no items at all, where the extractor itself is what failed.
	if job.restored || job.unfetchable || len(job.Items) == 0 {
		ctx := m.rereadLocked(job)
		m.mu.Unlock()

		m.markDirty()
		m.wg.Add(1)
		go m.resolve(ctx, job)
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
	it, ok := m.findItemLocked(jobID, itemID)
	if !ok {
		m.mu.Unlock()
		return ErrNotFound
	}
	if it.Status == StatusRunning || it.Status == StatusQueued {
		m.mu.Unlock()
		return errors.New("item is already in progress")
	}
	job := m.jobs[jobID]
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
	if job != nil && (job.restored || job.unfetchable) {
		ctx := m.rereadLocked(job)
		m.mu.Unlock()

		m.markDirty()
		m.wg.Add(1)
		go m.resolve(ctx, job)
		return nil
	}

	m.enqueueLocked(it)
	m.mu.Unlock()

	m.markDirty()
	m.signal()
	return nil
}

// rereadLocked puts a job back to being resolved from its source, dropping
// whatever items it holds, and returns the context its extractor should run
// under. Caller holds mu, and must release it before starting the goroutine.
func (m *Manager) rereadLocked(job *Job) context.Context {
	job.restored = false
	job.unfetchable = false
	job.canceled = false
	job.Items = nil
	job.Err = ""
	job.resolving = true
	ctx, cancel := context.WithCancel(m.ctx)
	job.cancel = cancel
	return ctx
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
	it.speed = 0
	it.stallDefers = 0
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
func (m *Manager) itemNoteLocked(it *Item) string {
	if it.waitingFor == "" {
		return it.Note
	}
	limit, _ := m.hostGate.waiting(it.waitingFor)
	return waitingNote(it.waitingFor, limit)
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
	turns := it.overloadWaits
	m.enqueueLocked(it) // clears the note along with the rest; say why after
	it.overloadWaits = turns
	// The host, not the sentence: what it is taking is read afresh for every
	// snapshot, so this row and the hundred others behind the same host
	// never disagree about it.
	it.waitingFor = queued.host
	it.Note = waitingNote(queued.host, queued.limit)
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
	for _, id := range append([]string(nil), m.order...) {
		job := m.jobs[id]
		if job == nil || !job.status().Terminal() {
			continue
		}
		delete(m.jobs, id)
		m.order = util.Remove(m.order, id)
		removed++
	}
	m.mu.Unlock()

	if removed > 0 {
		m.markDirty()
	}
	return removed
}

// SetConcurrency resizes the worker pool. Shrinking it lets running
// transfers finish; only new starts are held back.
func (m *Manager) SetConcurrency(n int) error {
	// Something the user did, so they see it at once. See nudge.
	defer m.nudge()
	if n < 1 || n > config.MaxConcurrency {
		return fmt.Errorf("concurrency must be between 1 and %d", config.MaxConcurrency)
	}
	m.mu.Lock()
	m.limit = n
	m.mu.Unlock()

	m.markDirty()
	m.signal()
	return nil
}

// SetStreams caps how many connections a single slow file may be split
// across. Transfers already running keep the ceiling they started with.
func (m *Manager) SetStreams(n int) error {
	// Something the user did, so they see it at once. See nudge.
	defer m.nudge()
	if n < 1 || n > config.MaxStreams {
		return fmt.Errorf("streams must be between 1 and %d", config.MaxStreams)
	}
	m.mu.Lock()
	m.streams = n
	m.mu.Unlock()

	m.markDirty()
	return nil
}

// ErrNotFound is returned for an unknown job or item id.
var ErrNotFound = errors.New("not found")

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
	// Something the user did, so they see it at once. See nudge.
	defer m.nudge()
	dir, err := config.PrepareDir(path)
	if err != nil {
		return err
	}

	m.dirMu.Lock()
	changed := dir != m.dir
	m.dir = dir
	m.dirMu.Unlock()

	if changed {
		m.log.Info("download directory changed", "dir", dir)
		m.markDirty()
	}
	return nil
}

// SetPaused stops or resumes the whole queue.
//
// Pausing holds the connections open rather than dropping them: a running
// transfer parks inside its read, and the dispatcher stops handing out work.
// A long pause may still cost a connection to a server that times it out,
// which the usual retry and resume handle.
func (m *Manager) SetPaused(paused bool) {
	// Something the user did, so they see it at once. See nudge.
	defer m.nudge()
	m.throttle.setPaused(paused)
	if !paused {
		// Releasing the queue is the word a restored job was waiting for.
		m.resumeRestored()
		// Workers freed while paused left the queue untouched.
		m.signal()
	}
	m.markDirty()
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
	type pending struct {
		job *Job
		ctx context.Context
	}

	m.mu.Lock()
	var starting []pending
	for _, id := range m.order {
		job, ok := m.jobs[id]
		if !ok || !job.restored {
			continue
		}
		starting = append(starting, pending{job: job, ctx: m.rereadLocked(job)})
	}
	m.mu.Unlock()

	if len(starting) == 0 {
		return
	}
	for _, p := range starting {
		m.wg.Add(1)
		go m.resolve(p.ctx, p.job)
	}
	m.log.Info("resuming restored jobs", "jobs", len(starting))
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
	// Something the user did, so they see it at once. See nudge.
	defer m.nudge()
	if bytesPerSecond < 0 {
		return fmt.Errorf("speed limit cannot be negative, got %d", bytesPerSecond)
	}
	m.throttle.setLimit(bytesPerSecond)
	m.markDirty()
	return nil
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

// Snapshot renders the current state.
func (m *Manager) Snapshot() Snapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.snapshotLocked()
}

// snapshotLocked builds the wire state. Caller holds mu.
func (m *Manager) snapshotLocked() Snapshot {
	snap := Snapshot{
		Jobs:        make([]JobView, 0, len(m.order)),
		Concurrency: m.limit,
		MaxConcur:   config.MaxConcurrency,
		Streams:     m.streams,
		MaxStreams:  config.MaxStreams,
		Active:      m.running,
		Queued:      0,
		Paused:      m.throttle.isPaused(),
		SpeedLimit:  m.throttle.currentLimit(),
		DownloadDir: m.DownloadDir(),
		DiskFree:    m.diskFree.Load(),
		DiskTotal:   m.diskTotal.Load(),
		DiskMinFree: m.minFree,
		HostCount:   m.hostCount,
	}
	// Newest first: the job someone just added belongs at the top.
	for _, v := range slices.Backward(m.order) {
		job, ok := m.jobs[v]
		if !ok {
			continue
		}
		v := job.view(m.itemNoteLocked)
		snap.Speed += v.Speed
		if v.Held {
			snap.Held++
			snap.Jobs = append(snap.Jobs, v)
			continue
		}
		for _, it := range v.Items {
			if it.Status == StatusQueued {
				snap.Queued++
			}
		}
		snap.Jobs = append(snap.Jobs, v)
	}
	return snap
}

// Subscribe returns a channel of serialised snapshots and an unsubscribe
// function. The channel is closed when the manager shuts down.
//
// A subscriber arriving after Close gets a channel that is closed already.
// The alternative — one nobody will ever close — would hold an event stream
// open through shutdown: http.Server.Shutdown waits for in-flight requests
// without cancelling their contexts, so a browser reloading in the moment
// between the manager closing and the listener closing would turn a clean
// exit into a wait for the deadline and an error.
// subscriber is one open stream. stale means it is owed a whole snapshot
// rather than a patch, because it has just arrived or because a frame was
// dropped on the way to it.
type subscriber struct{ stale bool }

// Subscribe registers for state snapshots and hands back the whole of the
// current one to send first.
//
// The initial frame comes from here rather than from the caller so that it
// is the same act as registering: a subscriber that took its own snapshot
// afterwards was sent the whole queue twice, once by itself and once by the
// first broadcast finding it with nothing to merge into.
//
// A patch that arrives before this frame is written is harmless. It carries
// the rows that changed up to the broadcast before it, and this snapshot is
// at least that new, so applying it afterwards lands on the same values.
func (m *Manager) Subscribe() (<-chan []byte, []byte, func()) {
	ch := make(chan []byte, 1)

	m.mu.Lock()
	snap := m.snapshotLocked()
	// Records that browsers have now been told this, so the next broadcast
	// is a patch rather than the whole queue over again. Without it the
	// first frame after a connection found every row unlike the nothing it
	// had been compared against, and sent the lot a second time.
	m.patchLocked(snap)

	m.subsMu.Lock()
	if m.closed {
		m.subsMu.Unlock()
		m.mu.Unlock()
		close(ch)
		return ch, nil, func() {}
	}
	m.subs[ch] = &subscriber{}
	// Everyone else is owed a whole frame: what they were last told is now
	// recorded as sent, and the difference between that and this snapshot
	// would otherwise go to nobody.
	for other, sub := range m.subs {
		if other != ch {
			sub.stale = true
		}
	}
	m.subsMu.Unlock()
	m.mu.Unlock()

	// Encoded outside the locks. The snapshot is a copy down to the item
	// values, so nothing it holds can change underneath this.
	initial, err := json.Marshal(snap)
	if err != nil {
		m.log.Error("marshal snapshot", "err", err)
		initial = nil
	}

	return ch, initial, func() {
		m.subsMu.Lock()
		if _, ok := m.subs[ch]; ok {
			delete(m.subs, ch)
			close(ch)
		}
		m.subsMu.Unlock()
	}
}

// broadcast samples progress and pushes state to subscribers.
func (m *Manager) broadcast() {
	defer m.wg.Done()

	ticker := time.NewTicker(config.ProgressTick)
	defer ticker.Stop()

	// When the last frame went out, so a queue that is running but not
	// moving can be refreshed on a slower beat than one that is.
	var lastFrame time.Time

	for {
		select {
		case <-m.ctx.Done():
			return
		case now := <-ticker.C:
			m.sampleDisk(now)
			listening := m.hasSubscribers()

			m.mu.Lock()
			active := m.running > 0
			moved := false
			if active {
				moved = m.sampleLocked(now)
			}
			// With nobody subscribed there is no one to build a snapshot
			// for: a headless run polls Snapshot itself, and a browser that
			// connects later is sent the state as its first frame. The
			// rates above are still sampled, since the terminal reads
			// them, and dirty is left standing for whoever arrives next.
			// What is saved is encoding every item of every job — tens of
			// thousands of them on a large listing — twice a second for no
			// reader.
			if !listening {
				m.mu.Unlock()
				continue
			}
			// One frame a second, whatever is happening. The tick above is
			// a sampling rate — rates are measured often so they read
			// smoothly — and a number on a screen is not worth redrawing
			// faster than this. A job being read for the first time marks
			// the state changed on every tick as its items arrive, and
			// without a ceiling here that alone put out two and a half
			// frames a second.
			since := now.Sub(lastFrame)
			if since < config.FrameInterval {
				m.mu.Unlock()
				continue
			}
			// Read rather than taken: a frame that is not sent must not
			// swallow the change that would have justified the next one.
			dirty := m.dirty.Load()
			if !dirty && !active {
				m.mu.Unlock()
				continue
			}
			// Running, but nothing actually moving: a queue held behind a
			// host taking one download at a time spends most of its life
			// here. It still wants refreshing, because the rates decay
			// towards zero and a viewer should see that, but far less
			// often.
			if !dirty && !moved && since < config.IdleFrameInterval {
				m.mu.Unlock()
				continue
			}
			m.dirty.Store(false)
			lastFrame = now
			m.frameLocked()
		case <-m.urgent:
			// Straight past the ceiling: this is a person waiting to see
			// what they just did. The frame is a patch like any other, so
			// it costs the rows the action touched and nothing more.
			//
			// Nudges that arrive together are one frame. Pasting a list of
			// links is an Add per link, and each would otherwise build a
			// snapshot of the whole queue under the lock — a burst of them
			// back to back is the ceiling not applying at all. Waiting a
			// moment folds the rest in, which nobody watching can tell
			// apart from at once.
			settle := time.NewTimer(config.NudgeCoalesce)
		drain:
			for {
				select {
				case <-m.urgent:
				case <-settle.C:
					break drain
				case <-m.ctx.Done():
					settle.Stop()
					return
				}
			}
			if !m.hasSubscribers() {
				continue
			}
			m.mu.Lock()
			m.dirty.Store(false)
			lastFrame = time.Now()
			m.frameLocked()
		}
	}
}

// sampleDisk refreshes the destination's free space, at its own cadence.
//
// Deliberately outside mu: Statfs is a syscall, the destination may be a
// network mount, and the locking rule here is that mu is never held across
// anything that can block. Only the broadcaster calls this, so the timestamp
// needs no guarding of its own; the figures do, since /api/state reads them
// from whichever goroutine asked.
//
// A change is published in its own right. A disk filling up from somewhere
// else is exactly what this number exists to show, and an idle queue would
// otherwise never mention it — while a disk that is not moving marks nothing
// dirty, so an idle queue stays quiet.
func (m *Manager) sampleDisk(now time.Time) {
	if !m.diskSampled.IsZero() && now.Sub(m.diskSampled) < config.DiskSampleInterval {
		return
	}
	m.diskSampled = now

	free, total, err := diskSpace(m.DownloadDir())
	if err != nil {
		// A destination that cannot be measured reports nothing rather than
		// a zero, which reads as a disk with no room left.
		free, total = 0, 0
	}
	// Both swaps, every time: || would short-circuit past the second and
	// leave the total behind whenever the free figure had moved.
	freeMoved := m.diskFree.Swap(free) != free
	totalMoved := m.diskTotal.Swap(total) != total
	if freeMoved || totalMoved {
		m.markDirty()
	}
}

// sampleLocked refreshes the per-item transfer rate. Caller holds mu.
// sampleLocked refreshes every running item's rate, and reports whether any
// of them actually moved a byte since the last sample.
//
// That answer is what separates a queue that is working from one that is
// merely open. A thousand items waiting behind a throttled host look exactly
// like a thousand items downloading, from the broadcaster's side: transfers
// are running either way. Only the byte counters tell the two apart, and
// there is nothing worth sending twice a second about the second one.
func (m *Manager) sampleLocked(now time.Time) bool {
	moved := false
	for _, job := range m.jobs {
		for _, it := range job.Items {
			if it.Status != StatusRunning {
				it.speed = 0
				continue
			}
			elapsed := now.Sub(it.lastSample).Seconds()
			if elapsed <= 0 {
				continue
			}
			current := it.downloaded.Load()
			if current != it.lastBytes {
				moved = true
			}
			instant := float64(current-it.lastBytes) / elapsed
			if instant < 0 {
				instant = 0
			}
			// Exponential smoothing: readable numbers without lagging a
			// genuine change in rate.
			if it.speed == 0 {
				it.speed = instant
			} else {
				it.speed = config.SpeedSmoothing*it.speed + (1-config.SpeedSmoothing)*instant
			}
			it.lastBytes, it.lastSample = current, now
		}
	}
	return moved
}

// patchLocked reduces a snapshot to the rows that have changed since the
// last frame went out.
//
// Almost nothing in a queue changes from one second to the next. A thousand
// finished files say exactly what they said before, and a browser that has
// them already needs to be told about the four that moved — which is the
// difference between tens of kilobytes a second and a fraction of one.
// Everything outside the item lists is small and always sent, so a frame
// still carries the totals, the rates and every job's own state whole.
//
// A job whose item list has changed length is sent whole instead: items may
// have gone as well as arrived — a job re-read after a restart replaces its
// list outright — and a patch cannot say that. Caller holds mu, and this
// must be called only on the broadcast path: it records what browsers have
// been told, and a snapshot read by the terminal or by /api/state tells
// them nothing.
func (m *Manager) patchLocked(snap Snapshot) Snapshot {
	out := snap
	out.Jobs = make([]JobView, len(snap.Jobs))
	for i, view := range snap.Jobs {
		job, ok := m.jobs[view.ID]
		if !ok || len(job.Items) != len(view.Items) {
			out.Jobs[i] = view
			continue
		}

		shrank := len(view.Items) < job.lastCount
		job.lastCount = len(view.Items)

		changed := make([]ItemView, 0, 8)
		for k, iv := range view.Items {
			if it := job.Items[k]; it.lastView != iv {
				it.lastView = iv
				changed = append(changed, iv)
			}
		}
		// Whole when a merge could not say what happened. A list that got
		// shorter has lost rows, and a merge only ever adds or replaces
		// them. A list where *every* row is new is either the first sight
		// of this job or its contents replaced wholesale — a re-read drops
		// the items and resolves them again, which can land between two
		// frames and leave the count unchanged while nothing else is.
		//
		// A list that merely grew is a patch: the new rows are the changed
		// ones, and the client appends what it does not recognise. That is
		// the common case while a large album resolves, which is exactly
		// when a whole list is most expensive to send.
		if shrank || len(changed) == len(view.Items) {
			out.Jobs[i] = view
			continue
		}
		view.Items = changed
		view.Patch = true
		out.Jobs[i] = view
	}
	return out
}

// frameLocked builds a frame and sends it. Called with mu held; releases
// it before publishing, so encoding happens outside mu.
//
// subsMu is taken before mu is let go. Otherwise a Subscribe could land in
// the gap, record a newer snapshot as sent and hand it out, and then receive
// this older patch after it — rolling rows back, with every later patch a
// diff against the newer state, so they would stay rolled back.
func (m *Manager) frameLocked() {
	snap := m.snapshotLocked()
	patch := m.patchLocked(snap)
	m.subsMu.Lock()
	m.mu.Unlock()
	defer m.subsMu.Unlock()
	m.publishLocked(snap, patch)
}

// nudge asks for a frame now rather than on the next beat. For the user's
// own actions only: anything that happens by itself — bytes arriving, a
// transfer finishing — goes out at the ordinary rate, which is the rate that
// keeps the stream cheap.
//
// It asks only when something is waiting to be told. Every action marks the
// state changed itself when it changed anything, so an action that was
// refused or changed nothing — an unknown id, a setting set to what it was —
// costs no frame at all, now or on the next beat.
func (m *Manager) nudge() {
	if !m.dirty.Load() {
		return
	}
	select {
	case m.urgent <- struct{}{}:
	default:
	}
}

// hasSubscribers reports whether anyone is waiting on the event stream.
func (m *Manager) hasSubscribers() bool {
	m.subsMu.Lock()
	defer m.subsMu.Unlock()
	return len(m.subs) > 0
}

// publishLocked sends a payload to every subscriber, replacing any snapshot
// a slow client has not read yet: only the newest state is worth delivering.
// Caller holds subsMu.
func (m *Manager) publishLocked(full, patch Snapshot) {
	// Encoded at most once each, and only if somebody is owed that kind of
	// frame. The usual case is every browser taking the patch.
	var fullPayload, patchPayload []byte
	encode := func(snap Snapshot, into *[]byte) []byte {
		if *into == nil {
			payload, err := json.Marshal(snap)
			if err != nil {
				m.log.Error("marshal snapshot", "err", err)
				payload = []byte{}
			}
			*into = payload
		}
		return *into
	}

	for ch, sub := range m.subs {
		payload := encode(patch, &patchPayload)
		if sub.stale {
			payload = encode(full, &fullPayload)
		}
		if len(payload) == 0 {
			continue
		}
		// A frame dropped for a slow reader takes its changes with it, so
		// that subscriber is owed a whole snapshot next time. This is the
		// one thing a patch stream cannot recover from on its own, and the
		// only reason any of this stays self-healing.
		sub.stale = m.deliverLocked(ch, payload)
	}
}

// deliverLocked hands a payload to one subscriber, replacing any snapshot it
// has not read yet: only the newest state is worth delivering. It reports
// whether it had to replace one, which is the subscriber having missed
// whatever that frame carried. Caller holds subsMu.
func (m *Manager) deliverLocked(ch chan []byte, payload []byte) bool {
	select {
	case ch <- payload:
		return false
	default:
	}
	// The buffer holds one frame. A client that has not read the last one
	// gets it dropped for this one — and is owed a whole snapshot after,
	// since what it missed is no longer coming.
	select {
	case <-ch:
	default:
	}
	select {
	case ch <- payload:
	default:
	}
	return true
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
