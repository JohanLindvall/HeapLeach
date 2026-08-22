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
	"sync"
	"sync/atomic"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/extractor"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
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

	wake chan struct{}
	ctx  context.Context
	stop context.CancelFunc
	wg   sync.WaitGroup

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

	subsMu    sync.Mutex
	subs      map[chan []byte]struct{}
	closeOnce sync.Once

	// dirty records a state change worth pushing to subscribers even though
	// nothing is transferring. It is taken rather than peeked at and cleared
	// separately, so a change landing while a snapshot is being built sets
	// the flag again and is published on the next tick instead of being lost.
	dirty atomic.Bool
}

// New builds a Manager. Call Start before use.
func New(cfg *config.Config, reg *extractor.Registry, client *httpx.Client, log *slog.Logger) *Manager {
	ctx, stop := context.WithCancel(context.Background())
	return &Manager{
		cfg:      cfg,
		reg:      reg,
		client:   client.Streaming(),
		log:      log,
		jobs:     make(map[string]*Job),
		parts:    make(map[string]struct{}),
		throttle: newThrottle(cfg.SpeedLimit),
		limit:    cfg.Concurrency,
		streams:  cfg.Streams,
		timings:  defaultDownloadTimings(),
		hosts:    newHostLimiter(config.MaxConnectionsPerHost),
		wake:     make(chan struct{}, 1),
		ctx:      ctx,
		stop:     stop,
		subs:     make(map[chan []byte]struct{}),
	}
}

// Start launches the dispatcher and the progress broadcaster.
func (m *Manager) Start() {
	m.wg.Add(2)
	go m.dispatch()
	go m.broadcast()
}

// Close cancels every in-flight transfer, waits for the workers to exit and
// releases every subscriber. It is safe to call more than once, so callers
// can both defer it and call it explicitly at the right moment.
func (m *Manager) Close() {
	m.closeOnce.Do(func() {
		m.stop()
		m.wg.Wait()

		m.subsMu.Lock()
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
			if itemHeld != nil {
				itemHeld(it, true)
			}
			it.Status = StatusRunning
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

// nextLocked pops the next runnable item, skipping ones cancelled while
// they sat in the queue. Caller holds mu.
func (m *Manager) nextLocked() *Item {
	// A paused queue starts nothing new. Transfers already running park
	// inside their reads instead, so they keep their place in the file.
	if m.throttle.isPaused() {
		return nil
	}
	for m.running < m.limit && len(m.queue) > 0 {
		it := m.queue[0]
		m.queue = m.queue[1:]
		// inFlight guards against handing out an item a worker still owns.
		if it.Status == StatusQueued && !it.inFlight {
			return it
		}
	}
	return nil
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
	case ctx.Err() != nil || httpx.IsCanceled(err):
		it.Status = StatusCanceled
		it.Err = ""
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
	m.mu.Unlock()

	m.markDirty()
	m.signal()
}

// CancelJob stops a job and everything under it.
func (m *Manager) CancelJob(id string) error {
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
	m.mu.Lock()
	job, ok := m.jobs[id]
	if !ok {
		m.mu.Unlock()
		return ErrNotFound
	}
	job.canceled = false

	if len(job.Items) == 0 {
		// The extractor itself failed; run it again.
		job.Err = ""
		job.resolving = true
		ctx, cancel := context.WithCancel(m.ctx)
		job.cancel = cancel
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
	if job := m.jobs[jobID]; job != nil {
		job.canceled = false
	}
	m.enqueueLocked(it)
	m.mu.Unlock()

	m.markDirty()
	m.signal()
	return nil
}

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
	it.speed = 0
	it.startedAt = time.Time{}
	it.finishedAt = time.Time{}
	it.downloaded.Store(0)
	it.lastBytes = 0
	m.queue = append(m.queue, it)
}

// RemoveJob cancels a job and forgets it. Files already on disk stay.
func (m *Manager) RemoveJob(id string) error {
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

// SetPaused stops or resumes the whole queue.
//
// Pausing holds the connections open rather than dropping them: a running
// transfer parks inside its read, and the dispatcher stops handing out work.
// A long pause may still cost a connection to a server that times it out,
// which the usual retry and resume handle.
func (m *Manager) SetPaused(paused bool) {
	m.throttle.setPaused(paused)
	if !paused {
		// Workers freed while paused left the queue untouched.
		m.signal()
	}
	m.markDirty()
}

// Paused reports whether the queue is held.
func (m *Manager) Paused() bool { return m.throttle.isPaused() }

// SetSpeedLimit caps total throughput in bytes per second. Zero is
// unlimited. The cap is shared: it bounds everything moving at once, not
// each transfer separately.
func (m *Manager) SetSpeedLimit(bytesPerSecond int64) error {
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
		DownloadDir: m.cfg.DownloadDir,
		HostCount:   len(m.reg.Hosts()),
	}
	// Newest first: the job someone just added belongs at the top.
	for i := len(m.order) - 1; i >= 0; i-- {
		job, ok := m.jobs[m.order[i]]
		if !ok {
			continue
		}
		v := job.view()
		snap.Speed += v.Speed
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
func (m *Manager) Subscribe() (<-chan []byte, func()) {
	ch := make(chan []byte, 1)

	m.subsMu.Lock()
	m.subs[ch] = struct{}{}
	m.subsMu.Unlock()

	return ch, func() {
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

	for {
		select {
		case <-m.ctx.Done():
			return
		case now := <-ticker.C:
			m.mu.Lock()
			active := m.running > 0
			if active {
				m.sampleLocked(now)
			}
			if !m.dirty.Swap(false) && !active {
				m.mu.Unlock()
				continue
			}
			snap := m.snapshotLocked()
			m.mu.Unlock()

			payload, err := json.Marshal(snap)
			if err != nil {
				m.log.Error("marshal snapshot", "err", err)
				continue
			}
			m.publish(payload)
		}
	}
}

// sampleLocked refreshes the per-item transfer rate. Caller holds mu.
func (m *Manager) sampleLocked(now time.Time) {
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
}

// publish sends a payload to every subscriber, replacing any snapshot a slow
// client has not read yet: only the newest state is worth delivering.
func (m *Manager) publish(payload []byte) {
	m.subsMu.Lock()
	defer m.subsMu.Unlock()
	for ch := range m.subs {
		select {
		case ch <- payload:
		default:
			select {
			case <-ch:
			default:
			}
			select {
			case ch <- payload:
			default:
			}
		}
	}
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
