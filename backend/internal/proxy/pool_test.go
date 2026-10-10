// SPDX-License-Identifier: MIT

package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

func testPool(t *testing.T, endpoints, feeds []string) *Pool {
	t.Helper()
	p, err := Open(filepath.Join(t.TempDir(), "proxies.db"), endpoints, feeds,
		httpx.New("test", "en", 0, time.Second), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := p.Close(); err != nil {
			t.Error(err)
		}
	})
	return p
}

func TestInventoryHealthThroughputAndCooldownSurviveRestart(t *testing.T) {
	p := testPool(t, []string{"http://one.example.test:8000", "http://two.example.test:8000"}, nil)
	lease := p.Acquire("keep2share", "", 1)
	if lease == nil {
		t.Fatal("no route")
	}
	id, raw := lease.ID(), lease.entry.URL
	lease.observe(httpx.RouteObservation{Status: 200, Bytes: 4 << 20, Duration: 2 * time.Second})
	// Tiny successful API replies must not replace the measured file speed.
	lease.observe(httpx.RouteObservation{Status: 200, Bytes: 100, Duration: time.Millisecond})
	until := time.Now().Add(time.Hour)
	lease.Cooldown(until)
	lease.Release()
	path := p.db.Path()
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	p2, err := Open(path, p.static, nil, p.client, p.log)
	if err != nil {
		t.Fatal(err)
	}
	defer p2.Close()
	e := p2.entries[raw]
	s := e.stat("keep2share")
	if s.BytesPerSecond != 2<<20 || s.Tries != 2 || !s.Until.Equal(until) || e.LastSuccess.IsZero() || !s.LastSuccess.Equal(e.LastSuccess) {
		t.Fatalf("lost persisted measurement: %+v %+v", e, s)
	}
	if p2.priors["keep2share"].Attempts != 1 {
		t.Fatal("lost first-attempt prior")
	}
	other := p2.Acquire("keep2share", id, 1)
	if other == nil || other.ID() == id {
		t.Fatal("restored cooldown was ignored")
	}
	other.Release()
	other = p2.Acquire("another-service", id, 1)
	if other == nil || other.ID() != id {
		t.Fatal("site cooldown leaked into another service")
	}
	other.Release()
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("database permissions: %v %v", info, err)
	}
}

func TestTransportFailureIsGlobalButRefusalIsPerService(t *testing.T) {
	p := testPool(t, []string{"http://one.example.test:8000"}, nil)
	l := p.Acquire("keep2share", "", 1)
	l.observe(httpx.RouteObservation{Status: 403, Err: errors.New("refused"), Duration: time.Second})
	l.Release()
	if p.Acquire("keep2share", "", 1) != nil {
		t.Fatal("refused route was still available")
	}
	l = p.Acquire("other", "", 1)
	if l == nil {
		t.Fatal("refusal affected a different service")
	}
	l.observe(httpx.RouteObservation{Err: errors.New("connection lost"), Duration: time.Second})
	l.Release()
	if p.Acquire("third", "", 1) != nil {
		t.Fatal("a broken proxy was still available")
	}
}

func TestLeaseLimitsAcrossProtocolsAndConcurrentAcquisitions(t *testing.T) {
	for _, preferred := range []string{"", routeID("http://one.example.test:8000")} {
		t.Run("preferred="+preferred, func(t *testing.T) {
			// Every endpoint is unrated, including alternate ports and
			// protocols for the same address. All contenders start together.
			p := testPool(t, []string{
				"http://one.example.test:8000", "http://one.example.test:8080",
				"https://one.example.test:8443", "socks5://one.example.test:1080",
				"http://two.example.test:8000", "socks5://two.example.test:1080",
			}, nil)
			var wg sync.WaitGroup
			start := make(chan struct{})
			leases := make(chan *Lease, 40)
			for range cap(leases) {
				wg.Go(func() {
					<-start
					if l := p.Acquire("keep2share", preferred, 1); l != nil {
						leases <- l
					}
				})
			}
			close(start)
			wg.Wait()
			close(leases)
			var held []*Lease
			addresses := make(map[string]bool)
			for l := range leases {
				held = append(held, l)
				t.Cleanup(l.Release)
				u, err := url.Parse(l.entry.URL)
				if err != nil {
					t.Fatal(err)
				}
				if addresses[u.Hostname()] {
					t.Fatalf("concurrent attempts shared proxy address %s", u.Hostname())
				}
				addresses[u.Hostname()] = true
			}
			if len(held) != 2 {
				t.Fatalf("leased %d routes; want two distinct addresses", len(held))
			}
			for _, l := range held {
				if extra := p.Acquire("keep2share", l.ID(), 1); extra != nil {
					extra.Release()
					t.Fatal("a preferred route bypassed its active reservation")
				}
			}
			// Freeing one address must leave the other reserved, even if
			// Release is called twice during attempt cleanup.
			held[0].Release()
			held[0].Release()
			again := p.Acquire("keep2share", held[0].ID(), 1)
			if again == nil {
				t.Fatal("released route remained unavailable")
			}
			defer again.Release()
			if again.ID() != held[0].ID() {
				t.Fatal("did not reuse the released preferred route")
			}
			if extra := p.Acquire("keep2share", "", 1); extra != nil {
				extra.Release()
				t.Fatal("releasing one route freed another active reservation")
			}
		})
	}
}

func TestFeedRefreshKeepsFailuresAndRetiresOnlyOldAbsentEntries(t *testing.T) {
	var mu sync.Mutex
	body := `["http://one.example.test:8000", {"proxy":"socks5://two.example.test:1080","score":999}]`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		fmt.Fprint(w, body)
	}))
	defer srv.Close()
	p := testPool(t, nil, []string{srv.URL})
	refresh := func(next string) {
		mu.Lock()
		body = next
		mu.Unlock()
		p.sources[srv.URL].Attempted = time.Time{}
		p.Refresh(context.Background())
	}
	p.Refresh(context.Background())
	if len(p.entries) != 2 {
		t.Fatalf("entries: %d", len(p.entries))
	}
	l := p.Acquire("keep2share", "", 1)
	l.observe(httpx.RouteObservation{Err: errors.New("dead feed entry"), Duration: time.Second})
	l.Release()
	failed := l.entry
	if time.Until(failed.Until) < 23*time.Hour {
		t.Fatal("dead feed entry was not benched for a day")
	}
	refresh("<html>feed unavailable</html>")
	if len(p.entries) != 2 || len(p.sources[srv.URL].Members) != 2 {
		t.Fatal("a failed feed erased inventory")
	}
	refresh(`["http://one.example.test:8000", "socks5://two.example.test:1080"]`)
	if failed.Fails != 1 || time.Until(failed.Until) < 23*time.Hour {
		t.Fatal("refresh reset proxy health")
	}
	for _, e := range p.entries {
		e.FirstSeen = time.Now().Add(-31 * 24 * time.Hour)
	}
	p.retire(time.Now())
	if len(p.entries) != 2 {
		t.Fatal("listed entries were retired")
	}
	p.entries["http://one.example.test:8000"].LastSuccess = time.Now()
	refresh(`[]`)
	if len(p.entries) != 1 || p.entries["http://one.example.test:8000"] == nil {
		t.Fatal("recently working inventory was not retained")
	}
}

func TestParseFeedsAndRejectMalformedEndpoints(t *testing.T) {
	for _, tc := range []struct {
		body string
		want []string
		bad  bool
	}{
		{"# comment\nexample.test:8080\nsocks5://example.test:1080\n", []string{"http://example.test:8080", "socks5h://example.test:1080"}, false},
		{`["https://example.test:443",{"proxy":"http://example.test:8000/"},"https://example.test:443",42]`, []string{"https://example.test:443", "http://example.test:8000"}, false},
		{`[]`, nil, false},
		{`{"message":"not a feed"}`, nil, true},
		{`["http://example.test:0", "http://example.test:99999", "file:///tmp/a", "direct", "http://example.test:80/path", "socks4://example.test:1080"]`, nil, true},
	} {
		got, err := parseFeed([]byte(tc.body))
		if (err != nil) != tc.bad || !slices.Equal(got, tc.want) {
			t.Errorf("parse %q = %v, %v", tc.body, got, err)
		}
	}
}

func TestScoringPrefersMeasuredBandwidthWithoutRewardingRefusals(t *testing.T) {
	p := testPool(t, []string{"http://fast.example.test:80", "http://slow.example.test:80", "http://refused.example.test:80"}, nil)
	var entries []*entry
	for raw, e := range p.entries {
		s := e.stat("keep2share")
		s.Tries, s.OK, s.Duration = 20, 10, 2
		s.BytesPerSecond = 1 << 20
		if raw == "http://fast.example.test:80" {
			s.BytesPerSecond = 8 << 20
		}
		if raw == "http://refused.example.test:80" {
			s.OK, s.Bad, s.Duration, s.BytesPerSecond = 0, 10, 0.01, 16<<20
		}
		entries = append(entries, e)
	}
	counts := make(map[string]int)
	for range 1000 {
		counts[p.selectEntry("keep2share", entries).URL]++
	}
	if counts["http://fast.example.test:80"] < 900 || counts["http://refused.example.test:80"] > 10 {
		t.Fatalf("bandwidth/reliability ranking: %v", counts)
	}
	// A large untried feed gets one exploration opportunity, not a thousand.
	for i := range 1000 {
		e := p.ensure(fmt.Sprintf("http://new-%d.example.test:80", i), time.Now())
		entries = append(entries, e)
	}
	fast := 0
	for range 1000 {
		if p.selectEntry("keep2share", entries).URL == "http://fast.example.test:80" {
			fast++
		}
	}
	if fast < 900 {
		t.Fatalf("untried inventory swamped the proven route: %d/1000", fast)
	}
}

func TestDatabaseLockFailsPromptly(t *testing.T) {
	p := testPool(t, []string{Direct}, nil)
	start := time.Now()
	if other, err := Open(p.db.Path(), []string{Direct}, nil, p.client, p.log); err == nil {
		other.Close()
		t.Fatal("opened the same database twice")
	}
	if time.Since(start) > 3*config.ProxyDBTimeout {
		t.Fatal("database lock was unbounded")
	}
}
