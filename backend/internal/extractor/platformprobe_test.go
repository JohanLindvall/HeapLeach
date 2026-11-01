// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

// probeRegistry is a registry with a working client, for tests that follow
// a URL through identification to its extractor.
func probeRegistry() *Registry {
	return NewRegistry(&config.Config{}, testClient())
}

// TestFediverseAsksNodeinfoOncePerServer: three posts on one server are
// recognised through the registry with one nodeinfo discovery between them,
// not one per post, since a server does not change what it runs between
// one paste and the next and instances rate-limit per address.
func TestFediverseAsksNodeinfoOncePerServer(t *testing.T) {
	var nodeinfo atomic.Int32
	srv := fediServer(t, "mastodon", func(w http.ResponseWriter, r *http.Request) bool {
		var id string
		if _, err := fmt.Sscanf(r.URL.Path, "/api/v1/statuses/%s", &id); err != nil {
			return false
		}
		writeJSON(w, fmt.Sprintf(`{"id":%q,"account":{"acct":"alice","username":"alice"},
			"media_attachments":[{"id":"m%s","type":"image","url":"https://media.example.test/%s.jpg"}]}`,
			id, id, id))
		return true
	})
	counted := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == fediWellKnown {
			nodeinfo.Add(1)
		}
		srv.Config.Handler.ServeHTTP(w, r)
	})
	front := httptest.NewServer(counted)
	t.Cleanup(front.Close)

	reg := probeRegistry()
	for _, id := range []string{"101", "102", "103"} {
		res, ex, err := reg.Extract(context.Background(), front.URL+"/@alice/"+id, Options{})
		if err != nil {
			t.Fatalf("post %s: %v", id, err)
		}
		if ex.Name() != "fediverse" || len(res.Files) != 1 {
			t.Fatalf("post %s: %s with %d files, want the fediverse's one", id, ex.Name(), len(res.Files))
		}
	}
	if got := nodeinfo.Load(); got != 1 {
		t.Errorf("asked nodeinfo %d times for three posts on one server, want once", got)
	}
}

// TestFediverseNodeinfoSurvivesOneFailure: a server whose nodeinfo answers
// one 503 is asked again rather than taken for no fediverse server at all,
// which would hand its post's application shell to the fallback as a file.
func TestFediverseNodeinfoSurvivesOneFailure(t *testing.T) {
	var failed atomic.Bool
	srv := fediServer(t, "mastodon", func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != "/api/v1/statuses/7" {
			return false
		}
		writeJSON(w, `{"id":"7","account":{"acct":"alice","username":"alice"},
			"media_attachments":[{"id":"m7","type":"image","url":"https://media.example.test/7.jpg"}]}`)
		return true
	})
	flaky := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == fediWellKnown && failed.CompareAndSwap(false, true) {
			http.Error(w, "busy", http.StatusServiceUnavailable)
			return
		}
		srv.Config.Handler.ServeHTTP(w, r)
	})
	front := httptest.NewServer(flaky)
	t.Cleanup(front.Close)

	res, ex, err := probeRegistry().Extract(context.Background(), front.URL+"/@alice/7", Options{})
	if err != nil {
		t.Fatal(err)
	}
	if ex.Name() != "fediverse" || len(res.Files) != 1 {
		t.Fatalf("got %s with %d files, want the post's one file from the fediverse", ex.Name(), len(res.Files))
	}
}

// TestPlatformProbeIsNotStarvedByAdmission: the bounded identification
// request's deadline is its own, started once it is admitted, so a probe
// that waited for a slot behind slow requests still gets its answer, and
// no timeout caused by the wait is kept on the shared page.
func TestPlatformProbeIsNotStarvedByAdmission(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, `{"ok":true}`)
	}))
	t.Cleanup(srv.Close)
	adm := httpx.NewAdmission(1)
	ctx := httpx.WithAdmission(context.Background(), adm)
	// Hold the only slot for longer than the probe's own bound.
	hold := make(chan struct{})
	release, err := httpx.Admit(ctx)
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		<-hold
		release()
	}()
	old := platformProbeTimeout
	platformProbeTimeout = 100 * time.Millisecond
	t.Cleanup(func() { platformProbeTimeout = old })
	go func() {
		time.Sleep(3 * platformProbeTimeout)
		close(hold)
	}()

	p := &Page{URL: mustParse(t, srv.URL), Final: mustParse(t, srv.URL)}
	var out struct {
		OK bool `json:"ok"`
	}
	if !pageJSON(ctx, p, testClient(), srv.URL+"/api", nil, &out) || !out.OK {
		t.Fatal("the probe timed out waiting for admission rather than on the network")
	}
}

// TestKemonoRevisionThroughTheRegistry: a link to a revision of a post,
// served by the sites' app shell, is the post's, not a page for the
// fallback to queue.
func TestKemonoRevisionThroughTheRegistry(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/sample/user/creator/profile":
			writeJSON(w, `{"id":"creator","service":"sample","name":"A Creator"}`)
		case "/api/v1/sample/user/creator/post/42":
			writeJSON(w, `{"post":{"id":"42","title":"A Post","file":{"name":"one.jpg","path":"/a/one.jpg"}}}`)
		default:
			w.Header().Set(httpx.HeaderContentType, "text/html")
			_, _ = w.Write([]byte(kemonoShellPage))
		}
	}))
	t.Cleanup(srv.Close)
	res, ex, err := probeRegistry().Extract(context.Background(), srv.URL+"/sample/user/creator/post/42/revision/7", Options{})
	if err != nil || ex.Name() != "kemono" || len(res.Files) != 1 || res.Files[0].Name != "one.jpg" {
		t.Fatalf("got %v from %v: %v, want the post's file", res, ex, err)
	}
}

// TestRefreshReadsThePageAgain: a file refreshed at transfer time, inside
// the page cache's lifetime, is read from the page as the site serves it
// now, not from the page kept from the listing, whose signed link is the
// one that went stale.
func TestRefreshReadsThePageAgain(t *testing.T) {
	var served atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/watch/clip" {
			http.NotFound(w, r)
			return
		}
		n := served.Add(1)
		w.Header().Set(httpx.HeaderContentType, "text/html")
		fmt.Fprintf(w, `<html><head><title>A Clip</title></head><body>`+
			`<video src="/media/clip.mp4?sig=%d"></video></body></html>`, n)
	}))
	t.Cleanup(srv.Close)
	res, _, err := probeRegistry().Extract(context.Background(), srv.URL+"/watch/clip", Options{})
	if err != nil || len(res.Files) != 1 || res.Files[0].Refresh == nil {
		t.Fatalf("got %v: %v, want one refreshable file", res, err)
	}
	if !strings.HasSuffix(res.Files[0].URL, "sig=1") {
		t.Fatalf("listed %s", res.Files[0].URL)
	}
	target, err := res.Files[0].Refresh(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(target.URL, "sig=2") {
		t.Errorf("refreshed to %s, want the link the page serves now", target.URL)
	}
}
