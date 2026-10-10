// SPDX-License-Identifier: MIT

package proxy

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func refreshTestPool(t *testing.T) (*Pool, string, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		fmt.Fprint(w, `["http://one.example.test:8000", "socks5://one.example.test:1080", "http://two.example.test:8000"]`)
	}))
	t.Cleanup(srv.Close)
	p := testPool(t, nil, []string{srv.URL})
	p.Refresh(context.Background())
	return p, srv.URL, &calls
}

func ageRefresh(p *Pool, src string, age time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sources[src].Attempted = time.Now().Add(-age)
	p.sources[src].Fetched = p.sources[src].Attempted
}

func TestFeedRefreshWaits24HoursAndKeepsScheduleAcrossRestart(t *testing.T) {
	p, src, calls := refreshTestPool(t)
	ageRefresh(p, src, 23*time.Hour)
	p.Refresh(context.Background())
	if calls.Load() != 1 {
		t.Fatal("an idle pool reloaded before 24 hours")
	}
	p.mu.Lock()
	err := p.saveAll()
	p.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	path := p.db.Path()
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	restored, err := Open(path, nil, []string{src}, p.client, p.log)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	restored.Refresh(context.Background())
	if calls.Load() != 1 {
		t.Fatal("restart reloaded a cached feed")
	}
	ageRefresh(restored, src, 24*time.Hour)
	restored.Refresh(context.Background())
	if calls.Load() != 2 {
		t.Fatal("daily refresh did not run")
	}
}

func TestEarlyRefreshRequiresScoredCapacityForRunnableDemand(t *testing.T) {
	for _, tc := range []struct {
		name        string
		prepare     func(*Pool)
		wantRefresh bool
	}{
		{"idle", func(p *Pool) {}, false},
		{"paused demand", func(p *Pool) {
			p.SetDemand(map[string]int{"keep2share": 4})
			p.SetDemand(map[string]int{"keep2share": 0})
		}, false},
		{"untried list", func(p *Pool) { p.SetDemand(map[string]int{"keep2share": 1}) }, true},
		{"enough scored addresses", func(p *Pool) { scoreRefreshRoutes(p); p.SetDemand(map[string]int{"keep2share": 2}) }, false},
		{"another service needs measurements", func(p *Pool) {
			scoreRefreshRoutes(p)
			p.SetDemand(map[string]int{"keep2share": 1, "fileboom": 1})
		}, true},
		{"completed service clears its demand", func(p *Pool) {
			scoreRefreshRoutes(p)
			p.SetDemand(map[string]int{"fileboom": 4})
			p.SetDemand(map[string]int{"keep2share": 2})
		}, false},
		{"ports do not add capacity", func(p *Pool) { scoreRefreshRoutes(p); p.SetDemand(map[string]int{"keep2share": 3}) }, true},
		{"low throughput", func(p *Pool) {
			scoreRefreshRoutes(p)
			p.entries["http://two.example.test:8000"].stat("keep2share").BytesPerSecond = 100
			p.SetDemand(map[string]int{"keep2share": 2})
		}, true},
		{"low throughput while active", func(p *Pool) {
			scoreRefreshRoutes(p)
			s := p.entries["http://two.example.test:8000"].stat("keep2share")
			s.BytesPerSecond, s.active = 100, 1
			p.SetDemand(map[string]int{"keep2share": 2})
		}, true},
		{"unreliable", func(p *Pool) {
			scoreRefreshRoutes(p)
			s := p.entries["http://two.example.test:8000"].stat("keep2share")
			s.OK, s.Bad = 0.01, 10
			p.SetDemand(map[string]int{"keep2share": 2})
		}, true},
		{"cooling address", func(p *Pool) {
			scoreRefreshRoutes(p)
			p.entries["http://one.example.test:8000"].stat("keep2share").Until = time.Now().Add(time.Hour)
			p.SetDemand(map[string]int{"keep2share": 2})
		}, true},
		{"active unmeasured transfers", func(p *Pool) {
			p.entries["http://one.example.test:8000"].stat("keep2share").active = 1
			p.entries["http://two.example.test:8000"].stat("keep2share").active = 1
			p.SetDemand(map[string]int{"keep2share": 2})
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, src, calls := refreshTestPool(t)
			ageRefresh(p, src, time.Hour)
			tc.prepare(p)
			p.Refresh(context.Background())
			if refreshed := calls.Load() == 2; refreshed != tc.wantRefresh {
				t.Fatalf("refresh=%v, want %v", refreshed, tc.wantRefresh)
			}
			p.Refresh(context.Background())
			if calls.Load() > 2 {
				t.Fatal("shortage bypassed the retry interval")
			}
		})
	}
}

func scoreRefreshRoutes(p *Pool) {
	for _, e := range p.entries {
		s := e.stat("keep2share")
		s.Tries, s.OK, s.Duration, s.BytesPerSecond = 20, 10, 2, 1<<20
	}
}

func TestFailedFeedDoesNotRetryEarlyWithoutDemand(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	p := testPool(t, nil, []string{srv.URL})
	p.Refresh(context.Background())
	ageRefresh(p, srv.URL, time.Hour)
	p.sources[srv.URL].Fetched = time.Time{}
	p.Refresh(context.Background())
	if calls.Load() != 1 {
		t.Fatal("failure made an idle pool retry before 24 hours")
	}
	p.SetDemand(map[string]int{"keep2share": 1})
	p.Refresh(context.Background())
	if calls.Load() != 2 {
		t.Fatal("pending work could not retry a failed feed")
	}
}
