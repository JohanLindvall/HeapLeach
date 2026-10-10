// SPDX-License-Identifier: MIT

package download

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
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
	m.cfg.ProxyDB = filepath.Join(t.TempDir(), "proxies.db")
	enabled := true
	if err := m.ApplySettings(Settings{Proxies: &enabled, ProxyEndpoints: &endpoints}); err != nil {
		t.Fatal(err)
	}
	return m.proxies
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
	for _, service := range proxy.Services() {
		t.Run(service, func(t *testing.T) {
			m := busyManager(t)
			m.limit = 1
			arrived := make(chan string, 4)
			finish := make(chan struct{})
			var once sync.Once
			defer once.Do(func() { close(finish) })
			payload := strings.Repeat("x", 128<<10)
			var endpoints []string
			for i, address := range []string{"127.0.0.2", "127.0.0.3", "127.0.0.4", "127.0.0.5"} {
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
			for _, it := range job.Items {
				it.pace = &extractor.Pace{Files: 1, Streams: 1, Group: service, PerRoute: true}
			}
			m.Start()
			m.signal()
			ids := make(map[string]bool)
			for n := range 3 {
				select {
				case id := <-arrived:
					ids[id] = true
				case <-time.After(5 * time.Second):
					t.Fatal("downloads did not overlap")
				}
				if n == 0 {
					if snap := m.Snapshot(); snap.Active != 1 {
						t.Fatalf("ignored initial concurrency: %d", snap.Active)
					}
					limit := 3
					if err := m.ApplySettings(Settings{Concurrency: &limit}); err != nil {
						t.Fatal(err)
					}
				}
			}
			if len(ids) != 3 {
				t.Fatal("parallel transfers shared an address")
			}
			if snap := m.Snapshot(); snap.Active != 3 || snap.Queued != 1 {
				t.Fatalf("proxy downloads must follow general concurrency: active=%d queued=%d", snap.Active, snap.Queued)
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
		})
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

// A host that turns an address away, not the file — Keep2Share's bare
// "Download is not available" — sends the file to another route, and the
// refusing one is held back from that host instead of being offered again.
func TestARefusedAddressRetriesTheFileOnAnotherRoute(t *testing.T) {
	m := busyManager(t)
	handler := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		io.WriteString(w, "file")
	}
	a := proxyServer(t, "127.0.0.2", handler)
	b := proxyServer(t, "127.0.0.3", handler)
	pool := attachPool(t, m, []string{a.URL, b.URL})
	var refused sync.Map
	var attempts atomic.Int32
	job := addProxyFiles(m, 1, func(ctx context.Context) (*extractor.Target, error) {
		attempts.Add(1)
		id := httpx.RouteID(ctx)
		if _, again := refused.Load(id); again {
			t.Error("the file was sent back to the route that refused it")
		}
		if attempts.Load() == 1 {
			refused.Store(id, true)
			return nil, &extractor.RefusedError{Err: fmt.Errorf("keep2share: Download is not available")}
		}
		return &extractor.Target{URL: "http://storage.example.test/file", Size: 4}, nil
	})
	m.Start()
	m.signal()
	waitFor(t, 5*time.Second, func() bool { m.mu.Lock(); defer m.mu.Unlock(); return job.Items[0].Status == StatusDone })
	if attempts.Load() != 2 {
		t.Fatalf("attempts=%d, want the refusal and one more route", attempts.Load())
	}
	lease := pool.Acquire("keep2share", "", 1)
	if lease == nil {
		t.Fatal("the route that served the file was not released")
	}
	defer lease.Release()
	if _, wasRefused := refused.Load(lease.ID()); wasRefused {
		t.Error("the refusing route was offered again straight away")
	}
	if pool.Acquire("keep2share", "", 1) != nil {
		t.Error("the refusing route was not held back")
	}
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

// errTestTimeout stands in for a dial's i/o timeout.
type errTestTimeout struct{}

func (errTestTimeout) Error() string   { return "i/o timeout" }
func (errTestTimeout) Timeout() bool   { return true }
func (errTestTimeout) Temporary() bool { return true }

// The shape net/http gives a proxy that never answered: a request through
// it, a proxyconnect around a dial, and the dial's own failure.
func deadProxyError() error {
	return fmt.Errorf("keep2share: requestCaptcha: %w", &httpx.RouteError{Err: &url.Error{
		Op: "Post", URL: "https://service.example.test/api/v2/requestCaptcha",
		Err: &net.OpError{Op: "proxyconnect", Net: "tcp", Err: &net.OpError{Op: "dial", Net: "tcp", Err: errTestTimeout{}}},
	}})
}

func TestRouteFailuresAreToldApart(t *testing.T) {
	reset := &net.OpError{Op: "read", Net: "tcp", Err: syscall.ECONNRESET}
	for name, tc := range map[string]struct {
		err             error
		dead, transient bool
	}{
		"unreachable proxy": {err: deadProxyError(), dead: true, transient: true},
		"unreachable SOCKS proxy": {err: &url.Error{Op: "Get", URL: "https://service.example.test/f",
			Err: &net.OpError{Op: "socks connect", Net: "tcp", Err: &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}}}, dead: true, transient: true},
		"proxy that could not reach the host": {err: &url.Error{Op: "Get", URL: "https://service.example.test/f",
			Err: &net.OpError{Op: "socks connect", Net: "tcp", Err: errors.New("host unreachable")}}},
		"reset mid-body":      {err: fmt.Errorf("read body: %w", reset), transient: true},
		"deadline":            {err: fmt.Errorf("captcha: %w", context.DeadlineExceeded), transient: true},
		"cut short":           {err: io.ErrUnexpectedEOF, transient: true},
		"unread CAPTCHAs":     {err: &extractor.TransientError{Err: errors.New("could not obtain a free download")}, transient: true},
		"premium only":        {err: errors.New("keep2share: this file requires a Premium account")},
		"cancelled by a user": {err: context.Canceled},
	} {
		if got := deadProxy(tc.err); got != tc.dead {
			t.Errorf("%s: deadProxy = %v, want %v", name, got, tc.dead)
		}
		if got := transientRouteFailure(tc.err); got != tc.transient {
			t.Errorf("%s: transientRouteFailure = %v, want %v", name, got, tc.transient)
		}
	}
}

// Public proxy lists are mostly dead addresses. Meeting more of them than
// the retry budget allows must not fail the file: they cost it nothing.
func TestUnreachableProxiesDoNotSpendTheRetryBudget(t *testing.T) {
	m := busyManager(t)
	handler := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		io.WriteString(w, "file")
	}
	a := proxyServer(t, "127.0.0.2", handler)
	b := proxyServer(t, "127.0.0.3", handler)
	attachPool(t, m, []string{a.URL, b.URL})
	unreachable := m.cfg.ProxyRetries + 3
	var attempts atomic.Int32
	job := addProxyFiles(m, 1, func(ctx context.Context) (*extractor.Target, error) {
		if int(attempts.Add(1)) <= unreachable {
			return nil, deadProxyError()
		}
		return &extractor.Target{URL: "http://storage.example.test/file", Size: 4}, nil
	})
	m.Start()
	m.signal()
	waitFor(t, 10*time.Second, func() bool {
		m.mu.Lock()
		defer m.mu.Unlock()
		return job.Items[0].Status == StatusDone || job.Items[0].Status == StatusFailed
	})
	m.mu.Lock()
	status, failure := job.Items[0].Status, job.Items[0].Err
	m.mu.Unlock()
	if status != StatusDone {
		t.Fatalf("status=%s error=%q after %d unreachable proxies; want them retried past the budget", status, failure, unreachable)
	}
}

// A deadline or a dropped connection through a proxy is retried on the next
// route; something the file itself cannot do still fails it straight away.
func TestTransientRouteFailuresRetryAndPermanentOnesDoNot(t *testing.T) {
	for name, tc := range map[string]struct {
		err      error
		attempts int32
		status   Status
	}{
		"deadline":     {err: fmt.Errorf("captcha: %w", context.DeadlineExceeded), attempts: 2, status: StatusDone},
		"reset":        {err: &net.OpError{Op: "read", Net: "tcp", Err: syscall.ECONNRESET}, attempts: 2, status: StatusDone},
		"premium only": {err: errors.New("keep2share: this file requires a Premium account"), attempts: 1, status: StatusFailed},
	} {
		t.Run(name, func(t *testing.T) {
			m := busyManager(t)
			handler := func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/octet-stream")
				io.WriteString(w, "file")
			}
			a := proxyServer(t, "127.0.0.2", handler)
			b := proxyServer(t, "127.0.0.3", handler)
			attachPool(t, m, []string{a.URL, b.URL})
			var attempts atomic.Int32
			job := addProxyFiles(m, 1, func(ctx context.Context) (*extractor.Target, error) {
				if attempts.Add(1) == 1 {
					return nil, tc.err
				}
				return &extractor.Target{URL: "http://storage.example.test/file", Size: 4}, nil
			})
			m.Start()
			m.signal()
			waitFor(t, 10*time.Second, func() bool {
				m.mu.Lock()
				defer m.mu.Unlock()
				return job.Items[0].Status == StatusDone || job.Items[0].Status == StatusFailed
			})
			m.mu.Lock()
			status := job.Items[0].Status
			m.mu.Unlock()
			if status != tc.status || attempts.Load() != tc.attempts {
				t.Fatalf("status=%s after %d attempts; want %s after %d", status, attempts.Load(), tc.status, tc.attempts)
			}
		})
	}
}
