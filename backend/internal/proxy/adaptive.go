// SPDX-License-Identifier: MIT

package proxy

import (
	"math"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/config"
)

// Constrain also covers transfers that finish between progress ticks: a
// user-imposed pause or speed ceiling must not train their EOF average.
func (l *Lease) Constrain() {
	l.pool.mu.Lock()
	defer l.pool.mu.Unlock()
	if !l.pool.closed && !l.released {
		l.constrain()
	}
}

func (l *Lease) constrain() {
	l.constrained = true
	l.windowBytes, l.windowTime, l.transferTime, l.upgradeWins = 0, 0, 0, 0
}

// Progress learns from newly written bytes, never the resumed prefix. The
// caller samples the file counter after its baseline has been reset. Pauses
// and an imposed speed ceiling reset the window rather than training the
// proxy to look slow. No database I/O occurs on this path.
// The result says a complete measurement window is ready for reconsidering
// the route. Setup includes CAPTCHA and ticket waits before the first bytes.
func (l *Lease) Progress(now time.Time, bytes int64, elapsed time.Duration, limited bool) bool {
	p := l.pool
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed || l.released || elapsed <= 0 {
		return false
	}
	if limited || bytes < 0 {
		l.constrain()
		return false
	}
	s := l.entry.stat(l.site)
	if !l.sawBytes {
		if bytes == 0 {
			return false
		}
		l.sawBytes = true
		if !l.constrained {
			setup := max(0, now.Sub(l.started)-elapsed).Seconds()
			if s.SetupSamples == 0 {
				s.SetupSeconds = setup
			} else {
				s.SetupSeconds = config.ProxyDurationAlpha*setup + (1-config.ProxyDurationAlpha)*s.SetupSeconds
			}
			s.SetupSamples++
			p.dirty[l.entry.URL] = true
		}
	}
	l.received += bytes
	l.windowBytes += bytes
	l.windowTime += elapsed
	l.transferTime += elapsed
	if l.windowTime < config.ProxySampleWindow || l.received < config.ProxyThroughputMinBytes {
		return false
	}
	rate := float64(l.windowBytes) / l.windowTime.Seconds()
	if s.BytesPerSecond == 0 {
		s.BytesPerSecond = rate
		p.noteMeasured(l.entry)
	} else {
		s.BytesPerSecond = config.ProxyDurationAlpha*rate + (1-config.ProxyDurationAlpha)*s.BytesPerSecond
	}
	l.windowBytes, l.windowTime, l.measured = 0, 0, true
	p.dirty[l.entry.URL] = true
	return true
}

// Upgrade reserves a proven alternative only after sustained evidence that
// the remaining bytes will finish sooner there, including a new ticket and
// failure recovery. The caller stops its old request and resumes on this
// lease, or releases the reservation if cancellation/completion wins.
// Reservation and comparison share the pool lock, so two slow transfers
// cannot both abandon their route in favour of the same free address.
func (l *Lease) Upgrade(now time.Time, remaining int64, currentRate float64) *Lease {
	p := l.pool
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed || l.released || !l.measured || l.transferTime < config.ProxyUpgradeWarmup ||
		remaining < config.ProxyUpgradeMinBytes || currentRate <= 0 || math.IsNaN(currentRate) || math.IsInf(currentRate, 0) {
		return nil
	}
	if now.Sub(l.lastUpgrade) < config.ProxyUpgradeInterval {
		return nil
	}
	l.lastUpgrade = now
	best, bestRate := p.bestUpgrade(l.site, now, l.waf, remaining)
	if best == nil {
		l.upgradeWins = 0
		return nil
	}
	currentSeconds := float64(remaining) / currentRate
	nextSeconds := float64(remaining) / bestRate
	if currentSeconds < nextSeconds*config.ProxyUpgradeRatio || currentSeconds-nextSeconds < config.ProxyUpgradeMinGain.Seconds() {
		l.upgradeWins = 0
		return nil
	}
	l.upgradeWins++
	if l.upgradeWins < config.ProxyUpgradeConfirm {
		return nil
	}
	l.upgradeWins = 0
	next := p.lease(best, l.site)
	if next != nil {
		next.waf = l.waf
	}
	return next
}

// bestUpgrade is the fastest route that could take over a transfer: measured
// routes only, since a working file is never interrupted to try an unknown
// one. Caller holds mu.
func (p *Pool) bestUpgrade(site string, now time.Time, waf bool, remaining int64) (*entry, float64) {
	prior := p.measuredPrior(site) // every route rated below is measured
	ix := p.index()
	var best *entry
	var bestRate float64
	for _, e := range ix.measured {
		if e.speed(site) <= 0 || !p.eligible(ix, e, site, 1, now, waf) {
			continue
		}
		if s := e.scoreStat(site); s.OK <= 0 {
			continue
		}
		if rate := prior.entryRate(e, site, waf, remaining); rate > bestRate {
			best, bestRate = e, rate
		}
	}
	return best, bestRate
}
