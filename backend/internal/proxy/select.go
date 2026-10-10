// SPDX-License-Identifier: MIT

package proxy

import (
	"math"
	"math/rand/v2"

	"github.com/JohanLindvall/HeapLeach/internal/config"
)

// selectEntry follows amzscrape's throughput policy: sample each tried route's
// chance of success, divided by its observed cost. Untried routes get ONE
// shared draw, so thousands of dead feed entries cannot outvote a proven one
// merely through the number of lottery tickets they hold.
func (p *Pool) selectEntry(site string, candidates []*entry) *entry {
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
		if speed := e.stat(site).BytesPerSecond; speed > 0 && p.enabled(e) {
			bandwidth += speed
			sampled++
		}
	}
	if sampled > 0 {
		bandwidth /= float64(sampled)
	} else {
		bandwidth = config.ProxyPriorBytesPerSecond
	}
	var best *entry
	bestRate := -1.0
	var untried []*entry
	rate := func(e *entry) float64 {
		s := e.stat(site)
		x, y := gamma(a+s.OK), gamma(b+s.Bad)
		chance := x / (x + y)
		seconds := d
		if s.Tries > 0 {
			seconds = s.Duration
		}
		speed := s.BytesPerSecond
		if speed == 0 {
			speed = bandwidth
		}
		// Expected useful bytes/second, discounted for failed attempts and
		// their recovery cost. File size itself must not make a long, fast
		// download rank below a tiny API response.
		return chance * speed / (1 + config.ProxyInvalidPenalty*(1-chance)/max(seconds, config.ProxyMinSeconds))
	}
	for _, e := range candidates {
		if e.stat(site).Tries == 0 {
			untried = append(untried, e)
			continue
		}
		if r := rate(e); r > bestRate {
			best, bestRate = e, r
		}
	}
	if len(untried) > 0 {
		next := untried[0]
		for _, e := range untried {
			if e.URL > p.last[site] {
				next = e
				break
			}
		}
		if best == nil || rate(next) > bestRate {
			best = next
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
