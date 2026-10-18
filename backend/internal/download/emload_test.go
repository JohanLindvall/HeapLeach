// SPDX-License-Identifier: MIT

package download

import (
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/extractor"
	"github.com/JohanLindvall/HeapLeach/internal/proxy"
)

func TestEmloadResolvesAndTransfersThroughItsProxyPool(t *testing.T) {
	for _, scenario := range []string{"free", "daily limit", "premium only"} {
		t.Run(scenario, func(t *testing.T) {
			m := busyManager(t)
			const source = "http://files.example.test/v2/file/test-id/Hint.mp4"
			payload := strings.Repeat("video bytes", 1024)
			var pages, transfers atomic.Int32
			endpoints := []string{proxy.Direct}
			for _, address := range []string{"127.0.0.2", "127.0.0.3"} {
				srv := proxyServer(t, address, func(w http.ResponseWriter, r *http.Request) {
					switch r.URL.Path {
					case "/v2/file/test-id/Hint.mp4":
						visit := pages.Add(1)
						w.Header().Set("Content-Type", "text/html")
						http.SetCookie(w, &http.Cookie{Name: "visit", Value: address, Path: "/"})
						server, button := "http%3A%2F%2Ffiles.example.test%2F", `<button class="init-free-download">Free</button>`
						if scenario == "daily limit" && visit == 1 {
							server, button = "dlreached", ""
						}
						if scenario == "premium only" {
							button = "This link is for premium only user."
						}
						fmt.Fprintf(w, `<div class="dl-file"><div class="filename">First.mp4</div><div class="filesize">10 KB</div>%s<div class="free-downloader" data-srv="%s" data-dl="%s"></div></div>`, button, server, address)
					case "/download.php":
						transfers.Add(1)
						cookie, err := r.Cookie("visit")
						if err != nil || cookie.Value != address || r.URL.Query().Get("accesstoken") != address || r.Referer() != source {
							t.Error("page cookie, token or referer changed between proxy resolution and transfer")
							http.Error(w, "wrong session", http.StatusForbidden)
							return
						}
						w.Header().Set("Content-Type", "video/mp4")
						http.ServeContent(w, r, "First.mp4", time.Time{}, strings.NewReader(payload))
					default:
						t.Errorf("unexpected proxy request: %s", r.URL.Path)
						http.NotFound(w, r)
					}
				})
				endpoints = append(endpoints, srv.URL)
			}
			pool := attachPool(t, m, endpoints)
			types := map[string]string{"emload": config.ProxyTypeHTTP}
			if err := m.ApplySettings(Settings{ProxyTypes: &types}); err != nil {
				t.Fatal(err)
			}
			u, _ := url.Parse(source)
			res, err := extractor.NewEmload(m.client).Extract(t.Context(), u, extractor.Options{})
			if err != nil || pages.Load() != 0 {
				t.Fatalf("discovery fetched a page before a route was assigned: %v", err)
			}
			m.mu.Lock()
			job := &Job{ID: newID(), Source: source}
			m.jobs[job.ID] = job
			m.order = append(m.order, job.ID)
			it := m.newItem(job, res.Files[0], "", 0)
			job.Items = append(job.Items, it)
			m.queue = append(m.queue, it)
			m.mu.Unlock()
			m.Start()
			m.signal()
			wantStatus, wantPages, wantTransfers := StatusDone, int32(1), int32(1)
			if scenario == "daily limit" {
				wantPages = 2
			} else if scenario == "premium only" {
				wantStatus, wantTransfers = StatusFailed, 0
			}
			waitFor(t, 5*time.Second, func() bool { m.mu.Lock(); defer m.mu.Unlock(); return it.Status == wantStatus })
			if pages.Load() != wantPages || transfers.Load() != wantTransfers {
				t.Fatalf("pages=%d transfers=%d; want %d and %d", pages.Load(), transfers.Load(), wantPages, wantTransfers)
			}
			if wantStatus == StatusDone {
				body, err := os.ReadFile(filepath.Join(m.DownloadDir(), "First.mp4"))
				if err != nil || string(body) != payload {
					t.Fatalf("downloaded bytes=%d err=%v", len(body), err)
				}
			}
			// Daily limits cool only the affected route for this service;
			// a premium restriction belongs to the file and cools no route.
			wantCooling := 0
			if scenario == "daily limit" {
				wantCooling = 1
			}
			if got := pool.Page("emload", proxy.Query{Status: "cooling"}).Total; got != wantCooling {
				t.Errorf("cooling routes=%d; want %d", got, wantCooling)
			}
			if got := pool.Page("keep2share", proxy.Query{Status: "cooling"}).Total; got != 0 {
				t.Errorf("a different service inherited %d cooldowns", got)
			}
		})
	}
}
