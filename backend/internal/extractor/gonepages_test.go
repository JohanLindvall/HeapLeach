// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/JohanLindvall/HeapLeach/internal/config"
)

// A pasted album the host deleted answers 410 with the site's own page: it
// is the album extractor's, which says it was deleted, rather than the
// fallback's generic refusal.
func TestEromeDeletedAlbumIsReportedAsDeleted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusGone)
		_, _ = io.WriteString(w, `<html><head><title>EroMe - Video &amp; Photo Sharing</title></head>`+
			`<body><h1>Album deleted</h1><p>The requested page no longer exists.</p></body></html>`)
	}))
	t.Cleanup(srv.Close)
	_, ex, err := NewRegistry(&config.Config{}, testClient()).Extract(context.Background(), srv.URL+"/a/AAAA1111", Options{})
	if err == nil || ex == nil || ex.Name() != "erome" || !strings.Contains(err.Error(), "has been deleted") {
		t.Fatalf("got %v: %v, want erome saying the album was deleted", ex, err)
	}
	// Another site's 410 at the same form is not the album's.
	expectRecognised(t, NewErome(nil), fixturePage(t, "https://albums.example.test/a/AAAA1111", http.StatusGone, "text/html",
		`<html><head><title>Gone</title></head><body></body></html>`), false)
}

// An item the host aged out answers 200 with a notice and no media: it is
// the extractor's to report as expired.
func TestImagePondExpiredItemIsReportedAsExpired(t *testing.T) {
	expired := `<!DOCTYPE html><html><head><title>Item expired - ImagePond</title>` +
		`<meta property="og:site_name" content="ImagePond"></head><body><h1>This item has expired</h1></body></html>`
	share, root := imagePondServer(t, expired)
	link := root + "/i/abc123"
	expectRecognised(t, share, htmlPage(t, link, expired), true)
	_, err := share.Extract(context.Background(), mustParse(t, link), Options{})
	if err == nil || !strings.Contains(err.Error(), "has expired") {
		t.Fatalf("err = %v, want the item said to have expired", err)
	}
	// A notice without the site's name is somebody else's.
	expectRecognised(t, share, htmlPage(t, link, strings.ReplaceAll(expired, "ImagePond", "A Host")), false)
}
