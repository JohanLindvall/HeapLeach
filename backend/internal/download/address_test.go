// SPDX-License-Identifier: MIT

package download

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/extractor"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
	"github.com/JohanLindvall/HeapLeach/internal/proxy"
)

// refuseForAddress answers as a storage host does for a link bound to the
// address that obtained it, when another address is using it.
func refuseForAddress(w http.ResponseWriter) {
	http.Error(w, "this link is assigned to another address", http.StatusConflict)
}

// boundLink is a resolver handing out links bound to the address that
// obtained them: next gives the link to hand out after n discards.
func boundLink(discards *atomic.Int32, next func(n int32) string) func(context.Context) (*extractor.Target, error) {
	return func(context.Context) (*extractor.Target, error) {
		return &extractor.Target{URL: next(discards.Load()), Size: -1, Discard: func() { discards.Add(1) }}, nil
	}
}

// The route's address changed partway through a file, so its link, bound to
// the address it was obtained from, is refused. The link is dropped, a new one
// minted from the address in use, and the file resumes from what it has.
func TestALinkRefusedForItsAddressIsMintedAgainAndResumed(t *testing.T) {
	payload := randomPayload(t, 64<<10)
	first := &droppingServer{payload: payload, chunks: []int{len(payload) / 2}}
	second := &droppingServer{payload: payload, chunks: []int{len(payload)}}
	var refusing atomic.Bool
	var resumedAt atomic.Int64
	resumedAt.Store(-1)
	mux := http.NewServeMux()
	mux.HandleFunc("/first", func(w http.ResponseWriter, r *http.Request) {
		if refusing.Swap(true) { // the next connection left by another address
			refuseForAddress(w)
			return
		}
		first.handler().ServeHTTP(w, r)
	})
	mux.HandleFunc("/second", func(w http.ResponseWriter, r *http.Request) {
		from, _ := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(r.Header.Get(httpx.HeaderRange), "bytes="), "-"))
		resumedAt.CompareAndSwap(-1, int64(from))
		second.handler().ServeHTTP(w, r)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	m := progressManager(t, 1)
	var discards atomic.Int32
	it := &Item{ID: newID(), Name: "clip.bin", Size: -1, resolve: boundLink(&discards, func(n int32) string {
		if n == 0 {
			return srv.URL + "/first"
		}
		return srv.URL + "/second"
	})}
	if err := m.transfer(t.Context(), it); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(m.DownloadDir(), "clip.bin"))
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("downloaded %d bytes (%v), want the %d-byte file", len(got), err, len(payload))
	}
	if discards.Load() != 1 {
		t.Fatalf("link discarded %d times, want once", discards.Load())
	}
	if at := resumedAt.Load(); at != int64(len(payload)/2) {
		t.Fatalf("the new link started at byte %d, want %d: what was on disk was not kept", at, len(payload)/2)
	}
}

// A link refused every time is not tried for ever: the retry budget still
// bounds attempts that get nowhere, and the refusal is what fails the file.
func TestLinksAlwaysRefusedForTheirAddressFailWithinTheBudget(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { refuseForAddress(w) }))
	t.Cleanup(srv.Close)
	m := progressManager(t, 1)
	var discards atomic.Int32
	it := &Item{ID: newID(), Name: "clip.bin", Size: -1,
		resolve: boundLink(&discards, func(n int32) string { return srv.URL + "/link" + strconv.Itoa(int(n)) })}
	err := m.transfer(t.Context(), it)
	if !httpx.HasStatus(err, http.StatusConflict) {
		t.Fatalf("error = %v; want the refusal", err)
	}
	if discards.Load() != 2 {
		t.Fatalf("link discarded %d times, want once per attempt", discards.Load())
	}
}

// Only a resolver that says its links are bound to an address has a 409 read
// that way. Any other 409 still fails the file at once.
func TestAConflictOnAnUnboundLinkStillFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { refuseForAddress(w) }))
	t.Cleanup(srv.Close)
	m := progressManager(t, 1)
	var resolves atomic.Int32
	it := &Item{ID: newID(), Name: "clip.bin", Size: -1, resolve: func(context.Context) (*extractor.Target, error) {
		resolves.Add(1)
		return &extractor.Target{URL: srv.URL + "/link", Size: -1}, nil
	}}
	start := time.Now()
	if err := m.transfer(t.Context(), it); !httpx.HasStatus(err, http.StatusConflict) {
		t.Fatalf("error = %v; want the 409", err)
	}
	if resolves.Load() != 1 || time.Since(start) > 900*time.Millisecond {
		t.Fatalf("resolved %d times in %s; want one attempt, not retried", resolves.Load(), time.Since(start))
	}
}

// Through a proxy, a route whose address changed under a link has not kept
// the address it had. The route is held back for the service and the file
// goes to another, which mints its own link; it costs the file no retry.
func TestALinkRefusedForItsAddressMovesTheFileToAnotherRoute(t *testing.T) {
	m := busyManager(t)
	route := proxyServer(t, "127.0.0.2", func(w http.ResponseWriter, r *http.Request) { refuseForAddress(w) })
	pool := attachPool(t, m, []string{route.URL})
	var discards atomic.Int32
	job := addProxyFiles(m, 1, boundLink(&discards, func(int32) string { return "http://storage.example.test/link" }))
	it := job.Items[0]
	row := pool.Page(config.GroupKeep2Share, proxy.Query{Search: route.URL}).Rows[0]
	if it.route = pool.Acquire(config.GroupKeep2Share, row.ID, 1); it.route == nil {
		t.Fatal("no route")
	}
	retries := it.proxyRetries
	err := m.transferRouted(t.Context(), it)
	if wait, ok := errors.AsType[*extractor.WaitError](err); !ok || !strings.Contains(wait.Reason, "another download route") {
		t.Fatalf("error = %v; want the file sent to another route", err)
	}
	if discards.Load() != 1 || it.proxyRetries != retries {
		t.Fatalf("discards=%d, retries %d -> %d; want the link dropped at no cost to the file",
			discards.Load(), retries, it.proxyRetries)
	}
	if row = pool.Page(config.GroupKeep2Share, proxy.Query{Search: route.URL}).Rows[0]; row.Status != "cooling" {
		t.Fatalf("route status %q, want it held back for the service", row.Status)
	}
}
