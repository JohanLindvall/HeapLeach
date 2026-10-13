// SPDX-License-Identifier: MIT

package download

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/extractor"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
	"github.com/JohanLindvall/HeapLeach/internal/proxy"
)

// routeRig is a host whose free downloads are capped links bound to the
// route that obtained them, served through proxy routes that are each their
// own storage. A route serves only its own links, answers anyone else's
// with 409, and fails the test if it is ever asked for two connections at
// once: one transfer to an address, as such a host allows.
type routeRig struct {
	t       *testing.T
	m       *Manager
	pool    *proxy.Pool
	payload []byte
	ids     []string // route IDs, in proxy order
	urls    []string // and their proxy URLs
	mu      sync.Mutex
	served  map[string]map[string]int64 // file -> route -> bytes
	asked   map[string]int              // route -> links minted
	refuse  map[string]bool             // route IDs answering 409
	hang    map[string]int64            // route ID -> bytes after which it goes silent
	// abandoned counts responses the client stopped reading before their
	// end. Short of a cancellation, none should be: a connection that does
	// so asks for its next range on a new one, while the host may still be
	// counting the old.
	abandoned atomic.Int32
	// exits is the public address each route leaves by, as its echo answers,
	// and exitBusy how many responses each exit is streaming: never more
	// than one, as a host counting free downloads per address allows.
	exits    map[string]string
	exitBusy map[string]int
	// echoHost is the echo service's address, which answers with each
	// route's exit through the route, or with answers[route] where a test
	// has a route answer something else; and echoDown makes it answer 503
	// to everyone, the normal connection included.
	echoHost string
	answers  map[string]string
	echoDown atomic.Bool
}

func newRouteRig(t *testing.T, routes int) *routeRig {
	t.Helper()
	// Requests are the production size, 8 MiB: long enough beside the file
	// that a split which ignored what had been asked for would cut into it.
	r := &routeRig{t: t, m: busyManager(t), payload: randomPayload(t, 32<<20),
		served: map[string]map[string]int64{}, asked: map[string]int{},
		refuse: map[string]bool{}, hang: map[string]int64{},
		exits: map[string]string{}, exitBusy: map[string]int{}, answers: map[string]string{}}
	r.m.cfg.SlowSpeed = 1 << 40 // always slow, so extra streams are wanted
	r.m.timings.poll, r.m.timings.probe, r.m.timings.cooldown = 10*time.Millisecond, 40*time.Millisecond, 40*time.Millisecond
	r.m.timings.wantedRecheck = 10 * time.Millisecond
	var urls []string
	ids := make([]string, routes)
	for i := range routes {
		var open atomic.Int32
		url := proxyServer(t, fmt.Sprintf("127.0.0.%d", 2+i), func(w http.ResponseWriter, req *http.Request) {
			if n := open.Add(1); n > 1 {
				t.Errorf("route %d asked for %d connections at once", i, n)
			}
			defer open.Add(-1)
			r.serve(w, req, ids[i])
		}).URL
		urls = append(urls, url)
	}
	r.pool = attachPool(t, r.m, urls)
	for i, u := range urls {
		ids[i] = r.pool.Page("keep2share", proxy.Query{Search: u}).Rows[0].ID
	}
	r.ids, r.urls = ids, urls
	for i, id := range ids {
		r.exits[id] = fmt.Sprintf("198.51.100.%d", 1+i) // each its own, unless a test says otherwise
	}
	echo := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if r.echoDown.Load() {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		_, _ = fmt.Fprint(w, "192.0.2.1") // the normal connection's own address
	}))
	t.Cleanup(echo.Close)
	r.echoHost = echo.Listener.Addr().String()
	r.m.cfg.ProxyExitProbes = []string{echo.URL + "/"}
	streams := 4
	if err := r.m.ApplySettings(Settings{Streams: &streams}); err != nil {
		t.Fatal(err)
	}
	return r
}

func (r *routeRig) serve(w http.ResponseWriter, req *http.Request, route string) {
	if req.URL.Host == "api.example.test" {
		_, _ = fmt.Fprint(w, `{"status":"success"}`)
		return
	}
	r.mu.Lock()
	refused, hang, exit, answer := r.refuse[route], r.hang[route], r.exits[route], r.answers[route]
	r.mu.Unlock()
	if req.URL.Host == r.echoHost {
		switch {
		case r.echoDown.Load():
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
		case answer != "":
			_, _ = fmt.Fprint(w, answer)
		default:
			_, _ = fmt.Fprint(w, exit)
		}
		return
	}
	if refused || req.URL.Query().Get("for") != route {
		http.Error(w, "this link is assigned to another address", http.StatusConflict)
		return
	}
	from, to := 0, len(r.payload)-1
	if spec, ok := strings.CutPrefix(req.Header.Get(httpx.HeaderRange), "bytes="); ok {
		a, b, _ := strings.Cut(spec, "-")
		from, _ = strconv.Atoi(a)
		if b != "" {
			to, _ = strconv.Atoi(b)
		}
	}
	to = min(to, len(r.payload)-1)
	w.Header().Set(httpx.HeaderContentType, "application/octet-stream")
	w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", from, to, len(r.payload)))
	w.Header().Set("Content-Length", strconv.Itoa(to-from+1))
	w.WriteHeader(http.StatusPartialContent)
	r.mu.Lock()
	r.exitBusy[exit]++
	if n := r.exitBusy[exit]; n > 1 {
		r.t.Errorf("%d downloads streamed through exit %s at once", n, exit)
	}
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		r.exitBusy[exit]--
		r.mu.Unlock()
	}()
	file := req.URL.Query().Get("file")
	for at := from; at <= to; {
		time.Sleep(5 * time.Millisecond) // the link's cap
		r.mu.Lock()
		sent := r.served[file][route]
		r.mu.Unlock()
		if hang > 0 && sent >= hang {
			<-req.Context().Done() // silent, with the connection open
			return
		}
		n := min(64<<10, to+1-at)
		if _, err := w.Write(r.payload[at : at+n]); err != nil || req.Context().Err() != nil {
			r.abandoned.Add(1)
			return
		}
		w.(http.Flusher).Flush()
		r.mu.Lock()
		if r.served[file] == nil {
			r.served[file] = map[string]int64{}
		}
		r.served[file][route] += int64(n)
		r.mu.Unlock()
		at += n
	}
}

// add queues a file whose resolver mints a link for whichever route asks,
// after first asking hold, when set.
func (r *routeRig) add(name string, hold func(ctx context.Context, route string) error) *Item {
	r.m.mu.Lock()
	defer r.m.mu.Unlock()
	job := &Job{ID: newID(), Source: "http://service.example.test/" + name}
	r.m.jobs[job.ID] = job
	r.m.order = append(r.m.order, job.ID)
	it := r.m.newItem(job, extractor.File{Name: name, Size: int64(len(r.payload)),
		Resolve: func(ctx context.Context) (*extractor.Target, error) {
			route := httpx.RouteID(ctx)
			r.mu.Lock()
			r.asked[route]++
			r.mu.Unlock()
			if hold != nil {
				if err := hold(ctx, route); err != nil {
					return nil, err
				}
			}
			return &extractor.Target{URL: "http://storage.example.test/f?file=" + name + "&for=" + route,
				Size: int64(len(r.payload))}, nil
		},
		Pace: &extractor.Pace{Files: 1, Streams: 1, Group: "keep2share", PerRoute: true}}, "", 0)
	job.Items = append(job.Items, it)
	r.m.queue = append(r.m.queue, it)
	r.m.signal()
	return it
}

func (r *routeRig) servedBy(file string) map[string]int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := map[string]int64{}
	for route, n := range r.served[file] {
		if n > 0 {
			out[route] = n
		}
	}
	return out
}

func (r *routeRig) done(its ...*Item) bool {
	r.m.mu.Lock()
	defer r.m.mu.Unlock()
	for _, it := range its {
		if it.Status != StatusDone {
			return false
		}
	}
	return true
}

func (r *routeRig) waitDone(timeout time.Duration, its ...*Item) {
	r.t.Helper()
	if !waitForCond(timeout, func() bool { return r.done(its...) }) {
		r.m.mu.Lock()
		defer r.m.mu.Unlock()
		for _, it := range its {
			r.t.Errorf("%s: %s %s", it.Name, it.Status, it.Err)
		}
		r.t.FailNow()
	}
	for _, it := range its {
		got, err := os.ReadFile(filepath.Join(r.m.DownloadDir(), it.Name))
		if err != nil || !bytes.Equal(got, r.payload) {
			r.t.Fatalf("%s holds %d bytes (%v), want the %d-byte file", it.Name, len(got), err, len(r.payload))
		}
	}
}

// With routes idle, a slow file is read through several, each with a link
// of its own and one connection, and arrives whole.
func TestAFileIsReadThroughIdleRoutesOfTheirOwn(t *testing.T) {
	r := newRouteRig(t, 3)
	it := r.add("clip.bin", nil)
	r.m.Start()
	r.waitDone(30*time.Second, it)
	if n := len(r.servedBy("clip.bin")); n != 3 {
		t.Fatalf("%d routes served the file; want all three", n)
	}
	if n := r.abandoned.Load(); n != 0 {
		t.Fatalf("%d responses were abandoned partway; want every one read to its end", n)
	}
}

// A file waiting behind the queue's limit on files at once waits for a slot,
// not a route: nothing starts until a transfer ends, and that transfer's
// route goes back with it. So it keeps no running file from extra streams,
// as fifty such files kept 254 running ones to a stream apiece. A file that
// could start does hold them back (routeWantedLocked).
func TestAFileWaitingBehindTheLimitLeavesTheRunningOneItsExtraStreams(t *testing.T) {
	r := newRouteRig(t, 3) // one worker
	first, second := r.add("first.bin", nil), r.add("second.bin", nil)
	r.m.Start()
	r.waitDone(60*time.Second, first, second)
	if n := len(r.servedBy("first.bin")); n < 2 {
		t.Fatalf("first.bin came through %d route while second.bin waited for the only worker; want extra streams", n)
	}
	if n := len(r.servedBy("second.bin")); n < 2 {
		t.Fatalf("second.bin, the last, came through %d route; want extra streams", n)
	}
	if n := r.abandoned.Load(); n != 0 {
		t.Fatalf("%d responses were abandoned partway; want every one read to its end", n)
	}
}

// A route being obtained for an extra stream is given up the moment a file
// finds none, and the file runs on it.
func TestARouteStillBeingObtainedGoesToAFileThatFindsNone(t *testing.T) {
	r := newRouteRig(t, 2)
	concurrency := 2
	if err := r.m.ApplySettings(Settings{Concurrency: &concurrency}); err != nil {
		t.Fatal(err)
	}
	obtaining, gaveUp := make(chan struct{}), make(chan struct{})
	var once sync.Once
	var servedThen atomic.Int64
	first := r.add("first.bin", func(ctx context.Context, route string) error {
		if route != r.ids[1] {
			return nil
		}
		once.Do(func() { close(obtaining) })
		<-ctx.Done() // a CAPTCHA and a ticket's timer, say
		var n int64
		for _, bytes := range r.servedBy("first.bin") {
			n += bytes
		}
		servedThen.Store(n)
		close(gaveUp)
		return ctx.Err()
	})
	r.m.mu.Lock()
	first.preferredRoute = r.ids[0]
	r.m.mu.Unlock()
	r.m.Start()
	select {
	case <-obtaining:
	case <-time.After(20 * time.Second):
		t.Fatal("no extra stream was sought")
	}
	second := r.add("second.bin", nil)
	select {
	case <-gaveUp:
	case <-time.After(20 * time.Second):
		t.Fatal("the route being obtained was not given up")
	}
	if n := servedThen.Load(); n >= int64(len(r.payload))/2 {
		t.Fatalf("the route was given up with %d of the %d bytes it was for served; want at once", n, len(r.payload))
	}
	r.waitDone(60*time.Second, first, second)
	if got := r.servedBy("first.bin"); len(got) != 1 || got[r.ids[0]] == 0 {
		t.Fatalf("first.bin came through %v; want its own route alone", got)
	}
	if got := r.servedBy("second.bin"); got[r.ids[1]] == 0 {
		t.Fatalf("second.bin came through %v; want the route given up", got)
	}
}

// An extra stream whose route refuses its link gives its range back, the
// route is held back, and the file still arrives whole. Each try costs the
// host a CAPTCHA, so the refusing routes are not asked again and again.
func TestAnExtraStreamRefusedGivesItsRangeBack(t *testing.T) {
	r := newRouteRig(t, 3)
	r.refuse[r.ids[1]], r.refuse[r.ids[2]] = true, true
	it := r.add("clip.bin", nil)
	r.m.mu.Lock()
	it.preferredRoute = r.ids[0]
	r.m.mu.Unlock()
	r.m.Start()
	r.waitDone(60*time.Second, it)
	r.mu.Lock()
	defer r.mu.Unlock()
	if tries := r.asked[r.ids[1]] + r.asked[r.ids[2]]; tries == 0 || tries > 4 {
		t.Fatalf("links minted on the refusing routes: %d; want a try or two", tries)
	}
}

// An extra stream that gets nowhere costs the host a CAPTCHA each time, so
// one whose route stays free is tried less and less often.
func TestExtraStreamsThatGetNowhereAreTriedLessOften(t *testing.T) {
	r := newRouteRig(t, 2)
	it := r.add("clip.bin", func(ctx context.Context, route string) error {
		if route == r.ids[1] {
			return errors.New("test service: no free download for you")
		}
		return nil
	})
	r.m.mu.Lock()
	it.preferredRoute = r.ids[0]
	r.m.mu.Unlock()
	r.m.Start()
	r.waitDone(60*time.Second, it)
	r.mu.Lock()
	defer r.mu.Unlock()
	if tries := r.asked[r.ids[1]]; tries == 0 || tries > 10 {
		t.Fatalf("an extra stream was sought %d times on a route that never gave one; want a few", tries)
	}
}

// Routes that cannot carry a request at all cost the host nothing, so they
// do not slow the search for an extra stream: most untested routes are dead,
// and each one tried is set aside.
func TestDeadRoutesDoNotSlowTheSearchForAnExtraStream(t *testing.T) {
	r := newRouteRig(t, 1)
	var dead []string
	for i := range 8 {
		l, err := net.Listen("tcp", fmt.Sprintf("127.0.1.%d:0", 1+i))
		if err != nil {
			t.Fatal(err)
		}
		dead = append(dead, "http://"+l.Addr().String())
		l.Close() // refused from now on
	}
	attachPool(t, r.m, append([]string{r.urls[0]}, dead...))
	// Counted at the resolver, which a dead route reaches only without the
	// question about its exit; with it, the dead route fails there instead.
	r.m.cfg.ProxyExitProbes = nil
	it := r.add("clip.bin", func(ctx context.Context, route string) error {
		// The host's API comes first, through the route, as a CAPTCHA would.
		req, err := r.m.client.NewRequest(ctx, http.MethodGet, "http://api.example.test/ticket", nil)
		if err != nil {
			return err
		}
		resp, err := r.m.client.DoOnce(req)
		if err != nil {
			return fmt.Errorf("test service: ticket: %w", err)
		}
		resp.Body.Close()
		return nil
	})
	r.m.mu.Lock()
	it.preferredRoute = r.ids[0]
	r.m.mu.Unlock()
	r.m.Start()
	r.waitDone(60*time.Second, it)
	r.mu.Lock()
	defer r.mu.Unlock()
	tried := 0
	for route, n := range r.asked {
		if route != r.ids[0] && n > 0 {
			tried++
		}
	}
	if tried != len(dead) {
		t.Fatalf("tried %d of the %d dead routes while the file downloaded; want every one, without waiting longer each time", tried, len(dead))
	}
}

// Two routes that leave by one exit never carry two downloads at once: the
// host would refuse the second. The file sent to the second is turned away
// at no cost and goes on through the route with an exit of its own, while
// the first keeps downloading. The rig fails the test if an exit ever
// streams two responses at once, extra streams' included.
func TestTwoDownloadsNeverShareAnExit(t *testing.T) {
	r := newRouteRig(t, 3)
	r.exits[r.ids[1]] = r.exits[r.ids[0]] // one exit behind two routes
	concurrency, streams := 2, 1
	if err := r.m.ApplySettings(Settings{Concurrency: &concurrency, Streams: &streams}); err != nil {
		t.Fatal(err)
	}
	first := r.add("first.bin", nil)
	r.m.mu.Lock()
	first.preferredRoute = r.ids[0]
	r.m.mu.Unlock()
	r.m.Start()
	if !waitForCond(10*time.Second, func() bool { return len(r.servedBy("first.bin")) > 0 }) {
		t.Fatal("first.bin never started")
	}
	second := r.add("second.bin", nil)
	r.m.mu.Lock()
	second.preferredRoute = r.ids[1] // the route sharing first.bin's exit
	r.m.mu.Unlock()
	r.waitDone(60*time.Second, first, second)
	if got := r.servedBy("second.bin"); got[r.ids[2]] == 0 {
		t.Fatalf("second.bin came through %v; want the route with an exit of its own", got)
	}
	r.m.mu.Lock()
	shared := r.m.routeOutcomes[routeOutcome{"keep2share", "shared"}]
	r.m.mu.Unlock()
	if shared == 0 {
		t.Fatal("no attempt was turned away from the shared exit")
	}
}

// The echo is also how a route shows it works. One that answers it with a
// page of its own instead of an address is shelved before a CAPTCHA is
// spent on it, and the file goes on through another, at no cost.
func TestARouteAnsweringWithoutAnAddressIsShelved(t *testing.T) {
	r := newRouteRig(t, 2)
	r.answers[r.ids[0]] = "<html><body>Access denied</body></html>"
	streams := 1
	if err := r.m.ApplySettings(Settings{Streams: &streams}); err != nil {
		t.Fatal(err)
	}
	it := r.add("clip.bin", nil)
	r.m.mu.Lock()
	it.preferredRoute = r.ids[0]
	r.m.mu.Unlock()
	r.m.Start()
	r.waitDone(60*time.Second, it)
	r.mu.Lock()
	asked := r.asked[r.ids[0]]
	r.mu.Unlock()
	if asked != 0 {
		t.Fatalf("the host was asked through the broken route %d times; want none", asked)
	}
	if got := r.servedBy("clip.bin"); got[r.ids[1]] == 0 {
		t.Fatalf("served %v; want the working route", got)
	}
	if row := r.pool.Page("keep2share", proxy.Query{Search: r.urls[0]}).Rows; len(row) != 1 || row[0].Status != "cooling" {
		t.Fatalf("the broken route: %+v; want it shelved", row)
	}
	if it.proxyRetries != 0 {
		t.Fatalf("the broken route cost the file %d retries; want none", it.proxyRetries)
	}
}

// With the echo services down for everyone, the normal connection
// included, nothing is blamed on the routes: downloads go ahead without
// their exits known.
func TestAnEchoOutageDoesNotStopDownloads(t *testing.T) {
	r := newRouteRig(t, 1)
	r.echoDown.Store(true)
	it := r.add("clip.bin", nil)
	r.m.Start()
	r.waitDone(60*time.Second, it)
	if row := r.pool.Page("keep2share", proxy.Query{Search: r.urls[0]}).Rows; len(row) != 1 || row[0].Status == "cooling" {
		t.Fatalf("the route: %+v; want it not blamed for the outage", row)
	}
}

// An extra stream whose route goes silent gives its range up, and another
// connection reads it, while the rest of the file keeps moving.
func TestASilentExtraStreamGivesUpItsRange(t *testing.T) {
	r := newRouteRig(t, 2)
	r.m.cfg.StallTimeout = time.Second
	r.hang[r.ids[1]] = 256 << 10
	it := r.add("clip.bin", nil)
	r.m.mu.Lock()
	it.preferredRoute = r.ids[0]
	r.m.mu.Unlock()
	r.m.Start()
	r.waitDone(60*time.Second, it)
	if got := r.servedBy("clip.bin"); got[r.ids[1]] == 0 || got[r.ids[1]] > 1<<20 {
		t.Fatalf("served %v; want the silent route's few bytes and the rest through the other", got)
	}
	if row := r.pool.Page("keep2share", proxy.Query{Search: r.urls[1]}).Rows; len(row) != 1 || row[0].Status != "cooling" {
		t.Fatalf("the silent route: %+v; want it held back", row)
	}
}

// Resuming a file split across routes, with one route to go on: its ranges
// are read one at a time on it, never two at once, and the file is whole.
func TestAResumedSplitFileIsReadOneRangeAtATimeOnOneRoute(t *testing.T) {
	r := newRouteRig(t, 1)
	it := r.add("clip.bin", nil)
	r.m.mu.Lock()
	source := r.m.jobs[it.JobID].Source
	r.m.mu.Unlock()
	third := int64(len(r.payload) / 3)
	segments := []segmentState{
		{Start: 0, Pos: third / 2, End: third},
		{Start: third, Pos: third + third/4, End: 2 * third},
		{Start: 2 * third, Pos: 2 * third, End: int64(len(r.payload))},
	}
	part := filepath.Join(r.m.DownloadDir(), partNames("clip.bin", source, it.ID)[0])
	file := make([]byte, len(r.payload))
	for _, s := range segments {
		copy(file[s.Start:s.Pos], r.payload[s.Start:s.Pos])
	}
	if err := os.WriteFile(part, file, 0o644); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(r.m.DownloadDir())
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := saveTransferState(part, &transferState{Size: int64(len(r.payload)), Segments: segments},
		transferFiles{root: root}); err != nil {
		t.Fatal(err)
	}
	r.m.Start()
	r.waitDone(30*time.Second, it)
}

// The item's own route is scored on what it read itself, not on what extra
// streams read through theirs.
func TestTheItemsRouteIsCreditedOnlyWithItsOwnBytes(t *testing.T) {
	m := busyManager(t)
	route := proxyServer(t, "127.0.0.2", func(http.ResponseWriter, *http.Request) {}).URL
	pool := attachPool(t, m, []string{route})
	row := pool.Page("keep2share", proxy.Query{Search: route}).Rows[0]
	lease := pool.Acquire("keep2share", row.ID, 1)
	if lease == nil {
		t.Fatal("no route")
	}
	defer lease.Release()
	now := time.Now()
	it := &Item{ID: "item", Status: StatusRunning, route: lease, lastSample: now.Add(-6 * time.Second)}
	it.downloaded.Store(3 << 20)
	it.extraBytes.Store(2 << 20)
	m.mu.Lock()
	m.jobs["job"] = &Job{ID: "job", Items: []*Item{it}}
	m.sampleLocked(now)
	m.mu.Unlock()
	got := pool.Page("keep2share", proxy.Query{Search: route}).Rows[0].Throughput
	if want := float64(1<<20) / 6; math.Abs(got-want) > want/100 {
		t.Fatalf("the route is credited %.0f B/s; want its own %.0f", got, want)
	}
}

// A multi-route connection's requests are bounded, and no split takes any
// of what has been asked for.
func TestARequestedRangeIsNeverSplit(t *testing.T) {
	const mib = 1 << 20
	table := newSegmentTable(100*mib, 0)
	head := table.snapshot()[0]
	if from, to := table.request(head, 8*mib); from != 0 || to != 8*mib {
		t.Fatalf("asked for %d-%d, want the first 8 MiB", from, to)
	}
	// Half of what is left goes, from the midpoint, which is past the range.
	tail := table.splitUnrequested(mib)
	if tail == nil || tail.start != 50*mib || head.end.Load() != 50*mib {
		t.Fatalf("split off %+v; want the second half", tail)
	}
	// Asked to its end, the head has nothing to give; the tail is split.
	head.pos.Store(45 * mib)
	if from, to := table.request(head, 8*mib); from != 45*mib || to != 50*mib {
		t.Fatalf("asked for %d-%d, want up to the segment's end", from, to)
	}
	if next := table.splitUnrequested(mib); next == nil || next.start != 75*mib {
		t.Fatalf("split off %+v; want the tail's second half", next)
	}
	// Asked past the midpoint, the split comes after the range instead.
	short := newSegmentTable(20*mib, 0)
	seg := short.snapshot()[0]
	short.request(seg, 15*mib)
	if next := short.splitUnrequested(mib); next == nil || next.start != 15*mib {
		t.Fatalf("split off %+v; want the 5 MiB not asked for", next)
	}
	// And with less than a piece not asked for, nothing.
	tiny := newSegmentTable(20*mib, 14*mib)
	tiny.request(tiny.snapshot()[0], 5*mib+mib/2)
	if next := tiny.splitUnrequested(mib); next != nil {
		t.Fatalf("split off %+v of a range asked for", next)
	}
}

// A range nobody reads is claimed once, and given back, claimed again.
func TestAnUnreadRangeIsClaimedOnce(t *testing.T) {
	table := restoreSegmentTable(30, []segmentState{{Start: 0, Pos: 10, End: 10}, {Start: 10, Pos: 12, End: 20}, {Start: 20, Pos: 20, End: 30}})
	first := table.claim()
	if first == nil || first.start != 10 {
		t.Fatalf("claimed %+v; want the first unfinished range", first)
	}
	table.request(first, 4)
	if second := table.claim(); second == nil || second.start != 20 {
		t.Fatalf("claimed %+v; want the next one", second)
	}
	if none := table.claim(); none != nil {
		t.Fatalf("claimed %+v of ranges already being read", none)
	}
	table.release(first)
	if again := table.claim(); again != first || again.requested.Load() != 0 {
		t.Fatalf("claimed %+v; want the range given back, asked for afresh", again)
	}
}

// The queue sets up only so many extra streams at once: each is a hunt and a
// CAPTCHA, which the files' own routes need first. Extra streams still come,
// a few at a time; it is no bound on how many run.
func TestTheQueueSetsUpOnlySoManyExtraStreamsAtOnce(t *testing.T) {
	r := newRouteRig(t, 6)
	concurrency := 2
	if err := r.m.ApplySettings(Settings{Concurrency: &concurrency}); err != nil {
		t.Fatal(err)
	}
	r.m.timings.extraAcquisitions = 1
	var mu sync.Mutex
	own := map[string]string{}
	slow := func(name string) func(context.Context, string) error {
		return func(ctx context.Context, route string) error {
			mu.Lock()
			if own[name] == "" {
				own[name] = route
			}
			extra := own[name] != route
			mu.Unlock()
			if extra { // long enough for the two files' setups to overlap
				select {
				case <-time.After(300 * time.Millisecond):
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			return nil
		}
	}
	first, second := r.add("first.bin", slow("first.bin")), r.add("second.bin", slow("second.bin"))
	var most atomic.Int32
	stop := make(chan struct{})
	sampled := make(chan struct{})
	go func() {
		defer close(sampled)
		for {
			select {
			case <-stop:
				return
			case <-time.After(5 * time.Millisecond):
			}
			if n := r.m.extraAcquiring.Load(); n > most.Load() {
				most.Store(n)
			}
		}
	}()
	r.m.Start()
	r.waitDone(60*time.Second, first, second)
	close(stop)
	<-sampled
	if n := most.Load(); n > 1 {
		t.Fatalf("%d extra streams were set up at once; want one at a time", n)
	}
	if len(r.servedBy("first.bin"))+len(r.servedBy("second.bin")) < 3 {
		t.Fatal("no extra stream was set up at all")
	}
}
