// SPDX-License-Identifier: MIT

package proxy

import (
	"fmt"
	"math/rand/v2"
	"slices"
	"testing"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

// randomInventory is a pool of addresses with several ports each, holding
// any mix of leases, cooldowns on any port, WAF cooldowns, dead routes,
// routes from a feed no longer configured, and measured speeds.
func randomInventory(rng *rand.Rand, routes int) *Pool {
	p := &Pool{entries: make(map[string]*entry), priors: make(map[string]*prior), lastProbe: make(map[string]time.Time),
		static: []string{Direct}, feeds: []string{"feed"}, client: httpx.New("test", "en", 0, time.Second)}
	now := time.Now()
	for range routes {
		scheme := []string{"http", "https", "socks5h"}[rng.IntN(3)]
		e := p.ensure(fmt.Sprintf("%s://10.0.%d.%d:%d", scheme, rng.IntN(2), rng.IntN(8), 1000+rng.IntN(4)), now)
		e.Origins = []string{[]string{"feed", "feed", "gone"}[rng.IntN(3)]}
		if rng.IntN(6) == 0 {
			e.Until = now.Add(time.Hour)
		}
		for _, site := range []string{"keep2share", "fileboom", "example"} {
			if rng.IntN(3) == 0 {
				continue
			}
			s := e.stat(site)
			if rng.IntN(5) == 0 {
				s.Until = now.Add(time.Duration(rng.IntN(3)-1) * time.Hour)
			}
			if rng.IntN(6) == 0 {
				s.active = rng.IntN(2)
			}
			if rng.IntN(2) == 0 {
				s.OK, s.Bad, s.Tries = float64(rng.IntN(5)), float64(rng.IntN(3)), 1+rng.IntN(5)
				s.BytesPerSecond = rng.Float64() * 1e7
			}
		}
		if rng.IntN(6) == 0 {
			e.WAF.Until = now.Add(time.Hour)
		}
	}
	if rng.IntN(3) == 0 {
		p.ensure(Direct, now)
	}
	return p
}

func routeURLs(entries []*entry) []string {
	var out []string
	for _, e := range entries {
		out = append(out, e.URL)
	}
	slices.Sort(out)
	return out
}

// Ranking asks each route it considers whether it could take the transfer,
// instead of listing every route that could; the answers must agree.
func TestEligibleIsWhatAvailableLists(t *testing.T) {
	rng := rand.New(rand.NewPCG(7, 8))
	now := time.Now()
	for round := range 200 {
		p := randomInventory(rng, 1+rng.IntN(60))
		ix := p.index()
		for _, site := range []string{"keep2share", "fileboom", "cloudflare"} {
			for _, files := range []int{0, 1, 2} {
				for _, waf := range []bool{false, true} {
					var eligible []*entry
					for _, e := range ix.all {
						if p.eligible(ix, e, site, files, now, waf) {
							eligible = append(eligible, e)
						}
					}
					if got, want := routeURLs(eligible), routeURLs(p.available(site, files, now, waf)); !slices.Equal(got, want) {
						t.Fatalf("round %d %s files=%d waf=%v: eligible %v, available %v", round, site, files, waf, got, want)
					}
				}
			}
		}
	}
}

// bestUpgradeByScan is how an upgrade chose before the index: the measured
// routes among everything available.
func bestUpgradeByScan(p *Pool, site string, now time.Time, waf bool, remaining int64) (*entry, float64) {
	prior := p.scorePrior(site)
	var best *entry
	var bestRate float64
	for _, e := range p.available(site, 1, now, waf) {
		if s := e.scoreStat(site); s.BytesPerSecond <= 0 || s.OK <= 0 {
			continue
		}
		if rate := prior.entryRate(e, site, waf, remaining); rate > bestRate {
			best, bestRate = e, rate
		}
	}
	return best, bestRate
}

func TestBestUpgradeIsWhatTheScanChose(t *testing.T) {
	rng := rand.New(rand.NewPCG(9, 10))
	now := time.Now()
	chosen := 0
	for round := range 300 {
		p := randomInventory(rng, 1+rng.IntN(60))
		for _, site := range []string{"keep2share", "fileboom", "cloudflare"} {
			for _, waf := range []bool{false, true} {
				remaining := 1 + rng.Int64N(1<<34)
				got, gotRate := p.bestUpgrade(site, now, waf, remaining)
				want, wantRate := bestUpgradeByScan(p, site, now, waf, remaining)
				if got != want || gotRate != wantRate {
					t.Fatalf("round %d %s waf=%v: chose %v at %v, the scan %v at %v", round, site, waf, got, gotRate, want, wantRate)
				}
				if got != nil {
					chosen++
				}
			}
		}
	}
	if chosen == 0 {
		t.Fatal("no round had an upgrade to choose")
	}
}

// With nothing measured, every route that could take the transfer has one
// equal chance, and no other route has any. Drawn from the whole inventory
// while most routes qualify, and listed when few do: both must hold.
func TestUnmeasuredRoutesAreDrawnUniformly(t *testing.T) {
	for _, tc := range []struct {
		name       string
		eligible   int
		ineligible int
	}{
		{"most routes qualify", 8, 4},
		{"few routes qualify", 3, 3000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &Pool{entries: make(map[string]*entry), priors: make(map[string]*prior), lastProbe: make(map[string]time.Time),
				static: []string{}, feeds: []string{"feed"}, client: httpx.New("test", "en", 0, time.Second)}
			now := time.Now()
			want := map[string]bool{}
			for i := range tc.eligible {
				e := p.ensure(fmt.Sprintf("http://10.1.0.%d:8080", i), now)
				e.Origins = []string{"feed"}
				want[e.URL] = true
			}
			for i := range tc.ineligible {
				e := p.ensure(fmt.Sprintf("http://10.2.%d.%d:8080", i/250, i%250), now)
				e.Origins = []string{"feed"}
				switch i % 4 {
				case 0:
					e.Until = now.Add(time.Hour)
				case 1:
					e.stat("keep2share").Until = now.Add(time.Hour)
				case 2:
					e.Origins = []string{"gone"}
				case 3:
					e.stat("keep2share").active = 1
				}
			}
			counts := map[string]int{}
			draws := 400 * tc.eligible
			for range draws {
				l := p.Acquire("keep2share", "", 1)
				if l == nil {
					t.Fatal("no route, with routes free")
				}
				if !want[l.entry.URL] {
					t.Fatalf("drew %s, which could not take the transfer", l.entry.URL)
				}
				counts[l.entry.URL]++
				l.Release()
			}
			for url := range want {
				if n, mean := counts[url], draws/tc.eligible; n < mean*3/4 || n > mean*5/4 {
					t.Fatalf("%s drawn %d times of %d, want about %d: %v", url, n, draws, mean, counts)
				}
			}
		})
	}
}

// A file's own route is reused for its ticket while it can take the transfer,
// and passed over once it cannot.
func TestThePreferredRouteIsTakenOnlyWhileItCanServe(t *testing.T) {
	for _, tc := range []struct {
		name  string
		spoil func(*entry)
		taken bool
	}{
		{"free", func(*entry) {}, true},
		{"dead", func(e *entry) { e.Until = time.Now().Add(time.Hour) }, false},
		{"cooling", func(e *entry) { e.stat("keep2share").Until = time.Now().Add(time.Hour) }, false},
		{"at its limit", func(e *entry) { e.stat("keep2share").active = 1 }, false},
		{"no longer configured", func(e *entry) { e.Origins = []string{"gone"} }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &Pool{entries: make(map[string]*entry), priors: make(map[string]*prior), lastProbe: make(map[string]time.Time),
				feeds: []string{"feed"}, client: httpx.New("test", "en", 0, time.Second)}
			now := time.Now()
			own := p.ensure("http://10.3.0.1:8080", now)
			other := p.ensure("http://10.3.0.2:8080", now)
			own.Origins, other.Origins = []string{"feed"}, []string{"feed"}
			other.stat("keep2share").BytesPerSecond, other.stat("keep2share").OK = 1e9, 50 // far the better route
			tc.spoil(own)
			l := p.Acquire("keep2share", routeID(own.URL), 1)
			if l == nil {
				t.Fatal("no route")
			}
			defer l.Release()
			if taken := l.entry == own; taken != tc.taken {
				t.Fatalf("own route taken=%v, want %v", taken, tc.taken)
			}
		})
	}
}

// The index is rebuilt when routes come and go, and a route measured after it
// was built counts as measured.
func TestTheIndexFollowsTheInventory(t *testing.T) {
	p := &Pool{entries: make(map[string]*entry), priors: make(map[string]*prior), lastProbe: make(map[string]time.Time),
		dirty: make(map[string]bool), feeds: []string{"feed"}, client: httpx.New("test", "en", 0, time.Second)}
	now := time.Now()
	first := p.ensure("http://10.4.0.1:8080", now)
	first.Origins = []string{"feed"}
	l := p.Acquire("keep2share", "", 1)
	if l == nil || l.entry != first {
		t.Fatal("the only route was not taken")
	}
	l.Release()

	added := p.ensure("http://10.4.0.2:8080", now)
	added.Origins = []string{"feed"}
	first.Until = now.Add(time.Hour)
	if l := p.Acquire("keep2share", "", 1); l == nil || l.entry != added {
		t.Fatalf("a route added after the index was built was not taken: %v", l)
	} else {
		// Measured through a transfer after the index was built.
		l.Progress(l.started.Add(10*time.Second), 64<<20, 10*time.Second, false)
		added.stat("keep2share").OK = 1 // and a success on record, which upgrades also ask for
		l.Release()
	}
	p.mu.Lock()
	best, _ := p.bestUpgrade("keep2share", now, false, 1<<30)
	p.mu.Unlock()
	if best != added {
		t.Fatalf("a route measured after the index was built is not an upgrade: %v", best)
	}

	added.LastSuccess = now.Add(-2 * 30 * 24 * time.Hour)
	p.mu.Lock()
	p.retire(now)
	p.mu.Unlock()
	if _, ok := p.entries[added.URL]; ok {
		t.Fatal("test set-up: the stale route was not retired")
	}
	first.Until = time.Time{}
	for range 20 {
		l := p.Acquire("keep2share", "", 1)
		if l == nil || l.entry != first {
			t.Fatalf("a retired route is still offered: %v", l)
		}
		l.Release()
	}
}

func TestLeasesAreCountedPerRoute(t *testing.T) {
	p := &Pool{entries: make(map[string]*entry), priors: make(map[string]*prior), lastProbe: make(map[string]time.Time),
		feeds: []string{"feed"}, client: httpx.New("test", "en", 0, time.Second)}
	e := p.ensure("http://10.5.0.1:8080", time.Now())
	e.Origins = []string{"feed"}
	a, b := p.Acquire("keep2share", "", 1), p.Acquire("fileboom", "", 1)
	if a == nil || b == nil || p.leased[e] != 2 {
		t.Fatalf("leases %v %v, counted %d; want two on the one route", a, b, p.leased[e])
	}
	a.Release()
	a.Release() // twice is once
	if p.leased[e] != 1 {
		t.Fatalf("counted %d after one release, want 1", p.leased[e])
	}
	b.Release()
	if _, held := p.leased[e]; held {
		t.Fatal("a route with no leases is still counted as leased")
	}
}
