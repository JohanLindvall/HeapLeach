package server

import (
	"bufio"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// A bare run is a desktop session: once nothing is downloading and no browser
// is watching, the process should stop. Each test below pins one half of "is
// anyone there", which is deliberately two signals rather than one — see
// Server.Idle for why either alone gives the wrong answer.

func TestIdleIsCountedFromLaunch(t *testing.T) {
	_, srv := newTestParts(t)

	idle, since := srv.Idle()
	if !idle {
		t.Fatal("a fresh server with no jobs is idle")
	}
	// From startup rather than from zero, so a process nobody visits still
	// gets its full grace period instead of exiting on the first tick.
	if since > time.Second {
		t.Errorf("idle for %s at startup, want approximately none", since)
	}
}

func TestARequestCountsAsSomeoneBeingThere(t *testing.T) {
	_, srv := newTestParts(t)
	srv.lastSeen.Store(time.Now().Add(-time.Hour).UnixNano())

	if _, since := srv.Idle(); since < time.Minute {
		t.Fatalf("expected a long-idle server, got %s", since)
	}

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/state", nil))

	if _, since := srv.Idle(); since > time.Second {
		t.Errorf("idle for %s after a request, want the clock reset", since)
	}
}

// An open stream is a browser watching even though it sends nothing once
// established — which is exactly why the request clock alone is not enough.
func TestAnOpenStreamMeansNotIdle(t *testing.T) {
	manager, srv := newTestParts(t)
	http := httptest.NewServer(srv.Handler())
	defer http.Close()

	resp, err := getStream(http.URL + "/api/events")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	// The stream is established once its first frame has arrived.
	if _, err := bufio.NewReader(resp.Body).ReadString('\n'); err != nil {
		t.Fatalf("reading the first frame: %v", err)
	}
	if srv.streams.Load() == 0 {
		t.Fatal("an open event stream was not counted")
	}

	// Even with the request clock wound right back, a live stream means
	// somebody is there.
	srv.lastSeen.Store(time.Now().Add(-time.Hour).UnixNano())
	if idle, _ := srv.Idle(); idle {
		t.Error("a server with a browser watching reported itself idle")
	}

	// And closing the tab is what eventually lets the process stop.
	resp.Body.Close()
	manager.Close()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && srv.streams.Load() > 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if got := srv.streams.Load(); got != 0 {
		t.Errorf("stream count = %d after the stream ended, want 0", got)
	}
}

// Work outstanding keeps the process alive whatever the browser is doing.
// Exiting mid-download would be the one unforgivable version of this.
func TestBusyIsNeverIdle(t *testing.T) {
	manager, srv := newTestParts(t)
	srv.lastSeen.Store(time.Now().Add(-time.Hour).UnixNano())

	// A job that never resolves is the case worth pinning: it is work
	// outstanding, and it has no items yet, so a queue-length check on its
	// own would call this idle at exactly the wrong moment.
	if _, err := manager.Add("https://never.example.test/a", ""); err != nil {
		t.Fatal(err)
	}
	if idle, _ := srv.Idle(); idle {
		t.Error("a server with a job still resolving reported itself idle")
	}
}

func getStream(url string) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	return http.DefaultClient.Do(req)
}
