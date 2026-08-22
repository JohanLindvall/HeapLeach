package server

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/JohanLindvall/HeapLeach/internal/download"
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

	if rec := get(t, handler, "/api/health"); rec.Code != http.StatusOK {
		t.Errorf("health = %d", rec.Code)
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
