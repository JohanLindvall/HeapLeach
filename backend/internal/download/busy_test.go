package download

import (
	"bytes"
	"context"
	"crypto/rand"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/extractor"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

// busyManager is a full manager with the busy backoff shrunk to test speed.
// The dispatcher is not started: these tests drive transfer directly.
func busyManager(t *testing.T) *Manager {
	t.Helper()
	cfg := &config.Config{
		DownloadDir: t.TempDir(), Concurrency: 1, Streams: 1,
		UserAgent: config.DefaultUserAgent, Language: config.DefaultLanguage,
		GofileSecret: config.FallbackGofileSecret,
		MaxRetries:   1, Timeout: 30 * time.Second,
	}
	client := httpx.New(cfg.UserAgent, cfg.AcceptLanguage(), cfg.MaxRetries, cfg.Timeout)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	m := New(cfg, extractor.NewRegistry(cfg, client), client, log)
	m.timings.busyBase = time.Millisecond
	m.timings.busyMax = 5 * time.Millisecond
	t.Cleanup(m.Close)
	return m
}

func alwaysHTML() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte("<!doctype html><html><body>a page</body></html>"))
	})
}

// A URL with nothing to re-resolve that only ever answers with a web page is
// a web page — a pasted link no extractor recognised — and must stop saying
// "host busy" after the cap and say that instead.
func TestBusyCapStopsResolverlessItems(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		alwaysHTML().ServeHTTP(w, r)
	}))
	defer srv.Close()

	m := busyManager(t)
	it := &Item{ID: newID(), Name: "clip.mp4", URL: srv.URL + "/clip.mp4", Size: -1}

	err := m.transfer(context.Background(), it)
	if err == nil {
		t.Fatal("a URL that is a web page resolved to a download")
	}
	if !strings.Contains(err.Error(), "serves a web page") {
		t.Errorf("the error should say what the URL actually is, got: %v", err)
	}
	// The first ask plus one per allowed wait, and then it stops.
	if got := hits.Load(); got != int32(config.BusyRetryLimit)+1 {
		t.Errorf("server saw %d requests, want %d", got, config.BusyRetryLimit+1)
	}
}

// An item that can re-resolve — a mirror set, a re-signed link — is the case
// the patience exists for, and must ride out more busy answers than the cap.
func TestBusyCapDoesNotBindItemsThatCanReResolve(t *testing.T) {
	payload := make([]byte, 64<<10)
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}

	busyAnswers := int32(config.BusyRetryLimit) + 3
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) <= busyAnswers {
			alwaysHTML().ServeHTTP(w, r)
			return
		}
		(&rangeServer{payload: payload}).handler().ServeHTTP(w, r)
	}))
	defer srv.Close()

	m := busyManager(t)
	var resolves atomic.Int32
	it := &Item{
		ID: newID(), Name: "clip.mp4", URL: srv.URL + "/clip.mp4", Size: -1,
		resolve: func(context.Context) (*extractor.Target, error) {
			resolves.Add(1)
			return &extractor.Target{URL: srv.URL + "/clip.mp4"}, nil
		},
	}

	if err := m.transfer(context.Background(), it); err != nil {
		t.Fatalf("an item with a resolver gave up on a busy host: %v", err)
	}
	if resolves.Load() == 0 {
		t.Error("the resolver was never consulted between attempts")
	}

	got, err := os.ReadFile(filepath.Join(m.cfg.DownloadDir, "clip.mp4"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Error("the file that finally downloaded differs from the source")
	}
}
