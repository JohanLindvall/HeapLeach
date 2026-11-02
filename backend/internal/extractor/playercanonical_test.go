// SPDX-License-Identifier: MIT

package extractor

import (
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

// A canonical page may show a preview to navigation while serving the full
// video to its own reader. Ask for that representation on the first read:
// a second use of the same page may already be spent.
func TestCanonicalPlayerUsesReaderReferer(t *testing.T) {
	for _, direct := range []bool{false, true} {
		for _, vary := range []bool{false, true} {
			t.Run(fmt.Sprintf("direct=%t/vary=%t", direct, vary), func(t *testing.T) {
				// This host only selects the production row; the local proxy
				// handles every request, including redirects.
				const origin = "http://vids.st"
				id := strconv.Itoa(42)
				source, canonical := origin+"/e/"+id, origin+"/v/"+id
				if direct {
					source = canonical
				}
				var reads, invited atomic.Int32
				proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set(httpx.HeaderContentType, "text/html")
					if vary {
						w.Header().Set(httpx.HeaderVary, httpx.HeaderReferer)
					}
					switch r.URL.Path {
					case "/e/" + id:
						if r.Referer() != "" {
							t.Errorf("noncanonical page Referer = %q, want ordinary navigation", r.Referer())
						}
						fmt.Fprint(w, `<html><title>Open the watch page</title></html>`)
					case "/v/" + id:
						if reads.Add(1) != 1 {
							http.Error(w, "page already consumed", http.StatusGone)
							return
						}
						http.SetCookie(w, &http.Cookie{Name: "visit", Value: "opened", Path: "/v/"})
						name := "preview.mp4"
						if r.Referer() == canonical {
							invited.Add(1)
							name = "full-recording.mp4"
						}
						fmt.Fprintf(w, `<html><script>var playerConfig = {"videoUrl":%q,"videoName":"Example Recording.mp4"};</script></html>`, "https://media.example.test/"+name)
					default:
						http.NotFound(w, r)
					}
				}))
				t.Cleanup(proxy.Close)
				base := testClient()
				t.Cleanup(base.CloseIdleConnections)
				client, err := base.ThroughProxy(proxy.URL)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(client.CloseIdleConnections)
				jar, _ := cookiejar.New(nil)
				ctx := httpx.WithCookieJar(t.Context(), jar)
				res, ex, err := NewRegistry(&config.Config{}, client).Extract(ctx, source, Options{})
				if err != nil || res == nil || len(res.Files) != 1 || res.Files[0].URL != "https://media.example.test/full-recording.mp4" {
					t.Fatalf("extractor=%v result=%+v err=%v; want the full recording", ex, res, err)
				}
				if reads.Load() != 1 || invited.Load() != 1 {
					t.Errorf("canonical reads=%d, matching Referer reads=%d; want one of each", reads.Load(), invited.Load())
				}
			})
		}
	}
}

// Redirect-generated Referers and response cookies belong to the canonical
// request chain. Its original reader can reuse that answer; another Referer
// or a subsequent cookie change requires that reader's own response.
func TestCanonicalPlayerTracksRedirectIdentity(t *testing.T) {
	var reads atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v/42" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set(httpx.HeaderVary, httpx.HeaderReferer)
		if r.URL.Query().Get("player") == "" {
			http.SetCookie(w, &http.Cookie{Name: "visit", Value: "opened", Path: "/v/"})
			http.Redirect(w, r, "/v/42?player=1", http.StatusFound)
			return
		}
		reads.Add(1)
		w.Header().Set(httpx.HeaderContentType, "text/html")
		http.SetCookie(w, &http.Cookie{Name: "visit", Value: "ready", Path: "/v/"})
		name := "preview.mp4"
		if r.Referer() == "http://"+r.Host+"/v/42" {
			name = "full-recording.mp4"
		}
		if cookie, err := r.Cookie("visit"); err == nil && cookie.Value == "changed" {
			name = "changed-session.mp4"
		}
		fmt.Fprintf(w, `<html><script>var playerConfig = {"videoUrl":%q,"videoName":"Example Recording.mp4"};</script></html>`, "https://media.example.test/"+name)
	}))
	t.Cleanup(srv.Close)
	client := testClient()
	t.Cleanup(client.CloseIdleConnections)
	jar, _ := cookiejar.New(nil)
	ctx := httpx.WithCookieJar(t.Context(), jar)
	row := vidsPlayer
	row.asks = nil
	player := NewPlayer(client, &row)
	u := mustParse(t, srv.URL+"/e/42")
	canonical := player.canonicalPage(ctx, &Page{URL: u, Final: u})
	if canonical == nil {
		t.Fatal("canonical page was not recognized")
	}
	ctx = withPage(ctx, canonical)
	for _, tc := range []struct {
		name, referer, want string
		changeCookie        bool
		reads               int32
	}{
		{"matching final request", canonical.URL.String(), "full-recording.mp4", false, 1},
		{"different final Referer", "", "preview.mp4", false, 2},
		{"changed final session", canonical.URL.String(), "changed-session.mp4", true, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.changeCookie {
				jar.SetCookies(canonical.Final, []*http.Cookie{{Name: "visit", Value: "changed", Path: "/v/"}})
			}
			body, _, err := fetchDocument(ctx, client, canonical.Final, httpx.Referer(tc.referer))
			if err != nil || !strings.Contains(body, "https://media.example.test/"+tc.want) {
				t.Fatalf("body=%q err=%v; want %s", body, err, tc.want)
			}
			if reads.Load() != tc.reads {
				t.Errorf("canonical destination reads=%d, want %d", reads.Load(), tc.reads)
			}
		})
	}
}

// A custom reader may require a different representation from the canonical
// recognizer. A real canonical response must retain its identity, unlike an
// identity-free Page built only as a parser fixture.
func TestCanonicalPlayerPreservesRequestIdentity(t *testing.T) {
	for _, matching := range []bool{true, false} {
		t.Run(fmt.Sprintf("matching=%t", matching), func(t *testing.T) {
			var reads atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v/42" {
					http.NotFound(w, r)
					return
				}
				w.Header().Set(httpx.HeaderContentType, "text/html")
				w.Header().Set(httpx.HeaderVary, httpx.HeaderReferer)
				if reads.Add(1) > 1 && matching {
					http.Error(w, "page already consumed", http.StatusGone)
					return
				}
				name := "preview.mp4"
				if r.Referer() != "" {
					name = "full-recording.mp4"
				}
				fmt.Fprintf(w, `<html><script>var playerConfig = {"videoUrl":%q,"videoName":"Example Recording.mp4"};</script></html>`, "https://media.example.test/"+name)
			}))
			t.Cleanup(srv.Close)
			client := testClient()
			t.Cleanup(client.CloseIdleConnections)
			row := vidsPlayer
			row.asks = nil
			player := NewPlayer(client, &row)
			u := mustParse(t, srv.URL+"/e/42")
			canonical := player.canonicalPage(t.Context(), &Page{URL: u, Final: u})
			if canonical == nil {
				t.Fatal("canonical page was not recognized")
			}
			ask := ""
			want, count := "preview.mp4", int32(1)
			if !matching {
				ask = canonical.URL.String()
				want, count = "full-recording.mp4", 2
			}
			body, _, err := fetchDocument(withPage(t.Context(), canonical), client, canonical.URL, httpx.Referer(ask))
			if err != nil || !strings.Contains(body, "https://media.example.test/"+want) {
				t.Fatalf("body=%q err=%v; want %s", body, err, want)
			}
			if reads.Load() != count {
				t.Errorf("canonical reads=%d, want %d", reads.Load(), count)
			}
		})
	}
}
