// SPDX-License-Identifier: MIT

package download

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/extractor"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
	"github.com/JohanLindvall/HeapLeach/internal/proxy"
)

func TestProxyCompletedRangeUpdatesLastSuccess(t *testing.T) {
	for _, mode := range []string{"known length", "chunked", "short chunked"} {
		t.Run(mode, func(t *testing.T) {
			m := busyManager(t)
			m.streams = 1
			m.cfg.MaxRetries = 0
			payload := []byte("a synthetic completed range")
			srv := proxyServer(t, "127.0.0.2", func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/probe" {
					fmt.Fprint(w, "ready")
					return
				}
				w.Header().Set(httpx.HeaderContentType, "application/octet-stream")
				w.Header().Set(httpx.HeaderContentRange, fmt.Sprintf("bytes 0-%d/%d", len(payload)-1, len(payload)))
				if mode == "known length" {
					w.Header().Set(httpx.HeaderContentLength, fmt.Sprint(len(payload)))
				}
				w.WriteHeader(http.StatusPartialContent)
				w.(http.Flusher).Flush()
				if mode == "short chunked" {
					_, _ = w.Write(payload[:len(payload)/2])
					return
				}
				_, _ = w.Write(payload)
				if mode == "chunked" {
					w.(http.Flusher).Flush()
					// The range is complete before the HTTP EOF/trailer. A
					// bounded reader must close without waiting for either.
					<-r.Context().Done()
				}
			})
			pool := attachPool(t, m, []string{srv.URL})
			job := addProxyFiles(m, 1, func(context.Context) (*extractor.Target, error) {
				return &extractor.Target{URL: "http://storage.example.test/file", Size: int64(len(payload))}, nil
			})
			it := job.Items[0]
			it.pace.Group = config.GroupMega
			row := pool.Page(config.GroupMega, proxy.Query{Search: srv.URL}).Rows[0]
			it.route = pool.Acquire(config.GroupMega, row.ID, 1)
			if it.route == nil {
				t.Fatal("could not lease fixture proxy")
			}
			req, _ := m.client.NewRequest(it.route.Context(t.Context()), http.MethodGet, "http://storage.example.test/probe", nil)
			if _, err := m.client.Bytes(req); err != nil {
				t.Fatal(err)
			}
			before := pool.Page(config.GroupMega, proxy.Query{Search: srv.URL}).Rows[0]
			if before.LastSuccess.IsZero() || before.Requests != 1 {
				t.Fatalf("probe did not seed health: %+v", before)
			}
			err := m.transferRouted(t.Context(), it)
			if mode == "short chunked" {
				after := pool.Page(config.GroupMega, proxy.Query{Search: srv.URL}).Rows[0]
				if err == nil || !after.LastSuccess.Equal(before.LastSuccess) || after.Requests != 2 || after.SuccessRate >= 1 {
					t.Fatalf("incomplete transfer credited as successful: err=%v before=%+v after=%+v", err, before, after)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(filepath.Join(m.DownloadDir(), it.Name))
			if err != nil || !bytes.Equal(got, payload) {
				t.Fatalf("downloaded bytes=%q err=%v", got, err)
			}
			after := pool.Page(config.GroupMega, proxy.Query{Search: srv.URL}).Rows[0]
			if !after.LastSuccess.After(before.LastSuccess) || after.Requests != 2 || after.SuccessRate != 1 {
				t.Fatalf("completed transfer kept stale health: before=%+v after=%+v", before, after)
			}
			if err := pool.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := proxy.Open(m.cfg.ProxyDB, []string{srv.URL}, nil, m.client, m.log)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			saved := reopened.Page(config.GroupMega, proxy.Query{Search: srv.URL}).Rows[0]
			if !saved.LastSuccess.Equal(after.LastSuccess) || saved.Requests != after.Requests {
				t.Fatalf("successful transfer health was not persisted: %+v", saved)
			}
		})
	}
}
