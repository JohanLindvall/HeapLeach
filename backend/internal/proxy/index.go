// SPDX-License-Identifier: MIT

package proxy

import (
	"math/rand/v2"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/config"
)

// inventory indexes the routes for ranking, which runs on every lease and,
// for every running transfer, every ProxyUpgradeInterval. Walking a public
// feed's tens of thousands of routes each time was most of the service's CPU,
// nearly all of it spent on routes that could not be chosen: dead, cooling,
// or never measured where only measured routes count.
//
// It is rebuilt when routes are added or removed, and only then. A route's
// first speed adds it to measured where that speed is written (noteMeasured),
// which is the only way a speed leaves zero. Nothing else in it can go stale:
// cooldowns, leases and speeds are read from the routes themselves whenever
// one is considered, so measured may hold a route whose speed has since gone
// back to zero, never miss one that has one.
type inventory struct {
	valid    bool
	all      []*entry          // every route, for uniform draws
	byHost   [][]*entry        // by Pool.hostID: every port and protocol on one address
	byID     map[string]*entry // by routeID
	measured []*entry          // routes with a speed on some site
}

// index returns the inventory index, rebuilt if routes were added or removed
// since it was built. Caller holds mu.
func (p *Pool) index() *inventory {
	ix := &p.idx
	if ix.valid && len(ix.all) == len(p.entries) {
		return ix
	}
	ix.all = make([]*entry, 0, len(p.entries))
	ix.byID = make(map[string]*entry, len(p.entries))
	ix.measured = ix.measured[:0]
	for _, e := range p.entries {
		p.hostID(e)
		ix.all = append(ix.all, e)
		ix.byID[e.id()] = e
		e.inMeasured = false
		for _, s := range e.Sites {
			if s.BytesPerSecond != 0 {
				e.inMeasured = true
				ix.measured = append(ix.measured, e)
				break
			}
		}
	}
	ix.byHost = make([][]*entry, len(p.hostIDs)+1)
	for _, e := range ix.all {
		ix.byHost[e.hostID] = append(ix.byHost[e.hostID], e)
	}
	ix.valid = true
	return ix
}

// noteMeasured registers a route whose speed on some site has just left
// zero. Caller holds mu.
func (p *Pool) noteMeasured(e *entry) {
	if p.idx.valid && !e.inMeasured && p.entries[e.URL] == e {
		e.inMeasured = true
		p.idx.measured = append(p.idx.measured, e)
	}
}

// eligible reports whether route e could take a transfer for site now. It is
// the rule available applies to the whole inventory at once, asked of one
// route: alive, enabled, and its address neither cooling for the site (nor,
// for WAF recovery, after a challenge) nor at the site's per-address limit.
// Caller holds mu, and ix is p.index().
func (p *Pool) eligible(ix *inventory, e *entry, site string, files int, now time.Time, waf bool) bool {
	if e.Until.After(now) || !p.enabled(e) {
		return false
	}
	active := 0
	for _, o := range ix.byHost[e.hostID] {
		if s := o.Sites[site]; s != nil {
			if s.Until.After(now) {
				return false
			}
			active += s.active
		}
		if waf && o.WAF.Until.After(now) {
			return false
		}
	}
	return active < max(1, files)
}

// drawUnmeasured draws one route uniformly from those not measured for site
// that could take the transfer: selection's single exploration ticket.
// Drawing from the whole inventory and drawing again until a route qualifies
// is exactly uniform over the routes that do, and costs a few draws while
// many do. When few do, ProxyDrawTries misses in a row fall back to listing
// them, which is just as uniform. Caller holds mu, and ix is p.index().
func (p *Pool) drawUnmeasured(ix *inventory, site string, files int, now time.Time, waf bool) *entry {
	if len(ix.all) == 0 {
		return nil
	}
	for range config.ProxyDrawTries {
		if e := ix.all[rand.IntN(len(ix.all))]; e.speed(site) == 0 && p.eligible(ix, e, site, files, now, waf) {
			return e
		}
	}
	var untried []*entry
	for _, e := range p.available(site, files, now, waf) {
		if e.speed(site) == 0 {
			untried = append(untried, e)
		}
	}
	if len(untried) == 0 {
		return nil
	}
	return untried[rand.IntN(len(untried))]
}
