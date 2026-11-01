// SPDX-License-Identifier: MIT

package download

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/extractor"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
	"github.com/JohanLindvall/HeapLeach/internal/proxy"
)

func TestSourceRecoveryRedrawsBrokenProxiesBeyondTheRetryBudget(t *testing.T) {
	m := busyManager(t)
	var requests atomic.Int32
	var endpoints []string
	for _, address := range []string{"127.0.0.2", "127.0.0.3", "127.0.0.4", "127.0.0.5"} {
		srv := proxyServer(t, address, func(w http.ResponseWriter, r *http.Request) {
			if requests.Add(1) <= 3 {
				conn, _, err := w.(http.Hijacker).Hijack()
				if err != nil {
					t.Error(err)
					return
				}
				conn.Close()
				return
			}
			if strings.HasPrefix(r.URL.Path, "/pass_md5/") {
				fmt.Fprint(w, "http://media.example.test/file/")
			} else {
				fmt.Fprint(w, `<title>Test Clip</title><script>fetch('/pass_md5/session/synthetic')</script>`)
			}
		})
		endpoints = append(endpoints, srv.URL)
	}
	pool := attachPool(t, m, endpoints)
	m.cfg.ProxyRetries = 1
	client := httpx.New("test", "en", 0, time.Second)
	defer client.CloseIdleConnections()
	dood := extractor.NewDoodStream(client)
	u, _ := url.Parse("http://player.example.test/e/synthetic")
	var notes []string
	ctx := extractor.WithResolveNote(context.Background(), func(note string) { notes = append(notes, note) })
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	res, err := m.recoverExtraction(ctx, func(ctx context.Context) (*extractor.Result, error) {
		return dood.Extract(ctx, u, extractor.Options{})
	}, &extractor.TransientError{Err: errors.New("doodstream: player requested RELOAD")})
	if err != nil || res == nil || len(res.Files) != 1 {
		t.Fatalf("source failed before reaching a working proxy: result=%+v err=%v", res, err)
	}
	if requests.Load() != 5 || pool.Page(config.GroupWAF, proxy.Query{}).Summary.Cooling != 3 ||
		pool.Page(config.GroupWAF, proxy.Query{}).Summary.Active != 0 {
		t.Fatalf("requests=%d inventory=%+v", requests.Load(), pool.Page(config.GroupWAF, proxy.Query{}).Summary)
	}
	if notes[len(notes)-1] != "Doodstream: requesting media token (1/3) — connection attempt 5" {
		t.Fatalf("proxy redraws were not visible: %v", notes)
	}
}

func TestDoodProxyTimeoutRecovery(t *testing.T) {
	for _, tc := range []struct {
		name       string
		headers    bool
		failures   int32
		wantFailed bool
	}{
		{name: "connection", failures: 3},
		{name: "headers", headers: true, failures: 1},
		{name: "header retry limit", headers: true, failures: 3, wantFailed: true},
	} {
		for _, stage := range []string{"source", "file"} {
			t.Run(tc.name+"/"+stage, func(t *testing.T) {
				m := busyManager(t)
				finish := make(chan struct{})
				defer close(finish)
				var players, connects atomic.Int32
				var endpoints []string
				const origin = "http://player.example.test"
				const payload = "synthetic video bytes"
				for _, address := range []string{"127.0.0.2", "127.0.0.3", "127.0.0.4", "127.0.0.5"} {
					srv := proxyServer(t, address, func(w http.ResponseWriter, r *http.Request) {
						switch {
						case r.Method == http.MethodConnect:
							connects.Add(1)
							select {
							case <-finish:
							case <-r.Context().Done():
							}
							w.WriteHeader(http.StatusBadGateway)
						case r.URL.Path == "/e/synthetic":
							if players.Add(1) <= tc.failures {
								if tc.headers {
									select {
									case <-finish:
									case <-r.Context().Done():
									}
									return
								}
								// A successful first hop must not hide a timeout
								// connecting through this proxy after a redirect.
								http.Redirect(w, r, "https://player.example.test/e/synthetic", http.StatusFound)
								return
							}
							fmt.Fprint(w, `<title>Test Clip</title><script>fetch('/pass_md5/session/synthetic')</script>`)
						case strings.HasPrefix(r.URL.Path, "/pass_md5/"):
							fmt.Fprint(w, origin+"/media/")
						default:
							w.Header().Set(httpx.HeaderContentType, "video/mp4")
							http.ServeContent(w, r, "clip.mp4", time.Time{}, strings.NewReader(payload))
						}
					})
					endpoints = append(endpoints, srv.URL)
				}
				pool := attachPool(t, m, endpoints)
				m.cfg.ProxyRetries = 1
				client := httpx.New("test", "en", 3, 100*time.Millisecond)
				defer client.CloseIdleConnections()
				dood := extractor.NewDoodStream(client)
				u, _ := url.Parse(origin + "/e/synthetic")
				read := func(ctx context.Context) (*extractor.Result, error) {
					return dood.Extract(ctx, u, extractor.Options{})
				}
				if stage == "source" {
					ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					defer cancel()
					res, err := m.recoverExtraction(ctx, read, context.DeadlineExceeded)
					if tc.wantFailed {
						if !errors.Is(err, context.DeadlineExceeded) {
							t.Fatalf("source timeout=%v, want exhausted retries", err)
						}
					} else if err != nil || res == nil || len(res.Files) != 1 {
						t.Fatalf("source recovery: result=%+v err=%v", res, err)
					}
				} else {
					job := addProxyFiles(m, 1, func(ctx context.Context) (*extractor.Target, error) {
						res, err := read(ctx)
						if err != nil {
							return nil, err
						}
						f := res.Files[0]
						return &extractor.Target{URL: f.URL, Headers: f.Headers, Size: f.Size}, nil
					})
					job.Items[0].pace = challengePace(nil)
					m.Start()
					m.signal()
					waitFor(t, 5*time.Second, func() bool { return m.Snapshot().Jobs[0].Items[0].Status.Terminal() })
					if tc.wantFailed {
						if it := m.Snapshot().Jobs[0].Items[0]; it.Status != StatusFailed || it.Error == "" {
							t.Fatalf("file timeout=%+v, want exhausted retries", it)
						}
					} else {
						body, err := os.ReadFile(filepath.Join(m.DownloadDir(), "file-0.bin"))
						if err != nil || string(body) != payload {
							t.Fatalf("file recovery: body=%q err=%v job=%+v", body, err, m.Snapshot().Jobs[0])
						}
					}
				}
				page := pool.Page(config.GroupWAF, proxy.Query{})
				wantPlayers, wantCooling, wantConnects := tc.failures+1, int(tc.failures), int32(0)
				if !tc.headers {
					wantConnects = tc.failures
				}
				if tc.wantFailed {
					wantPlayers, wantCooling = 2, 2
				}
				if players.Load() != wantPlayers || connects.Load() != wantConnects || page.Summary.Cooling != wantCooling || page.Summary.Active != 0 {
					t.Fatalf("players=%d connects=%d inventory=%+v", players.Load(), connects.Load(), page.Summary)
				}
				for _, row := range page.Rows {
					if row.Status == "cooling" && (row.Requests != 1 || row.SuccessRate != 0 || row.WAFRequests != 0) {
						t.Fatalf("silent proxy did not receive exactly one transport penalty: %+v", row)
					}
				}
				if pool.Page(config.GroupKeep2Share, proxy.Query{}).Summary.Cooling != wantCooling {
					t.Fatal("silent proxies remained available for other services")
				}
			})
		}
	}
}

func TestDoodDirectFailureRetriesWithProxyProgressAndFreshSession(t *testing.T) {
	for _, stage := range []string{"reload", "media", "stall", "timeout"} {
		t.Run(stage, func(t *testing.T) {
			m := busyManager(t)
			m.cfg.StallTimeout = 90 * time.Millisecond
			var failed atomic.Bool
			var origin string
			atToken, finish := make(chan struct{}), make(chan struct{})
			var arrived, release sync.Once
			defer release.Do(func() { close(finish) })
			const payload = "synthetic video bytes"
			handler := func(route string) http.HandlerFunc {
				return func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/e/synthetic" {
						if route == "direct" && failed.Load() && stage == "timeout" {
							<-r.Context().Done()
							return
						}
						http.SetCookie(w, &http.Cookie{Name: "route", Value: route, Path: "/"})
						fmt.Fprintf(w, `<title>Test Clip</title><script>fetch('/pass_md5/session/%s')</script>`, route)
						return
					}
					cookie, err := r.Cookie("route")
					if err != nil || cookie.Value != route || r.Referer() != origin+"/e/synthetic" {
						t.Errorf("request crossed sessions: route=%s cookie=%v err=%v referer=%s", route, cookie, err, r.Referer())
					}
					if strings.HasPrefix(r.URL.Path, "/pass_md5/") {
						if route == "direct" && failed.Load() && stage == "reload" {
							fmt.Fprint(w, "RELOAD")
							return
						}
						if route == "proxy" {
							arrived.Do(func() { close(atToken) })
							select {
							case <-finish:
							case <-r.Context().Done():
								return
							}
						}
						fmt.Fprintf(w, "%s/media/%s/", origin, route)
						return
					}
					if route == "direct" && failed.Load() {
						if stage == "stall" {
							w.Header().Set(httpx.HeaderContentType, "video/mp4")
							w.Header().Set("Content-Length", fmt.Sprint(len(payload)))
							w.WriteHeader(http.StatusOK)
							w.(http.Flusher).Flush()
							<-r.Context().Done()
							return
						}
						w.WriteHeader(http.StatusForbidden)
						return
					}
					if !strings.HasPrefix(r.URL.Path, "/media/"+route+"/") || r.URL.Query().Get("token") != route {
						t.Errorf("media did not use its new proxy token: %s", r.URL.Path)
					}
					w.Header().Set(httpx.HeaderContentType, "video/mp4")
					http.ServeContent(w, r, "clip.mp4", time.Time{}, strings.NewReader(payload))
				}
			}
			direct := testServer(t, handler("direct"))
			origin = direct.URL
			route := proxyServer(t, "127.0.0.2", handler("proxy"))
			pool := attachPool(t, m, []string{proxy.Direct, route.URL})
			client := m.client.WithTimeout(5 * time.Second)
			if stage == "timeout" {
				client = client.WithTimeout(100 * time.Millisecond)
			}
			defer client.CloseIdleConnections()
			u, _ := url.Parse(origin + "/e/synthetic")
			res, err := extractor.NewDoodStream(client).Extract(context.Background(), u, extractor.Options{})
			if err != nil {
				t.Fatal(err)
			}
			if pool.Page(config.GroupWAF, proxy.Query{}).Summary.Active != 0 {
				t.Fatal("healthy direct extraction used a proxy")
			}
			failed.Store(true)
			m.mu.Lock()
			job := &Job{ID: newID(), Source: u.String()}
			m.queueJobLocked(job)
			m.applyResultLocked(job, "doodstream", res, nil)
			m.mu.Unlock()
			m.Start()
			m.signal()
			select {
			case <-atToken:
			case <-time.After(5 * time.Second):
				t.Fatal("direct Dood failure did not start a proxy attempt")
			}
			it := m.Snapshot().Jobs[0].Items[0]
			if it.Note != "Doodstream: requesting media token (1/3) — connection attempt 2" {
				t.Fatalf("proxy retry note=%q", it.Note)
			}
			release.Do(func() { close(finish) })
			waitFor(t, 5*time.Second, func() bool { return m.Snapshot().Jobs[0].Items[0].Status.Terminal() })
			body, err := os.ReadFile(filepath.Join(m.DownloadDir(), "Test Clip.mp4"))
			if err != nil || string(body) != payload {
				t.Fatalf("retried media=%q err=%v status=%+v", body, err, m.Snapshot().Jobs[0])
			}
		})
	}
}

// Discover a file through a folder, then refresh its strict nested source on
// another address. A proxy serving a bare refusal must be cooled and replaced,
// without turning the recognised file into unsupported navigation.
func TestDoodFolderRefreshSkipsRefusedProxy(t *testing.T) {
	m := busyManager(t)
	var origin string
	var refusedRoute, refusedPages atomic.Int32
	const payload = "synthetic video bytes"
	handler := func(route int32) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set(httpx.HeaderContentType, "text/html")
			if route != 0 && (r.URL.Path == "/d/synthetic" || r.URL.Path == "/e/synthetic") {
				refusedRoute.CompareAndSwap(0, route)
				if refusedRoute.Load() == route {
					refusedPages.Add(1)
					fmt.Fprint(w, "The resource you are looking for has been removed or is temporarily unavailable.")
					return
				}
			}
			switch {
			case r.URL.Path == "/folder":
				fmt.Fprint(w, `<title>Invented Folder</title><a href="/d/synthetic">Invented Clip</a>`)
			case r.URL.Path == "/d/synthetic":
				fmt.Fprint(w, `<meta name="og:sitename" content="DoodStream.com"><iframe src="/e/synthetic"></iframe>`)
			case r.URL.Path == "/e/synthetic":
				http.SetCookie(w, &http.Cookie{Name: "route", Value: fmt.Sprint(route), Path: "/"})
				fmt.Fprintf(w, `<title>Invented Clip</title><script>fetch('/pass_md5/session/route%d')</script>`, route)
			case strings.HasPrefix(r.URL.Path, "/pass_md5/") || strings.HasPrefix(r.URL.Path, "/media/"):
				cookie, err := r.Cookie("route")
				if err != nil || cookie.Value != fmt.Sprint(route) || r.Referer() != origin+"/e/synthetic" {
					t.Errorf("request crossed sessions: route=%d cookie=%v error=%v referer=%s", route, cookie, err, r.Referer())
				}
				if strings.HasPrefix(r.URL.Path, "/pass_md5/") {
					fmt.Fprintf(w, "%s/media/route%d/", origin, route)
					return
				}
				if route == 0 {
					w.WriteHeader(http.StatusForbidden)
					return
				}
				if !strings.HasPrefix(r.URL.Path, fmt.Sprintf("/media/route%d/", route)) {
					t.Error("media reused a different route's signed URL")
				}
				w.Header().Set(httpx.HeaderContentType, "video/mp4")
				http.ServeContent(w, r, "clip.mp4", time.Time{}, strings.NewReader(payload))
			default:
				http.NotFound(w, r)
			}
		}
	}
	origin = testServer(t, handler(0)).URL
	var endpoints []string
	for i, address := range []string{"127.0.0.2", "127.0.0.3"} {
		endpoints = append(endpoints, proxyServer(t, address, handler(int32(i+1))).URL)
	}
	pool := attachPool(t, m, endpoints)
	// The local server plays DoodStream, whose folder links its own items.
	m.reg.AliasHosts("doodstream", "127.0.0.1")
	m.cfg.ProxyRetries = 0 // An address refusal must not spend this budget.
	m.Start()
	id, err := m.Add(origin+"/folder", "")
	if err != nil {
		t.Fatal(err)
	}
	waitLocked(t, m, 5*time.Second, func() bool {
		job := m.jobs[id]
		return job.Err != "" || (len(job.Items) == 1 && job.Items[0].Status.Terminal())
	})
	m.mu.Lock()
	job := m.jobs[id]
	if job.Err != "" || len(job.Items) != 1 || job.Items[0].Status != StatusDone {
		m.mu.Unlock()
		t.Fatalf("folder file did not recover: %+v", m.Snapshot().Jobs[0])
	}
	path, retries := job.Items[0].Path, job.Items[0].proxyRetries
	m.mu.Unlock()
	body, err := os.ReadFile(filepath.Join(m.DownloadDir(), path))
	if err != nil || string(body) != payload || retries != 0 {
		t.Fatalf("download=%q error=%v proxy retries=%d", body, err, retries)
	}
	if refusedPages.Load() != 2 || pool.Page(config.GroupWAF, proxy.Query{}).Summary.Cooling != 1 {
		t.Fatalf("refused pages=%d inventory=%+v", refusedPages.Load(), pool.Page(config.GroupWAF, proxy.Query{}).Summary)
	}
}

func TestSourceProgressIncludesRetryAttemptAndClearsAfterCancellation(t *testing.T) {
	m := busyManager(t)
	origin := testServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set(httpx.HeaderCFMitigated, "challenge")
		w.WriteHeader(http.StatusForbidden)
	}))
	started := make(chan struct{})
	route := proxyServer(t, "127.0.0.2", func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
	})
	pool := attachPool(t, m, []string{route.URL})
	id, err := m.Add(origin.URL+"/watch", "")
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("source did not retry through its proxy")
	}
	job := m.Snapshot().Jobs[0]
	if job.Status != StatusResolving || !strings.Contains(job.Note, "connection attempt 2") {
		t.Fatalf("source progress=%+v", job)
	}
	if err := m.CancelJob(id); err != nil {
		t.Fatal(err)
	}
	waitFor(t, time.Second, func() bool { return pool.Page(config.GroupWAF, proxy.Query{}).Summary.Active == 0 })
	if job := m.Snapshot().Jobs[0]; job.Note != "" || job.Status != StatusCanceled {
		t.Fatalf("canceled source retained retry progress: %+v", job)
	}
}

func TestSourceRecoveryKeepsTemporaryFailuresBoundedAndPermanentErrorsFinal(t *testing.T) {
	for _, tc := range []struct {
		name     string
		err      error
		attempts int
	}{
		{"reload", &extractor.TransientError{Err: errors.New("RELOAD")}, 3},
		{"timeout", context.DeadlineExceeded, 3},
		{"removed", &httpx.StatusError{Code: http.StatusNotFound}, 0},
		{"missing token", errors.New("doodstream: no media token"), 0},
		{"canceled", context.Canceled, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := busyManager(t)
			pool := attachPool(t, m, []string{"http://route.example.test:8000"})
			attempts := 0
			_, err := m.recoverExtraction(context.Background(), func(context.Context) (*extractor.Result, error) {
				attempts++
				return nil, tc.err
			}, tc.err)
			if !errors.Is(err, tc.err) || attempts != tc.attempts || pool.Page(config.GroupWAF, proxy.Query{}).Summary.Active != 0 {
				t.Fatalf("attempts=%d want=%d err=%v", attempts, tc.attempts, err)
			}
		})
	}
}

func TestDoodFallbackDisabledKeepsOrdinaryTransferRetries(t *testing.T) {
	m := busyManager(t)
	m.timings.progressRetry = time.Millisecond
	var calls atomic.Int32
	srv := testServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set(httpx.HeaderContentType, "video/mp4")
		w.Header().Set("Content-Length", "4")
		if calls.Add(1) == 1 {
			fmt.Fprint(w, "fi") // incomplete body: the ordinary retry is still needed
			return
		}
		fmt.Fprint(w, "file")
	}))
	it := &Item{ID: newID(), Name: "clip.mp4", URL: srv.URL, Size: 4, proxyFallback: true}
	if err := m.transferRouted(context.Background(), it); err != nil || calls.Load() < 2 {
		t.Fatalf("disabled fallback changed direct retries: calls=%d err=%v", calls.Load(), err)
	}
}
