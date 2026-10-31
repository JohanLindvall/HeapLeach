// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/JohanLindvall/HeapLeach/internal/httpx"
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

func TestDoodProxyRecoveryIncludesTokensAndRefreshesOnTheNewRoute(t *testing.T) {
	for _, mode := range []string{"cloudflare", "interstitial", "turnstile", "reload", "refused"} {
		t.Run(mode, func(t *testing.T) {
			origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if mode == "reload" {
					if strings.HasPrefix(r.URL.Path, "/pass_md5/") {
						fmt.Fprint(w, "RELOAD")
					} else {
						fmt.Fprint(w, `<script>fetch('/pass_md5/session/expired')</script>`)
					}
					return
				}
				if mode == "refused" {
					w.WriteHeader(http.StatusForbidden)
					return
				}
				if mode == "turnstile" {
					w.Header().Set(httpx.HeaderContentType, "text/html")
					fmt.Fprint(w, doodGateFixture)
					return
				}
				if mode == "interstitial" {
					w.Header().Set(httpx.HeaderContentType, "text/html")
					fmt.Fprint(w, doodInterstitialFixture)
					return
				}
				w.Header().Set(httpx.HeaderCFMitigated, "challenge")
				w.WriteHeader(http.StatusForbidden)
			}))
			defer origin.Close()
			client := testClient()
			defer client.CloseIdleConnections()
			route := func(id string) context.Context {
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if strings.HasPrefix(r.URL.Path, "/pass_md5/") {
						cookie, err := r.Cookie("route")
						if err != nil || cookie.Value != id {
							t.Error("token lost its page's route cookie")
						}
						fmt.Fprintf(w, "http://media.example.test/%s/", id)
						return
					}
					http.SetCookie(w, &http.Cookie{Name: "route", Value: id, Path: "/"})
					fmt.Fprint(w, `<title>Test Clip</title><script>fetch('/pass_md5/session/synthetic')</script>`)
				}))
				t.Cleanup(srv.Close)
				proxyClient, err := client.ThroughProxy(srv.URL)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(proxyClient.CloseIdleConnections)
				return httpx.WithRoute(context.Background(), id, proxyClient, nil)
			}
			first, second := route("first"), route("second")
			reg := &Registry{fallback: NewDoodStream(client)}
			ctx := WithChallengeRecovery(context.Background(), func(_ context.Context, read func(context.Context) (*Result, error), cause error) (*Result, error) {
				if mode == "cloudflare" || mode == "interstitial" || mode == "turnstile" {
					if _, ok := errors.AsType[*httpx.ChallengeError](cause); !ok {
						t.Fatalf("recovery did not receive a challenge: %v", cause)
					}
				}
				return read(first)
			})
			res, _, err := reg.Extract(ctx, origin.URL+"/e/synthetic", Options{})
			if err != nil || len(res.Files) != 1 || res.Files[0].Refresh == nil {
				t.Fatalf("result=%v err=%v", res, err)
			}
			target, err := res.Files[0].Resolve(second)
			if err != nil || !strings.HasPrefix(target.URL, "http://media.example.test/second/") {
				t.Fatalf("fresh token=%+v err=%v", target, err)
			}
			u, _ := url.Parse(target.URL)
			if u.Query().Get("token") != "synthetic" {
				t.Fatal("fresh token was lost")
			}
		})
	}
}

func TestChallengeRefreshIdentifiesFilesAfterListingChanges(t *testing.T) {
	for _, tc := range []struct {
		name  string
		files []File
		want  string
	}{
		{"reordered", []File{{Name: "Second", URL: "second"}, {Name: "First", URL: "fresh"}}, "fresh"},
		{"removed", []File{{Name: "Second", URL: "second"}}, ""},
		{"ambiguous", []File{{Name: "First", URL: "one"}, {Name: "First", URL: "two"}}, ""},
		{"playlist", []File{{Name: "First", Segments: []string{"segment"}}}, "segment"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			refresh := refreshFile(func(context.Context) (*Result, error) { return &Result{Files: tc.files}, nil }, File{Name: "First"}, false)
			target, err := refresh(context.Background())
			if tc.want == "" {
				if err == nil {
					t.Fatal("selected a missing or ambiguous file")
				}
			} else if err != nil || target.URL != tc.want {
				t.Fatalf("target=%+v err=%v", target, err)
			}
		})
	}
}
