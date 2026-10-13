// SPDX-License-Identifier: MIT

package proxy

import (
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/config"
)

// A route's exit is the public address its requests leave by, which is what
// a host counting free downloads per address counts. It need not be the
// address the proxy listens on: a gateway's ports leave by addresses of
// their own, a few of its ports share one, and one exit has been seen behind
// two gateways. So no rule about listening addresses can keep two downloads
// off one exit, and two on one is a refused free download.
//
// The downloader asks each lease's route for its exit before the host sees
// the route, and claims it (ClaimExit): one lease per exit per service. A
// route whose known exit is claimed, or cooling for the service, is not
// leased at all, and a host's cooldown met through one route is the exit's.

type exitSite struct {
	exit, site string
}

// exitKnown reports e's exit, when it was found recently enough to go by:
// a gateway may move a port to another exit. Caller holds the pool's lock.
func (e *entry) exitKnown(now time.Time) (string, bool) {
	if e.Exit == "" || now.Sub(e.ExitAt) >= config.ProxyExitTTL {
		return "", false
	}
	return e.Exit, true
}

// exitFree reports whether e's known exit, if it has one, could take a
// lease for site: neither held by as many leases as the site allows an
// address, nor cooling. A route whose exit is not known yet is free as far as
// its exit goes. Caller holds mu.
func (p *Pool) exitFree(e *entry, site string, files int, now time.Time) bool {
	exit, known := e.exitKnown(now)
	if !known {
		return true
	}
	key := exitSite{exit, site}
	return p.exitLeases[key] < max(1, files) && !p.exitCooling[key].After(now)
}

// ClaimExit records exit as the public address the lease's route leaves by,
// and claims it for the lease. It refuses when other leases hold that exit
// for the service, as many as it allows an address, or the service's host
// has it cooling. The caller then gives the route back, and the pool passes
// over the route while that lasts.
func (l *Lease) ClaimExit(exit string) bool {
	p := l.pool
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed || l.released {
		return false
	}
	if exit == l.exit {
		return true
	}
	now, e := time.Now(), l.entry
	e.Exit, e.ExitAt = exit, now
	if p.dirty != nil {
		p.dirty[e.URL] = true
	}
	key := exitSite{exit, l.site}
	if p.exitLeases[key] >= max(1, l.files) || p.exitCooling[key].After(now) {
		return false
	}
	p.unclaimExit(l)
	if p.exitLeases == nil {
		p.exitLeases = make(map[exitSite]int)
	}
	p.exitLeases[key]++
	l.exit = exit
	return true
}

// Claimed reports whether the lease holds its route's exit.
func (l *Lease) Claimed() bool {
	l.pool.mu.Lock()
	defer l.pool.mu.Unlock()
	return l.exit != ""
}

// unclaimExit gives up the exit the lease holds, if it holds one. Caller
// holds mu.
func (p *Pool) unclaimExit(l *Lease) {
	if l.exit == "" {
		return
	}
	key := exitSite{l.exit, l.site}
	if p.exitLeases[key] > 1 {
		p.exitLeases[key]--
	} else {
		delete(p.exitLeases, key)
	}
	l.exit = ""
}

// coolExit holds back the exit the lease holds until until, for the lease's
// service: a host's timer or refusal met through one route is meant for the
// address it saw. Caller holds mu.
func (p *Pool) coolExit(l *Lease, until time.Time) {
	if l.exit == "" {
		return
	}
	p.coolExitUntil(exitSite{l.exit, l.site}, until)
}

func (p *Pool) coolExitUntil(key exitSite, until time.Time) {
	if !until.After(p.exitCooling[key]) {
		return
	}
	if p.exitCooling == nil {
		p.exitCooling = make(map[exitSite]time.Time)
	}
	p.exitCooling[key] = until
}

// seedExitCooling restores the exits' cooldowns from the routes read back
// from the database: each route keeps its own, and its exit's is the longest
// among the routes known to leave by it. Caller holds mu, or owns the pool.
func (p *Pool) seedExitCooling(now time.Time) {
	for _, e := range p.entries {
		exit, known := e.exitKnown(now)
		if !known {
			continue
		}
		for site, s := range e.Sites {
			if s.Until.After(now) {
				p.coolExitUntil(exitSite{exit, site}, s.Until)
			}
		}
	}
}
