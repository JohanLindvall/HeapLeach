// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

// TestDoodProbeFailureStillRecoversThroughAProxy: a player link whose probe
// was refused with a page carrying no challenge marker, or failed with a
// server error, is no page to recognise the player by, but the row opted
// into proxy recovery for exactly an address the host refuses: recovery is
// offered, and the read on the new route is the player's.
func TestDoodProbeFailureStillRecoversThroughAProxy(t *testing.T) {
	for _, status := range []int{http.StatusForbidden, http.StatusBadGateway} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set(httpx.HeaderContentType, "text/html")
				w.WriteHeader(status)
				fmt.Fprint(w, "<html><title>Access denied</title></html>")
			}))
			t.Cleanup(origin.Close)
			client := testClient()
			t.Cleanup(client.CloseIdleConnections)
			proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set(httpx.HeaderContentType, "text/html")
				if strings.HasPrefix(r.URL.Path, "/pass_md5/") {
					fmt.Fprint(w, "http://media.example.test/clip/")
					return
				}
				fmt.Fprint(w, `<title>Test Clip</title><script>fetch('/pass_md5/session/synthetic')</script>`)
			}))
			t.Cleanup(proxy.Close)
			routed, err := client.ThroughProxy(proxy.URL)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(routed.CloseIdleConnections)
			called := false
			ctx := WithChallengeRecovery(t.Context(), func(_ context.Context, read func(context.Context) (*Result, error), _ error) (*Result, error) {
				called = true
				return read(httpx.WithRoute(t.Context(), "route", routed, nil))
			})
			reg := NewRegistry(&config.Config{}, client)
			// The local origin stands in for this player's own host,
			// even when its failed probe carries no identifying markup.
			reg.AliasHosts("doodstream", mustParse(t, origin.URL).Hostname())
			res, ex, err := reg.Extract(ctx, origin.URL+"/e/abcdef123", Options{})
			if !called {
				t.Fatalf("recovery was not offered: %v", err)
			}
			if err != nil || ex.Name() != "doodstream" || len(res.Files) != 1 {
				t.Fatalf("got %v from %v: %v, want the player read on the new route", res, ex, err)
			}
		})
	}
}

// TestDoodRemovedVideoIsReportedAsRemoved: the host's own page for a removed
// video, under its name, is the row's, and says the video was removed,
// rather than naming another site whose links share the /e/ or /d/ form.
func TestDoodRemovedVideoIsReportedAsRemoved(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(httpx.HeaderContentType, "text/html")
		fmt.Fprint(w, `<!doctype html><html><head><title>Video not found | DoodStream</title></head>`+
			`<body><div class="container not_found"><h1>Not Found</h1></div></body></html>`)
	}))
	t.Cleanup(srv.Close)
	for _, path := range []string{"/e/abcdef123456", "/d/abcdef123457"} {
		_, ex, err := probeRegistry().Extract(t.Context(), srv.URL+path, Options{})
		if err == nil || ex == nil || ex.Name() != "doodstream" || !strings.Contains(err.Error(), "removed") {
			t.Errorf("%s: %v from %v, want doodstream saying the video was removed", path, err, ex)
		}
	}
}

// TestPlayerGoneOnlyForItsOwnPage: a 404 on the video's page is the video
// gone, in the row's words; a 404 from the token endpoint the page names is
// that request's failure, not a removed video.
func TestPlayerGoneOnlyForItsOwnPage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/e/gone":
			http.NotFound(w, r)
		case strings.HasPrefix(r.URL.Path, "/pass_md5/"):
			http.NotFound(w, r)
		default:
			fmt.Fprint(w, `<title>Test Clip</title><script>fetch('/pass_md5/session/synthetic')</script>`)
		}
	}))
	t.Cleanup(srv.Close)
	client := testClient()
	t.Cleanup(client.CloseIdleConnections)
	_, err := extract(t, NewDoodStream(client), srv.URL+"/e/gone", Options{})
	if err == nil || !strings.Contains(err.Error(), "is not there") || !httpx.HasStatus(err, http.StatusNotFound) {
		t.Errorf("page 404: %v, want the video reported gone with its status", err)
	}
	_, err = extract(t, NewDoodStream(client), srv.URL+"/e/live", Options{})
	if err == nil || strings.Contains(err.Error(), "is not there") || !strings.Contains(err.Error(), "media token") ||
		!httpx.HasStatus(err, http.StatusNotFound) {
		t.Errorf("token 404: %v, want the token request's failure, not a removed video", err)
	}
}
