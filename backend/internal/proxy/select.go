// SPDX-License-Identifier: MIT

package proxy

import (
	"math"
	"math/rand/v2"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/config"
)

type scorePrior struct{ a, b, seconds, bandwidth float64 }

func (p *Pool) scorePrior(site string) scorePrior {
	first := p.priors[site]
	if first == nil {
		first = &prior{}
	}
	w := config.ProxyPriorWeight
	q := (first.Valid + config.ProxyPriorValid*w) / (float64(first.Attempts) + w)
	d := (first.Seconds + config.ProxyPriorSeconds*w) / (float64(first.Timed) + w)
	a, b := config.ProxyPriorStrength*q, config.ProxyPriorStrength*(1-q)
	bandwidth, sampled := 0.0, 0
	for _, e := range p.entries {
		if speed := e.scoreStat(site).BytesPerSecond; speed > 0 && p.enabled(e) {
			bandwidth += speed
			sampled++
		}
	}
	if sampled > 0 {
		bandwidth /= float64(sampled)
	} else {
		bandwidth = config.ProxyPriorBytesPerSecond
	}
	return scorePrior{a, b, d, bandwidth}
}

func (e *entry) readStat(site string) siteStat {
	if s := e.Sites[site]; s != nil {
		return *s
	}
	return siteStat{}
}

// WAF recovery reuses the proxy's ordinary measurements. Its own samples
// take precedence once available. Free-host speed caps describe the host,
// so those samples cannot estimate capacity for an unrestricted download.
// User-throttled and paused windows are already excluded when measuring.
func (e *entry) scoreStat(site string) siteStat {
	s := e.readStat(site)
	if site != "cloudflare" {
		return s
	}
	var ok, bad, duration, speed float64
	var tries int
	for name, other := range e.Sites {
		if name == site {
			continue
		}
		ok, bad = ok+other.OK, bad+other.Bad
		duration += other.Duration * float64(other.Tries)
		tries += other.Tries
		if name != "keep2share" && name != "fileboom" {
			speed = max(speed, other.BytesPerSecond)
		}
	}
	if s.OK+s.Bad == 0 {
		s.OK, s.Bad = ok, bad
	}
	if s.Duration == 0 && tries > 0 {
		s.Duration = duration / float64(tries)
	}
	if s.BytesPerSecond == 0 {
		s.BytesPerSecond = speed
	}
	return s
}

func (e *entry) wafChance() float64 {
	return (config.ProxyPriorStrength + e.WAF.OK) / (config.ProxyPriorStrength + e.WAF.OK + e.WAF.Bad)
}

func (prior scorePrior) entryRate(e *entry, site string, waf bool, remaining int64) float64 {
	s := e.scoreStat(site)
	chance := (prior.a + s.OK) / (prior.a + prior.b + s.OK + s.Bad)
	if waf {
		chance *= e.wafChance()
	}
	return prior.workRate(s, chance, remaining)
}

// expectedRate is shared by the selector and its deterministic UI estimate.
func (prior scorePrior) expectedRate(s siteStat, chance float64) float64 {
	return prior.workRate(s, chance, 0)
}

func (prior scorePrior) workRate(s siteStat, chance float64, remaining int64) float64 {
	if remaining <= 0 {
		remaining = config.ProxyScoreBytes
	}
	setup := config.ProxyPriorSetup.Seconds()
	if s.SetupSamples > 0 {
		setup = s.SetupSeconds
	}
	speed := s.BytesPerSecond
	if speed <= 0 {
		speed = prior.bandwidth
	}
	seconds := setup + float64(remaining)/max(speed, 1) +
		(1-chance)*max(prior.seconds, config.ProxyInvalidPenalty)
	return chance * float64(remaining) / max(seconds, config.ProxyMinSeconds)
}

// meanRate is the same deterministic, throughput-weighted score used by the
// inventory UI and by discovery's assessment of standby routes.
func (prior scorePrior) meanRate(s siteStat) float64 {
	chance := (prior.a + s.OK) / (prior.a + prior.b + s.OK + s.Bad)
	return prior.expectedRate(s, chance)
}

// selectEntry follows amzscrape's throughput policy: sample each tried route's
// chance of success, divided by its observed cost. Untried routes get ONE
// shared draw, so thousands of dead feed entries cannot outvote a proven one
// merely through the number of lottery tickets they hold.
func (p *Pool) selectEntry(site string, candidates []*entry, work ...int64) *entry {
	var remaining int64
	if len(work) > 0 {
		remaining = work[0]
	}
	return p.selectFor(site, candidates, remaining, site == "cloudflare")
}

func (p *Pool) selectFor(site string, candidates []*entry, remaining int64, waf bool) *entry {
	prior := p.scorePrior(site)
	var best *entry
	bestRate := -1.0
	var untried []*entry
	rate := func(e *entry) float64 {
		s := e.scoreStat(site)
		x, y := gamma(prior.a+s.OK), gamma(prior.b+s.Bad)
		chance := x / (x + y)
		if waf {
			chance *= e.wafChance()
		}
		return prior.workRate(s, chance, remaining)
	}
	for _, e := range candidates {
		if e.scoreStat(site).BytesPerSecond == 0 {
			untried = append(untried, e)
			continue
		}
		if r := rate(e); r > bestRate {
			best, bestRate = e, r
		}
	}
	if len(untried) > 0 {
		// While proven routes are free, at most one unmeasured transfer
		// explores, and new exploration slots are spaced apart. When no
		// proven route is free, fill idle workers to bootstrap or recover.
		if best != nil {
			if time.Since(p.lastProbe[site]) < config.ProxyExploreInterval {
				return best
			}
			for _, e := range p.entries {
				s := e.scoreStat(site)
				if s.active > 0 && s.BytesPerSecond == 0 {
					return best
				}
			}
		}
		// Draw one eligible unmeasured endpoint uniformly. Walking sorted
		// URLs can spend many attempts on adjacent dead networks or ports.
		next := untried[rand.IntN(len(untried))]
		if best == nil || rate(next) > bestRate {
			best = next
			p.lastProbe[site] = time.Now()
		}
	}
	return best
}

// Marsaglia-Tsang gamma sampler; the ratio of two draws is Beta(a,b).
func gamma(shape float64) float64 {
	if shape < 1 {
		return gamma(shape+1) * math.Pow(1-rand.Float64(), 1/shape)
	}
	d := shape - 1.0/3
	c := 1 / math.Sqrt(9*d)
	for {
		x := rand.NormFloat64()
		v := 1 + c*x
		if v <= 0 {
			continue
		}
		v = v * v * v
		u := 1 - rand.Float64()
		if u < 1-0.0331*x*x*x*x || math.Log(u) < x*x/2+d*(1-v+math.Log(v)) {
			return d * v
		}
	}
}
