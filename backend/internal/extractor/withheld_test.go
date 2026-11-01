// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

// refusedPage is a probe of rawURL the site refused: a 403 carrying a block
// page with nothing of the site's own in it.
func refusedProbe(t *testing.T, rawURL string) *Page {
	t.Helper()
	return fixturePage(t, rawURL, http.StatusForbidden, "text/html", `<html><title>Access denied</title></html>`)
}

func TestProbeWithheld(t *testing.T) {
	for _, tc := range []struct {
		name string
		page func() *Page
		want bool
	}{
		{"refused", func() *Page { return refusedProbe(t, "https://a.example.test/x") }, true},
		{"challenged", func() *Page { return challengedPage(t, "https://a.example.test/x") }, true},
		{"rate limited", func() *Page { return fixturePage(t, "https://a.example.test/x", http.StatusTooManyRequests, "", "") }, true},
		{"server error", func() *Page { return fixturePage(t, "https://a.example.test/x", http.StatusBadGateway, "", "") }, true},
		{"transport", func() *Page {
			p := fixturePage(t, "https://a.example.test/x", 0, "", "")
			p.Err = errors.New("connection reset")
			return p
		}, true},
		{"served", func() *Page { return htmlPage(t, "https://a.example.test/x", "<p>hello</p>") }, false},
		{"missing", func() *Page { return fixturePage(t, "https://a.example.test/x", http.StatusNotFound, "text/html", "") }, false},
		{"no client", func() *Page {
			p := fixturePage(t, "https://a.example.test/x", 0, "", "")
			p.Err = errNoProbe
			return p
		}, false},
	} {
		if got := probeWithheld(tc.page()); got != tc.want {
			t.Errorf("%s: probeWithheld = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// jsonServer answers each path in answers with its JSON, counting requests,
// and 404 for anything else.
func jsonServer(t *testing.T, answers map[string]string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var asked atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked.Add(1)
		body, ok := answers[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set(httpx.HeaderContentType, httpx.ContentTypeJSON)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, &asked
}

// A thread whose board page is refused is read through the API, which is
// asked once for recognition and extraction together.
func TestFourChanTakesAWithheldThreadByItsAPI(t *testing.T) {
	srv, asked := jsonServer(t, map[string]string{"/po/thread/570368.json": fourChanThread})
	f := NewFourChan(testClient())
	f.api, f.files = srv.URL, "https://files.example.test"

	p := refusedProbe(t, "https://boards.example.test/po/thread/570368")
	if !f.Recognise(context.Background(), p) {
		t.Fatal("a refused thread page whose API answers was not taken")
	}
	res, err := f.Extract(withPage(context.Background(), p), p.URL, Options{})
	if err != nil || len(res.Files) != 2 {
		t.Fatalf("extract = %v, %v; want the thread's two files", res, err)
	}
	if n := asked.Load(); n != 1 {
		t.Errorf("API asked %d times, want once", n)
	}
	expectRecognised(t, f, p, true)
	// A thread the API does not know, and a served page without the
	// markers, are not taken.
	expectRecognised(t, f, refusedProbe(t, "https://boards.example.test/po/thread/1"), false)
	expectRecognised(t, f, htmlPage(t, "https://boards.example.test/po/thread/570368", "<p>a mirror</p>"), false)
}

// An archive's withheld thread page answers as an archive at its own origin,
// so it stays the archive's even when the board's API still has the thread.
func TestFourChanLeavesAWithheldArchiveThreadToTheArchive(t *testing.T) {
	board, _ := jsonServer(t, map[string]string{"/b/thread/9998.json": fourChanThread})
	archive, _ := jsonServer(t, map[string]string{foolFuukaThreadAPI: foolFuukaThreadJSON})
	f := NewFourChan(testClient())
	f.api = board.URL
	p := refusedProbe(t, archive.URL+"/b/thread/9998/")
	if f.Recognise(context.Background(), p) {
		t.Error("an archive's withheld thread was taken for the board's")
	}
	if !NewFoolFuuka(testClient()).Recognise(context.Background(), p) {
		t.Error("the archive did not take its own withheld thread")
	}
}

// A file link on another host whose page is refused is not taken, and
// neither service's API is asked about that host's id.
func TestKeep2ShareLeavesAnotherHostsWithheldLink(t *testing.T) {
	var asked atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked.Add(1)
		_, _ = w.Write([]byte(`{"status":"success","name":"First Song.mp3","size":12,"is_available":true}`))
	}))
	t.Cleanup(srv.Close)
	k := NewKeep2Share(testClient())
	k.services[0].api, k.services[1].api = srv.URL+"/k2", srv.URL+"/fb"
	expectRecognised(t, k, refusedProbe(t, "https://share.example.test/file/abc123def4567/First_Song.mp3"), false)
	expectRecognised(t, k, challengedPage(t, "https://share.example.test/file/abc123def4567"), false)
	if n := asked.Load(); n != 0 {
		t.Errorf("another host's id was asked of the services' APIs %d times", n)
	}
}

// A share link on another host whose page is refused is not taken, and
// gofile's API is not asked about that host's code: no guest account, no
// signed request, no cooldown a refused one would start for real links.
func TestGofileLeavesAnotherHostsWithheldShare(t *testing.T) {
	g, asked := gofileContentServer(t, [][]string{{"a"}}, func(int) bool { return false })
	g.account.value, g.account.valid, g.account.expires = "guest", true, time.Now().Add(time.Hour)
	expectRecognised(t, g, refusedProbe(t, "https://share.example.test/d/root"), false)
	expectRecognised(t, g, challengedPage(t, "https://share.example.test/d/root"), false)
	if n := asked.Load(); n != 0 {
		t.Errorf("another host's code was asked of gofile's API %d times", n)
	}
	if !g.rejectedAt.IsZero() {
		t.Error("another host's page started gofile's cooldown")
	}
}

func TestMegaTakesAWithheldLinkByItsAPI(t *testing.T) {
	key, _, _, err := megaFileKey(syntheticFileKey())
	if err != nil {
		t.Fatal(err)
	}
	attrs := encryptMegaAttrs(t, "First Clip.mp4", key)
	m := megaTestAPI(t, func(cmd megaCommand) any {
		if cmd.P == "AAAAAAAA" {
			return map[string]any{"s": 4, "at": attrs}
		}
		return -9
	})
	good := "https://share.example.test/file/AAAAAAAA#" + megaKeyString(syntheticFileKey())
	expectRecognised(t, m, refusedProbe(t, good), true)
	expectRecognised(t, m, refusedProbe(t, "https://share.example.test/file/BBBBBBBB#"+megaKeyString(syntheticFileKey())), false)
	// Attributes the link's key does not open are some other file's.
	wrong := syntheticFileKey()
	wrong[0]++
	expectRecognised(t, m, refusedProbe(t, "https://share.example.test/file/AAAAAAAA#"+megaKeyString(wrong)), false)
}

func TestRedGifsTakesAWithheldClipByItsAPI(t *testing.T) {
	srv, _ := jsonServer(t, map[string]string{
		"/auth/temporary":         `{"token":"t"}`,
		"/gifs/firstclip":         `{"gif":{"id":"firstclip","urls":{"hd":"https://media.example.test/firstclip.mp4"}}}`,
		"/users/a-creator/search": `{"gifs":[{"id":"firstclip"}],"pages":1}`,
	})
	r := NewRedGifs(testClient())
	r.api = srv.URL
	expectRecognised(t, r, refusedProbe(t, "https://clips.example.test/watch/firstclip"), true)
	expectRecognised(t, r, refusedProbe(t, "https://clips.example.test/users/a-creator"), true)
	expectRecognised(t, r, refusedProbe(t, "https://clips.example.test/watch/otherclip"), false)
}

// A download page on another host whose probe was refused is not taken, and
// WeTransfer's API is not asked to mint that host's id.
func TestWeTransferLeavesAnotherHostsWithheldPage(t *testing.T) {
	var asked atomic.Int32
	w := wetransferServer(t, func(string, map[string]any) (int, string) {
		asked.Add(1)
		return http.StatusOK, `{"direct_link":"https://download.example.test/archive"}`
	})
	expectRecognised(t, w, refusedProbe(t, "https://transfer.example.test/downloads/abc123/def456"), false)
	if n := asked.Load(); n != 0 {
		t.Errorf("another host's id was asked of WeTransfer's API %d times", n)
	}
}

func TestTurboTakesAWithheldPageBySigning(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/sign" && r.URL.Query().Get("v") == "abc123" {
			_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "url": srv.URL + "/media/abc123.mp4"})
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	s := newShare(testClient(), &turboSite)
	// The signing endpoint is the page's own origin, so the page stays put.
	if !s.Recognise(context.Background(), refusedProbe(t, srv.URL+"/d/abc123")) {
		t.Error("a refused page whose origin signs its id was not taken")
	}
	if s.Recognise(context.Background(), refusedProbe(t, srv.URL+"/d/zzz999")) {
		t.Error("a refused page whose id is not signed was taken")
	}
}

func TestStreamableTakesAWithheldPageByItsDocument(t *testing.T) {
	srv, _ := jsonServer(t, map[string]string{"/videos/abcd12": `{"status":2,"files":{}}`})
	p := refusedProbe(t, "https://clips.example.test/abcd12")
	if !streamableConfirm(context.Background(), testClient(), p, srv.URL+"/videos/") {
		t.Error("a refused page whose document answers was not taken")
	}
	if streamableConfirm(context.Background(), testClient(), refusedProbe(t, "https://clips.example.test/zzzz99"), srv.URL+"/videos/") {
		t.Error("a refused page whose document does not answer was taken")
	}
	// A bare word is every site's navigation, and is not asked about.
	if streamableConfirm(context.Background(), testClient(), refusedProbe(t, "https://clips.example.test/watch"), srv.URL+"/videos/") {
		t.Error("a refused navigation page was asked about")
	}
	// A served page needs the name, as before.
	if streamableConfirm(context.Background(), testClient(), htmlPage(t, "https://clips.example.test/abcd12", "<p>x</p>"), srv.URL+"/videos/") {
		t.Error("a served page naming nobody was taken")
	}
}

// An explicit article is settled by the API both behind a refusal and when
// an unfamiliar frontend serves ordinary HTML.
func TestMediaWikiAsksAboutAWithheldArticle(t *testing.T) {
	srv, _ := mediaWikiServer(t)
	m := NewMediaWiki(testClient())
	if !m.Recognise(context.Background(), refusedProbe(t, srv.URL+"/wiki/An_article")) {
		t.Error("a refused article whose API answers was not taken")
	}
	if !m.Recognise(context.Background(), htmlPage(t, srv.URL+"/wiki/An_article", "<p>A custom frontend</p>")) {
		t.Error("a served article whose API proves the wiki was not taken")
	}
}
