// SPDX-License-Identifier: MIT

package download

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/extractor"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
	"github.com/JohanLindvall/HeapLeach/internal/proxy"
)

func proxyServer(t *testing.T, address string, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewUnstartedServer(handler)
	srv.Listener.Close()
	listener, err := net.Listen("tcp", address+":0")
	if err != nil {
		t.Fatal(err)
	}
	srv.Listener = listener
	srv.Start()
	t.Cleanup(srv.Close)
	return srv
}

func attachPool(t *testing.T, m *Manager, endpoints []string) *proxy.Pool {
	t.Helper()
	m.cfg.ProxyRetries = 2
	p, err := proxy.Open(filepath.Join(t.TempDir(), "proxies.db"), endpoints, nil,
		httpx.New("test", "en", 0, time.Second), m.log)
	if err != nil {
		t.Fatal(err)
	}
	m.SetProxyPool(p)
	t.Cleanup(func() {
		m.Close()
		if err := p.Close(); err != nil {
			t.Error(err)
		}
	})
	return p
}

func addProxyFiles(m *Manager, count int, resolve func(context.Context) (*extractor.Target, error)) *Job {
	m.mu.Lock()
	defer m.mu.Unlock()
	job := &Job{ID: newID(), Source: "http://service.example.test/collection"}
	m.jobs[job.ID] = job
	m.order = append(m.order, job.ID)
	for i := range count {
		it := m.newItem(job, extractor.File{Name: fmt.Sprintf("file-%d.bin", i), Size: -1,
			Resolve: resolve, Pace: &extractor.Pace{Files: 1, Streams: 1, Group: "keep2share", PerRoute: true}}, "", i)
		job.Items = append(job.Items, it)
		m.queue = append(m.queue, it)
	}
	return job
}

func TestProxyDownloadsOverlapAndKeepResolverAndFileOnTheirRoute(t *testing.T) {
	m := busyManager(t)
	m.limit = 4
	arrived := make(chan string, 4)
	finish := make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(finish) })
	payload := strings.Repeat("x", 128<<10)
	var endpoints []string
	for i, address := range []string{"127.0.0.2", "127.0.0.3"} {
		id := fmt.Sprint(i)
		var active atomic.Int32
		srv := proxyServer(t, address, func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/ticket" {
				fmt.Fprint(w, id)
				return
			}
			if r.URL.Path != "/file/"+id {
				t.Errorf("ticket changed routes: path=%s route=%s", r.URL.Path, id)
			}
			if active.Add(1) != 1 {
				t.Error("two files used the same egress simultaneously")
			}
			defer active.Add(-1)
			arrived <- id
			select {
			case <-finish:
			case <-r.Context().Done():
				return
			}
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Header().Set("Content-Length", fmt.Sprint(len(payload)))
			fmt.Fprint(w, payload)
		})
		endpoints = append(endpoints, srv.URL)
	}
	attachPool(t, m, endpoints)
	client := httpx.New("test", "en", 0, time.Second)
	job := addProxyFiles(m, 4, func(ctx context.Context) (*extractor.Target, error) {
		id, err := client.GetString(ctx, "http://api.example.test/ticket", nil)
		if err != nil {
			return nil, err
		}
		return &extractor.Target{URL: "http://storage.example.test/file/" + id, Size: int64(len(payload))}, nil
	})
	m.Start()
	m.signal()
	ids := make(map[string]bool)
	for range 2 {
		select {
		case id := <-arrived:
			ids[id] = true
		case <-time.After(5 * time.Second):
			t.Fatal("downloads did not overlap")
		}
	}
	if len(ids) != 2 {
		t.Fatal("parallel transfers shared an address")
	}
	once.Do(func() { close(finish) })
	waitFor(t, 5*time.Second, func() bool {
		m.mu.Lock()
		defer m.mu.Unlock()
		for _, it := range job.Items {
			if it.Status != StatusDone {
				return false
			}
		}
		return true
	})
	for i := range 4 {
		body, err := os.ReadFile(filepath.Join(m.DownloadDir(), fmt.Sprintf("file-%d.bin", i)))
		if err != nil || string(body) != payload {
			t.Fatalf("file %d: bytes=%d err=%v", i, len(body), err)
		}
	}
}

func TestProxyCooldownRotatesAndDoesNotConsumeAnotherWorker(t *testing.T) {
	m := busyManager(t)
	var seen sync.Map
	var attempted atomic.Int32
	srv := proxyServer(t, "127.0.0.2", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		io.WriteString(w, "file")
	})
	other := proxyServer(t, "127.0.0.3", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		io.WriteString(w, "file")
	})
	pool := attachPool(t, m, []string{srv.URL, other.URL})
	job := addProxyFiles(m, 1, func(ctx context.Context) (*extractor.Target, error) {
		id := httpx.RouteID(ctx)
		if _, loaded := seen.LoadOrStore(id, true); loaded {
			t.Error("returned to a route still cooling")
		}
		if attempted.Add(1) == 1 {
			return nil, &extractor.WaitError{Until: time.Now().Add(time.Hour), Reason: "free-download limit"}
		}
		return &extractor.Target{URL: "http://storage.example.test/file", Size: 4}, nil
	})
	m.Start()
	m.signal()
	waitFor(t, 5*time.Second, func() bool { m.mu.Lock(); defer m.mu.Unlock(); return job.Items[0].Status == StatusDone })
	if attempted.Load() != 2 {
		t.Fatalf("attempts=%d", attempted.Load())
	}
	lease := pool.Acquire("keep2share", "", 1)
	if lease == nil {
		t.Fatal("successful route was not released")
	}
	if pool.Acquire("keep2share", "", 1) != nil {
		t.Fatal("cooldown was lost")
	}
	lease.Release()
}

func TestProxyTransportFailureRetriesOnAnotherRoute(t *testing.T) {
	m := busyManager(t)
	var first atomic.Bool
	first.Store(true)
	handler := func(w http.ResponseWriter, r *http.Request) {
		if first.Swap(false) {
			conn, _, _ := w.(http.Hijacker).Hijack()
			conn.Close()
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		io.WriteString(w, "file")
	}
	a := proxyServer(t, "127.0.0.2", handler)
	b := proxyServer(t, "127.0.0.3", handler)
	attachPool(t, m, []string{a.URL, b.URL})
	job := addProxyFiles(m, 1, func(ctx context.Context) (*extractor.Target, error) {
		return &extractor.Target{URL: "http://storage.example.test/file", Size: 4}, nil
	})
	m.Start()
	m.signal()
	waitFor(t, 5*time.Second, func() bool { m.mu.Lock(); defer m.mu.Unlock(); return job.Items[0].Status == StatusDone })
}

func TestUnavailableProxyRoutesDoNotBlockOtherHosts(t *testing.T) {
	m := busyManager(t)
	pool := attachPool(t, m, []string{"http://proxy.example.test:8000"})
	lease := pool.Acquire("keep2share", "", 1)
	defer lease.Release()
	job := addProxyFiles(m, 2, nil)
	m.mu.Lock()
	defer m.mu.Unlock()
	free := m.newItem(job, extractor.File{Name: "other.bin", URL: "https://other.example.test/file"}, "", 2)
	m.queue = append(m.queue, free)
	if got := m.nextLocked(); got != free {
		t.Fatalf("dispatch=%v; unrelated host was blocked", got)
	}
	if len(m.queue) != 2 || m.queue[0].route != nil {
		t.Fatal("waiting files consumed a route or disappeared")
	}
}

func TestCancelReleasesRouteWithoutCoolingIt(t *testing.T) {
	m := busyManager(t)
	started := make(chan struct{})
	srv := proxyServer(t, "127.0.0.2", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", "4")
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		close(started)
		<-r.Context().Done()
	})
	pool := attachPool(t, m, []string{srv.URL})
	job := addProxyFiles(m, 1, func(ctx context.Context) (*extractor.Target, error) {
		return &extractor.Target{URL: "http://storage.example.test/file", Size: 4}, nil
	})
	m.Start()
	m.signal()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("transfer did not start")
	}
	if err := m.CancelJob(job.ID); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, func() bool { m.mu.Lock(); defer m.mu.Unlock(); return !job.Items[0].inFlight })
	lease := pool.Acquire("keep2share", "", 1)
	if lease == nil {
		t.Fatal("cancelled download leaked or penalised its route")
	}
	lease.Release()
}

func TestRouteFailuresAreBoundedAndManualRetryResetsBudget(t *testing.T) {
	m := busyManager(t)
	var calls atomic.Int32
	handler := func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(http.StatusForbidden) }
	a := proxyServer(t, "127.0.0.2", handler)
	b := proxyServer(t, "127.0.0.3", handler)
	attachPool(t, m, []string{a.URL, b.URL})
	m.cfg.ProxyRetries = 1
	job := addProxyFiles(m, 1, func(ctx context.Context) (*extractor.Target, error) {
		return &extractor.Target{URL: "http://storage.example.test/file", Size: 4}, nil
	})
	m.Start()
	m.signal()
	waitFor(t, 5*time.Second, func() bool { m.mu.Lock(); defer m.mu.Unlock(); return job.Items[0].Status == StatusFailed })
	if calls.Load() != 2 {
		t.Fatalf("proxy requests=%d; want initial attempt plus one retry", calls.Load())
	}
	if err := m.RetryItem(job.ID, job.Items[0].ID); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if job.Items[0].proxyRetries != 0 {
		t.Fatal("manual retry inherited its exhausted budget")
	}
}

func TestProxyChangeResumesBytesAndDoesNotSpendRetriesOnProgress(t *testing.T) {
	m := busyManager(t)
	payload := bytes.Repeat([]byte("synthetic data "), 20000)
	var started atomic.Bool
	var resumed atomic.Int64
	handler := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Accept-Ranges", "bytes")
		w.Header().Set("ETag", `"synthetic-etag"`)
		if !started.Swap(true) {
			w.Header().Set("Content-Length", fmt.Sprint(len(payload)))
			w.Write(payload[:len(payload)/2])
			return // deliberate truncated body: the next route must resume
		}
		rangeValue, _, _ := strings.Cut(strings.TrimPrefix(r.Header.Get("Range"), "bytes="), "-")
		offset, err := strconv.Atoi(rangeValue)
		if err != nil || offset <= 0 || offset >= len(payload) {
			t.Errorf("resume Range=%q", r.Header.Get("Range"))
			w.WriteHeader(400)
			return
		}
		resumed.Store(int64(offset))
		w.Header().Set("Content-Length", fmt.Sprint(len(payload)-offset))
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", offset, len(payload)-1, len(payload)))
		w.WriteHeader(http.StatusPartialContent)
		w.Write(payload[offset:])
	}
	a := proxyServer(t, "127.0.0.2", handler)
	b := proxyServer(t, "127.0.0.3", handler)
	attachPool(t, m, []string{a.URL, b.URL})
	m.cfg.ProxyRetries = 0 // progress still allows a retry
	job := addProxyFiles(m, 1, func(ctx context.Context) (*extractor.Target, error) {
		return &extractor.Target{URL: "http://storage.example.test/file", Size: int64(len(payload))}, nil
	})
	m.Start()
	m.signal()
	waitFor(t, 5*time.Second, func() bool { m.mu.Lock(); defer m.mu.Unlock(); return job.Items[0].Status.Terminal() })
	got, err := os.ReadFile(filepath.Join(m.DownloadDir(), "file-0.bin"))
	if err != nil || !bytes.Equal(got, payload) || resumed.Load() <= 0 {
		t.Fatalf("resumed=%d bytes=%d err=%v state=%+v", resumed.Load(), len(got), err, m.Snapshot().Jobs)
	}
}
