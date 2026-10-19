// SPDX-License-Identifier: MIT

package download

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/extractor"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
	"github.com/JohanLindvall/HeapLeach/internal/proxy"
)

// quotaRefused reads 509 as the address's quota used up, for as long as
// X-MEGA-Time-Left says, as mega's extractor does.
func quotaRefused(_ context.Context, status int, header http.Header) error {
	if status != 509 {
		return nil
	}
	left, _ := strconv.Atoi(header.Get("X-MEGA-Time-Left"))
	return &extractor.WaitError{Until: time.Now().Add(time.Duration(left) * time.Second), Reason: "transfer quota used up"}
}

func overQuota(w http.ResponseWriter, left string) {
	w.Header().Set("X-MEGA-Time-Left", left)
	w.WriteHeader(509)
}

var (
	directFirstPace = &extractor.Pace{Group: "mega", Files: 1, PerRoute: true, DirectFirst: true}
	routedQuotaPace = &extractor.Pace{Group: "mega", Files: 1, PerRoute: true}
)

func addQuotaFile(m *Manager, size int64, pace *extractor.Pace, resolve func(context.Context) (*extractor.Target, error)) *Item {
	m.mu.Lock()
	defer m.mu.Unlock()
	job := &Job{ID: newID(), Source: "http://quota.example.test/share"}
	m.jobs[job.ID] = job
	m.order = append(m.order, job.ID)
	it := m.newItem(job, extractor.File{Name: "First.bin", Size: size, Resolve: resolve, Refused: quotaRefused, Pace: pace}, "", 0)
	job.Items = append(job.Items, it)
	m.queue = append(m.queue, it)
	return it
}

func serveFile(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/octet-stream")
	io.WriteString(w, "file")
}

// A file that starts on the normal connection goes on through a proxy once
// that connection's quota is used up, asking the normal connection once.
func TestAQuotaSpentOnTheNormalConnectionMovesTheFileOntoRoutes(t *testing.T) {
	m := busyManager(t)
	var normal, routed atomic.Int32
	direct := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		normal.Add(1)
		overQuota(w, "3600")
	}))
	t.Cleanup(direct.Close)
	a := proxyServer(t, "127.0.0.2", serveFile)
	b := proxyServer(t, "127.0.0.3", serveFile)
	attachPool(t, m, []string{a.URL, b.URL})
	it := addQuotaFile(m, 4, directFirstPace, func(ctx context.Context) (*extractor.Target, error) {
		if httpx.RouteID(ctx) == "" {
			return &extractor.Target{URL: direct.URL + "/file", Size: 4}, nil
		}
		routed.Add(1)
		return &extractor.Target{URL: "http://storage.example.test/file", Size: 4}, nil
	})
	m.mu.Lock()
	unpaced := it.pace == nil
	m.mu.Unlock()
	if !unpaced {
		t.Fatal("the file was paced before the normal connection had to wait")
	}
	m.Start()
	m.signal()
	waitFor(t, 5*time.Second, func() bool { m.mu.Lock(); defer m.mu.Unlock(); return it.Status == StatusDone })
	if normal.Load() != 1 || routed.Load() != 1 {
		t.Fatalf("the normal connection was asked %d times and a route resolved %d; want once each",
			normal.Load(), routed.Load())
	}
	if body, err := os.ReadFile(filepath.Join(m.DownloadDir(), "First.bin")); err != nil || string(body) != "file" {
		t.Fatalf("downloaded %q, %v", body, err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if it.pace != directFirstPace || it.proxyRetries != 0 {
		t.Errorf("pace %+v after %d route retries; want the group's, at no cost", it.pace, it.proxyRetries)
	}
}

// Without proxies there is nowhere else to go, and the file waits in the
// queue for the quota rather than failing.
func TestWithoutProxiesAQuotaSpentWaitsInTheQueue(t *testing.T) {
	m := busyManager(t)
	var asked atomic.Int32
	direct := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		asked.Add(1)
		overQuota(w, "5400")
	}))
	t.Cleanup(direct.Close)
	it := addQuotaFile(m, 4, directFirstPace, func(context.Context) (*extractor.Target, error) {
		return &extractor.Target{URL: direct.URL + "/file", Size: 4}, nil
	})
	m.Start()
	m.signal()
	waitFor(t, 5*time.Second, func() bool {
		m.mu.Lock()
		defer m.mu.Unlock()
		return it.Status == StatusQueued && !it.notBefore.IsZero()
	})
	m.mu.Lock()
	defer m.mu.Unlock()
	if left := time.Until(it.notBefore); left < 89*time.Minute || left > 91*time.Minute {
		t.Errorf("the file waits %s; want the 90 minutes the host named", left.Round(time.Minute))
	}
	if note := m.itemNoteLocked(it); !strings.Contains(note, "quota") || it.pace != nil {
		t.Errorf("waiting as %q with pace %+v; want the quota's wait, still on the normal connection", note, it.pace)
	}
	if asked.Load() != 1 {
		t.Errorf("the host was asked %d times; want once", asked.Load())
	}
}

// A file waiting for the normal connection's quota goes on through proxies
// as soon as they are switched on, rather than at the end of the wait.
func TestEnablingProxiesMovesAFileWaitingForTheQuota(t *testing.T) {
	m := busyManager(t)
	direct := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		overQuota(w, "5400")
	}))
	t.Cleanup(direct.Close)
	it := addQuotaFile(m, 4, directFirstPace, func(ctx context.Context) (*extractor.Target, error) {
		if httpx.RouteID(ctx) == "" {
			return &extractor.Target{URL: direct.URL + "/file", Size: 4}, nil
		}
		return &extractor.Target{URL: "http://storage.example.test/file", Size: 4}, nil
	})
	m.Start()
	m.signal()
	waitFor(t, 5*time.Second, func() bool {
		m.mu.Lock()
		defer m.mu.Unlock()
		return it.Status == StatusQueued && !it.notBefore.IsZero()
	})
	a := proxyServer(t, "127.0.0.2", serveFile)
	attachPool(t, m, []string{a.URL})
	waitFor(t, 5*time.Second, func() bool { m.mu.Lock(); defer m.mu.Unlock(); return it.Status == StatusDone })
}

// A route whose quota is used up is held back from the host until the
// quota is back, and the file goes on through another, at no cost to it.
func TestARouteWhoseQuotaIsSpentIsHeldBackAndTheFileGoesOn(t *testing.T) {
	m := busyManager(t)
	var refusals atomic.Int32
	handler := func(w http.ResponseWriter, r *http.Request) {
		if refusals.CompareAndSwap(0, 1) {
			overQuota(w, "5400")
			return
		}
		serveFile(w, r)
	}
	a := proxyServer(t, "127.0.0.2", handler)
	b := proxyServer(t, "127.0.0.3", handler)
	pool := attachPool(t, m, []string{a.URL, b.URL})
	it := addQuotaFile(m, 4, routedQuotaPace, func(context.Context) (*extractor.Target, error) {
		return &extractor.Target{URL: "http://storage.example.test/file", Size: 4}, nil
	})
	m.Start()
	m.signal()
	waitFor(t, 5*time.Second, func() bool { m.mu.Lock(); defer m.mu.Unlock(); return it.Status == StatusDone })
	cooling := pool.Page("mega", proxy.Query{Status: "cooling"})
	if cooling.Total != 1 {
		t.Fatalf("%d routes cooling; want the one whose quota is used up", cooling.Total)
	}
	if left := time.Until(cooling.Rows[0].CooldownUntil); left < 89*time.Minute {
		t.Errorf("the route is held back for %s; want the 90 minutes the host named", left.Round(time.Minute))
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if it.proxyRetries != 0 {
		t.Errorf("the quota spent %d of the file's route retries", it.proxyRetries)
	}
}

// A quota used up partway through a file is not asked again in place: the
// address refuses every request until it is back. What arrived stays on disk.
func TestAQuotaSpentPartwayIsNotRetriedInPlace(t *testing.T) {
	m := progressManager(t, 3)
	const size = 256 << 10
	payload := bytes.Repeat([]byte("x"), size)
	var asked atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if asked.Add(1) > 1 {
			overQuota(w, "5400")
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Range", fmt.Sprintf("bytes 0-%d/%d", size-1, size))
		w.Header().Set("Content-Length", strconv.Itoa(size))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(payload[:size/2])
		w.(http.Flusher).Flush()
		panic(http.ErrAbortHandler)
	}))
	t.Cleanup(srv.Close)
	it := addQuotaFile(m, size, directFirstPace, func(context.Context) (*extractor.Target, error) {
		return &extractor.Target{URL: srv.URL + "/file", Size: size}, nil
	})
	m.Start()
	m.signal()
	waitFor(t, 10*time.Second, func() bool {
		m.mu.Lock()
		defer m.mu.Unlock()
		return it.Status == StatusQueued && !it.notBefore.IsZero()
	})
	if asked.Load() != 2 {
		t.Errorf("the host was asked %d times; want the transfer and the one refused request after it", asked.Load())
	}
	if !m.startedOnDisk(it) {
		t.Error("what arrived before the refusal was not kept")
	}
}
