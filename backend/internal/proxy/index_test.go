// SPDX-License-Identifier: MIT

package proxy

import (
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/config"
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
		if rng.IntN(3) == 0 {
			e.WAF.Tries = 1
		}
		// A few exits, some shared, some held or cooling, some found long ago.
		if rng.IntN(2) == 0 {
			e.Exit = fmt.Sprintf("203.0.113.%d", rng.IntN(6))
			e.ExitAt = now.Add(-time.Duration(rng.IntN(2)) * 2 * config.ProxyExitTTL)
		}
	}
	p.exitLeases, p.exitCooling = map[exitSite]int{}, map[exitSite]time.Time{}
	for i := range 6 {
		exit := fmt.Sprintf("203.0.113.%d", i)
		for _, site := range []string{"keep2share", "fileboom", "cloudflare"} {
			switch rng.IntN(4) {
			case 0:
				p.exitLeases[exitSite{exit, site}] = 1 + rng.IntN(2)
			case 1:
				p.exitCooling[exitSite{exit, site}] = now.Add(time.Duration(rng.IntN(3)-1) * time.Hour)
			}
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

// An address's leases are counted as they come and go, and the count survives
// the index being rebuilt partway through a lease.
func TestAddressLeasesAreCountedAsLeasesComeAndGo(t *testing.T) {
	p := &Pool{entries: make(map[string]*entry), priors: make(map[string]*prior), lastProbe: make(map[string]time.Time),
		feeds: []string{"feed"}, client: httpx.New("test", "en", 0, time.Second)}
	now := time.Now()
	a, b := p.ensure("http://10.6.0.1:8080", now), p.ensure("socks5h://10.6.0.1:1080", now) // one address
	fresh := p.ensure("socks5h://10.6.0.1:1081", now)
	a.Origins, b.Origins, fresh.Origins = []string{"feed"}, []string{"feed"}, []string{"feed"}
	b.stat("keep2share").Tries = 1 // tried: it shares its address's leases from now on
	can := func(e *entry, site string) bool {
		p.mu.Lock()
		defer p.mu.Unlock()
		return p.eligible(p.index(), e, site, 1, time.Now(), false)
	}
	l := p.Acquire("keep2share", routeID(a.URL), 1)
	if l == nil || l.entry != a {
		t.Fatal("did not lease the first port")
	}
	if can(b, "keep2share") || !can(b, "fileboom") {
		t.Fatal("the address's other port must be held for the site leased, and only that one")
	}
	if !can(fresh, "keep2share") {
		t.Fatal("a port never tried was held back by its address's lease")
	}
	p.mu.Lock()
	p.ensure("http://10.6.0.2:8080", now).Origins = []string{"feed"} // the index is rebuilt mid-lease
	p.mu.Unlock()
	if can(b, "keep2share") {
		t.Fatal("a rebuilt index lost the lease")
	}
	l.Release()
	if !can(b, "keep2share") {
		t.Fatal("the address is still counted as leased after its lease ended")
	}
}

// An address found cooling is remembered until its cooldown ends, and no
// longer; an address found not cooling is not remembered at all, so a
// cooldown starting afterwards is seen at once.
func TestAnAddressFoundCoolingIsRememberedUntilItsCooldownEnds(t *testing.T) {
	p := &Pool{entries: make(map[string]*entry), feeds: []string{"feed"}}
	now := time.Now()
	a, b := p.ensure("http://10.7.0.1:8080", now), p.ensure("http://10.7.0.1:8081", now)
	fresh := p.ensure("http://10.7.0.1:8082", now)
	a.Origins, b.Origins, fresh.Origins = []string{"feed"}, []string{"feed"}, []string{"feed"}
	a.stat("keep2share").Tries = 1 // tried: it shares its address's cooldowns
	ix := p.index()
	if !p.eligible(ix, a, "keep2share", 1, now, false) {
		t.Fatal("a free address was held back")
	}
	b.stat("keep2share").Until = now.Add(time.Hour)
	if p.eligible(ix, a, "keep2share", 1, now, false) {
		t.Fatal("a cooldown starting after the address was found free was missed")
	}
	if !p.eligible(ix, fresh, "keep2share", 1, now, false) {
		t.Fatal("a port never tried was held back by another port's cooldown")
	}
	if p.eligible(ix, a, "keep2share", 1, now.Add(30*time.Minute), false) {
		t.Fatal("the remembered cooldown ended early")
	}
	if !p.eligible(ix, a, "keep2share", 1, now.Add(2*time.Hour), false) {
		t.Fatal("the address stayed cooling after its cooldown ended")
	}
	a.stat("keep2share").Until = now.Add(3 * time.Hour)
	if p.eligible(ix, a, "keep2share", 1, now.Add(2*time.Hour), false) {
		t.Fatal("a cooldown starting after the remembered one ended was missed")
	}
}

// Ports never tried on one address are each potentially free: each can be
// leased, once, while the address's tried ports are held by those leases.
func TestPortsNeverTriedAreEachPotentiallyFree(t *testing.T) {
	p := testPool(t, []string{
		"socks5h://gateway.example.test:20001", "socks5h://gateway.example.test:20002",
		"socks5h://gateway.example.test:20003", "socks5h://gateway.example.test:20004",
	}, nil)
	p.mu.Lock()
	tried := p.entries["socks5h://gateway.example.test:20004"]
	tried.stat("keep2share").Tries = 1
	p.mu.Unlock()
	first := p.Acquire("keep2share", routeID(tried.URL), 1)
	if first == nil || first.entry != tried {
		t.Fatal("did not lease the tried port")
	}
	t.Cleanup(first.Release)
	seen := map[string]bool{}
	for range 3 {
		l := p.Acquire("keep2share", "", 1)
		if l == nil {
			t.Fatalf("leased %d of the three ports never tried, beside a leased one", len(seen))
		}
		t.Cleanup(l.Release)
		if seen[l.entry.URL] || l.entry == tried {
			t.Fatalf("leased %s, which was not free", l.entry.URL)
		}
		seen[l.entry.URL] = true
	}
	if l := p.Acquire("keep2share", "", 1); l != nil {
		t.Fatalf("leased %s; want each port once", l.entry.URL)
	}
	first.Release()
	// Once its siblings are leased, the tried port is held by them.
	if l := p.Acquire("keep2share", routeID(tried.URL), 1); l != nil {
		t.Fatal("the tried port was leased beside its address's other leases")
	}
}

// A host's cooldown is written on the route it was met through, not copied
// onto every port of its address, which was a synced write per port. Tried
// ports on the address take it up where routes are chosen; one never tried
// answers for itself.
func TestACooldownIsWrittenOnTheRouteItWasMetThrough(t *testing.T) {
	met, tried, fresh := "http://gateway.example.test:30001", "http://gateway.example.test:30002", "http://gateway.example.test:30003"
	p := testPool(t, []string{met, tried, fresh}, nil)
	p.mu.Lock()
	p.entries[tried].stat("keep2share").Tries = 1
	p.mu.Unlock()
	lease := p.Acquire("keep2share", routeID(met), 1)
	if lease == nil {
		t.Fatal("no route")
	}
	lease.observe(httpx.RouteObservation{Status: 200, Duration: time.Second})
	lease.Cooldown(time.Now().Add(time.Hour))
	lease.Release()
	p.mu.Lock()
	copied := p.entries[tried].stat("keep2share").Until
	p.mu.Unlock()
	if !copied.IsZero() {
		t.Fatalf("the cooldown was copied onto another port: %v", copied)
	}
	if l := p.Acquire("keep2share", routeID(tried), 1); l != nil {
		l.Release()
		if l.entry.URL == tried {
			t.Fatal("a tried port escaped its address's cooldown")
		}
	}
	if l := p.Acquire("keep2share", routeID(fresh), 1); l == nil || l.entry.URL != fresh {
		t.Fatal("the port never tried was held back by its address's cooldown")
	} else {
		l.Release()
	}
}

// Untested routes are drawn by address, then by port: a gateway of a
// thousand ports is one address among several, not nearly every draw.
func TestAGatewayIsOneAddressAmongTheDraws(t *testing.T) {
	p := &Pool{entries: make(map[string]*entry), priors: make(map[string]*prior), lastProbe: make(map[string]time.Time),
		static: []string{}, feeds: []string{"feed"}, client: httpx.New("test", "en", 0, time.Second)}
	now := time.Now()
	for port := range 1000 {
		p.ensure(fmt.Sprintf("socks5h://10.3.0.1:%d", 20000+port), now).Origins = []string{"feed"}
	}
	for i := range 9 {
		p.ensure(fmt.Sprintf("http://10.3.1.%d:8080", 1+i), now).Origins = []string{"feed"}
	}
	gateway, draws := 0, 2000
	for range draws {
		p.mu.Lock()
		ix := p.index()
		e := p.drawUnmeasured(ix, "keep2share", 1, now, false)
		p.mu.Unlock()
		if e == nil {
			t.Fatal("no route, with routes free")
		}
		if strings.HasPrefix(e.URL, "socks5h://10.3.0.1:") {
			gateway++
		}
	}
	// One address in ten; drawn by route it would be 1000 in 1009.
	if share := float64(gateway) / float64(draws); share < 0.05 || share > 0.16 {
		t.Fatalf("the gateway took %.0f%% of the draws; want about one in ten", 100*share)
	}
}

// One lease per exit per service: a second route found leaving by an exit
// another lease holds is refused it, and passed over while that lasts. A
// host's cooldown met through one route holds back every route known to
// leave by the same exit.
func TestAnExitIsHeldByOneLeaseAtATime(t *testing.T) {
	first, second, other := "http://one.example.test:8080", "http://two.example.test:8080", "http://three.example.test:8080"
	p := testPool(t, []string{first, second, other}, nil)
	const exit = "203.0.113.7"
	a := p.Acquire("keep2share", routeID(first), 1)
	if a == nil || !a.ClaimExit(exit) || !a.Claimed() {
		t.Fatal("the first route could not claim its exit")
	}
	b := p.Acquire("keep2share", routeID(second), 1)
	if b == nil || b.entry.URL != second {
		t.Fatal("the second route was not leased")
	}
	if b.ClaimExit(exit) || b.Claimed() {
		t.Fatal("a second lease claimed an exit already held")
	}
	b.Release()
	if l := p.Acquire("keep2share", routeID(second), 1); l == nil || l.entry.URL == second {
		t.Fatalf("leased %v; want the route whose exit is held passed over", l)
	} else {
		l.Release()
	}
	if l := p.Acquire("fileboom", routeID(second), 1); l == nil || l.entry.URL != second {
		t.Fatal("another service's lease on the exit held this route back")
	} else {
		l.Release()
	}
	a.Release()
	c := p.Acquire("keep2share", routeID(second), 1)
	if c == nil || c.entry.URL != second || !c.ClaimExit(exit) {
		t.Fatal("the exit was not free once its lease ended")
	}
	// The host's timer, met through this route, is the exit's.
	c.Cooldown(time.Now().Add(time.Hour))
	c.Release()
	d := p.Acquire("keep2share", routeID(other), 1)
	if d == nil || d.ClaimExit(exit) {
		t.Fatal("a route found leaving by a cooling exit claimed it")
	}
	d.Release()
	if l := p.Acquire("keep2share", routeID(first), 1); l != nil && l.entry.URL == first {
		t.Fatal("a route whose exit is cooling was leased")
	}
}

// With every single proxy cooling and the free routes a sliver of a few
// gateways' ports, a draw still finds one by route rather than listing the
// whole inventory, which the dispatcher did for nearly every file it started.
func TestADrawAmongAFewGatewaysPortsDoesNotListTheInventory(t *testing.T) {
	p := &Pool{entries: make(map[string]*entry), priors: make(map[string]*prior), lastProbe: make(map[string]time.Time),
		static: []string{}, feeds: []string{"feed"}, client: httpx.New("test", "en", 0, time.Second)}
	now := time.Now()
	for i := range 4000 {
		e := p.ensure(fmt.Sprintf("http://10.4.%d.%d:8080", i/250, i%250), now)
		e.Origins = []string{"feed"}
		e.Until = now.Add(time.Hour) // tried and shelved
	}
	for g := range 4 {
		for port := range 1000 {
			e := p.ensure(fmt.Sprintf("socks5h://10.5.0.%d:%d", 1+g, 20000+port), now)
			e.Origins = []string{"feed"}
			if port%100 != 0 {
				e.Until = now.Add(time.Hour)
			}
		}
	}
	var listed int
	listedForDraw = func() { listed++ }
	defer func() { listedForDraw = nil }()
	for range 200 {
		p.mu.Lock()
		e := p.drawUnmeasured(p.index(), "keep2share", 1, now, false)
		p.mu.Unlock()
		if e == nil || !strings.HasPrefix(e.URL, "socks5h://10.5.0.") {
			t.Fatalf("drew %v; want a free gateway port", e)
		}
	}
	if listed > 10 {
		t.Fatalf("listed the inventory for %d of 200 draws; want hardly ever", listed)
	}
}
