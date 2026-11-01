// SPDX-License-Identifier: MIT

package cli

import (
	"bytes"
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
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/download"
	"github.com/JohanLindvall/HeapLeach/internal/extractor"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

// Finishing every discovered file does not finish a source whose later page
// failed. An intentional file cap, however, is a successful bounded request.
func TestRunDistinguishesAnIncompleteListingFromAFileCap(t *testing.T) {
	const fileBody = "synthetic gallery file\n"
	for _, capped := range []bool{false, true} {
		name := "failed next page"
		if capped {
			name = "deliberate file cap"
		}
		t.Run(name, func(t *testing.T) {
			var laterReads, fileReads atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/gallery":
					if r.URL.Query().Get("page") == "2" {
						laterReads.Add(1)
						http.Error(w, "later page unavailable", http.StatusNotFound)
						return
					}
					w.Header().Set(httpx.HeaderContentType, "text/html")
					object := fmt.Sprintf(`{"filename":"first.bin","url":"/files/first.bin","size":%d}`, len(fileBody))
					fmt.Fprintf(w, `<html><head><meta name="generator" content="Chevereto"><meta property="og:title" content="Example gallery"></head><body>`+
						`<div class="list-item" data-type="image" data-object='%s'></div>`+
						`<a data-pagination="next" href="?page=2">Next</a></body></html>`, url.QueryEscape(object))
				case "/files/first.bin":
					fileReads.Add(1)
					w.Header().Set(httpx.HeaderContentType, "application/octet-stream")
					w.Header().Set(httpx.HeaderContentLength, strconv.Itoa(len(fileBody)))
					_, _ = io.WriteString(w, fileBody)
				default:
					t.Errorf("unexpected request %s", r.URL.RequestURI())
					http.NotFound(w, r)
				}
			}))
			t.Cleanup(srv.Close)
			t.Setenv("HEAPLEACH_UTLS", "0")
			cfg := &config.Config{
				DownloadDir: t.TempDir(), Concurrency: 2,
				UserAgent: config.DefaultUserAgent, Language: config.DefaultLanguage,
				GofileSecret: config.FallbackGofileSecret, Timeout: 5 * time.Second,
			}
			if capped {
				cfg.MaxFiles = 1
			}
			client := httpx.New(cfg.UserAgent, cfg.AcceptLanguage(), 0, cfg.Timeout)
			mgr := download.New(cfg, extractor.NewRegistry(cfg, client), client, slog.New(slog.NewTextHandler(io.Discard, nil)))
			mgr.Start()
			t.Cleanup(mgr.Close)
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			var out bytes.Buffer
			err := Run(ctx, mgr, Options{URLs: []string{srv.URL + "/gallery"}, Out: &out, Width: 120})
			if capped {
				if err != nil {
					t.Errorf("capped Run = %v, want nil\n%s", err, out.String())
				}
				if !strings.Contains(out.String(), "\n✓ 1 file,") || strings.Contains(out.String(), "source listing incomplete") {
					t.Errorf("intentional cap was not reported as success:\n%s", out.String())
				}
			} else {
				if !errors.Is(err, ErrIncomplete) {
					t.Errorf("Run = %v, want ErrIncomplete\n%s", err, out.String())
				}
				if !strings.Contains(out.String(), "\n✗ 1 file,") || !strings.Contains(out.String(), "1 source listing incomplete:") {
					t.Errorf("missing failed summary and explicit incomplete-listing notice:\n%s", out.String())
				}
			}
			snap := mgr.Snapshot()
			if len(snap.Jobs) != 1 {
				t.Fatalf("got %d jobs, want 1", len(snap.Jobs))
			}
			job := snap.Jobs[0]
			if job.Status != download.StatusDone || job.Done != 1 || job.Failed != 0 || len(job.Items) != 1 || job.Incomplete == capped {
				t.Fatalf("job = %+v; want one saved file and Incomplete=%v", job, !capped)
			}
			item := job.Items[0]
			if item.Status != download.StatusDone || item.Downloaded != int64(len(fileBody)) {
				t.Errorf("saved item = %+v, want all %d bytes", item, len(fileBody))
			}
			saved, err := os.ReadFile(filepath.Join(cfg.DownloadDir, item.Path))
			if err != nil || string(saved) != fileBody {
				t.Errorf("saved bytes = %q, %v; want %q", saved, err, fileBody)
			}
			wantLater := int32(1)
			if capped {
				wantLater = 0
			}
			if laterReads.Load() != wantLater || fileReads.Load() == 0 {
				t.Errorf("later-page reads=%d, file reads=%d; want %d later-page reads and a completed transfer", laterReads.Load(), fileReads.Load(), wantLater)
			}
		})
	}
}
