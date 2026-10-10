// SPDX-License-Identifier: MIT

package download

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/extractor"
	"github.com/JohanLindvall/HeapLeach/internal/proxy"
	"github.com/JohanLindvall/HeapLeach/internal/util"
)

// challengePace preserves connection limits and existing service admission.
// Every other host can opt into the shared Cloudflare fallback after a
// challenge, without needing a hostname registration.
func challengePace(original *extractor.Pace) *extractor.Pace {
	var pace extractor.Pace
	if original != nil {
		pace = *original
	}
	if !pace.PerRoute || !slices.Contains(proxy.Services(), pace.Group) {
		pace.Group = "cloudflare"
	}
	pace.PerRoute, pace.Files = true, max(1, pace.Files)
	pace.WAF = true
	return &pace
}

// recoverExtraction retries the whole source operation, so a failure in
// the middle of a multi-request protocol cannot move its cookies or signed
// URLs to another address. Successful files refresh their source once their
// own worker has a lease; Registry installs that resolver.
func (m *Manager) recoverExtraction(ctx context.Context, read func(context.Context) (*extractor.Result, error), cause error) (*extractor.Result, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !retryRouteFailure(cause) {
		return nil, cause
	}
	m.mu.Lock()
	pool, enabled := m.proxies, m.proxyEnabled
	m.mu.Unlock()
	if !enabled || pool == nil {
		return nil, cause
	}
	m.mu.Lock()
	m.proxyResolving++
	m.updateProxyDemandLocked()
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		m.proxyResolving--
		m.updateProxyDemandLocked()
		m.mu.Unlock()
	}()
	pool.RefusedDirect("cloudflare", cause, 0)
	for attempt, retries := 2, 0; ; attempt++ {
		attemptCtx := extractor.WithResolveNote(ctx, func(note string) {
			extractor.ResolveNote(ctx, connectionNote(note, attempt))
		})
		extractor.ResolveNote(attemptCtx, "Waiting for an available proxy")
		lease, err := m.challengeLease(attemptCtx, pool)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, fmt.Errorf("%w: %v", cause, err)
		}
		start := time.Now()
		res, err := read(lease.Context(attemptCtx))
		if _, refused := errors.AsType[*extractor.RefusedError](err); refused && ctx.Err() == nil {
			lease.Failed(err, time.Since(start), false)
		}
		lease.Release()
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if err == nil {
			if res != nil {
				for i := range res.Files {
					res.Files[i].Pace = challengePace(res.Files[i].Pace)
				}
			}
			return res, nil
		}
		cause = err
		if !retryRouteFailure(err) {
			return nil, err
		}
		// Match file downloads: unreachable/dropped/refused proxy routes
		// do not exhaust the file's budget before a usable address is found.
		if !routeAtFault(err) {
			if retries >= m.cfg.ProxyRetries {
				return nil, cause
			}
			retries++
		}
	}
}

func connectionNote(note string, attempt int) string {
	if note != "" && attempt > 1 {
		return fmt.Sprintf("%s — connection attempt %d", note, attempt)
	}
	return note
}

// Source discovery has no queued Item yet. Wait for a lease within the
// normal request timeout; a canceled job or disabled pool stops the wait.
func (m *Manager) challengeLease(ctx context.Context, pool *proxy.Pool) (*proxy.Lease, error) {
	timeout := m.cfg.Timeout
	if timeout <= 0 {
		timeout = config.DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("waiting for a proxy recovery route: %w", err)
		}
		m.mu.Lock()
		enabled := m.proxyEnabled
		m.mu.Unlock()
		if !enabled {
			return nil, errors.New("proxy routing is disabled")
		}
		if lease := pool.Acquire("cloudflare", "", 1); lease != nil {
			return lease, nil
		}
		if err := util.SleepCtx(ctx, config.ProxyDispatchTick); err != nil {
			return nil, err
		}
	}
}
