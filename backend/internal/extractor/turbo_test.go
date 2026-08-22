package extractor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

// turboServer stands in for the signing endpoint, which lives on the site's
// own origin — that is what lets the extractor be tested end to end offline.
func turboServer(t *testing.T, handle func(id string) (int, string)) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/sign" {
			t.Errorf("path = %q, want /api/sign", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		status, payload := handle(r.URL.Query().Get("v"))
		w.Header().Set(httpx.HeaderContentType, httpx.ContentTypeJSON)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(payload))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestTurboExtractSignsAndDefersResolution(t *testing.T) {
	var calls int
	srv := turboServer(t, func(id string) (int, string) {
		if id != "abc123" {
			t.Errorf("signed id = %q, want abc123", id)
		}
		calls++
		return http.StatusOK, `{"success":true,` +
			`"url":"https://cdn.example.test/v/abc123.mp4?token=t` + strings.Repeat("x", calls) + `",` +
			`"original_filename":"a clip.mp4","filename":"abc123.mp4"}`
	})

	turbo := NewTurbo(httpx.New("test-agent", "en-US", 0, 5*time.Second))
	u, _ := ParseURL(srv.URL + "/e/abc123")
	res, err := turbo.Extract(context.Background(), u, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Files) != 1 {
		t.Fatalf("got %d files, want 1", len(res.Files))
	}
	file := res.Files[0]
	if file.Name != "a clip.mp4" {
		t.Errorf("name = %q, want the original filename", file.Name)
	}
	if file.Resolve == nil {
		t.Fatal("no resolver; turbo links expire and must be re-signed per attempt")
	}

	// Each attempt signs afresh, so a link signed while an item waited its
	// turn is never the one fetched.
	target, err := file.Resolve(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if target.URL == file.URL {
		t.Error("the resolver handed back the extraction-time link")
	}
	if calls != 2 {
		t.Errorf("signing endpoint called %d times, want once at extract and once at resolve", calls)
	}
}

func TestTurboExtractReportsARefusal(t *testing.T) {
	srv := turboServer(t, func(string) (int, string) {
		return http.StatusOK, `{"success":false,"message":"file deleted"}`
	})

	turbo := NewTurbo(httpx.New("test-agent", "en-US", 0, 5*time.Second))
	u, _ := ParseURL(srv.URL + "/d/gone99")
	_, err := turbo.Extract(context.Background(), u, Options{})
	if err == nil || !strings.Contains(err.Error(), "file deleted") {
		t.Errorf("err = %v, want the endpoint's own message", err)
	}
}
