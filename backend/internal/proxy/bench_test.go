// SPDX-License-Identifier: MIT

package proxy

import (
	"fmt"
	"math/rand/v2"
	"slices"
	"testing"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

// benchPool is an inventory the size a public feed fills: tens of thousands
// of addresses, several ports on many of them, some cooling, a few leased.
// benchPool is shaped like a long-running public pool: ten ports to an
// address, a fifth of the entries dead for the day, one address in ten
// cooling for the site, a few routes leased, and one route in two hundred
// measured, here or on another site.
func benchPool(b *testing.B, n int) *Pool {
	b.Helper()
	p := &Pool{entries: make(map[string]*entry), priors: make(map[string]*prior), lastProbe: make(map[string]time.Time),
		static: []string{Direct}, feeds: []string{"feed"}, client: httpx.New("bench", "en", 0, time.Second)}
	now := time.Now()
	for i := range n {
		host := i / 10
		e := p.ensure(fmt.Sprintf("socks5h://10.%d.%d.%d:%d", host>>16&255, host>>8&255, host&255, 1080+i%10), now)
		e.Origins = []string{"feed"}
		if i%5 == 1 {
			e.Until = now.Add(24 * time.Hour)
		}
		if host%10 == 3 && i%10 == 0 {
			e.stat("keep2share").Until = now.Add(time.Hour)
		}
		if i%97 == 0 {
			e.stat("keep2share").active = 1
		}
		if i%200 == 7 {
			s := e.stat([]string{"keep2share", "cloudflare", "example"}[i/200%3])
			s.OK, s.Tries, s.BytesPerSecond, s.Duration = 3, 4, float64(1+i%37)*1e5, 2
		}
		if i%3 == 0 {
			e.stat("cloudflare").Bad = 1 // a probe answered, nothing carried
		}
	}
	return p
}

func BenchmarkAvailableAcrossALargeInventory(b *testing.B) {
	p := benchPool(b, 50000)
	now := time.Now()
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		p.mu.Lock()
		_ = p.available("keep2share", 1, now, false)
		p.mu.Unlock()
	}
}

// A running transfer asks every ProxyUpgradeInterval whether a measured
// route would finish it sooner; with many running that is many a second.
func BenchmarkUpgradeAcrossALargeInventory(b *testing.B) {
	for _, site := range []string{"keep2share", "cloudflare"} {
		b.Run(site, func(b *testing.B) {
			p := benchPool(b, 50000)
			var e *entry
			for _, c := range p.entries {
				if c.Until.IsZero() && c.Sites[site] == nil {
					e = c
					break
				}
			}
			p.mu.Lock()
			l := p.lease(e, site)
			p.mu.Unlock()
			l.waf = site == "cloudflare"
			l.measured, l.transferTime = true, config.ProxyUpgradeWarmup
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				l.lastUpgrade = time.Time{}
				if next := l.Upgrade(time.Now(), config.ProxyUpgradeMinBytes, 1e12); next != nil {
					b.Fatal("a route finishing sooner than 1 TB/s")
				}
			}
		})
	}
}

func BenchmarkAcquireAcrossALargeInventory(b *testing.B) {
	for _, site := range []string{"keep2share", "cloudflare"} {
		b.Run(site, func(b *testing.B) {
			p := benchPool(b, 50000)
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				l := p.AcquireFor(site, "", 1, 1<<30, site == "cloudflare")
				if l == nil {
					b.Fatal("no route")
				}
				l.Release()
			}
		})
	}
}

// referenceAvailable is available as it was before it was made fast: the
// same rules, with every entry's address parsed and hashed on every call.
func referenceAvailable(p *Pool, site string, files int, now time.Time, waf bool) []*entry {
	var candidates []*entry
	active := make(map[string]int)
	cooling := make(map[string]bool)
	for _, e := range p.entries {
		active[identity(e.URL)] += e.stat(site).active
		if e.stat(site).Until.After(now) || (waf && e.WAF.Until.After(now)) {
			cooling[identity(e.URL)] = true
		}
	}
	for _, e := range p.entries {
		if !p.enabled(e) || e.Until.After(now) || cooling[identity(e.URL)] {
			continue
		}
		if active[identity(e.URL)] < max(1, files) {
			candidates = append(candidates, e)
		}
	}
	return candidates
}

// The fast selection must choose exactly what the plain one did, over
// inventories with shared addresses, leases, cooldowns on any port, WAF
// cooldowns, dead entries and entries from a feed no longer configured.
func TestAvailableChoosesWhatThePlainScanDid(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	now := time.Now()
	for round := range 200 {
		p := &Pool{entries: make(map[string]*entry), static: []string{Direct}, feeds: []string{"feed"}}
		for range 1 + rng.IntN(60) {
			host := rng.IntN(12)
			scheme := []string{"http", "https", "socks5h"}[rng.IntN(3)]
			e := p.ensure(fmt.Sprintf("%s://10.0.0.%d:%d", scheme, host, 1000+rng.IntN(4)), now)
			e.Origins = []string{[]string{"feed", "gone"}[rng.IntN(2)]}
			if rng.IntN(5) == 0 {
				e.Until = now.Add(time.Hour)
			}
			if rng.IntN(4) == 0 {
				e.stat("keep2share").Until = now.Add(time.Duration(rng.IntN(3)-1) * time.Hour)
			}
			if rng.IntN(4) == 0 {
				e.stat("keep2share").active = rng.IntN(3)
			}
			if rng.IntN(6) == 0 {
				e.WAF.Until = now.Add(time.Hour)
			}
		}
		if rng.IntN(3) == 0 {
			p.ensure(Direct, now)
		}
		for _, files := range []int{0, 1, 2} {
			for _, waf := range []bool{false, true} {
				want := urls(referenceAvailable(p, "keep2share", files, now, waf))
				got := urls(p.available("keep2share", files, now, waf))
				if !slices.Equal(got, want) {
					t.Fatalf("round %d files=%d waf=%v: chose %v, want %v", round, files, waf, got, want)
				}
			}
		}
	}
}

func urls(entries []*entry) []string {
	var out []string
	for _, e := range entries {
		out = append(out, e.URL)
	}
	slices.Sort(out)
	return out
}
