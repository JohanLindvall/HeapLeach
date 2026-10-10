// SPDX-License-Identifier: MIT

package download

import (
	"encoding/json"
	"slices"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/config"
)

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
		Proxies:     m.proxyEnabled,
		Active:      m.running,
		Queued:      0,
		Paused:      m.throttle.isPaused(),
		SpeedLimit:  m.throttle.currentLimit(),
		DownloadDir: m.DownloadDir(),
		DiskFree:    m.diskFree.Load(),
		DiskTotal:   m.diskTotal.Load(),
		DiskMinFree: m.minFree,
		HostCount:   m.hostCount,
		Version:     m.version,
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

// subscriber is one open stream. stale means it is owed a whole snapshot
// because another subscriber's initial snapshot advanced the shared baseline.
type subscriber struct{ stale bool }

// Subscribe registers for state snapshots and hands back the whole of the
// current one to send first.
// A subscriber arriving after Close gets an already closed channel.
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
	dir := m.DownloadDir()
	if dir == m.diskDir && !m.diskSampled.IsZero() && now.Sub(m.diskSampled) < config.DiskSampleInterval {
		return
	}
	m.diskSampled = now
	m.diskDir = dir

	free, total, err := diskSpace(dir)
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
		// An idle queue has no worker completion to wake it after space is
		// freed. Reconsider dispatch when the sampled filesystem changes.
		m.signal()
	}
}

// sampleLocked refreshes every running item's rate, and reports whether any
// of them actually moved a byte since the last sample.
// Caller holds mu.
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
// A job whose list shrank or was replaced is sent whole: a patch cannot
// remove rows. New rows may be appended through patches. Caller holds mu, and this
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
		select {
		case ch <- payload:
		default:
			// Recover in this frame. The queue may now be idle, so there
			// need not be another broadcast to repair a dropped patch.
			select {
			case <-ch:
			default:
			}
			ch <- encode(full, &fullPayload)
		}
		sub.stale = false
	}
}
