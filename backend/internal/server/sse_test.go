package server

import (
	"bufio"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/download"
	"github.com/JohanLindvall/HeapLeach/internal/extractor"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
	"github.com/JohanLindvall/HeapLeach/internal/webui"
)

// An open /api/events stream must end when the manager closes. It does not
// end on its own, and http.Server.Shutdown waits for in-flight requests
// without cancelling their contexts — so if the stream ignored the manager,
// a single open browser tab would hold shutdown until the deadline expired
// ("shutdown: context deadline exceeded").
func TestEventStreamEndsWhenManagerCloses(t *testing.T) {
	manager, handler := newTestServer(t)

	srv := httptest.NewServer(handler)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/events")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %s", resp.Status)
	}
	if got := resp.Header.Get("Content-Type"); got != "text/event-stream; charset=utf-8" {
		t.Errorf("content-type = %q", got)
	}

	// The first frame is the current state, sent immediately.
	reader := bufio.NewReader(resp.Body)
	line, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("reading the initial frame: %v", err)
	}
	if len(line) < 6 || line[:6] != "data: " {
		t.Fatalf("first line = %q, want a data frame", line)
	}

	done := make(chan error, 1)
	go func() {
		_, err := io.Copy(io.Discard, reader)
		done <- err
	}()

	manager.Close()

	select {
	case <-done:
		// The stream ended, which is what lets Shutdown return promptly.
	case <-time.After(5 * time.Second):
		t.Fatal("the event stream outlived the manager; shutdown would hang")
	}
}

func TestManagerCloseIsIdempotent(t *testing.T) {
	manager, _ := newTestServer(t)

	done := make(chan struct{})
	go func() {
		manager.Close()
		manager.Close() // a deferred Close after an explicit one must be safe
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Close blocked when called twice")
	}
}

func newTestServer(t *testing.T) (*download.Manager, http.Handler) {
	t.Helper()

	cfg := &config.Config{
		DownloadDir:  t.TempDir(),
		Concurrency:  1,
		UserAgent:    config.DefaultUserAgent,
		Language:     config.DefaultLanguage,
		GofileSecret: config.FallbackGofileSecret,
		MaxRetries:   0,
		Timeout:      10 * time.Second,
	}
	client := httpx.New(cfg.UserAgent, cfg.AcceptLanguage(), cfg.MaxRetries, cfg.Timeout)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	manager := download.New(cfg, extractor.NewRegistry(cfg, client), client, log)
	manager.Start()
	t.Cleanup(manager.Close)

	assets, err := webui.FS()
	if err != nil {
		t.Fatal(err)
	}
	return manager, New(cfg, manager, log, assets).Handler()
}
