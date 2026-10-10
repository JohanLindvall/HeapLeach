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
		if speed := e.readStat(site).BytesPerSecond; speed > 0 && p.enabled(e) {
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
	prior := p.scorePrior(site)
	var remaining int64
	if len(work) > 0 {
		remaining = work[0]
	}
	var best *entry
	bestRate := -1.0
	var untried []*entry
	rate := func(e *entry) float64 {
		s := e.readStat(site)
		x, y := gamma(prior.a+s.OK), gamma(prior.b+s.Bad)
		chance := x / (x + y)
		return prior.workRate(s, chance, remaining)
	}
	for _, e := range candidates {
		if e.stat(site).BytesPerSecond == 0 {
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
				s := e.readStat(site)
				if s.active > 0 && s.BytesPerSecond == 0 {
					return best
				}
			}
		}
		next := untried[0]
		for _, e := range untried {
			if e.URL > p.last[site] {
				next = e
				break
			}
		}
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
