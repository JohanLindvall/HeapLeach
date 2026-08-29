package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/download"
	"github.com/JohanLindvall/HeapLeach/internal/extractor"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

// newRunManager builds a real manager writing into a temp dir, so Run is
// exercised end to end rather than against a stub.
func newRunManager(t *testing.T) *download.Manager {
	t.Helper()
	t.Setenv("HEAPLEACH_UTLS", "0")
	cfg := &config.Config{
		DownloadDir:  t.TempDir(),
		Concurrency:  2,
		UserAgent:    config.DefaultUserAgent,
		Language:     config.DefaultLanguage,
		GofileSecret: config.FallbackGofileSecret,
		Timeout:      10 * time.Second,
	}
	client := httpx.New(cfg.UserAgent, cfg.AcceptLanguage(), 0, cfg.Timeout)
	m := download.New(cfg, extractor.NewRegistry(cfg, client), client, slog.New(slog.NewTextHandler(io.Discard, nil)))
	m.Start()
	t.Cleanup(m.Close)
	return m
}

func TestRunDownloadsAndReportsClean(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", "5")
		_, _ = w.Write([]byte("hello"))
	}))
	defer server.Close()

	var out bytes.Buffer
	err := Run(t.Context(), newRunManager(t), Options{
		URLs:    []string{server.URL + "/greeting.bin"},
		Out:     &out,
		Animate: false,
		Width:   80,
	})
	if err != nil {
		t.Fatalf("Run = %v, want nil\noutput:\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "greeting.bin") {
		t.Errorf("the finished file was never announced:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "1 file,") {
		t.Errorf("no closing summary:\n%s", out.String())
	}
}

func TestRunFailsTheRunWhenADownloadFails(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "gone", http.StatusNotFound)
	}))
	defer server.Close()

	var out bytes.Buffer
	err := Run(t.Context(), newRunManager(t), Options{
		URLs:    []string{server.URL + "/missing.bin"},
		Out:     &out,
		Animate: false,
		Width:   80,
	})
	if !errors.Is(err, ErrIncomplete) {
		t.Fatalf("Run = %v, want ErrIncomplete\noutput:\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "failed") {
		t.Errorf("the failure was never reported:\n%s", out.String())
	}
}

func TestRunRefusesAnEmptyQueue(t *testing.T) {
	var out bytes.Buffer
	err := Run(t.Context(), newRunManager(t), Options{
		// Not a URL at all: the manager rejects it at Add.
		URLs:    []string{"::not a url::"},
		Out:     &out,
		Animate: false,
		Width:   80,
	})
	if err == nil || err.Error() != "no usable URLs" {
		t.Fatalf("Run = %v, want the no-usable-URLs refusal", err)
	}
}

func TestRunStopsWhenCancelled(t *testing.T) {
	// A body that never finishes, so only cancellation can end the run.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", "1000000000")
		w.WriteHeader(http.StatusOK)
		for {
			if _, err := w.Write(make([]byte, 1024)); err != nil {
				return
			}
			w.(http.Flusher).Flush()
			select {
			case <-r.Context().Done():
				return
			case <-time.After(20 * time.Millisecond):
			}
		}
	}))
	// Registered before the manager exists, so cleanup closes the manager
	// first: its connection is what the handler above is serving, and the
	// server cannot shut down while that read is still open.
	t.Cleanup(server.Close)

	mgr := newRunManager(t)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	var out bytes.Buffer
	go func() {
		done <- Run(ctx, mgr, Options{
			URLs:    []string{server.URL + "/endless.bin"},
			Out:     &out,
			Animate: true, // the animated path is the interactive default
			Width:   80,
		})
	}()

	time.Sleep(400 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run = %v, want context.Canceled", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after cancellation")
	}
	if !strings.Contains(out.String(), "interrupted") {
		t.Errorf("cancellation was not announced:\n%s", out.String())
	}
	if !strings.Contains(out.String(), ansiShowCursor) {
		t.Error("the cursor was not restored on the way out")
	}
}
