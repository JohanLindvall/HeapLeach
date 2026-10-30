// SPDX-License-Identifier: MIT

package download

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
	"github.com/JohanLindvall/HeapLeach/internal/proxy"
)

func TestExitProbeFallbackRetainsRouteIdentity(t *testing.T) {
	m := busyManager(t)
	var calls atomic.Int32
	var mu sync.Mutex
	var agent string
	server := proxyServer(t, "127.0.0.2", func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		mu.Lock()
		if agent == "" {
			agent = r.UserAgent()
		} else if r.UserAgent() != agent {
			t.Error("probe fallback changed the route's browser identity")
		}
		mu.Unlock()
		if r.URL.Path == "/first" {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		io.WriteString(w, `{"ip":"198.51.100.9"}`)
	})
	pool := attachPool(t, m, []string{server.URL})
	m.cfg.ProxyExitProbes = []string{"http://echo.example.test/first", "http://echo.example.test/second"}
	route := pool.Acquire(config.GroupKeep2Share, "", 1)
	if route == nil {
		t.Fatal("no route")
	}
	defer route.Release()
	for range 2 {
		if err := m.claimExit(t.Context(), nil, route); err != nil {
			t.Fatal(err)
		}
	}
	if route.Exit() != "198.51.100.9" || !route.Claimed() || calls.Load() != 2 {
		t.Fatalf("exit=%s claimed=%v calls=%d", route.Exit(), route.Claimed(), calls.Load())
	}
	route.Release()
	if next := pool.Acquire(config.GroupKeep2Share, route.ID(), 1); next == nil {
		t.Fatal("probe service refusal cooled the destination scope")
	} else {
		next.Release()
	}
}

func TestExitProbeOutagePolicyPreservesRetryAccounting(t *testing.T) {
	for _, reachable := range []bool{false, true} {
		t.Run(map[bool]string{false: "outage", true: "broken route"}[reachable], func(t *testing.T) {
			m := busyManager(t)
			var directCalls atomic.Int32
			echo := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				directCalls.Add(1)
				if reachable {
					io.WriteString(w, "192.0.2.1")
				} else {
					w.WriteHeader(http.StatusServiceUnavailable)
				}
			}))
			defer echo.Close()
			server := proxyServer(t, "127.0.0.2", func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusForbidden)
			})
			pool := attachPool(t, m, []string{server.URL})
			m.cfg.ProxyExitProbes = []string{echo.URL}
			route := pool.Acquire(config.GroupKeep2Share, "", 1)
			if route == nil {
				t.Fatal("no route")
			}
			defer route.Release()
			err := m.claimExit(t.Context(), nil, route)
			if reachable {
				var routed *httpx.RouteError
				if !errors.Is(err, proxy.ErrExitProbeFailed) || !errors.As(err, &routed) || !routeAtFault(err) {
					t.Fatalf("confirmed proxy failure consumed the file's retries: %v", err)
				}
			} else if err != nil || route.Claimed() {
				t.Fatalf("probe outage stopped the route: %v", err)
			}
			if up, err := m.echoReachable(t.Context()); err != nil || up != reachable || directCalls.Load() != 1 {
				t.Fatalf("outage check was not cached: up=%v err=%v calls=%d", up, err, directCalls.Load())
			}
			route.Release()
			next := pool.Acquire(config.GroupKeep2Share, "", 1)
			if reachable && next != nil {
				next.Release()
				t.Fatal("confirmed broken route was not shelved")
			}
			if !reachable {
				if next == nil {
					t.Fatal("probe outage penalized route health")
				}
				next.Release()
			}
		})
	}
}

func TestExitProbeCanceledOutageCheckIsNotCached(t *testing.T) {
	m := busyManager(t)
	started := make(chan struct{})
	echo := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
	}))
	defer echo.Close()
	server := proxyServer(t, "127.0.0.2", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	pool := attachPool(t, m, []string{server.URL})
	m.cfg.ProxyExitProbes = []string{echo.URL}
	route := pool.Acquire(config.GroupKeep2Share, "", 1)
	if route == nil {
		t.Fatal("no route")
	}
	defer route.Release()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	finished := make(chan error, 1)
	go func() { finished <- m.claimExit(ctx, nil, route) }()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("normal-route availability check never started")
	}
	cancel()
	if err := <-finished; !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled probe: %v", err)
	}
	if !m.echoCheckedAt.IsZero() || route.Claimed() {
		t.Fatal("cancellation cached a false outage or claimed an exit")
	}
	route.Release()
	if next := pool.Acquire(config.GroupKeep2Share, "", 1); next == nil {
		t.Fatal("cancellation penalized the route")
	} else {
		next.Release()
	}
}
