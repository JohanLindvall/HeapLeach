// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"runtime"
	"sync/atomic"
	"testing"

	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

// A probed page answers only the request it was the response to: the same
// visitor (client identity), and no header that changes a representation.
// The same client with another timeout, or another route making the same
// request — whatever agent the route presents — still takes it; a different
// requested Referer, another session, an agent or an Accept asks for its own.
func TestProbeAnswersOnlyItsOwnRequest(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set(httpx.HeaderContentType, "text/html")
		_, _ = io.WriteString(w, "<html><head><title>page</title></head></html>")
	}))
	t.Cleanup(srv.Close)
	client := testClient()
	u, _ := url.Parse(srv.URL + "/a")
	p := probePage(context.Background(), client, u)
	if _, ok := p.Document(); !ok {
		t.Fatalf("probe failed: %+v", p)
	}
	ctx := withPage(context.Background(), p)

	for _, tc := range []struct {
		name    string
		ctx     context.Context
		client  *httpx.Client
		headers httpx.Header
		fetches int32
	}{
		{"same request", ctx, client, nil, 0},
		{"with a referer", ctx, client, httpx.Referer(srv.URL + "/"), 1},
		{"explicitly without a referer", ctx, client, httpx.Referer(""), 0},
		{"same visitor, other timeout", ctx, client.WithTimeout(1 << 30), nil, 0},
		{"another session", ctx, testClient(), nil, 1},
		{"another agent", ctx, client, httpx.Header{httpx.HeaderUserAgent: "other"}, 1},
		{"another representation", ctx, client, httpx.Header{httpx.HeaderAccept: "application/json"}, 1},
		{"another route, same request", httpx.WithRoute(ctx, "elsewhere", client, nil), client, nil, 0},
		{"another route with its own agent", httpx.WithRouteAgent(ctx, "elsewhere", client, nil, "other"), client, nil, 0},
		{"a fresh read", freshRead(ctx), client, nil, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := hits.Load()
			if _, _, err := fetchDocument(tc.ctx, tc.client, u, tc.headers); err != nil {
				t.Fatal(err)
			}
			if _, _, err := fetchRoot(tc.ctx, tc.client, u, tc.headers); err != nil {
				t.Fatal(err)
			}
			d, err := readDocument(tc.ctx, tc.client, u, tc.headers)
			if err != nil || d.tree() == nil {
				t.Fatalf("readDocument: %v", err)
			}
			if got := hits.Load() - before; got != 3*tc.fetches {
				t.Errorf("%d requests, want %d", got, 3*tc.fetches)
			}
		})
	}
}

// Only a probe that holds a complete, successful document of its own stands
// in for a reader's request, and it does for either end of its redirect
// chain. Each acquisition adapter decides the same way: a failed, partial,
// empty or non-200 probe, or one of another URL, is fetched again.
func TestAcquisitionReusesOnlyACompleteProbe(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set(httpx.HeaderContentType, "text/html")
		_, _ = io.WriteString(w, "<html><head><title>fetched</title></head></html>")
	}))
	t.Cleanup(srv.Close)
	client := testClient()
	submitted, _ := url.Parse(srv.URL + "/short")
	final, _ := url.Parse(srv.URL + "/canonical")
	other, _ := url.Parse(srv.URL + "/other")
	const probed = "<html><head><title>probed</title></head></html>"
	page := func(edit func(*Page)) *Page {
		p := &Page{URL: submitted, Final: final, Status: http.StatusOK, Body: probed}
		if edit != nil {
			edit(p)
		}
		return p
	}
	for _, tc := range []struct {
		name   string
		page   *Page
		ask    *url.URL
		reused bool
	}{
		{"the submitted URL", page(nil), submitted, true},
		{"the redirect's end", page(nil), final, true},
		{"another URL", page(nil), other, false},
		{"another status", page(func(p *Page) { p.Status = http.StatusNotFound }), submitted, false},
		{"a body cut short", page(func(p *Page) { p.Err = io.ErrUnexpectedEOF }), submitted, false},
		{"no body", page(func(p *Page) { p.Body = "" }), submitted, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := withPage(context.Background(), tc.page)
			before := hits.Load()
			doc, docFinal, err := fetchDocument(ctx, client, tc.ask, nil)
			if err != nil {
				t.Fatal(err)
			}
			root, rootFinal, err := fetchRoot(ctx, client, tc.ask, nil)
			if err != nil || root == nil {
				t.Fatalf("fetchRoot: %v", err)
			}
			d, err := readDocument(ctx, client, tc.ask, nil)
			if err != nil {
				t.Fatal(err)
			}
			fetches := hits.Load() - before
			if tc.reused {
				if fetches != 0 || doc != probed || d.body != probed {
					t.Errorf("%d requests and body %q, want the probe's without a request", fetches, doc)
				}
				for _, got := range []*url.URL{docFinal, rootFinal, d.final} {
					if got.String() != final.String() || got == tc.page.Final {
						t.Errorf("final URL %v: want a copy of the probe's", got)
					}
				}
				if tc.page.indexed.Load() {
					t.Error("a reader that wanted no index built one on the shared page")
				}
			} else if fetches != 3 || doc == probed || d.body == probed {
				t.Errorf("%d requests and body %q, want each adapter to fetch for itself", fetches, doc)
			}
		})
	}
}

// A JSON question is named by its endpoint and the headers that change its
// answer: one asked with an Accept of its own is not the answer to the bare
// question, and a client of another session neither reads nor fills the
// page's answers.
func TestPageJSONKeysTheRequest(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set(httpx.HeaderContentType, httpx.ContentTypeJSON)
		_, _ = io.WriteString(w, `{"id":7}`)
	}))
	t.Cleanup(srv.Close)
	client := testClient()
	u, _ := url.Parse(srv.URL + "/item/7")
	p := &Page{URL: u, Final: u, Status: http.StatusOK, via: client}
	endpoint := srv.URL + "/api/7"
	accept := httpx.Header{httpx.HeaderAccept: "text/css", httpx.HeaderReferer: srv.URL + "/"}

	var out struct{ ID int }
	if !pageJSON(context.Background(), p, client, endpoint, accept, &out) {
		t.Fatal("the question went unanswered")
	}
	if askedJSON(context.Background(), p, client, endpoint, nil, &out) {
		t.Error("an answer to another representation was reused")
	}
	if !askedJSON(context.Background(), p, client, endpoint, httpx.Header{httpx.HeaderAccept: "text/css"}, &out) {
		t.Error("the same question, referer aside, was not reused")
	}
	if pageJSON(context.Background(), p, testClient(), endpoint, accept, &out) {
		t.Error("another session took the page's answer")
	}
	if n := hits.Load(); n != 1 {
		t.Errorf("%d requests, want 1", n)
	}
}

// A question a page's memo puts to the network on behalf of a caller whose
// context then ended is that caller's cancellation, not the page's answer:
// the page is shared through the cache with every job reading the same
// URL, so another caller — later, or already waiting — asks again with its
// own context and gets the answer. A real failure is still kept.
func TestPageAnswerAskedByACancelledCallerIsNotKept(t *testing.T) {
	var hits atomic.Int32
	hold := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := hits.Add(1)
		if r.URL.Path == "/missing" {
			http.NotFound(w, r)
			return
		}
		if n == 1 {
			// The first asker is held until it gives up.
			select {
			case <-hold:
			case <-r.Context().Done():
				return
			}
		}
		w.Header().Set(httpx.HeaderContentType, httpx.ContentTypeJSON)
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	t.Cleanup(srv.Close)
	defer close(hold)
	client := testClient()
	p := &Page{URL: mustParse(t, srv.URL+"/"), Final: mustParse(t, srv.URL+"/"), via: client}
	endpoint := srv.URL + "/api"

	leader, cancel := context.WithCancel(context.Background())
	leaderDone := make(chan bool)
	go func() {
		var out struct{ OK bool }
		leaderDone <- pageJSON(leader, p, client, endpoint, nil, &out)
	}()
	for hits.Load() == 0 {
		runtime.Gosched()
	}
	waiterDone := make(chan bool)
	go func() {
		var out struct{ OK bool }
		waiterDone <- pageJSON(context.Background(), p, client, endpoint, nil, &out) && out.OK
	}()
	cancel()
	if <-leaderDone {
		t.Error("the cancelled caller was answered")
	}
	if !<-waiterDone {
		t.Error("a caller waiting on the cancelled ask got its missing answer instead of asking again")
	}
	var out struct{ OK bool }
	before := hits.Load()
	if !pageJSON(context.Background(), p, client, endpoint, nil, &out) || !out.OK || hits.Load() != before {
		t.Error("the answer the waiter got was not kept for the next caller")
	}

	// A failure is the page's answer, and is kept: asked once.
	before = hits.Load()
	for range 2 {
		if pageJSON(context.Background(), p, client, srv.URL+"/missing", nil, &out) {
			t.Error("a missing endpoint answered")
		}
	}
	if got := hits.Load() - before; got != 1 {
		t.Errorf("a failed question was asked %d times, want once", got)
	}
}
