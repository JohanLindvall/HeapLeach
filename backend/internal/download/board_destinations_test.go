// SPDX-License-Identifier: MIT

package download

import (
	"bytes"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/extractor"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

// Full hostnames preserve unknown boards' existing folders and keep boards
// with equal post IDs and file sizes from skipping each other's files.
// All registry and transfer requests terminate at the local proxy.
func TestUnknownBoardCollectionsKeepDestinations(t *testing.T) {
	for _, tc := range []struct {
		name          string
		reuseExisting bool
	}{
		{"existing files", true},
		{"independent boards", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const firstHost, secondHost = "drawings.example.test", "drawings.example.invalid"
			payloads := map[string]map[string][]byte{
				firstHost:  {"42.jpg": randomPayload(t, 12), "43.jpg": randomPayload(t, 13)},
				secondHost: {"42.jpg": randomPayload(t, 12), "43.jpg": randomPayload(t, 13)},
			}
			var mediaReads atomic.Int32
			proxy := testServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/posts":
					w.Header().Set(httpx.HeaderContentType, "text/html")
					_, _ = fmt.Fprint(w, `<html><title>Example board</title><body data-controller="posts"></body></html>`)
				case "/posts.json":
					w.Header().Set(httpx.HeaderContentType, httpx.ContentTypeJSON)
					if r.URL.Query().Get("page") == "1" {
						_, _ = fmt.Fprint(w, `[{"id":42,"file_url":"/original/first-picture.jpg","file_size":12},
							{"id":43,"file_url":"/original/second-picture.jpg","file_size":13}]`)
					} else {
						_, _ = fmt.Fprint(w, `[]`)
					}
				case "/original/first-picture.jpg", "/original/second-picture.jpg":
					mediaReads.Add(1)
					name := "42.jpg"
					if r.URL.Path == "/original/second-picture.jpg" {
						name = "43.jpg"
					}
					body := payloads[r.Host][name]
					if body == nil {
						t.Errorf("unexpected media request: %s", r.URL)
						http.NotFound(w, r)
						return
					}
					w.Header().Set(httpx.HeaderContentType, "image/jpeg")
					w.Header().Set(httpx.HeaderContentLength, strconv.Itoa(len(body)))
					_, _ = w.Write(body)
				default:
					t.Errorf("unexpected request: %s", r.URL)
					http.NotFound(w, r)
				}
			}))
			m := testManager(t, nil)
			client, err := m.client.ThroughProxy(proxy.URL)
			if err != nil {
				t.Fatal(err)
			}
			m.reg, m.client = extractor.NewRegistry(m.cfg, client), client.Streaming()
			t.Cleanup(func() {
				m.Close()
				client.CloseIdleConnections()
			})
			if tc.reuseExisting {
				folder := filepath.Join(m.DownloadDir(), firstHost+" paintings")
				if err := os.MkdirAll(folder, 0o755); err != nil {
					t.Fatal(err)
				}
				for name, body := range payloads[firstHost] {
					if err := os.WriteFile(filepath.Join(folder, name), body, 0o644); err != nil {
						t.Fatal(err)
					}
				}
			}
			m.Start()
			addAndWait := func(host string, completed int) []ItemView {
				t.Helper()
				id, err := m.Add("http://"+host+"/posts?tags=paintings", "")
				if err != nil {
					t.Fatal(err)
				}
				waitForStatus(t, m, StatusDone, completed, 10*time.Second)
				items := jobView(m, id).Items
				if len(items) != 2 {
					t.Fatalf("items=%d, want 2", len(items))
				}
				return items
			}
			checkFiles := func(host string, items []ItemView, skipped bool) {
				t.Helper()
				for _, item := range items {
					if item.Skipped != skipped || filepath.Dir(item.Path) != host+" paintings" {
						t.Errorf("path=%q skipped=%v, want folder=%q skipped=%v", item.Path, item.Skipped, host+" paintings", skipped)
					}
					body, err := os.ReadFile(filepath.Join(m.DownloadDir(), item.Path))
					want := payloads[host][item.Name]
					if err != nil || want == nil || !bytes.Equal(body, want) {
						t.Errorf("file %q does not contain its board's media: err=%v", item.Path, err)
					}
				}
			}
			first := addAndWait(firstHost, 2)
			if tc.reuseExisting {
				if got := mediaReads.Load(); got != 0 {
					t.Errorf("saved files were fetched again: media requests=%d, want 0", got)
				}
				if got := namesIn(t, m.DownloadDir()); len(got) != 1 || got[0] != firstHost+" paintings" {
					t.Errorf("collection folders=%q, want only the existing folder", got)
				}
			} else {
				second := addAndWait(secondHost, 4)
				if got := mediaReads.Load(); got != 4 {
					t.Errorf("independent boards fetched %d files, want 4", got)
				}
				checkFiles(secondHost, second, false)
			}
			checkFiles(firstHost, first, tc.reuseExisting)
		})
	}
}
