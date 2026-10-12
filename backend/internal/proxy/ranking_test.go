// SPDX-License-Identifier: MIT

package proxy

import (
	"fmt"
	"math/rand/v2"
	"testing"
	"time"
)

var rankingSites = []string{"keep2share", "fileboom", "cloudflare", "example", "unseen"}

// randomRoutes is an inventory whose routes carry any mix of measured and
// unmeasured stats, on the sites ranking treats differently.
func randomRoutes(rng *rand.Rand, n int) *Pool {
	p := &Pool{entries: make(map[string]*entry), priors: make(map[string]*prior),
		static: []string{Direct}, feeds: []string{"feed"}}
	now := time.Now()
	for i := range n {
		e := p.ensure(fmt.Sprintf("http://10.0.%d.%d:8080", i/250, i%250), now)
		e.Origins = []string{[]string{"feed", "gone"}[rng.IntN(2)]}
		for _, site := range rankingSites[:4] {
			if rng.IntN(3) == 0 {
				continue
			}
			s := e.stat(site)
			s.OK, s.Bad, s.Duration = float64(rng.IntN(4)), float64(rng.IntN(4)), rng.Float64()*5
			if rng.IntN(2) == 0 {
				s.BytesPerSecond = rng.Float64() * 1e7
			}
			if rng.IntN(3) == 0 {
				s.SetupSeconds, s.SetupSamples = rng.Float64(), 1
			}
		}
		if rng.IntN(4) == 0 {
			e.WAF.OK, e.WAF.Bad = float64(rng.IntN(3)), float64(rng.IntN(3))
		}
	}
	for _, site := range rankingSites[:3] {
		p.priors[site] = &prior{Valid: float64(rng.IntN(5)), Attempts: 5, Seconds: 3, Timed: 2}
	}
	return p
}

// speed exists so that ranking need not copy every route's stats to read one
// field of them; it must read the same field scoreStat would have given.
func TestSpeedIsTheScoredSpeed(t *testing.T) {
	rng := rand.New(rand.NewPCG(3, 4))
	for round := range 50 {
		p := randomRoutes(rng, 80)
		for _, e := range p.entries {
			for _, site := range rankingSites {
				if got, want := e.speed(site), e.scoreStat(site).BytesPerSecond; got != want {
					t.Fatalf("round %d, %s on %s: speed %v, scoreStat %v (stats %v)", round, e.URL, site, got, want, e.Sites)
				}
			}
		}
	}
}

// Leaving the inventory's mean bandwidth out of the prior changes nothing for
// a measured route, which is every route an upgrade or the measured half of
// selection rates.
func TestMeasuredRoutesRateTheSameWithoutTheMeanBandwidth(t *testing.T) {
	rng := rand.New(rand.NewPCG(5, 6))
	rated := 0
	for range 50 {
		p := randomRoutes(rng, 80)
		for _, site := range rankingSites {
			full, measured := p.scorePrior(site), p.measuredPrior(site)
			if measured.a != full.a || measured.b != full.b || measured.seconds != full.seconds {
				t.Fatalf("%s: measured prior %+v, full %+v", site, measured, full)
			}
			for _, e := range p.entries {
				if e.speed(site) <= 0 {
					continue
				}
				for _, waf := range []bool{false, true} {
					remaining := rng.Int64N(1 << 32)
					if got, want := measured.entryRate(e, site, waf, remaining), full.entryRate(e, site, waf, remaining); got != want {
						t.Fatalf("%s on %s: rated %v without the bandwidth, %v with it", e.URL, site, got, want)
					}
					rated++
				}
			}
		}
	}
	if rated == 0 {
		t.Fatal("no measured route was rated")
	}
}
