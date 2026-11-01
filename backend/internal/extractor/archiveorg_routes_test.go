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

// archiveRoutes serves an embed player page for an item, the archive's own
// page for a collection and for an identifier nobody registered, and an
// ordinary page at the embed form that is not the archive's.
func archiveRoutes(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/metadata/a-film":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"metadata":{"identifier":"a-film","mediatype":"movies"},`+
				`"files":[{"name":"A Film.mp4","source":"original","format":"MPEG4","size":"9"}]}`)
		case "/metadata/a-collection":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"is_collection":true,"metadata":{"identifier":"a-collection","mediatype":"collection"},`+
				`"files":[{"name":"header.jpg","source":"original","size":"1"}]}`)
		case "/metadata/no-such-item", "/metadata/a-clip":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{}`)
		case "/embed/a-film", "/stream/a-film":
			w.Header().Set("Content-Type", "text/html")
			_, _ = io.WriteString(w, `<html><head><title>A Film</title></head><body>`+
				`<video><source src="/download/a-film/A%20Film.mp4"></video></body></html>`)
		case "/details/a-collection":
			// A collection is served as the archive's app shell.
			w.Header().Set("Content-Type", "text/html")
			_, _ = io.WriteString(w, `<html><head><title>Internet Archive</title>`+
				`<script type="module" src="/offshoot_assets/index-abc.js"></script></head><body><app-root></app-root></body></html>`)
		case "/details/no-such-item":
			w.Header().Set("Content-Type", "text/html")
			_, _ = io.WriteString(w, `<html><head><title>Internet Archive</title></head><body><ia-topnav></ia-topnav></body></html>`)
		case "/embed/a-clip":
			w.Header().Set("Content-Type", "text/html")
			_, _ = io.WriteString(w, `<html><head><title>A clip</title></head><body><p>Somebody's player.</p></body></html>`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// An item's embed and stream pages are bare player documents, but they name
// the item's files: the item is resolved, renditions and all.
func TestArchiveEmbedRouteResolvesTheItem(t *testing.T) {
	srv := archiveRoutes(t)
	reg := NewRegistry(&config.Config{}, testClient())
	for _, route := range []string{"/embed/a-film", "/stream/a-film"} {
		res, ex, err := reg.Extract(context.Background(), srv.URL+route, Options{})
		if err != nil || ex.Name() != "archive.org" || len(res.Files) != 1 || res.Files[0].Name != "A Film.mp4" {
			t.Errorf("%s: got %v from %v: %v, want the item's file", route, res, ex, err)
		}
	}
	// An ordinary page at the form that names no item of its own.
	a := NewArchiveOrg(nil, testClient())
	if a.Recognise(context.Background(), htmlPage(t, srv.URL+"/embed/a-clip", `<html><title>A clip</title></html>`)) {
		t.Error("an ordinary page at the embed form was recognised")
	}
}

// The archive's own page for a collection or an identifier nobody
// registered is the archive's to explain, not a page of links.
func TestArchiveExplainsItsOwnEmptyPages(t *testing.T) {
	srv := archiveRoutes(t)
	reg := NewRegistry(&config.Config{}, testClient())
	for id, want := range map[string]string{
		"a-collection": "is a collection",
		"no-such-item": "there is no item called",
	} {
		_, _, err := reg.Extract(context.Background(), srv.URL+"/details/"+id, Options{})
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v, want it to say %q", id, err, want)
		}
	}
}
