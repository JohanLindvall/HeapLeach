// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// A next link that serves a page already read is the listing's end, the
// way a repeated guessed page is, and says nothing more than that.
func TestCheveretoRepeatedNextPageIsTheEnd(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, cheveretoPage("Holiday album",
			cheveretoTile(cheveretoObjectV3, "", "https://cdn.example.test/AAA111.th.png"),
			srv.URL+"/album/holiday.XYZ?page=2", ""))
	}))
	defer srv.Close()

	res := extractOK(t, NewChevereto(testClient()), srv.URL+"/album/holiday.XYZ", Options{})
	if len(res.Files) != 1 || res.Note != "" {
		t.Errorf("%d files, note %q; want the one item and no note", len(res.Files), res.Note)
	}
}

// A listing walk cancelled partway is the cancellation, never the listing's
// cover image read as though the page held a single item.
func TestCheveretoCancelledWalkIsNotASingleItem(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") == "2" {
			cancel()
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = io.WriteString(w, cheveretoPage("Holiday album",
			cheveretoTile(cheveretoObjectV3, "", "https://cdn.example.test/AAA111.th.png"),
			srv.URL+"/album/holiday.XYZ?page=2", ""))
	}))
	defer srv.Close()

	u := mustParse(t, srv.URL+"/album/holiday.XYZ")
	res, err := NewChevereto(testClient()).Extract(ctx, u, Options{})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("got %+v, %v; want the cancellation", res, err)
	}
}
