// SPDX-License-Identifier: MIT

package download

import (
	"context"
	"errors"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/extractor"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
	"github.com/JohanLindvall/HeapLeach/internal/proxy"
)

// Only K2S is opted in. PerRoute is a future extension point, not permission
// to send other hosts through public proxies today.
func proxyEligible(it *Item) bool {
	return it.pace != nil && it.pace.PerRoute && it.pace.Group == "keep2share"
}

func (m *Manager) usesProxies(it *Item) bool {
	return m.proxyEnabled && m.proxies != nil && proxyEligible(it)
}

// updateProxyDemandLocked lets discovery replenish only when the runnable
// K2S queue needs routes. Other hosts, deferred items, a pause or full disk
// must not turn a quiet pool into a five-minute feed poller. Active proxy
// transfers count toward demand; workers occupied by other hosts do not.
func (m *Manager) updateProxyDemandLocked() {
	if m.proxies != nil {
		m.proxies.SetDemand("keep2share", m.proxyDemandLocked())
	}
}

func (m *Manager) proxyDemandLocked() int {
	if !m.proxyEnabled || m.closing || m.throttle.isPaused() || m.lowOnSpace() ||
		m.hostActive["group:keep2share"] > m.proxyRunning {
		return 0
	}
	capacity := max(0, m.limit-(m.running-m.proxyRunning))
	wanted := min(m.proxyRunning, capacity)
	now := time.Now()
	for _, it := range m.queue {
		if wanted >= capacity {
			break
		}
		if proxyEligible(it) && it.Status == StatusQueued && !it.inFlight && !it.notBefore.After(now) {
			wanted++
		}
	}
	return wanted
}

// ProxyPage samples active transfer speeds under mu, then reads the large
// inventory without holding the queue lock.
func (m *Manager) ProxyPage(q proxy.Query) proxy.Page {
	m.mu.Lock()
	pool := m.proxies
	speeds := make(map[string]float64)
	for _, job := range m.jobs {
		for _, it := range job.Items {
			if it.inFlight && it.route != nil {
				speeds[it.route.ID()] += it.speed
			}
		}
	}
	m.mu.Unlock()
	if pool == nil {
		return proxy.EmptyPage(q)
	}
	page := pool.Page("keep2share", q)
	for i := range page.Rows {
		page.Rows[i].CurrentSpeed = speeds[page.Rows[i].ID]
	}
	return page
}

type routeGroup struct {
	name  string
	files int
}

// leaseRouteLocked is nonblocking: a busy/cooling route keeps the item in the
// queue, leaving workers free for other hosts. Called only at dispatch.
func (m *Manager) leaseRouteLocked(it *Item, unavailable map[routeGroup]bool) bool {
	if !m.usesProxies(it) {
		return true
	}
	if it.route != nil {
		return true
	}
	group := routeGroup{it.pace.Group, it.pace.Files}
	if unavailable[group] {
		return false
	}
	it.route = m.proxies.AcquireFor(it.pace.Group, it.preferredRoute, it.pace.Files, it.Size-it.downloaded.Load())
	if it.route == nil {
		// A large queue must not rescan the whole inventory for every file
		// while every route in the same service is already busy/cooling.
		unavailable[group] = true
		it.Note = "Waiting for an available download route"
		return false
	}
	it.preferredRoute = it.route.ID()
	return true
}

func (m *Manager) transferRouted(ctx context.Context, it *Item) error {
	m.mu.Lock()
	lease := it.route
	m.mu.Unlock()
	if lease == nil {
		return m.transfer(ctx, it)
	}
	start := time.Now()
	var err error
	for {
		attempt, abort := context.WithCancel(ctx)
		m.mu.Lock()
		it.routeCancel, it.routeResumable = abort, false
		if m.throttle.isPaused() || m.throttle.currentLimit() > 0 {
			lease.Constrain()
		}
		m.mu.Unlock()
		err = m.transfer(lease.Context(attempt), it)
		abort()
		m.mu.Lock()
		upgrade := it.routeUpgrade
		it.routeUpgrade, it.routeCancel = nil, nil
		retry := upgrade != nil && ctx.Err() == nil && errors.Is(err, context.Canceled)
		switchRoute := retry && m.proxyEnabled && upgrade.Configured() &&
			!m.throttle.isPaused() && m.throttle.currentLimit() == 0
		if switchRoute {
			it.route, it.preferredRoute = upgrade, upgrade.ID()
			it.speed, it.proxyRetries = 0, 0
			it.lastBytes, it.lastSample = it.downloaded.Load(), time.Now()
		}
		m.mu.Unlock()
		if !switchRoute {
			if upgrade != nil {
				upgrade.Release()
			}
			if retry {
				// Settings changed after sampling reserved a replacement.
				// Resume on the original lease instead of turning our own
				// optimization cancellation into a user-visible failure.
				continue
			}
			break
		}
		// transfer has closed the response and flushed the part file before
		// releasing the old address or minting a ticket on the new one.
		lease.Release()
		lease, start = upgrade, time.Now()
	}
	if ctx.Err() == nil {
		if stall, ok := errors.AsType[*stalledError](err); ok {
			lease.Failed(err, time.Since(start), true)
			err = &routeTransferError{err: &httpx.RouteError{Err: err}, moved: stall.moved}
		} else if _, busy := errors.AsType[*busyHostError](err); busy {
			lease.Failed(err, time.Since(start), false)
		}
		if wait, ok := errors.AsType[*extractor.WaitError](err); ok {
			lease.Cooldown(wait.Until)
			// Only this address is waiting. The next dispatch may use a
			// different one immediately, or wait in the queue for the pool.
			err = &extractor.WaitError{Until: time.Now(), Reason: "Waiting for an available download route"}
		} else if _, routed := errors.AsType[*httpx.RouteError](err); routed {
			m.mu.Lock()
			it.preferredRoute = ""
			progress, _ := errors.AsType[*routeTransferError](err)
			moved := progress != nil && progress.moved
			if moved {
				it.proxyRetries = 0
			}
			if moved || it.proxyRetries < m.cfg.ProxyRetries {
				if !moved {
					it.proxyRetries++
				}
				err = &extractor.WaitError{Until: time.Now(), Reason: "Retrying through another download route"}
			}
			m.mu.Unlock()
		}
	}
	lease.Release()
	// runItem clears the route together with its dispatch accounting.
	return err
}

// sampleProxyLocked feeds useful-byte windows into the route score. A
// replacement is reserved before aborting the current attempt, and only a
// confirmed range response permits moving a healthy transfer mid-file.
func (m *Manager) sampleProxyLocked(it *Item, now time.Time, bytes int64, elapsed time.Duration, limited bool) {
	if it.route == nil {
		return
	}
	if !it.route.Progress(now, bytes, elapsed, limited) || limited || !m.proxyEnabled ||
		!it.routeResumable || it.routeCancel == nil || it.routeUpgrade != nil || m.lowOnSpace() {
		return
	}
	if next := it.route.Upgrade(now, it.Size-it.downloaded.Load(), it.speed); next != nil {
		it.routeUpgrade = next
		it.Note = "Switching to a faster K2S route"
		it.routeCancel()
	}
}

func (m *Manager) constrainProxyMeasurementsLocked() {
	if !m.throttle.isPaused() && m.throttle.currentLimit() == 0 {
		return
	}
	for _, job := range m.jobs {
		for _, it := range job.Items {
			if it.route != nil {
				it.route.Constrain()
			}
		}
	}
}

// A resumed prefix already on disk is not progress from this attempt.
type routeTransferError struct {
	err   error
	moved bool
}

func (e *routeTransferError) Error() string { return e.err.Error() }
func (e *routeTransferError) Unwrap() error { return e.err }
