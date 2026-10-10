// SPDX-License-Identifier: MIT

package download

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/extractor"
	"github.com/JohanLindvall/HeapLeach/internal/proxy"
)

func TestLiveProxyTogglesDrainExistingRoutesAndRespectNewConcurrency(t *testing.T) {
	m := busyManager(t)
	m.cfg.ProxyDB = filepath.Join(t.TempDir(), "proxies.db")
	arrived := make(chan string, 8)
	finishDirect, finishA, finishB := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var directOnce, aOnce, bOnce sync.Once
	handler := func(id string, finish <-chan struct{}) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			arrived <- id
			select {
			case <-finish:
			case <-r.Context().Done():
				return
			}
			w.Header().Set("Content-Type", "application/octet-stream")
			fmt.Fprint(w, "file")
		}
	}
	origin := httptest.NewServer(handler("direct", finishDirect))
	defer origin.Close()
	defer directOnce.Do(func() { close(finishDirect) })
	defer aOnce.Do(func() { close(finishA) })
	defer bOnce.Do(func() { close(finishB) })
	a, b := proxyServer(t, "127.0.0.2", handler("a", finishA)), proxyServer(t, "127.0.0.3", handler("b", finishB))
	job := addProxyFiles(m, 4, func(context.Context) (*extractor.Target, error) {
		return &extractor.Target{URL: origin.URL, Size: 4}, nil
	})
	m.Start()
	m.signal()
	take := func(want string) {
		t.Helper()
		select {
		case got := <-arrived:
			if got != want && want != "proxy" {
				t.Fatalf("route=%s, want %s", got, want)
			}
			if want == "proxy" && got == "direct" {
				t.Fatal("used direct while proxies were enabled")
			}
		case <-time.After(5 * time.Second):
			t.Fatal("queued transfer did not start")
		}
	}
	take("direct")
	enabled, limit, endpoints := true, 3, []string{a.URL, b.URL}
	if err := m.ApplySettings(Settings{Proxies: &enabled, Concurrency: &limit, ProxyEndpoints: &endpoints}); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	blocked := m.nextLocked() == nil && m.running == 1
	m.mu.Unlock()
	if !blocked {
		t.Fatal("enabling overlapped an unleased direct transfer")
	}
	directOnce.Do(func() { close(finishDirect) })
	take("proxy")
	take("proxy")
	// Removing an active endpoint preserves its current lease. Disabling
	// waits for both routes before the remaining item can use direct again.
	enabled, limit, endpoints = false, 1, []string{b.URL}
	if err := m.ApplySettings(Settings{Proxies: &enabled, Concurrency: &limit, ProxyEndpoints: &endpoints}); err != nil {
		t.Fatal(err)
	}
	if snap := m.Snapshot(); snap.Proxies || snap.Concurrency != 1 || snap.Active != 2 {
		t.Fatalf("live settings interrupted the active transfers: %+v", snap)
	}
	aOnce.Do(func() { close(finishA) })
	waitFor(t, 5*time.Second, func() bool { return m.Snapshot().Active == 1 })
	select {
	case got := <-arrived:
		t.Fatalf("started %s before old routes drained", got)
	default:
	}
	bOnce.Do(func() { close(finishB) })
	take("direct")
	waitFor(t, 5*time.Second, func() bool {
		m.mu.Lock()
		defer m.mu.Unlock()
		for _, it := range job.Items {
			if it.Status != StatusDone {
				return false
			}
		}
		return m.proxyRunning == 0
	})
}

func TestEnablingAfterStartWakesForDiscoveredRoutes(t *testing.T) {
	m := busyManager(t)
	m.cfg.ProxyDB = filepath.Join(t.TempDir(), "proxies.db")
	m.SetPaused(true)
	m.Start()
	destination := proxyServer(t, "127.0.0.2", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		fmt.Fprint(w, "file")
	})
	finish := make(chan struct{})
	feed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { <-finish; fmt.Fprint(w, destination.URL) }))
	defer feed.Close()
	var once sync.Once
	defer once.Do(func() { close(finish) })
	job := addProxyFiles(m, 1, func(context.Context) (*extractor.Target, error) {
		return &extractor.Target{URL: "http://storage.example.test/file", Size: 4}, nil
	})
	m.mu.Lock()
	job.Items[0].notBefore = time.Now().Add(time.Hour)
	m.mu.Unlock()
	enabled, feeds := true, []string{feed.URL}
	if err := m.ApplySettings(Settings{Proxies: &enabled, ProxyFeeds: &feeds}); err != nil {
		t.Fatal(err)
	}
	m.SetPaused(false)
	once.Do(func() { close(finish) })
	waitFor(t, 5*time.Second, func() bool {
		m.mu.Lock()
		defer m.mu.Unlock()
		return job.Items[0].Status == StatusDone
	})
}

func TestProxyScopeAndSettingsErrorsAreAtomic(t *testing.T) {
	m := busyManager(t)
	// A failed lazy database open cannot apply the concurrency in the same request.
	enabled, limit, endpoints := true, 8, []string{proxy.Direct}
	if err := m.ApplySettings(Settings{Proxies: &enabled, Concurrency: &limit, ProxyEndpoints: &endpoints}); err == nil {
		t.Fatal("empty database path was accepted")
	}
	if snap := m.Snapshot(); snap.Proxies || snap.Concurrency != 1 {
		t.Fatal("failed settings were partially applied")
	}
	attachPool(t, m, endpoints)
	job := addProxyFiles(m, 1, nil)
	job.Items[0].pace = &extractor.Pace{Group: "another-host", PerRoute: true, Files: 1}
	m.mu.Lock()
	got := m.nextLocked()
	m.mu.Unlock()
	if got == nil || got.route != nil {
		t.Fatal("a non-K2S host was routed through the public pool")
	}
	before := m.CurrentSettings()
	bad := []string{"file:///not-a-proxy"}
	if err := m.ApplySettings(Settings{Concurrency: &limit, ProxyEndpoints: &bad}); err == nil {
		t.Fatal("accepted invalid proxy")
	}
	if m.Snapshot().Concurrency != *before.Concurrency || m.ProxyPage(proxy.Query{}).Total != 1 {
		t.Fatal("invalid live source edit changed the settings")
	}
}

func TestConcurrentProxySettingsAndShutdownReleaseDatabase(t *testing.T) {
	m := busyManager(t)
	attachPool(t, m, []string{proxy.Direct})
	m.Start()
	var wg sync.WaitGroup
	for i := range 12 {
		wg.Go(func() {
			enabled := i%2 == 0
			if err := m.ApplySettings(Settings{Proxies: &enabled}); err != nil && !errors.Is(err, ErrClosed) {
				t.Error(err)
			}
			m.CurrentSettings()
			m.ProxyPage(proxy.Query{})
		})
	}
	m.Close()
	wg.Wait()
	p, err := proxy.Open(m.cfg.ProxyDB, []string{proxy.Direct}, nil, m.client, m.log)
	if err != nil {
		t.Fatalf("manager leaked the database: %v", err)
	}
	p.Close()
}
