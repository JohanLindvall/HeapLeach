// SPDX-License-Identifier: MIT

package download

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/extractor"
	"github.com/JohanLindvall/HeapLeach/internal/proxy"
)

func TestAdaptiveProxyHandoffResumesAndReleasesBothLeases(t *testing.T) {
	for _, mode := range []string{"resume", "cancel", "no ranges", "disabled", "removed"} {
		t.Run(mode, func(t *testing.T) {
			m := busyManager(t)
			defer m.Close()
			payload := bytes.Repeat([]byte("synthetic bytes "), 1<<19)
			var resumed, resolutions atomic.Int64
			slow := proxyServer(t, "127.0.0.2", func(w http.ResponseWriter, r *http.Request) {
				value, _, _ := strings.Cut(strings.TrimPrefix(r.Header.Get("Range"), "bytes="), "-")
				offset, _ := strconv.Atoi(value)
				w.Header().Set("Content-Type", "application/octet-stream")
				w.Header().Set("Content-Length", fmt.Sprint(len(payload)-offset))
				w.Header().Set("ETag", `"adaptive-fixture"`)
				w.Header().Set("Accept-Ranges", "bytes")
				if mode != "no ranges" {
					w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", offset, len(payload)-1, len(payload)))
					w.WriteHeader(http.StatusPartialContent)
				}
				if offset > 0 {
					w.Write(payload[offset:])
					return
				}
				w.Write(payload[:128<<10])
				w.(http.Flusher).Flush()
				<-r.Context().Done()
			})
			fast := proxyServer(t, "127.0.0.3", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/octet-stream")
				if r.URL.Path == "/calibrate" {
					w.Write(payload[:128<<10])
					return
				}
				value, _, _ := strings.Cut(strings.TrimPrefix(r.Header.Get("Range"), "bytes="), "-")
				offset, err := strconv.Atoi(value)
				if err != nil || offset <= 0 || offset >= len(payload) {
					t.Errorf("adaptive handoff lost resumed prefix: Range=%q", r.Header.Get("Range"))
					http.Error(w, "invalid range", 400)
					return
				}
				resumed.Store(int64(offset))
				w.Header().Set("ETag", `"adaptive-fixture"`)
				w.Header().Set("Content-Length", fmt.Sprint(len(payload)-offset))
				w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", offset, len(payload)-1, len(payload)))
				w.WriteHeader(http.StatusPartialContent)
				w.Write(payload[offset:])
			})
			pool := attachPool(t, m, []string{slow.URL, fast.URL})
			ids := make(map[string]string)
			for _, row := range pool.Page("keep2share", proxy.Query{}).Rows {
				ids[row.URL] = row.ID
			}
			warm := pool.Acquire("keep2share", ids[fast.URL], 1)
			if warm == nil {
				t.Fatal("could not measure the alternative route")
			}
			_, err := m.client.GetString(warm.Context(context.Background()), "http://origin.example.test/calibrate", nil)
			warm.Release()
			if err != nil {
				t.Fatal(err)
			}
			job := addProxyFiles(m, 1, func(context.Context) (*extractor.Target, error) {
				resolutions.Add(1)
				return &extractor.Target{URL: "http://storage.example.test/file", Size: int64(len(payload))}, nil
			})
			m.mu.Lock()
			job.Items[0].preferredRoute = ids[slow.URL]
			m.mu.Unlock()
			m.Start()
			m.signal()
			it := job.Items[0]
			waitFor(t, 5*time.Second, func() bool { return it.downloaded.Load() >= 128<<10 })
			// Feed two controlled measurement windows without sleeping out
			// the production warmup. The transfer and resume are real HTTP.
			m.mu.Lock()
			now := time.Now()
			it.lastBytes, it.lastSample, it.speed = 0, now.Add(-config.ProxyUpgradeWarmup), 0
			m.sampleLocked(now)
			m.sampleLocked(now.Add(config.ProxyUpgradeInterval))
			reserved := it.routeUpgrade != nil
			if mode == "cancel" || mode == "no ranges" {
				cancelItemLocked(it)
			} else if mode == "disabled" {
				m.proxyEnabled = false
			} else if mode == "removed" {
				pool.Configure(proxy.Configuration{Endpoints: []string{slow.URL}}, true)
			}
			m.mu.Unlock()
			if reserved != (mode != "no ranges") {
				t.Fatalf("upgrade reserved=%v in mode %s", reserved, mode)
			}
			waitFor(t, 5*time.Second, func() bool {
				m.mu.Lock()
				defer m.mu.Unlock()
				return it.Status.Terminal() && !it.inFlight
			})
			if mode == "resume" || mode == "disabled" || mode == "removed" {
				got, err := os.ReadFile(filepath.Join(m.DownloadDir(), "file-0.bin"))
				usedUpgrade := resumed.Load() >= 128<<10
				if err != nil || !bytes.Equal(got, payload) || usedUpgrade != (mode == "resume") || resolutions.Load() != 2 {
					t.Fatalf("adaptive resume: bytes=%d offset=%d resolutions=%d error=%v", len(got), resumed.Load(), resolutions.Load(), err)
				}
			} else if it.Status != StatusCanceled || resolutions.Load() != 1 {
				t.Fatalf("cancellation started another transfer: status=%s resolutions=%d", it.Status, resolutions.Load())
			}
			page := pool.Page("keep2share", proxy.Query{})
			if page.Summary.Active != 0 || page.Summary.Cooling != 0 {
				t.Fatalf("handoff leaked leases or penalised cancellation: %+v", page.Summary)
			}
		})
	}
}
