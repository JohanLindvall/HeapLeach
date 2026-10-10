// SPDX-License-Identifier: MIT

package download

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/extractor"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
	"github.com/JohanLindvall/HeapLeach/internal/proxy"
)

func TestProxyZeroByteStallsPenaliseTheRouteAndRetry(t *testing.T) {
	for _, tc := range []struct {
		name                            string
		headers, unknown, playlist, key bool
	}{
		{name: "before headers", headers: true},
		{name: "known length"},
		{name: "unknown length", unknown: true},
		{name: "playlist segment", playlist: true},
		{name: "playlist key", playlist: true, key: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := busyManager(t)
			m.cfg.StallTimeout = 90 * time.Millisecond
			m.cfg.MaxRetries = 0
			// Connection setup and headers have their own request deadline;
			// the stall watchdog covers the streaming body after that.
			m.client = httpx.New("test", "en", 0, 90*time.Millisecond).Streaming()
			payload := []byte("synthetic file bytes")
			key := []byte("0123456789abcdef")
			served := payload
			if tc.key {
				served = encryptSegment(t, payload, key, make([]byte, 16))
			}
			var calls atomic.Int32
			stuck := proxyServer(t, "127.0.0.2", func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if !tc.headers {
					w.Header().Set("Content-Type", "application/octet-stream")
					if !tc.unknown {
						size := len(served)
						if tc.key {
							size = len(key)
						}
						w.Header().Set("Content-Length", fmt.Sprint(size))
					}
					w.WriteHeader(http.StatusOK)
					w.(http.Flusher).Flush()
				}
				<-r.Context().Done()
			})
			working := proxyServer(t, "127.0.0.3", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/octet-stream")
				if r.URL.Path == "/key" {
					w.Write(key)
				} else {
					w.Write(served)
				}
			})
			pool := attachPool(t, m, []string{stuck.URL, working.URL})
			m.cfg.ProxyRetries = 1
			job := addProxyFiles(m, 1, func(context.Context) (*extractor.Target, error) {
				target := &extractor.Target{URL: "http://storage.example.test/file", Size: -1}
				if tc.playlist {
					target.Segments = []string{"http://storage.example.test/segment"}
					if tc.key {
						target.SegmentKey = &extractor.SegmentKey{URI: "http://storage.example.test/key"}
					}
				}
				return target, nil
			})
			it := job.Items[0]
			row := pool.Page("keep2share", proxy.Query{Search: stuck.URL}).Rows[0]
			it.route = pool.Acquire("keep2share", row.ID, 1)
			if it.route == nil || it.route.ID() != row.ID {
				t.Fatal("did not acquire the stalled proxy")
			}
			it.preferredRoute = row.ID

			// This outer deadline only bounds a broken watchdog. A real stall
			// must be detected while the caller's context is still live.
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			err := m.transferRouted(ctx, it)
			if ctx.Err() != nil {
				t.Fatalf("zero-byte stall escaped the watchdog: %v", err)
			}
			if _, ok := errors.AsType[*extractor.WaitError](err); !ok {
				t.Fatalf("stalled transfer was not queued for another route: %v", err)
			}
			if calls.Load() != 1 || it.downloaded.Load() != 0 || it.proxyRetries != 1 || it.preferredRoute != "" {
				t.Fatalf("calls=%d bytes=%d retries=%d preferred=%q", calls.Load(), it.downloaded.Load(), it.proxyRetries, it.preferredRoute)
			}
			row = pool.Page("keep2share", proxy.Query{Search: stuck.URL}).Rows[0]
			if row.Status != "cooling" || row.Requests != 1 || row.SuccessRate != 0 || row.Active != 0 || !row.CooldownUntil.After(time.Now()) {
				t.Fatalf("stalled proxy was not penalised exactly once and released: %+v", row)
			}

			it.route = pool.Acquire("keep2share", row.ID, 1)
			if it.route == nil || it.route.ID() == row.ID {
				t.Fatal("a stalled proxy was selected again despite an available alternative")
			}
			if err := m.transferRouted(ctx, it); err != nil {
				t.Fatalf("retry through the working proxy: %v", err)
			}
			got, err := os.ReadFile(filepath.Join(m.DownloadDir(), it.Name))
			if err != nil || !bytes.Equal(got, payload) {
				t.Fatalf("downloaded %q: %v", got, err)
			}
		})
	}
}

func TestProxyBufferingWithinStallTimeoutIsNotPenalised(t *testing.T) {
	for _, playlist := range []bool{false, true} {
		t.Run(fmt.Sprintf("playlist=%t", playlist), func(t *testing.T) {
			m := busyManager(t)
			m.cfg.StallTimeout = 500 * time.Millisecond
			m.client = httpx.New("test", "en", 0, 90*time.Millisecond).Streaming()
			payload := []byte("buffered file bytes")
			key := []byte("0123456789abcdef")
			served := payload
			if playlist {
				served = encryptSegment(t, payload, key, make([]byte, 16))
			}
			srv := proxyServer(t, "127.0.0.2", func(w http.ResponseWriter, r *http.Request) {
				data := served
				if r.URL.Path == "/key" {
					data = key
				}
				w.Header().Set("Content-Type", "application/octet-stream")
				w.Header().Set("Content-Length", fmt.Sprint(len(data)))
				w.WriteHeader(http.StatusOK)
				w.(http.Flusher).Flush()
				// No bytes arrive until the proxy sends its whole buffer. The
				// header deadline must not become a streaming body deadline.
				select {
				case <-time.After(150 * time.Millisecond):
					w.Write(data)
				case <-r.Context().Done():
				}
			})
			pool := attachPool(t, m, []string{srv.URL})
			job := addProxyFiles(m, 1, func(context.Context) (*extractor.Target, error) {
				target := &extractor.Target{URL: "http://storage.example.test/file", Size: -1}
				if playlist {
					target.Segments = []string{"http://storage.example.test/segment"}
					target.SegmentKey = &extractor.SegmentKey{URI: "http://storage.example.test/key"}
				}
				return target, nil
			})
			it := job.Items[0]
			it.route = pool.Acquire("keep2share", "", 1)
			if it.route == nil {
				t.Fatal("no proxy route available")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			if err := m.transferRouted(ctx, it); err != nil {
				t.Fatalf("buffered download: %v", err)
			}
			got, err := os.ReadFile(filepath.Join(m.DownloadDir(), it.Name))
			if err != nil || !bytes.Equal(got, payload) {
				t.Fatalf("downloaded %q: %v", got, err)
			}
			row := pool.Page("keep2share", proxy.Query{}).Rows[0]
			if row.Status != "available" || row.SuccessRate != 1 || row.Active != 0 || !row.CooldownUntil.IsZero() {
				t.Fatalf("buffering penalised a working proxy: %+v", row)
			}
		})
	}
}
