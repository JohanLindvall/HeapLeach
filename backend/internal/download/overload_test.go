package download

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

// 503 is the one status that means the host cannot take what it is being
// given. The other fives are a server that broke or a gateway that could not
// reach what it fronts: worth repeating, which the ordinary budget does, but
// no reason to conclude the host is overloaded and slow every download to it.
func TestOverloadedHostRecognisesTemporaryUnavailabilityAlone(t *testing.T) {
	cases := map[int]bool{
		http.StatusServiceUnavailable:  true,
		http.StatusInternalServerError: false,
		http.StatusBadGateway:          false,
		http.StatusGatewayTimeout:      false,
		http.StatusForbidden:           false,
		http.StatusNotFound:            false,
	}
	for code, want := range cases {
		err := fmt.Errorf("wrapped: %w", &httpx.StatusError{Code: code, URL: "https://example.test/f"})
		if _, got := overloadedHost(err); got != want {
			t.Errorf("overloadedHost(%d) = %v, want %v", code, got, want)
		}
	}
	if _, got := overloadedHost(io.EOF); got {
		t.Error("a plain I/O error was read as an overloaded host")
	}
}

// Each refusal costs the host one slot, so several siblings failing together
// walk the cap down to what the host can sustain within a few refusals
// rather than in one guess. It never reaches zero: a host that can serve
// nothing is a failure to report, not a rate to find.
func TestThrottleHostWalksTheCapDownAndStopsAtOne(t *testing.T) {
	m := &Manager{
		hostActive: map[string]int{"files.example.test": 6},
		hostServed: map[string]bool{"files.example.test": true},
		hostLimits: map[string]int{},
	}

	if got := m.throttleHostLocked("files.example.test"); got != 5 {
		t.Errorf("first refusal set the cap to %d, want one fewer than the six in flight", got)
	}
	for want := 4; want >= 1; want-- {
		if got := m.throttleHostLocked("files.example.test"); got != want {
			t.Fatalf("cap = %d, want %d", got, want)
		}
	}
	if got := m.throttleHostLocked("files.example.test"); got != 1 {
		t.Errorf("cap = %d, want it to hold at 1", got)
	}
	// An item the dispatcher never charged against a host names none, and
	// there is nothing to throttle.
	if got := m.throttleHostLocked(""); got != 0 {
		t.Errorf("throttling an unnamed host returned %d, want 0", got)
	}
}

// A 503 from a host that has never got a transfer going is a host that is
// down, not one being asked for too much. Cutting it back neither helps it
// nor gets the file, and would slow a queue on the strength of nothing.
func TestThrottleHostWaitsForEvidenceTheHostServesAnything(t *testing.T) {
	m := &Manager{
		hostActive: map[string]int{"files.example.test": 4},
		hostServed: map[string]bool{},
		hostLimits: map[string]int{},
	}
	if got := m.throttleHostLocked("files.example.test"); got != 0 {
		t.Errorf("throttled a host that has served nothing, cap = %d", got)
	}
	if _, ok := m.hostLimits["files.example.test"]; ok {
		t.Error("a host that has served nothing was given a cap")
	}

	// Once something has come through, the same refusal means the opposite.
	m.hostServed["files.example.test"] = true
	if got := m.throttleHostLocked("files.example.test"); got != 3 {
		t.Errorf("cap = %d, want one fewer than the four in flight", got)
	}
}

// Down on evidence, up on evidence. A finished transfer is the only thing
// that says the host is coping, and one slot at a time means the next
// refusal costs one slot rather than undoing the whole recovery.
func TestEaseHostStepsBackUpAndForgetsAMeaninglessCap(t *testing.T) {
	m := &Manager{limit: 4, hostActive: map[string]int{}, hostLimits: map[string]int{"files.example.test": 1}}

	// One slot per finished transfer, while the cap still means something:
	// with the queue running four at once, caps of 1, 2 and 3 all hold the
	// host back and 4 does not.
	for _, want := range []int{2, 3} {
		m.easeHostLocked("files.example.test")
		if got := m.hostLimits["files.example.test"]; got != want {
			t.Fatalf("cap = %d, want %d", got, want)
		}
	}
	m.easeHostLocked("files.example.test")
	if got, ok := m.hostLimits["files.example.test"]; ok {
		t.Errorf("cap = %d, want it forgotten once it reaches what the queue would run anyway", got)
	}
	// A host that was never throttled gains nothing to forget.
	m.easeHostLocked("other.example.test")
	if _, ok := m.hostLimits["other.example.test"]; ok {
		t.Error("easing an unthrottled host invented a cap for it")
	}
}

// The dispatcher has to honour what the host has demonstrated as well as
// what its extractor asked for.
func TestHostFullHonoursTheLearnedCapAsWellAsThePace(t *testing.T) {
	m := &Manager{
		jobs:       map[string]*Job{"j": {ID: "j", Source: "https://files.example.test/a/AAAA"}},
		hostActive: map[string]int{"files.example.test": 2},
		hostLimits: map[string]int{},
	}
	it := &Item{JobID: "j", URL: "https://files.example.test/f/BBBB"}

	if m.hostFullLocked(it) {
		t.Error("an unpaced, unthrottled host was reported full")
	}
	m.hostLimits["files.example.test"] = 2
	if !m.hostFullLocked(it) {
		t.Error("a host at its learned cap was not reported full")
	}
	m.hostLimits["files.example.test"] = 3
	if m.hostFullLocked(it) {
		t.Error("a host below its learned cap was reported full")
	}
}

// overloadedServer answers the first refusals requests with 503 and serves
// the payload after that, which is what a storage backend does while it is
// carrying more than it can.
func overloadedServer(payload []byte, refusals int32) (http.Handler, *atomic.Int32) {
	var hits atomic.Int32
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) <= refusals {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, "<html><body>503 Service Unavailable</body></html>")
			return
		}
		(&rangeServer{payload: payload}).handler().ServeHTTP(w, r)
	})
	return handler, &hits
}

// bucklingServer serves anything at /ok/, and answers the first refusals
// requests for anything else with 503 before serving those too. It is the
// shape of a backend that copes with a few files at a time and falls over
// when given more.
func bucklingServer(payload []byte, refusals int32) (http.Handler, *atomic.Int32) {
	var hits atomic.Int32
	files := (&rangeServer{payload: payload}).handler()
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/ok/") {
			files.ServeHTTP(w, r)
			return
		}
		if hits.Add(1) <= refusals {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, "<html><body>503 Service Unavailable</body></html>")
			return
		}
		files.ServeHTTP(w, r)
	})
	return handler, &hits
}

// The whole point: once a host has shown it can serve something, a 503 from
// it is a rate to find. The transfer waits rather than failing, and the host
// is given less to do while it does.
func TestOverloadedHostIsWaitedOutAndThrottledOnceItHasServedSomething(t *testing.T) {
	payload := make([]byte, 64<<10)
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}

	handler, hits := bucklingServer(payload, 3)
	srv := httptest.NewServer(handler)
	defer srv.Close()

	m := busyManager(t)
	host, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}

	// One file through first: that is the evidence the throttle waits for.
	first := &Item{ID: newID(), Name: "first.mp4", URL: srv.URL + "/ok/first.mp4", Size: -1}
	if err := m.transfer(context.Background(), first); err != nil {
		t.Fatalf("the file that should have worked did not: %v", err)
	}
	m.mu.Lock()
	served := m.hostServed[host.Hostname()]
	_, throttledEarly := m.hostLimits[host.Hostname()]
	m.mu.Unlock()
	if !served {
		t.Fatal("a transfer that ran to the end did not record the host as serving")
	}
	if throttledEarly {
		t.Error("a host was throttled without having refused anything")
	}

	second := &Item{ID: newID(), Name: "second.mp4", URL: srv.URL + "/busy/second.mp4", Size: -1}
	if err := m.transfer(context.Background(), second); err != nil {
		t.Fatalf("a transfer gave up on a host that was only overloaded: %v", err)
	}
	if got := hits.Load(); got != 4 {
		t.Errorf("server saw %d refusable requests, want the three refusals plus the one that worked", got)
	}

	m.mu.Lock()
	limit, throttled := m.hostLimits[host.Hostname()]
	m.mu.Unlock()
	if !throttled {
		t.Error("the host was waited out but never given less to do")
	}
	if limit != 1 {
		t.Errorf("cap = %d, want it walked down by the refusals", limit)
	}

	got, err := os.ReadFile(filepath.Join(m.cfg.DownloadDir, "second.mp4"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Error("the file that finally downloaded differs from the source")
	}
}

// A host that has served nothing is unavailable rather than overloaded. It
// is still waited out — a host can come back — but there is no rate of ours
// to find, so nothing is cut back.
func TestAnUnavailableHostIsWaitedOutButNotThrottled(t *testing.T) {
	payload := make([]byte, 16<<10)
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}

	handler, hits := overloadedServer(payload, 2)
	srv := httptest.NewServer(handler)
	defer srv.Close()

	m := busyManager(t)
	it := &Item{ID: newID(), Name: "clip.mp4", URL: srv.URL + "/storage/media/AAAA.mp4", Size: -1}

	if err := m.transfer(context.Background(), it); err != nil {
		t.Fatalf("a transfer gave up on a host that came back: %v", err)
	}
	if got := hits.Load(); got != 3 {
		t.Errorf("server saw %d requests, want the two refusals plus the one that worked", got)
	}

	host, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	_, throttled := m.hostLimits[host.Hostname()]
	m.mu.Unlock()
	if throttled {
		t.Error("a host that had served nothing when it refused was still cut back")
	}
}

// A host can also be down in earnest, and a queue that waited on that
// forever would never finish.
func TestOverloadPatienceIsBounded(t *testing.T) {
	handler, hits := overloadedServer(nil, 1<<30)
	srv := httptest.NewServer(handler)
	defer srv.Close()

	m := busyManager(t)
	it := &Item{ID: newID(), Name: "clip.mp4", URL: srv.URL + "/storage/media/AAAA.mp4", Size: -1}

	err := m.transfer(context.Background(), it)
	if err == nil {
		t.Fatal("a host that is always 503 was waited on forever")
	}
	if !httpx.HasStatus(err, http.StatusServiceUnavailable) {
		t.Errorf("err = %v, want it to report what the host answered", err)
	}
	// One attempt per wait, plus the one that found the cap spent. Each
	// attempt is worth more than one request: the HTTP client treats 503 as
	// transient in its own right and retries within an attempt, which is
	// the general policy for every 5xx and not this branch's to change.
	attempts := config.OverloadRetries + 1
	if got, most := int(hits.Load()), attempts*(m.cfg.MaxRetries+1); got > most {
		t.Errorf("server saw %d requests, want no more than %d", got, most)
	}
}
