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

// SetProxyPool installs the process's shared pool before Start. The caller
// closes it after the manager, so no in-flight request outlives its database.
func (m *Manager) SetProxyPool(pool *proxy.Pool) { m.proxies = pool }

func (m *Manager) usesProxies(it *Item) bool {
	return m.proxies != nil && it.pace != nil && it.pace.PerRoute && it.pace.Group != ""
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
	it.route = m.proxies.Acquire(it.pace.Group, it.preferredRoute, it.pace.Files)
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
	err := m.transfer(lease.Context(ctx), it)
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
	m.mu.Lock()
	it.route = nil
	m.mu.Unlock()
	return err
}

// A resumed prefix already on disk is not progress from this attempt.
type routeTransferError struct {
	err   error
	moved bool
}

func (e *routeTransferError) Error() string { return e.err.Error() }
func (e *routeTransferError) Unwrap() error { return e.err }
