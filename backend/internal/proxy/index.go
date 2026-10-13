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
	// leases counts each address's leases per site, recounted from the
	// routes when the index is built and kept by lease and Release, the
	// only places a lease begins or ends.
	leases map[addressSite]int
	// cooling remembers an address found cooling for a site, until the
	// cooldown it found. See addressCooling.
	cooling map[addressCooldown]time.Time
}

type addressSite struct {
	host int
	site string
}

type addressCooldown struct {
	addressSite
	waf bool
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
	ix.leases = make(map[addressSite]int)
	ix.cooling = make(map[addressCooldown]time.Time)
	for _, e := range ix.all {
		ix.byHost[e.hostID] = append(ix.byHost[e.hostID], e)
		for site, s := range e.Sites {
			if s.active > 0 {
				ix.leases[addressSite{e.hostID, site}] += s.active
			}
		}
	}
	ix.valid = true
	return ix
}

// leaseAddress and unleaseAddress keep the index's count of an address's
// leases as a lease begins and ends. An index not yet built, or about to be
// rebuilt, counts them itself. Caller holds mu.
func (p *Pool) leaseAddress(e *entry, site string) {
	if p.idx.valid && p.entries[e.URL] == e {
		p.idx.leases[addressSite{p.hostID(e), site}]++
	}
}

func (p *Pool) unleaseAddress(e *entry, site string) {
	if !p.idx.valid || p.entries[e.URL] != e {
		return
	}
	key := addressSite{p.hostID(e), site}
	if p.idx.leases[key] > 1 {
		p.idx.leases[key]--
	} else {
		delete(p.idx.leases, key)
	}
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
// A route never tried for the site answers for itself alone (untested).
// Caller holds mu, and ix is p.index().
func (p *Pool) eligible(ix *inventory, e *entry, site string, files int, now time.Time, waf bool) bool {
	if e.Until.After(now) || !p.enabled(e) || !p.exitFree(e, site, files, now) {
		return false
	}
	if e.untested(site, waf) {
		return e.freeOnItsOwn(site, files, now, waf)
	}
	if ix.leases[addressSite{e.hostID, site}] >= max(1, files) {
		return false
	}
	return !ix.addressCooling(e.hostID, site, now, waf)
}

// untested reports a route never tried for the site, nor, for WAF recovery,
// challenged. Such a route is potentially free whatever the other ports on
// its address are doing. A gateway fronts thousands of ports on one address,
// and they leave through public addresses of their own: two on one gateway
// came out at two different addresses, neither the gateway's. So a host's
// limit met through one port says nothing about another, and one being
// leased does not make the rest busy, until the route has been tried. From
// then on it shares its address's leases and cooldowns like any other.
func (e *entry) untested(site string, waf bool) bool {
	s := e.Sites[site]
	return (s == nil || s.Tries == 0) && (!waf || e.WAF.Tries == 0)
}

// freeOnItsOwn is eligibility for an untested route: its own leases and its
// own cooldowns, and nothing of its address's.
func (e *entry) freeOnItsOwn(site string, files int, now time.Time, waf bool) bool {
	if waf && e.WAF.Until.After(now) {
		return false
	}
	s := e.Sites[site]
	return s == nil || (s.active < max(1, files) && !s.Until.After(now))
}

// addressCooling reports whether any port on the address is cooling for the
// site, or, for WAF recovery, after a challenge. Some providers list
// thousands of ports on one address, and walking them was most of what a
// route draw cost; so an address found cooling is remembered until the
// cooldown it found, and is not walked again before then. A cooldown is
// only ever lengthened, by Cooldown and by refusals in a row, so that cannot
// go stale but conservatively: a refusal's Retry-After shorter than a
// cooldown already running leaves the address cooling until the longer one.
// An address found not cooling is walked every time, since a cooldown can
// start at any moment.
func (ix *inventory) addressCooling(host int, site string, now time.Time, waf bool) bool {
	key := addressCooldown{addressSite{host, site}, waf}
	if until, ok := ix.cooling[key]; ok && now.Before(until) {
		return true
	}
	var until time.Time
	for _, o := range ix.byHost[host] {
		if s := o.Sites[site]; s != nil && s.Until.After(until) {
			until = s.Until
		}
		if waf && o.WAF.Until.After(until) {
			until = o.WAF.Until
		}
	}
	if !until.After(now) {
		delete(ix.cooling, key)
		return false
	}
	ix.cooling[key] = until
	return true
}

// listedForDraw, when set, is told each time drawUnmeasured lists the
// inventory. Only tests set it.
var listedForDraw func()

// drawUnmeasured draws one route not measured for site that could take the
// transfer: selection's single exploration ticket. It draws an address at
// random and then one of its ports, drawing again until the port qualifies,
// so each address is drawn in proportion to the share of its ports that do.
// Drawn by route instead, a gateway listing thousands of ports on one
// address took nearly every draw, while most of its ports were dead and
// most single proxies had been tried: a fresh inventory spent its first half
// hour finding that out, one port at a time.
//
// Once the single proxies have been tried, though, what qualifies is a
// share of a few gateways' ports, and an address drawn at random is almost
// never one of them. So after a quarter of ProxyDrawTries misses it draws
// by route, as uniformly, which finds one in a few dozen tries. Going
// straight to the listing instead had the dispatcher listing all 60,000
// routes for nearly every file it started, under the manager's lock. Only
// when that misses too does it list them, and draw an address and a port
// among those. Caller holds mu, and ix is p.index().
func (p *Pool) drawUnmeasured(ix *inventory, site string, files int, now time.Time, waf bool) *entry {
	if len(ix.byHost) <= 1 { // address numbers start at 1
		return nil
	}
	for range config.ProxyDrawTries / 4 {
		ports := ix.byHost[1+rand.IntN(len(ix.byHost)-1)]
		if len(ports) == 0 {
			continue
		}
		if e := ports[rand.IntN(len(ports))]; e.speed(site) == 0 && p.eligible(ix, e, site, files, now, waf) {
			return e
		}
	}
	for range config.ProxyDrawTries {
		if e := ix.all[rand.IntN(len(ix.all))]; e.speed(site) == 0 && p.eligible(ix, e, site, files, now, waf) {
			return e
		}
	}
	if listedForDraw != nil {
		listedForDraw()
	}
	byAddress := make(map[int][]*entry)
	var addresses []int
	for _, e := range p.available(site, files, now, waf) {
		if e.speed(site) == 0 {
			if byAddress[e.hostID] == nil {
				addresses = append(addresses, e.hostID)
			}
			byAddress[e.hostID] = append(byAddress[e.hostID], e)
		}
	}
	if len(addresses) == 0 {
		return nil
	}
	ports := byAddress[addresses[rand.IntN(len(addresses))]]
	return ports[rand.IntN(len(ports))]
}
