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
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/extractor"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

// Test cooldowns, short enough not to slow the suite down.
const (
	testQuietBase = time.Millisecond
	testQuietMax  = 5 * time.Millisecond
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
	if _, _, ok := g.tryAdmit(""); !ok {
		t.Error("an unnamed host refused a transfer")
	}
}

// A 503 from a host that has never got a transfer going is a host that is
// down, not one being asked for too much. There is no rate of ours to find,
// so nothing is cut back — though it is still left alone for a while.
func TestHostGateThrottlesOnlyAHostThatHasServedSomething(t *testing.T) {
	g := newHostGate()
	if _, _, ok := g.tryAdmit("files.example.test"); !ok {
		t.Fatal("an unthrottled host refused a transfer")
	}

	limit, wait := g.overloaded("files.example.test", testQuietBase, testQuietMax)
	if limit != 0 {
		t.Errorf("throttled a host that has served nothing, cap = %d", limit)
	}
	if wait <= 0 {
		t.Error("a host that refused was not left alone at all")
	}

	// Once something has come through, the same refusal means the opposite.
	g.serving("files.example.test")
	if limit, _ := g.overloaded("files.example.test", testQuietBase, testQuietMax); limit != 1 {
		t.Errorf("cap = %d, want one at a time", limit)
	}
}

// One at a time, from the first refusal. Walking the cap down a step per
// refusal sounds gentler and is not: while it walks, everything admitted
// before it started is still going, which is the state the screenshots
// showed — four rows retrying a host that was taking one download.
func TestHostGateCutsAnOverloadedHostToOneAtOnce(t *testing.T) {
	g := newHostGate()
	g.serving("files.example.test")
	for range 6 {
		if _, _, ok := g.tryAdmit("files.example.test"); !ok {
			t.Fatal("an unthrottled host refused a transfer")
		}
	}

	limit, _ := g.overloaded("files.example.test", testQuietBase, testQuietMax)
	if limit != 1 {
		t.Errorf("cap = %d after the first refusal, want 1 straight away", limit)
	}
	if limit, _ := g.overloaded("files.example.test", testQuietBase, testQuietMax); limit != 1 {
		t.Errorf("cap = %d, want it to hold at 1", limit)
	}
}

// A host that has refused is left alone for a while, and that wait grows
// with each refusal in a row. A cap on its own does not slow anything down
// when the queue behind it is long: the next item takes the freed slot the
// instant it is given back.
func TestHostGateLeavesARefusingHostAloneForAWhile(t *testing.T) {
	g := newHostGate()
	g.serving("files.example.test")
	release, _, ok := g.tryAdmit("files.example.test")
	if !ok {
		t.Fatal("an unthrottled host refused a transfer")
	}
	release()

	first, wait := g.overloaded("files.example.test", 50*time.Millisecond, time.Second)
	if first != 1 || wait <= 0 {
		t.Fatalf("overloaded = %d, %v", first, wait)
	}
	// Nothing may be admitted while the host is being left alone, even
	// though its one slot is free.
	if _, _, ok := g.tryAdmit("files.example.test"); ok {
		t.Error("a host being left alone admitted a transfer anyway")
	}
	if !g.full("files.example.test") {
		t.Error("a host being left alone did not report itself full")
	}
	if got := g.quiet("files.example.test"); got <= 0 {
		t.Errorf("quiet = %v, want the remaining wait", got)
	}

	// Each refusal in a row makes the next wait longer.
	_, second := g.overloaded("files.example.test", 50*time.Millisecond, time.Second)
	if second <= wait {
		t.Errorf("second wait %v, want longer than the first %v", second, wait)
	}

	// And a transfer that gets going says the bad spell is over.
	g.serving("files.example.test")
	if g.quiet("files.example.test") != 0 {
		t.Error("a host that started serving again was still being left alone")
	}
	if _, _, ok := g.tryAdmit("files.example.test"); !ok {
		t.Error("a host that started serving again refused the next transfer")
	}
}

// The cap is what stops a second transfer starting while one is running.
func TestHostGateGivesOutOnlyAsManySlotsAsTheHostAllows(t *testing.T) {
	g := newHostGate()
	g.serving("files.example.test")
	release, _, ok := g.tryAdmit("files.example.test")
	if !ok {
		t.Fatal("an unthrottled host refused a transfer")
	}
	if limit, _ := g.overloaded("files.example.test", 0, 0); limit != 1 {
		t.Fatalf("cap = %d, want 1", limit)
	}

	if _, limit, ok := g.tryAdmit("files.example.test"); ok {
		t.Error("a second transfer was admitted to a host taking one at a time")
	} else if limit != 1 {
		t.Errorf("refusal reported a cap of %d, want 1", limit)
	}
	if !g.full("files.example.test") {
		t.Error("a host with every slot taken did not report itself full")
	}
	if g.full("other.example.test") {
		t.Error("an unthrottled host reported itself full")
	}

	release()
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
	g.overloaded("files.example.test", 0, 0)

	for _, want := range []int{2, 3} {
		g.eased("files.example.test", 4)
		if limit, _ := g.waiting("files.example.test"); limit != want {
			t.Fatalf("cap = %d, want %d", limit, want)
		}
	}
	g.eased("files.example.test", 4)
	if limit, _ := g.waiting("files.example.test"); limit != 0 {
		t.Errorf("cap = %d, want it forgotten", limit)
	}
	g.eased("other.example.test", 4)
	if limit, _ := g.waiting("other.example.test"); limit != 0 {
		t.Errorf("easing an unthrottled host invented a cap of %d", limit)
	}
}

// A worker must never be parked waiting for a host. An item whose turn has
// not come is handed back to the queue, where the dispatcher passes over it
// until there is room — which is what leaves the workers free for the hosts
// that are coping.
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

	m.hostGate.serving(busy.Hostname())
	release, _, ok := m.hostGate.tryAdmit(busy.Hostname())
	if !ok {
		t.Fatal("the gate refused the first transfer")
	}
	if limit, _ := m.hostGate.overloaded(busy.Hostname(), 0, 0); limit != 1 {
		t.Fatalf("cap = %d, want 1", limit)
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

	m.mu.Lock()
	deferred := m.deferHostQueuedLocked(held, err)
	skipped := m.hostFullLocked(held)
	note, status := held.Note, held.Status
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

	// Meanwhile a host that is coping carries on, which is the whole point
	// of handing the first one back. Named through localhost so it is a
	// different host: the gate keys on the name, and both test servers
	// listen on the same address.
	freeURL := strings.Replace(other.URL, "127.0.0.1", "localhost", 1)
	free := &Item{ID: newID(), Name: "free.mp4", URL: freeURL + "/free.mp4", Size: -1}
	if err := m.transfer(context.Background(), free); err != nil {
		t.Fatalf("an untroubled host was held up by a throttled one: %v", err)
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

// A refused transfer gives its slot up there and then, and the host is cut
// to one at a time. Retrying in place instead is what put four rows on one
// struggling host all reading "attempt 5 of 10" — an item that has been
// refused holds nothing worth keeping.
func TestARefusedTransferGivesUpItsSlotInsteadOfRetryingInPlace(t *testing.T) {
	payload := make([]byte, 64<<10)
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}

	handler, hits := bucklingServer(payload, 1<<30)
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

	before := hits.Load()
	second := &Item{ID: newID(), Name: "second.mp4", URL: srv.URL + "/busy/second.mp4", Size: -1}
	err = m.transfer(context.Background(), second)
	if _, queued := errors.AsType[*hostQueuedError](err); !queued {
		t.Fatalf("a refused transfer returned %v, want the item handed back", err)
	}
	// One request, not a retry loop: the item left instead of trying again.
	if got := hits.Load() - before; got > int32(m.cfg.MaxRetries+1) {
		t.Errorf("the refused transfer made %d requests, want it to leave after the refusal", got)
	}
	if limit, active := m.hostGate.waiting(host.Hostname()); limit != 1 || active != 0 {
		t.Errorf("cap = %d with %d in flight; want one at a time and the slot given back", limit, active)
	}
	m.mu.Lock()
	turns := second.overloadWaits
	m.mu.Unlock()
	if turns != 1 {
		t.Errorf("the item recorded %d turns, want 1", turns)
	}
}

// The other thing the note used to get wrong: what is throttled is the host
// that answered, not the site the job was submitted from. A queue built from
// an index site is the case that proves it — every file lives elsewhere.
func TestOverloadThrottlesTheHostThatAnsweredNotTheJobsSource(t *testing.T) {
	payload := make([]byte, 32<<10)
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}

	// Exactly one transfer's worth of refusals. The HTTP client treats 503
	// as transient in its own right and retries within an attempt, so it
	// takes more than one refusal to reach the branch under test — and no
	// more than that, or the recovery below would be refused as well.
	handler, _ := bucklingServer(payload, 2)
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

	first := &Item{ID: newID(), JobID: "j", Name: "first.mp4", URL: srv.URL + "/ok/first.mp4", Size: -1}
	if err := m.transfer(context.Background(), first); err != nil {
		t.Fatalf("the file that should have worked did not: %v", err)
	}

	second := &Item{ID: newID(), JobID: "j", Name: "second.mp4", URL: srv.URL + "/busy/second.mp4", Size: -1}
	if err := m.transfer(context.Background(), second); err == nil {
		t.Fatal("a refused transfer reported success")
	}

	if limit, _ := m.hostGate.waiting(server.Hostname()); limit != 1 {
		t.Errorf("the host that answered is capped at %d, want 1", limit)
	}
	if limit, _ := m.hostGate.waiting(indexHost); limit != 0 {
		t.Errorf("the job's own source was throttled to %d, and it never served anything", limit)
	}

	// Once the host is past its bad spell the file downloads, which is what
	// the waiting was for.
	waitOutQuiet(t, m, server.Hostname())
	if err := m.transfer(context.Background(), second); err != nil {
		t.Fatalf("the transfer did not recover: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(m.cfg.DownloadDir, "second.mp4"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Error("the file that finally downloaded differs from the source")
	}
}

// A host that has served nothing is unavailable rather than overloaded: it
// is left alone, but there is no rate of ours to find, so nothing is capped.
func TestAnUnavailableHostIsLeftAloneButNotThrottled(t *testing.T) {
	handler, _ := bucklingServer(nil, 1<<30)
	srv := httptest.NewServer(handler)
	defer srv.Close()

	m := busyManager(t)
	it := &Item{ID: newID(), Name: "clip.mp4", URL: srv.URL + "/busy/clip.mp4", Size: -1}

	err := m.transfer(context.Background(), it)
	queued, ok := errors.AsType[*hostQueuedError](err)
	if !ok {
		t.Fatalf("transfer returned %v, want the item handed back", err)
	}
	if queued.limit != 0 {
		t.Errorf("a host that had served nothing was capped at %d", queued.limit)
	}

	server, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if limit, _ := m.hostGate.waiting(server.Hostname()); limit != 0 {
		t.Errorf("a host that had served nothing when it refused was capped at %d", limit)
	}
	if m.hostGate.quiet(server.Hostname()) <= 0 {
		t.Error("a host that refused was not left alone at all")
	}
}

// A host can also be down in earnest, and a queue that waited on that
// forever would never finish. The turns are counted on the item, because
// the waiting happens in the queue and each turn is a separate transfer.
func TestOverloadPatienceIsBounded(t *testing.T) {
	handler, _ := bucklingServer(nil, 1<<30)
	srv := httptest.NewServer(handler)
	defer srv.Close()

	m := busyManager(t)
	host, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	it := &Item{ID: newID(), Name: "clip.mp4", URL: srv.URL + "/busy/clip.mp4", Size: -1}

	for turn := 1; turn <= config.OverloadRetries+2; turn++ {
		waitOutQuiet(t, m, host.Hostname())
		err := m.transfer(context.Background(), it)
		if _, queued := errors.AsType[*hostQueuedError](err); queued {
			continue
		}
		if !httpx.HasStatus(err, http.StatusServiceUnavailable) {
			t.Fatalf("turn %d returned %v, want the host's own answer once patience ran out", turn, err)
		}
		if turn <= config.OverloadRetries {
			t.Errorf("gave up on turn %d of %d", turn, config.OverloadRetries)
		}
		return
	}
	t.Errorf("a host that is always 503 was never given up on")
}

// A signed link is minted for minutes, and an item can sit behind a host
// taking one download at a time for hours. So the turn comes first and the
// signature second.
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
		URL: srv.URL + "/stale.mp4",
		resolve: func(context.Context) (*extractor.Target, error) {
			signed.Add(1)
			return &extractor.Target{URL: srv.URL + "/fresh.mp4", Size: -1}, nil
		},
	}

	m.hostGate.serving(host.Hostname())
	release, _, ok := m.hostGate.tryAdmit(host.Hostname())
	if !ok {
		t.Fatal("the gate refused the first transfer")
	}
	if limit, _ := m.hostGate.overloaded(host.Hostname(), 0, 0); limit != 1 {
		t.Fatalf("cap = %d, want 1", limit)
	}

	if err := m.transfer(context.Background(), it); err == nil {
		t.Fatal("a transfer ran at a host with no slots left")
	} else if _, queued := errors.AsType[*hostQueuedError](err); !queued {
		t.Fatalf("transfer returned %v, want the item handed back", err)
	}
	if got := signed.Load(); got != 0 {
		t.Errorf("%d links were signed for a transfer that never ran", got)
	}

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

// waitOutQuiet sits out whatever is left of a host's cooldown, which the
// test timings keep to a few milliseconds.
func waitOutQuiet(t *testing.T, m *Manager, host string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		wait := m.hostGate.quiet(host)
		if wait <= 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s was still being left alone after 5s", host)
		}
		time.Sleep(wait)
	}
}

// Every row waiting on one host has to say the same thing about it. The
// note is written when an item is turned away, and what the host is taking
// changes while the item waits — so a stored sentence goes on stating
// whatever was true when it was written, and a queue behind one host ends up
// showing two different numbers for it at once.
func TestWaitingRowsAgreeAboutWhatTheHostIsTaking(t *testing.T) {
	const host = "files.example.test"
	m := busyManager(t)

	early := &Item{ID: "early", Name: "early.mp4", Status: StatusQueued, Size: -1}
	late := &Item{ID: "late", Name: "late.mp4", Status: StatusQueued, Size: -1}
	m.mu.Lock()
	job := &Job{ID: "j", Source: "https://" + host + "/a/AAAA", Items: []*Item{early, late}}
	m.jobs[job.ID] = job
	m.order = append(m.order, job.ID)

	// Turned away when the host was taking two, and again once it was down
	// to one — which is how a queue collects disagreeing rows.
	m.deferHostQueuedLocked(early, &hostQueuedError{host: host, limit: 2})
	m.deferHostQueuedLocked(late, &hostQueuedError{host: host, limit: 1})
	m.mu.Unlock()

	// What the host is actually taking now.
	m.hostGate.serving(host)
	release, _, ok := m.hostGate.tryAdmit(host)
	if !ok {
		t.Fatal("the gate refused a transfer")
	}
	defer release()
	if limit, _ := m.hostGate.overloaded(host, 0, 0); limit != 1 {
		t.Fatalf("cap = %d, want 1", limit)
	}

	want := "waiting for a slot at " + host + ", which is taking 1 download at a time"
	snap := m.Snapshot()
	var seen int
	for _, j := range snap.Jobs {
		for _, iv := range j.Items {
			seen++
			if iv.Note != want {
				t.Errorf("%s says %q, want %q", iv.Name, iv.Note, want)
			}
		}
	}
	if seen != 2 {
		t.Fatalf("saw %d rows, want 2", seen)
	}
}
