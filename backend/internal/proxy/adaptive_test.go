// SPDX-License-Identifier: MIT

package proxy

import (
	"context"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

func TestRouteChoiceAccountsForRemainingBytesAndSetup(t *testing.T) {
	p := testPool(t, []string{"http://fast.example.test:80", "http://quick.example.test:80"}, nil)
	fast, quick := p.entries[p.static[0]], p.entries[p.static[1]]
	for _, e := range []*entry{fast, quick} {
		s := e.stat("keep2share")
		s.Tries, s.OK, s.SetupSamples = 100, 100, 1
	}
	fast.stat("keep2share").BytesPerSecond, fast.stat("keep2share").SetupSeconds = 8<<20, 30
	quick.stat("keep2share").BytesPerSecond, quick.stat("keep2share").SetupSeconds = 256<<10, 0.01
	for _, tc := range []struct {
		bytes int64
		want  *entry
	}{{64 << 10, quick}, {128 << 20, fast}} {
		wins := 0
		for range 200 {
			if p.selectEntry("keep2share", []*entry{fast, quick}, tc.bytes) == tc.want {
				wins++
			}
		}
		if wins < 195 {
			t.Fatalf("%d remaining bytes: best route chosen %d/200 times", tc.bytes, wins)
		}
	}
}

func TestProgressLearnsBeforeCompletionAndPersistsOnClose(t *testing.T) {
	p := testPool(t, []string{"http://one.example.test:80"}, nil)
	l := p.Acquire("keep2share", "", 1)
	start := l.started
	if !l.Progress(start.Add(35*time.Second), 1<<20, 5*time.Second, false) {
		t.Fatal("first live window was not measured")
	}
	s := l.entry.stat("keep2share")
	if s.BytesPerSecond != float64(1<<20)/5 || s.SetupSeconds != 30 || s.SetupSamples != 1 || s.Tries != 0 {
		t.Fatalf("live measurements: %+v", s)
	}
	l.Progress(start.Add(40*time.Second), 2<<20, 5*time.Second, false)
	want := (1-config.ProxyDurationAlpha)*float64(1<<20)/5 + config.ProxyDurationAlpha*float64(2<<20)/5
	if math.Abs(s.BytesPerSecond-want) > 0.01 {
		t.Fatalf("EWMA=%f, want %f", s.BytesPerSecond, want)
	}
	// A whole-response average at EOF must not overwrite the fresh windows.
	l.observe(httpx.RouteObservation{Bytes: 4 << 20, Duration: time.Hour})
	if s.BytesPerSecond != want {
		t.Fatal("completed response replaced recent live throughput")
	}
	l.Progress(start.Add(45*time.Second), 2<<20, 5*time.Second, false)
	want = s.BytesPerSecond
	l.Release()
	path := p.db.Path()
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	restored, err := Open(path, p.static, nil, p.client, p.log)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	got := restored.entries[p.static[0]].stat("keep2share")
	if got.BytesPerSecond != want || got.SetupSeconds != 30 || got.SetupSamples != 1 {
		t.Fatalf("unfinished transfer measurements were lost: %+v", got)
	}
}

func TestConstrainedProgressDoesNotTrainSlowRatesOrSetup(t *testing.T) {
	p := testPool(t, []string{"http://one.example.test:80"}, nil)
	l := p.Acquire("keep2share", "", 1)
	l.entry.stat("keep2share").BytesPerSecond = 1 << 20
	start := l.started
	l.Progress(start.Add(time.Hour), 1000, time.Hour, true)
	l.observe(httpx.RouteObservation{Bytes: 1 << 20, Duration: time.Hour})
	s := l.entry.stat("keep2share")
	if s.BytesPerSecond != 1<<20 || s.SetupSamples != 0 {
		t.Fatalf("user limit penalised proxy: %+v", s)
	}
	l.Progress(start.Add(time.Hour+5*time.Second), 5<<20, 5*time.Second, false)
	if s.BytesPerSecond != 1<<20 || s.SetupSamples != 0 {
		t.Fatalf("resuming included the paused interval: %+v", s)
	}
	l.Release()
}

func TestExplorationIsBoundedWithoutLeavingWorkersIdle(t *testing.T) {
	p := testPool(t, []string{"http://known.example.test:80", "http://probe.example.test:80", "http://next.example.test:80"}, nil)
	known, probe, next := p.entries[p.static[0]], p.entries[p.static[1]], p.entries[p.static[2]]
	known.stat("keep2share").BytesPerSecond = 1 << 20
	active := p.Acquire("keep2share", routeID(probe.URL), 1)
	for range 100 {
		if chosen := p.selectEntry("keep2share", []*entry{known, next}); chosen != known {
			t.Fatal("a second probe displaced a free, measured route")
		}
	}
	if chosen := p.selectEntry("keep2share", []*entry{next}); chosen != next {
		t.Fatal("exploration budget left a worker idle during bootstrap")
	}
	active.Release()
	p.lastProbe["keep2share"] = time.Now()
	if p.selectEntry("keep2share", []*entry{known, next}) != known {
		t.Fatal("exploration spacing was ignored")
	}
}

func upgradeTestPool(t *testing.T) (*Pool, *Lease, *entry, time.Time) {
	t.Helper()
	p := testPool(t, []string{"http://slow.example.test:80", "http://fast.example.test:80"}, nil)
	for _, e := range p.entries {
		s := e.stat("keep2share")
		s.Tries, s.OK, s.SetupSamples, s.SetupSeconds = 100, 100, 1, 30
		s.BytesPerSecond = 64 << 10
	}
	fast := p.entries[p.static[1]]
	fast.stat("keep2share").BytesPerSecond = 2 << 20
	l := p.Acquire("keep2share", routeID(p.static[0]), 1)
	now := l.started.Add(50 * time.Second)
	l.Progress(now, 20*64<<10, 20*time.Second, false)
	return p, l, fast, now
}

func TestUpgradeRequiresSustainedGainAndReservesAlternative(t *testing.T) {
	p, slow, fast, now := upgradeTestPool(t)
	defer slow.Release()
	if next := slow.Upgrade(now, 128<<20, 64<<10); next != nil {
		t.Fatal("one window interrupted a working transfer")
	}
	next := slow.Upgrade(now.Add(config.ProxyUpgradeInterval), 128<<20, 64<<10)
	if next == nil || next.entry != fast {
		t.Fatal("sustained slow transfer did not reserve its faster alternative")
	}
	defer next.Release()
	if p.Acquire("keep2share", routeID(fast.URL), 1) != nil {
		t.Fatal("another transfer stole the reserved upgrade")
	}
}

func TestUpgradeDoesNotSpendTicketsWithoutExpectedGain(t *testing.T) {
	for _, tc := range []struct {
		name    string
		prepare func(*Lease, *entry)
		bytes   int64
		rate    float64
	}{
		{"near completion", func(*Lease, *entry) {}, 1 << 20, 64 << 10},
		{"already fast", func(*Lease, *entry) {}, 128 << 20, 2 << 20},
		{"expensive setup", func(_ *Lease, e *entry) { e.stat("keep2share").SetupSeconds = 3000 }, 128 << 20, 64 << 10},
		{"unmeasured alternative", func(_ *Lease, e *entry) { e.stat("keep2share").BytesPerSecond = 0 }, 128 << 20, 64 << 10},
		{"cooling alternative", func(_ *Lease, e *entry) { e.stat("keep2share").Until = time.Now().Add(time.Hour) }, 128 << 20, 64 << 10},
		{"paused", func(l *Lease, _ *entry) { l.Progress(time.Now(), 0, time.Second, true) }, 128 << 20, 64 << 10},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, slow, fast, now := upgradeTestPool(t)
			defer slow.Release()
			tc.prepare(slow, fast)
			for i := range 3 {
				if next := slow.Upgrade(now.Add(time.Duration(i)*config.ProxyUpgradeInterval), tc.bytes, tc.rate); next != nil {
					next.Release()
					t.Fatal("unprofitable or unsafe upgrade")
				}
			}
		})
	}
}

func TestConcurrentUpgradesCannotReserveOneAddressTwice(t *testing.T) {
	p, first, fast, now := upgradeTestPool(t)
	defer first.Release()
	secondEntry := p.ensure("http://second.example.test:80", now)
	p.static = append(p.static, secondEntry.URL)
	second := p.Acquire("keep2share", routeID(secondEntry.URL), 1)
	defer second.Release()
	second.Progress(now, 20*64<<10, 20*time.Second, false)
	first.Upgrade(now, 128<<20, 64<<10)
	second.Upgrade(now, 128<<20, 64<<10)
	var wg sync.WaitGroup
	results := make(chan *Lease, 2)
	for _, l := range []*Lease{first, second} {
		wg.Go(func() { results <- l.Upgrade(now.Add(config.ProxyUpgradeInterval), 128<<20, 64<<10) })
	}
	wg.Wait()
	close(results)
	count := 0
	for l := range results {
		if l != nil {
			if l.entry != fast {
				t.Fatal("reserved an active address")
			}
			count++
			l.Release()
		}
	}
	if count != 1 {
		t.Fatalf("reserved the free address %d times", count)
	}
	// Flush samples while the pool remains open, without fetching a feed.
	p.Refresh(context.Background())
}
