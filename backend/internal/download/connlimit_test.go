package download

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

// pixeldrainRefusal is the body this host sends with its 403, cut to the
// fields that decide anything. The value is the machine-readable half and is
// what the downloader matches on; the sentence beside it is for a person.
const pixeldrainRefusal = `{"success":false,"value":"max_concurrent_downloads",` +
	`"message":"You have reached the maximum number of open download connections for ` +
	`free/anonymous accounts. If you wish to download more please wait for your other ` +
	`downloads to finish."}`

// refusingServer answers the first refusals requests with a connection-limit
// 403 and serves the payload after that, which is what a host does while
// somebody else's download of ours is still holding a slot.
func refusingServer(payload []byte, refusals int32, body string) (http.Handler, *atomic.Int32) {
	var hits atomic.Int32
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) <= refusals {
			w.Header().Set(httpx.HeaderContentType, "application/json")
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, body)
			return
		}
		(&rangeServer{payload: payload}).handler().ServeHTTP(w, r)
	})
	return handler, &hits
}

// A host that refuses because this caller already has too many downloads
// open is describing a queue we are filling ourselves, and it clears when one
// of our own transfers finishes. Waiting is the whole answer.
func TestConnectionLimitIsWaitedOutRatherThanFailed(t *testing.T) {
	payload := make([]byte, 64<<10)
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}

	handler, hits := refusingServer(payload, 3, pixeldrainRefusal)
	srv := httptest.NewServer(handler)
	defer srv.Close()

	m := busyManager(t)
	it := &Item{ID: newID(), Name: "clip.mp4", URL: srv.URL + "/api/file/AAAA?download", Size: -1}

	if err := m.transfer(context.Background(), it); err != nil {
		t.Fatalf("a transfer gave up on a host that was only asking it to queue: %v", err)
	}
	if got := hits.Load(); got != 4 {
		t.Errorf("server saw %d requests, want the three refusals plus the one that worked", got)
	}

	got, err := os.ReadFile(filepath.Join(m.cfg.DownloadDir, "clip.mp4"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Error("the file that finally downloaded differs from the source")
	}
}

// The same refusal arrives when the connections belong to something else on
// this address, and then nothing here can free one — so the patience is
// bounded and a headless run still terminates.
func TestConnectionLimitStopsAtItsBudget(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set(httpx.HeaderContentType, "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, pixeldrainRefusal)
	}))
	defer srv.Close()

	m := busyManager(t)
	it := &Item{ID: newID(), Name: "clip.mp4", URL: srv.URL + "/api/file/AAAA?download", Size: -1}

	err := m.transfer(context.Background(), it)
	if err == nil {
		t.Fatal("a host that never lets go should eventually fail the transfer")
	}
	// The refusal is what the caller needs to read, so it survives the wait.
	if !strings.Contains(err.Error(), "max_concurrent_downloads") {
		t.Errorf("the error should carry what the host actually said, got: %v", err)
	}
	if got, want := hits.Load(), int32(config.ConnectionLimitRetries)+1; got != want {
		t.Errorf("server saw %d requests, want %d", got, want)
	}
}

// A 403 is forbidden until a host says otherwise in the body. Reading every
// one of them as a queueing problem would retry a genuinely private file ten
// times over before reporting what was wrong with it.
func TestAPlainForbiddenStillFailsAtOnce(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		http.Error(w, "this file is private", http.StatusForbidden)
	}))
	defer srv.Close()

	m := busyManager(t)
	it := &Item{ID: newID(), Name: "clip.mp4", URL: srv.URL + "/f/AAAA", Size: -1}

	if err := m.transfer(context.Background(), it); err == nil {
		t.Fatal("a forbidden file resolved to a download")
	}
	if got := hits.Load(); got != 1 {
		t.Errorf("server saw %d requests, want the one that was refused", got)
	}
}

// The refusal is evidence about the host, not only about this transfer: it
// was said of a connection the file could not proceed without, so there is
// certainly no room for the speculative extras splitting would add.
func TestARefusedConnectionClosesTheHostsExtraBudget(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set(httpx.HeaderContentType, "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, pixeldrainRefusal)
	}))
	defer srv.Close()

	m := busyManager(t)
	host := hostOf(srv.URL)
	if m.hosts.limit(host) == 0 {
		t.Fatal("the host started with no allowance, so this proves nothing")
	}

	it := &Item{ID: newID(), Name: "clip.mp4", URL: srv.URL + "/api/file/AAAA?download", Size: -1}
	_ = m.transfer(context.Background(), it)

	if got := m.hosts.limit(host); got != 0 {
		t.Errorf("the host's extra-connection allowance is %d, want none left", got)
	}
}

func TestRefusedForConnectionCount(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"pixeldrain's refusal", &httpx.StatusError{
			Code: http.StatusForbidden, Status: "403 Forbidden", Body: pixeldrainRefusal,
		}, true},
		{"the same, wrapped", fmt.Errorf("fetch: %w", &httpx.StatusError{
			Code: http.StatusForbidden, Body: pixeldrainRefusal,
		}), true},
		{"a plain forbidden", &httpx.StatusError{
			Code: http.StatusForbidden, Body: "this file is private",
		}, false},
		{"a refusal with no body at all", &httpx.StatusError{Code: http.StatusForbidden}, false},
		{"something that is not a status", io.ErrUnexpectedEOF, false},
	}
	for _, tc := range cases {
		if got := refusedForConnectionCount(tc.err); got != tc.want {
			t.Errorf("%s: refusedForConnectionCount = %v, want %v", tc.name, got, tc.want)
		}
		// An extra connection turned away for this reason is turned away
		// just as much as one met with a 429.
		if tc.want && !refusedExtraConnection(tc.err) {
			t.Errorf("%s: the segment supervisor would not recognise it", tc.name)
		}
	}
}

func TestSaturatedLeavesNoAllowance(t *testing.T) {
	l := newHostLimiter(4)
	if !l.reserve("example.test") {
		t.Fatal("a fresh host refused the first extra connection")
	}
	l.release("example.test")

	l.saturated("example.test")
	if got := l.limit("example.test"); got != 0 {
		t.Errorf("limit = %d, want 0", got)
	}
	if l.reserve("example.test") {
		t.Error("a saturated host handed out an extra connection anyway")
	}
}
