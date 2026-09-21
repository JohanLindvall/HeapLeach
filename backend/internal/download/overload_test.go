package download

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
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
	"github.com/JohanLindvall/HeapLeach/internal/extractor"
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
		if _, _, ok := g.tryAdmit("files.example.test"); !ok {
			t.Fatal("an unthrottled host refused a transfer")
		}
	}
	if limit, active := g.waiting("files.example.test"); limit != 0 || active != 5 {
		t.Errorf("limit = %d, active = %d; want no limit and five in flight", limit, active)
	}
	// A host nobody named admits without bookkeeping at all.
	if _, _, ok := g.tryAdmit(""); !ok {
		t.Error("an unnamed host refused a transfer")
	}
}

// A 503 from a host that has never got a transfer going is a host that is
// down, not one being asked for too much. Cutting it back neither helps it
// nor gets the file, and would hold back a queue on the strength of nothing.
func TestHostGateThrottlesOnlyAHostThatHasServedSomething(t *testing.T) {
	g := newHostGate()
	release, _, ok := g.tryAdmit("files.example.test")
	if !ok {
		t.Fatal("an unthrottled host refused a transfer")
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
		if _, _, ok := g.tryAdmit("files.example.test"); !ok {
			t.Fatal("an unthrottled host refused a transfer")
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

// The point of a global queue: a host that has asked to be given less gives
// out that many slots and no more, whichever job or worker asks. What the
// refused ones do is the next test's business — the rule here is simply that
// they are refused rather than admitted alongside.
func TestHostGateGivesOutOnlyAsManySlotsAsTheHostAllows(t *testing.T) {
	g := newHostGate()
	g.serving("files.example.test")
	release, _, ok := g.tryAdmit("files.example.test")
	if !ok {
		t.Fatal("an unthrottled host refused a transfer")
	}
	if got := g.overloaded("files.example.test"); got != 1 {
		t.Fatalf("cap = %d, want 1", got)
	}

	// The slot is taken, so nobody else may have one.
	if _, limit, ok := g.tryAdmit("files.example.test"); ok {
		t.Error("a second transfer was admitted to a host taking one at a time")
	} else if limit != 1 {
		t.Errorf("refusal reported a cap of %d, want 1", limit)
	}
	if !g.full("files.example.test") {
		t.Error("a host with every slot taken did not report itself full")
	}
	// And a host nobody has throttled is never full.
	if g.full("other.example.test") {
		t.Error("an unthrottled host reported itself full")
	}

	// Handing the slot back is somebody else's turn.
	release()
	if g.full("files.example.test") {
		t.Error("a host still reported itself full after a slot was freed")
	}
	if _, _, ok := g.tryAdmit("files.example.test"); !ok {
		t.Error("the freed slot was not given to the next transfer")
	}
}

// Down on evidence, up on evidence. A finished transfer is the only thing
// that says the host is coping, and one slot at a time means the next
// refusal costs one slot rather than undoing the whole recovery.
func TestHostGateEasesBackUpAndForgetsAMeaninglessCap(t *testing.T) {
	g := newHostGate()
	g.serving("files.example.test")
	release, _, ok := g.tryAdmit("files.example.test")
	if !ok {
		t.Fatal("an unthrottled host refused a transfer")
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

// A worker must never be parked waiting for a host. An item whose turn has
// not come is handed back to the queue, where the dispatcher passes over it
// until there is room — which is what leaves the workers free for the hosts
// that are coping. The screenshot this comes from had three of four workers
// sitting on one struggling host while everything else waited behind them.
func TestAFullHostHandsTheItemBackInsteadOfHoldingAWorker(t *testing.T) {
	payload := make([]byte, 16<<10)
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer((&rangeServer{payload: payload}).handler())
	defer srv.Close()
	other := httptest.NewServer((&rangeServer{payload: payload}).handler())
	defer other.Close()

	m := busyManager(t)
	busy, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}

	// The struggling host, capped at one and that one taken.
	m.hostGate.serving(busy.Hostname())
	release, _, ok := m.hostGate.tryAdmit(busy.Hostname())
	if !ok {
		t.Fatal("the gate refused the first transfer")
	}
	if got := m.hostGate.overloaded(busy.Hostname()); got != 1 {
		t.Fatalf("cap = %d, want 1", got)
	}

	held := &Item{ID: newID(), Name: "held.mp4", URL: srv.URL + "/held.mp4", Size: -1}
	err = m.transfer(context.Background(), held)
	queued, isQueued := errors.AsType[*hostQueuedError](err)
	if !isQueued {
		t.Fatalf("transfer to a full host returned %v, want it handed back", err)
	}
	if queued.host != busy.Hostname() || queued.limit != 1 {
		t.Errorf("handed back naming %s at %d, want %s at 1", queued.host, queued.limit, busy.Hostname())
	}

	// The queue is where it waits, and the dispatcher knows to pass over it.
	m.mu.Lock()
	deferred := m.deferHostQueuedLocked(held, err)
	skipped := m.hostFullLocked(held)
	note := held.Note
	status := held.Status
	m.mu.Unlock()
	if !deferred || status != StatusQueued {
		t.Errorf("the item was not returned to the queue (deferred=%v status=%s)", deferred, status)
	}
	if !skipped {
		t.Error("the dispatcher would start an item whose host is full")
	}
	if !strings.Contains(note, busy.Hostname()) {
		t.Errorf("note = %q, want it to name the host being waited for", note)
	}

	// And meanwhile a host that is coping carries on, which is the whole
	// point of handing the first one back. Named through localhost so it is
	// a different host from the one above: the gate keys on the name, and
	// both test servers listen on the same address.
	freeURL := strings.Replace(other.URL, "127.0.0.1", "localhost", 1)
	free := &Item{ID: newID(), Name: "free.mp4", URL: freeURL + "/free.mp4", Size: -1}
	if err := m.transfer(context.Background(), free); err != nil {
		t.Fatalf("an untroubled host was held up by a throttled one: %v", err)
	}
	m.mu.Lock()
	stillSkipped := m.hostFullLocked(free)
	m.mu.Unlock()
	if stillSkipped {
		t.Error("an untroubled host was reported full")
	}
	release()
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

// A signed link is minted for minutes, and an item can sit behind a host
// taking one download at a time for hours. So the turn comes first and the
// signature second: a link minted before the wait would expire where it
// stood, and one spent on a transfer that is then turned away is a request
// made of a host that has just asked for fewer of them.
func TestASignedLinkIsMintedAfterTheHostsTurnNotBefore(t *testing.T) {
	payload := make([]byte, 16<<10)
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer((&rangeServer{payload: payload}).handler())
	defer srv.Close()

	host, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}

	m := busyManager(t)
	var signed atomic.Int32
	it := &Item{
		ID: newID(), Name: "clip.mp4", Size: -1,
		// Where it last resolved to, which is how its host is known before
		// anything is signed.
		URL: srv.URL + "/stale.mp4",
		resolve: func(context.Context) (*extractor.Target, error) {
			signed.Add(1)
			return &extractor.Target{URL: srv.URL + "/fresh.mp4", Size: -1}, nil
		},
	}

	// The host is taking one download, and that one is taken.
	m.hostGate.serving(host.Hostname())
	release, _, ok := m.hostGate.tryAdmit(host.Hostname())
	if !ok {
		t.Fatal("the gate refused the first transfer")
	}
	if got := m.hostGate.overloaded(host.Hostname()); got != 1 {
		t.Fatalf("cap = %d, want 1", got)
	}

	if err := m.transfer(context.Background(), it); err == nil {
		t.Fatal("a transfer ran at a host with no slots left")
	} else if _, queued := errors.AsType[*hostQueuedError](err); !queued {
		t.Fatalf("transfer returned %v, want the item handed back", err)
	}
	if got := signed.Load(); got != 0 {
		t.Errorf("%d links were signed for a transfer that never ran", got)
	}

	// Its turn comes, and only now is a link minted.
	release()
	if err := m.transfer(context.Background(), it); err != nil {
		t.Fatalf("transfer: %v", err)
	}
	if got := signed.Load(); got != 1 {
		t.Errorf("%d links were signed, want exactly the one that was used", got)
	}

	got, err := os.ReadFile(filepath.Join(m.cfg.DownloadDir, "clip.mp4"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Error("the file differs from the source")
	}
}
