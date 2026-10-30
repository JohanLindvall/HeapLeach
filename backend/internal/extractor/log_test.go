// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// An extraction's job logger is httpx's too, so the job's log shows what
// each request it made got back.
func TestExtractionLoggerCarriesResponses(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "#EXTM3U\n#EXTINF:4,\nsegment.ts\n")
	}))
	defer srv.Close()

	var out strings.Builder
	log := slog.New(slog.NewTextHandler(&out, &slog.HandlerOptions{Level: slog.LevelDebug})).With("job", "j1")
	ctx := WithLogger(context.Background(), log)
	if extractionLogger(ctx) != log {
		t.Fatal("the extraction logger is not the one attached")
	}
	if _, _, err := fetchPlaylist(ctx, testClient(), srv.URL+"/media.m3u8", nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "http: response") || !strings.Contains(out.String(), "job=j1") {
		t.Errorf("the job's log has no response line:\n%s", out.String())
	}
}
