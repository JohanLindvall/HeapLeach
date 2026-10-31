// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"

	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

// A probed page answers only the request it was the response to: the same
// visitor (client identity), the same route, and no header that changes a
// representation. A referer, or the same client with another timeout, still
// takes it; another session, route, agent or Accept asks for its own.
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
		{"with a referer", ctx, client, httpx.Referer(srv.URL + "/"), 0},
		{"same visitor, other timeout", ctx, client.WithTimeout(1 << 30), nil, 0},
		{"another session", ctx, testClient(), nil, 1},
		{"another agent", ctx, client, httpx.Header{httpx.HeaderUserAgent: "other"}, 1},
		{"another representation", ctx, client, httpx.Header{httpx.HeaderAccept: "application/json"}, 1},
		{"another route", httpx.WithRoute(ctx, "elsewhere", client, nil), client, nil, 1},
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
			if got := hits.Load() - before; got != 2*tc.fetches {
				t.Errorf("%d requests, want %d", got, 2*tc.fetches)
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
