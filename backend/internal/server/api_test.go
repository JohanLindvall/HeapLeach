// SPDX-License-Identifier: MIT

package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/download"
	"github.com/JohanLindvall/HeapLeach/internal/extractor"
)

func postJSON(t *testing.T, handler http.Handler, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func get(t *testing.T, handler http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

func TestHealthAndState(t *testing.T) {
	_, handler := newTestServer(t)

	health := get(t, handler, "/api/health")
	if health.Code != http.StatusOK {
		t.Errorf("health = %d", health.Code)
	}
	// A deploy tool reads the version to tell the new build from the old.
	if !strings.Contains(health.Body.String(), `"version":"v0.0.0-test"`) {
		t.Errorf("health = %s, want the build's version", health.Body.String())
	}

	rec := get(t, handler, "/api/state")
	if rec.Code != http.StatusOK {
		t.Fatalf("state = %d", rec.Code)
	}
	// Live state served stale by an intermediary is worse than no state.
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
	var snap download.Snapshot
	if err := json.Unmarshal(rec.Body.Bytes(), &snap); err != nil {
		t.Fatalf("state is not a snapshot: %v", err)
	}
	if snap.MaxConcur <= 0 || snap.MaxStreams <= 0 {
		t.Errorf("snapshot is missing its bounds: %+v", snap)
	}
}

// The urls field accepts both shapes the UI and scripts send: one string of
// pasted lines, or a JSON array.
func TestAddAcceptsBothURLShapes(t *testing.T) {
	_, handler := newTestServer(t)

	rec := postJSON(t, handler, "/api/downloads",
		`{"urls":"https://one.example.test/a\nhttps://two.example.test/b, https://three.example.test/c"}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("string form = %d: %s", rec.Code, rec.Body)
	}
	var out struct {
		Accepted []struct{ ID, URL string }    `json:"accepted"`
		Rejected []struct{ URL, Error string } `json:"rejected"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Accepted) != 3 || len(out.Rejected) != 0 {
		t.Errorf("accepted %d rejected %d, want 3/0: %s", len(out.Accepted), len(out.Rejected), rec.Body)
	}

	rec = postJSON(t, handler, "/api/downloads", `{"urls":["https://four.example.test/d"]}`)
	if rec.Code != http.StatusAccepted {
		t.Errorf("array form = %d: %s", rec.Code, rec.Body)
	}
}

func TestAddRejectsTheUnusable(t *testing.T) {
	_, handler := newTestServer(t)

	// Nothing at all.
	if rec := postJSON(t, handler, "/api/downloads", `{"urls":""}`); rec.Code != http.StatusBadRequest {
		t.Errorf("empty urls = %d", rec.Code)
	}

	// A scheme the extractors cannot fetch. Every URL failing means the
	// request as a whole failed.
	rec := postJSON(t, handler, "/api/downloads", `{"urls":"ftp://example.test/file"}`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("all-rejected = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "ftp") {
		t.Errorf("the rejection should name the URL: %s", rec.Body)
	}

	// A body that is not JSON at all.
	if rec := postJSON(t, handler, "/api/downloads", `{"urls": nonsense`); rec.Code != http.StatusBadRequest {
		t.Errorf("bad body = %d", rec.Code)
	}

	// An unknown field is a client bug worth naming, not ignoring.
	if rec := postJSON(t, handler, "/api/downloads", `{"link":"https://a.test/"}`); rec.Code != http.StatusBadRequest {
		t.Errorf("unknown field = %d", rec.Code)
	}
}

func TestSettingsValidatesAndApplies(t *testing.T) {
	manager, handler := newTestServer(t)

	rec := postJSON(t, handler, "/api/settings", `{"concurrency":8,"streams":2,"speedLimit":1000000,"paused":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("settings = %d: %s", rec.Code, rec.Body)
	}
	var snap download.Snapshot
	if err := json.Unmarshal(rec.Body.Bytes(), &snap); err != nil {
		t.Fatal(err)
	}
	if snap.Concurrency != 8 || snap.Streams != 2 || snap.SpeedLimit != 1000000 || !snap.Paused {
		t.Errorf("settings did not apply: %+v", snap)
	}
	if !manager.Paused() {
		t.Error("the manager itself is not paused")
	}

	// Out-of-range values are refused with the bound in the message.
	rec = postJSON(t, handler, "/api/settings", `{"concurrency":0}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "between") {
		t.Errorf("bad concurrency = %d: %s", rec.Code, rec.Body)
	}
	if rec := postJSON(t, handler, "/api/settings", `{"speedLimit":-5}`); rec.Code != http.StatusBadRequest {
		t.Errorf("negative limit = %d", rec.Code)
	}

	// A body carrying nothing changes nothing and still answers with state.
	if rec := postJSON(t, handler, "/api/settings", `{}`); rec.Code != http.StatusOK {
		t.Errorf("empty settings = %d", rec.Code)
	}
}

// The manager's errors carry their meaning as sentinel values, and the
// status each maps to is what a client branches on: a password prompt is
// only ever shown for a 401.
func TestWriteManagerErrorMapsTheSentinels(t *testing.T) {
	cases := []struct {
		err  error
		want int
	}{
		{download.ErrNotFound, http.StatusNotFound},
		{extractor.ErrPasswordRequired, http.StatusUnauthorized},
		{fmt.Errorf("gofile: %w", extractor.ErrPasswordRequired), http.StatusUnauthorized},
		{errors.New("anything else"), http.StatusBadRequest},
	}
	for _, tc := range cases {
		rec := httptest.NewRecorder()
		writeManagerError(rec, tc.err)
		if rec.Code != tc.want {
			t.Errorf("%v -> %d, want %d", tc.err, rec.Code, tc.want)
		}
		var body struct {
			Error string `json:"error"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.Error == "" {
			t.Errorf("%v: body %q is not an error document", tc.err, rec.Body.String())
		}
	}
}

func TestJobActionsAnswerNotFoundForStrangers(t *testing.T) {
	_, handler := newTestServer(t)

	for _, target := range []struct{ method, path string }{
		{http.MethodPost, "/api/jobs/nope/cancel"},
		{http.MethodPost, "/api/jobs/nope/retry"},
		{http.MethodDelete, "/api/jobs/nope"},
		{http.MethodPost, "/api/jobs/nope/items/also-nope/cancel"},
		{http.MethodPost, "/api/jobs/nope/items/also-nope/retry"},
	} {
		req := httptest.NewRequest(target.method, target.path, nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s %s = %d, want 404", target.method, target.path, rec.Code)
		}
	}
}

func TestJobLifecycleOverTheAPI(t *testing.T) {
	manager, handler := newTestServer(t)

	// example.test never resolves, so the job exists but goes nowhere —
	// exactly what cancel and remove need.
	id, err := manager.Add("https://job.example.test/file.bin", "")
	if err != nil {
		t.Fatal(err)
	}

	if rec := postJSON(t, handler, "/api/jobs/"+id+"/cancel", ""); rec.Code != http.StatusOK {
		t.Errorf("cancel = %d: %s", rec.Code, rec.Body)
	}

	req := httptest.NewRequest(http.MethodDelete, "/api/jobs/"+id, nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("remove = %d: %s", rec.Code, rec.Body)
	}

	// Gone means gone.
	if rec := postJSON(t, handler, "/api/jobs/"+id+"/retry", ""); rec.Code != http.StatusNotFound {
		t.Errorf("retry after remove = %d, want 404", rec.Code)
	}
}

func TestClearRemovesOnlyTheFinished(t *testing.T) {
	manager, handler := newTestServer(t)

	id, err := manager.Add("https://clear.example.test/file.bin", "")
	if err != nil {
		t.Fatal(err)
	}
	// Still resolving or queued: not finished, so not cleared.
	rec := postJSON(t, handler, "/api/clear", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("clear = %d", rec.Code)
	}
	_ = id
	var out struct {
		Removed int `json:"removed"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Removed != 0 {
		t.Errorf("cleared %d jobs while none were finished", out.Removed)
	}
}

func TestSplitURLs(t *testing.T) {
	got := splitURLs("https://a.test/1,\nhttps://b.test/2;  <https://c.test/3>\t'https://d.test/4'")
	want := []string{"https://a.test/1", "https://b.test/2", "https://c.test/3", "https://d.test/4"}
	if len(got) != len(want) {
		t.Fatalf("splitURLs = %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("splitURLs[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestSPAFallsBackToIndex(t *testing.T) {
	_, handler := newTestServer(t)

	index := get(t, handler, "/")
	if index.Code != http.StatusOK {
		t.Fatalf("index = %d", index.Code)
	}

	// An unknown path is the SPA's problem, not a 404: a hard refresh on a
	// client-side route must serve the app.
	deep := get(t, handler, "/some/client/route")
	if deep.Code != http.StatusOK {
		t.Fatalf("deep link = %d", deep.Code)
	}
	if !strings.Contains(deep.Body.String(), "<html") && !strings.Contains(deep.Body.String(), "<!doctype") {
		t.Errorf("deep link did not serve the page: %.60q", deep.Body.String())
	}
	if got := deep.Header().Get("Cache-Control"); !strings.Contains(got, "no-cache") {
		t.Errorf("the entry document must not be cached, got %q", got)
	}
}

func TestAPIErrorsAndMissingAssetsDoNotServeTheSPA(t *testing.T) {
	_, handler := newTestServer(t)
	for _, tc := range []struct {
		method, path string
		status       int
	}{
		{http.MethodGet, "/api/missing", http.StatusNotFound},
		{http.MethodPost, "/api/state", http.StatusMethodNotAllowed},
		{http.MethodGet, "/api/downloads", http.StatusMethodNotAllowed},
		{http.MethodGet, "/assets/missing.js", http.StatusNotFound},
		{http.MethodPost, "/some/client/route", http.StatusMethodNotAllowed},
	} {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, nil))
		if rec.Code != tc.status || strings.Contains(rec.Body.String(), "<html") {
			t.Errorf("%s %s = %d %q", tc.method, tc.path, rec.Code, rec.Body.String())
		}
	}
	index := get(t, handler, "/")
	if index.Header().Get("X-Frame-Options") != "DENY" || index.Header().Get("Referrer-Policy") != "no-referrer" {
		t.Fatalf("UI security headers missing: %v", index.Header())
	}
}

func TestOversizedBodiesReturn413WithoutChangingSettings(t *testing.T) {
	m, handler := newTestServer(t)
	for _, body := range []string{
		`{"downloadDir":"` + strings.Repeat("a", config.MaxRequestBytes) + `"}`,
		`{"paused":true}` + strings.Repeat(" ", config.MaxRequestBytes),
	} {
		rec := postJSON(t, handler, "/api/settings", body)
		if rec.Code != http.StatusRequestEntityTooLarge || m.Paused() {
			t.Fatalf("oversized update: status %d, paused %v", rec.Code, m.Paused())
		}
	}
}

func TestPanicsBecomeInternalErrors(t *testing.T) {
	// Wrap a handler that panics behind the same recovery middleware the
	// real routes sit behind.
	mux := http.NewServeMux()
	mux.HandleFunc("/boom", func(http.ResponseWriter, *http.Request) { panic("kaboom") })
	s := &Server{log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	rec := httptest.NewRecorder()
	s.recoverPanics(mux).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/boom", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("panic answered %d, want 500", rec.Code)
	}
}

func TestTrailingJSONCannotMutateSettings(t *testing.T) {
	m, handler := newTestServer(t)
	for _, suffix := range []string{` {"paused":false}`, ` garbage`, ` null`} {
		rec := postJSON(t, handler, "/api/settings", `{"paused":true}`+suffix)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("trailing %q: status %d", suffix, rec.Code)
		}
		if m.Snapshot().Paused {
			t.Fatal("malformed request paused the queue")
		}
	}
	if rec := postJSON(t, handler, "/api/settings", "{\"paused\":true}\n\t"); rec.Code != http.StatusOK {
		t.Errorf("trailing whitespace: status %d", rec.Code)
	}
}

func TestInvalidSettingsLeaveEverySettingUnchanged(t *testing.T) {
	manager, handler := newTestServer(t)
	before := manager.Snapshot()
	for _, body := range []string{
		`{"concurrency":8,"streams":0}`,
		`{"concurrency":8,"streams":2,"speedLimit":-1}`,
		`{"concurrency":8,"streams":2,"downloadDir":""}`,
		`{"concurrency":8,"proxies":true,"proxyEndpoints":["file:///tmp/proxy"]}`,
		`{"concurrency":8,"proxies":true,"proxyEndpoints":[],"proxyFeeds":[]}`,
		`{"concurrency":8,"proxyFeeds":["not-a-feed"]}`,
	} {
		rec := postJSON(t, handler, "/api/settings", body)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("settings = %d: %s", rec.Code, rec.Body)
		}
		after := manager.Snapshot()
		if after.Concurrency != before.Concurrency || after.Streams != before.Streams || after.SpeedLimit != before.SpeedLimit || after.DownloadDir != before.DownloadDir || after.Proxies != before.Proxies {
			t.Fatalf("invalid settings partially applied: %s", body)
		}
	}
}

func TestProxySettingsAndInventoryAPI(t *testing.T) {
	m, handler := newTestServer(t)
	if rec := get(t, handler, "/api/proxies"); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"rows":[]`) {
		t.Fatalf("disabled pool: %d %s", rec.Code, rec.Body)
	}
	rec := postJSON(t, handler, "/api/settings", `{"proxies":true,"concurrency":4,"proxyEndpoints":["http://reader:private-password@proxy.example.test:80","direct"],"proxyFeeds":[]}`)
	if rec.Code != http.StatusOK || !m.Snapshot().Proxies || m.Snapshot().Concurrency != 4 {
		t.Fatalf("enable at runtime: %d %s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), "private-password") {
		t.Fatal("queue snapshot exposed a proxy password")
	}
	rec = get(t, handler, "/api/settings")
	if rec.Code != http.StatusOK || rec.Header().Get("Cache-Control") != "no-store" || !strings.Contains(rec.Body.String(), "private-password") {
		t.Fatal("explicit settings editor cannot round-trip an authenticated endpoint")
	}
	rec = get(t, handler, "/api/proxies?limit=1&offset=1&sort=address")
	if rec.Code != http.StatusOK || rec.Header().Get("Cache-Control") != "no-store" || strings.Contains(rec.Body.String(), "private-password") {
		t.Fatalf("inventory response or credential redaction failed: %d", rec.Code)
	}
	var page struct {
		Rows                 []json.RawMessage
		Total, Offset, Limit int
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil || len(page.Rows) != 1 || page.Total != 2 || page.Offset != 1 {
		t.Fatalf("bounded inventory: %v %+v", err, page)
	}
	for _, service := range []string{"fileboom", "cloudflare"} {
		if rec := get(t, handler, "/api/proxies?site="+service); rec.Code != http.StatusOK {
			t.Fatalf("%s inventory: %d %s", service, rec.Code, rec.Body)
		}
	}
	for _, query := range []string{"limit=0", "limit=999999", "offset=-1", "limit=no", "sort=wrong", "status=wrong", "site=other"} {
		if rec := get(t, handler, "/api/proxies?"+query); rec.Code != http.StatusBadRequest {
			t.Fatalf("accepted %q", query)
		}
	}
	if rec := postJSON(t, handler, "/api/settings", `{"proxies":false}`); rec.Code != http.StatusOK || m.Snapshot().Proxies {
		t.Fatal("could not disable proxies at runtime")
	}
	if rec := get(t, handler, "/api/proxies"); !strings.Contains(rec.Body.String(), `"total":2`) {
		t.Fatal("disabling lost the inventory")
	}
}
