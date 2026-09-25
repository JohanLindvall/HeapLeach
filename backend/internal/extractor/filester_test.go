package extractor

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

func TestFilesterSignsAtDownloadTime(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != filesterTokenPath {
			http.NotFound(w, r)
			return
		}
		var in struct {
			Slug string `json:"file_slug"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Slug != "AbC123x" {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"success":false,"error":"E2001","message":"File not found"}`))
			return
		}
		calls++
		_ = json.NewEncoder(w).Encode(map[string]any{
			"success": true, "expires_in": 1800,
			"server": "https://cdn.example.test", "file": "0000-1111.mp4",
			"name": "First Clip.mp4", "token": "t+" + string(rune('0'+calls)),
		})
	}))
	defer srv.Close()

	f := NewFilester(httpx.New("test-agent", "en-US", 0, 5*time.Second))
	u, _ := url.Parse(srv.URL + "/d/AbC123x")
	res, err := f.Extract(context.Background(), u, Options{})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if len(res.Files) != 1 || res.Files[0].Name != "First Clip.mp4" {
		t.Fatalf("files = %+v, want one named by the API", res.Files)
	}
	if res.Files[0].Resolve == nil {
		t.Fatal("no resolver: a half-hour token would expire in the queue")
	}

	target, err := res.Files[0].Resolve(context.Background())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if want := "https://cdn.example.test/v2/0000-1111.mp4?token=t%2B2"; target.URL != want {
		t.Errorf("url = %q, want %q (a fresh token, escaped)", target.URL, want)
	}

	u, _ = url.Parse(srv.URL + "/d/missing")
	if _, err := f.Extract(context.Background(), u, Options{}); err == nil {
		t.Error("a missing file extracted without error")
	}
}

func TestFilesterMatch(t *testing.T) {
	f := NewFilester(nil)
	for _, raw := range []string{"https://filester.si/d/AbC123x", "https://filester.me/d/AbC123x", "https://filester.gg/d/AbC123x"} {
		u, _ := url.Parse(raw)
		if !f.Match(u) {
			t.Errorf("%s not matched", raw)
		}
	}
}
