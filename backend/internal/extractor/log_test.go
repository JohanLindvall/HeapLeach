// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// An extraction's job logger is httpx's too, so the job's log shows a
// response that went wrong — and only those, a listing reading hundreds of
// pages that answered.
func TestExtractionLoggerCarriesResponses(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "gone", http.StatusGone)
	}))
	defer srv.Close()

	var out strings.Builder
	log := slog.New(slog.NewTextHandler(&out, &slog.HandlerOptions{Level: slog.LevelDebug})).With("job", "j1")
	ctx := WithLogger(context.Background(), log)
	if extractionLogger(ctx) != log {
		t.Fatal("the extraction logger is not the one attached")
	}
	if _, _, err := fetchPlaylist(ctx, testClient(), srv.URL+"/media.m3u8", nil); err == nil {
		t.Fatal("a refused playlist was read")
	}
	if !strings.Contains(out.String(), "http: response") || !strings.Contains(out.String(), "job=j1") {
		t.Errorf("the job's log has no response line:\n%s", out.String())
	}
}
