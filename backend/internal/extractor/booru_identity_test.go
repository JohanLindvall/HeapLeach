// SPDX-License-Identifier: MIT

package extractor

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

// Recognition of unlisted installs must retain their full hostname in both
// post and collection titles. The proxy answers every request locally.
func TestBooruUnknownInstallKeepsFullHostname(t *testing.T) {
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/posts", "/posts/42":
			w.Header().Set(httpx.HeaderContentType, "text/html")
			_, _ = fmt.Fprint(w, `<html><title>Example board</title><body data-controller="posts"></body></html>`)
		case "/posts.json":
			w.Header().Set(httpx.HeaderContentType, httpx.ContentTypeJSON)
			switch {
			case r.URL.Query().Get("page") != "1":
				_, _ = fmt.Fprint(w, `[]`)
			case r.URL.Query().Get("tags") == "id:42":
				_, _ = fmt.Fprint(w, `[{"id":42,"file_url":"/original/first-picture.jpg","file_size":12}]`)
			default:
				_, _ = fmt.Fprint(w, `[{"id":42,"file_url":"/original/first-picture.jpg","file_size":12},
					{"id":43,"file_url":"/original/second-picture.jpg","file_size":13}]`)
			}
		default:
			t.Errorf("unexpected request: %s", r.URL)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(proxy.Close)
	client, err := testClient().ThroughProxy(proxy.URL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.CloseIdleConnections)
	reg := NewRegistry(&config.Config{}, client)
	for _, host := range []string{"drawings.example.test", "drawings.example.invalid", "booru.example.test"} {
		for _, target := range []struct {
			path, suffix string
			files        int
		}{
			{"/posts/42", "-42", 1},
			{"/posts?tags=paintings", " paintings", 2},
		} {
			t.Run(host+target.path, func(t *testing.T) {
				res, _, err := reg.Extract(t.Context(), "http://"+host+target.path, Options{})
				if err != nil || res == nil {
					t.Fatalf("extract: result=%+v err=%v", res, err)
				}
				if want := host + target.suffix; res.Title != want {
					t.Errorf("title=%q, want %q", res.Title, want)
				}
				if len(res.Files) != target.files {
					t.Errorf("files=%d, want %d", len(res.Files), target.files)
				}
			})
		}
	}
}
