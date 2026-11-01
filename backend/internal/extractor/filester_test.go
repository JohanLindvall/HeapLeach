// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
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

	f := NewFilester(testClient())
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

func TestFilesterRecognise(t *testing.T) {
	f := NewFilester(nil)
	file := `<html><head><title>first.png | Files</title></head><body><button id="downloadButton">Download</button>` +
		`<script src="/js/file_dl.js?ver=1"></script></body></html>`
	folder := `<html><head><title>Example | Files</title></head><body>` +
		`<div class="file-card" onclick="window.location.href='/d/AbC123x'">first.png</div></body></html>`
	locked := `<html><body><form id="password-form" method="post"><input name="nonce" value="n"></form></body></html>`
	expectRecognised(t, f, htmlPage(t, "https://share.example.test/d/AbC123x", file), true)
	expectRecognised(t, f, htmlPage(t, "https://share.example.test/f/folder", folder), true)
	expectRecognised(t, f, htmlPage(t, "https://share.example.test/f/folder", locked), true)
	// The form and a title, without the download script or the folder's cards.
	plain := `<html><head><title>first.png | Files</title></head><body><button id="downloadButton">Download</button></body></html>`
	expectRecognised(t, f, htmlPage(t, "https://share.example.test/d/AbC123x", plain), false)
	expectRecognised(t, f, htmlPage(t, "https://share.example.test/f/folder", plain), false)
	// The markers on a page of another form.
	expectRecognised(t, f, htmlPage(t, "https://share.example.test/api-docs", file), false)
	expectShape(t, f, "https://share.example.test/d/AbC123x", true)
	expectShape(t, f, "https://share.example.test/f/folder?page=2", true)
	expectShape(t, f, "https://share.example.test/f/", false)
	expectShape(t, f, "https://share.example.test/", false)
}
