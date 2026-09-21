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
	"sync"
	"sync/atomic"
	"testing"
	"time"

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

// Until a host asks to be held back it is not queued behind anything.
func TestHostGateAdmitsEverythingUntilAHostAsksOtherwise(t *testing.T) {
	g := newHostGate()
	for range 5 {
		if _, err := g.admit(context.Background(), "files.example.test"); err != nil {
			t.Fatalf("admit: %v", err)
		}
	}
	if limit, active := g.waiting("files.example.test"); limit != 0 || active != 5 {
		t.Errorf("limit = %d, active = %d; want no limit and five in flight", limit, active)
	}
	// A host nobody named admits without bookkeeping at all.
	if _, err := g.admit(context.Background(), ""); err != nil {
		t.Errorf("admit of an unnamed host = %v", err)
	}
}

// A 503 from a host that has never got a transfer going is a host that is
// down, not one being asked for too much. Cutting it back neither helps it
// nor gets the file, and would hold back a queue on the strength of nothing.
func TestHostGateThrottlesOnlyAHostThatHasServedSomething(t *testing.T) {
	g := newHostGate()
	release, err := g.admit(context.Background(), "files.example.test")
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	if got := g.overloaded("files.example.test"); got != 0 {
		t.Errorf("throttled a host that has served nothing, cap = %d", got)
	}
	if limit, _ := g.waiting("files.example.test"); limit != 0 {
		t.Errorf("a host that has served nothing was given a cap of %d", limit)
	}

	// Once something has come through, the same refusal means the opposite.
	g.serving("files.example.test")
	if got := g.overloaded("files.example.test"); got != 1 {
		t.Errorf("cap = %d, want one fewer than the single transfer in flight", got)
	}
}

// Each refusal costs the host one slot, so siblings failing together walk
// the cap down to what the host can sustain within a few refusals rather
// than in one guess. It never reaches zero: a host that can serve nothing is
// a failure to report, not a rate to find.
func TestHostGateWalksTheCapDownAndStopsAtOne(t *testing.T) {
	g := newHostGate()
	g.serving("files.example.test")
	for range 6 {
		if _, err := g.admit(context.Background(), "files.example.test"); err != nil {
			t.Fatal(err)
		}
	}

	if got := g.overloaded("files.example.test"); got != 5 {
		t.Errorf("first refusal set the cap to %d, want one fewer than the six in flight", got)
	}
	for want := 4; want >= 1; want-- {
		if got := g.overloaded("files.example.test"); got != want {
			t.Fatalf("cap = %d, want %d", got, want)
		}
	}
	if got := g.overloaded("files.example.test"); got != 1 {
		t.Errorf("cap = %d, want it to hold at 1", got)
	}
}

// The point of a global queue: a dozen siblings retrying an overloaded host
// take their turn instead of arriving together. Without it each item is only
// ever patient on its own behalf, which is the shape that overloaded the
// host in the first place.
func TestHostGateSerialisesEveryTransferAtAThrottledHost(t *testing.T) {
	g := newHostGate()
	g.serving("files.example.test")
	release, err := g.admit(context.Background(), "files.example.test")
	if err != nil {
		t.Fatal(err)
	}
	if got := g.overloaded("files.example.test"); got != 1 {
		t.Fatalf("cap = %d, want 1", got)
	}
	release()

	var (
		inFlight atomic.Int32
		most     atomic.Int32
		wg       sync.WaitGroup
	)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			done, err := g.admit(context.Background(), "files.example.test")
			if err != nil {
				return
			}
			n := inFlight.Add(1)
			for {
				high := most.Load()
				if n <= high || most.CompareAndSwap(high, n) {
					break
				}
			}
			time.Sleep(time.Millisecond)
			inFlight.Add(-1)
			done()
		}()
	}
	wg.Wait()

	if got := most.Load(); got != 1 {
		t.Errorf("%d transfers were at the host at once, want 1 — the retries did not queue", got)
	}
}

// Down on evidence, up on evidence. A finished transfer is the only thing
// that says the host is coping, and one slot at a time means the next
// refusal costs one slot rather than undoing the whole recovery.
func TestHostGateEasesBackUpAndForgetsAMeaninglessCap(t *testing.T) {
	g := newHostGate()
	g.serving("files.example.test")
	release, err := g.admit(context.Background(), "files.example.test")
	if err != nil {
		t.Fatal(err)
	}
	release()
	g.overloaded("files.example.test")

	for _, want := range []int{2, 3} {
		g.eased("files.example.test", 4)
		if limit, _ := g.waiting("files.example.test"); limit != want {
			t.Fatalf("cap = %d, want %d", limit, want)
		}
	}
	// Allowed as many as the queue would ever run: the cap has stopped
	// meaning anything.
	g.eased("files.example.test", 4)
	if limit, _ := g.waiting("files.example.test"); limit != 0 {
		t.Errorf("cap = %d, want it forgotten", limit)
	}
	// A host that was never throttled gains nothing to forget.
	g.eased("other.example.test", 4)
	if limit, _ := g.waiting("other.example.test"); limit != 0 {
		t.Errorf("easing an unthrottled host invented a cap of %d", limit)
	}
}

// A cancelled transfer must not sit in a queue it can never reach the front
// of.
func TestHostGateAdmitGivesUpWhenCancelled(t *testing.T) {
	g := newHostGate()
	g.serving("files.example.test")
	if _, err := g.admit(context.Background(), "files.example.test"); err != nil {
		t.Fatal(err)
	}
	g.overloaded("files.example.test") // cap 1, and it is taken

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := g.admit(ctx, "files.example.test")
		done <- err
	}()
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Error("a cancelled wait was admitted anyway")
		}
	case <-time.After(5 * time.Second):
		t.Error("a cancelled wait never returned")
	}
}

// bucklingServer serves anything under /ok/ and answers the first refusals
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

// The whole feature, and the thing the note used to get wrong: what is
// throttled is the host that answered, not the site the job was submitted
// from. A queue built from an index site is the case that proves it — every
// file comes from somewhere else entirely.
func TestOverloadThrottlesTheHostThatAnsweredNotTheJobsSource(t *testing.T) {
	payload := make([]byte, 64<<10)
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}

	handler, hits := bucklingServer(payload, 3)
	srv := httptest.NewServer(handler)
	defer srv.Close()

	m := busyManager(t)
	const indexHost = "index.example.test"
	m.mu.Lock()
	m.jobs["j"] = &Job{ID: "j", Source: "https://" + indexHost + "/?search=something"}
	m.mu.Unlock()

	server, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}

	// One file through first: that is the evidence the throttle waits for.
	first := &Item{ID: newID(), JobID: "j", Name: "first.mp4", URL: srv.URL + "/ok/first.mp4", Size: -1}
	if err := m.transfer(context.Background(), first); err != nil {
		t.Fatalf("the file that should have worked did not: %v", err)
	}

	second := &Item{ID: newID(), JobID: "j", Name: "second.mp4", URL: srv.URL + "/busy/second.mp4", Size: -1}
	if err := m.transfer(context.Background(), second); err != nil {
		t.Fatalf("a transfer gave up on a host that was only overloaded: %v", err)
	}
	if got := hits.Load(); got != 4 {
		t.Errorf("server saw %d refusable requests, want three refusals plus the one that worked", got)
	}

	if limit, _ := m.hostGate.waiting(server.Hostname()); limit != 1 {
		t.Errorf("the host that answered is capped at %d, want 1", limit)
	}
	if limit, _ := m.hostGate.waiting(indexHost); limit != 0 {
		t.Errorf("the job's own source was throttled to %d, and it never served anything", limit)
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

	handler, hits := bucklingServer(payload, 2)
	srv := httptest.NewServer(handler)
	defer srv.Close()

	m := busyManager(t)
	it := &Item{ID: newID(), Name: "clip.mp4", URL: srv.URL + "/busy/clip.mp4", Size: -1}

	if err := m.transfer(context.Background(), it); err != nil {
		t.Fatalf("a transfer gave up on a host that came back: %v", err)
	}
	if got := hits.Load(); got != 3 {
		t.Errorf("server saw %d requests, want the two refusals plus the one that worked", got)
	}

	server, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if limit, _ := m.hostGate.waiting(server.Hostname()); limit != 0 {
		t.Errorf("a host that had served nothing when it refused was capped at %d", limit)
	}
}

// A host can also be down in earnest, and a queue that waited on that
// forever would never finish.
func TestOverloadPatienceIsBounded(t *testing.T) {
	handler, hits := bucklingServer(nil, 1<<30)
	srv := httptest.NewServer(handler)
	defer srv.Close()

	m := busyManager(t)
	it := &Item{ID: newID(), Name: "clip.mp4", URL: srv.URL + "/busy/clip.mp4", Size: -1}

	err := m.transfer(context.Background(), it)
	if err == nil {
		t.Fatal("a host that is always 503 was waited on forever")
	}
	if !httpx.HasStatus(err, http.StatusServiceUnavailable) {
		t.Errorf("err = %v, want it to report what the host answered", err)
	}
	// One attempt per wait, plus the one that found the patience spent.
	// Each attempt is worth more than one request: the HTTP client treats
	// 503 as transient in its own right and retries within an attempt,
	// which is the general policy for every 5xx.
	attempts := config.OverloadRetries + 1
	if got, most := int(hits.Load()), attempts*(m.cfg.MaxRetries+1); got > most {
		t.Errorf("server saw %d requests, want no more than %d", got, most)
	}
}
