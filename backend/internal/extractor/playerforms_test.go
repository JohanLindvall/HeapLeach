// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

// Link forms a player or file host hands out that its row's own Shape does
// not name, each extracted through a registry the way a pasted link is,
// against an invented site.

// pathCounter serves handlers by path and counts the requests each path got.
type pathCounter struct {
	mu   sync.Mutex
	hits map[string]int
}

func (c *pathCounter) count(path string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.hits == nil {
		c.hits = map[string]int{}
	}
	c.hits[path]++
}

func (c *pathCounter) get(path string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.hits[path]
}

// formServer is a site answering each path from pages, with a 404 for any
// other; a value starting "->" redirects there.
func formServer(t *testing.T, pages map[string]string) (*httptest.Server, *pathCounter) {
	t.Helper()
	var hits pathCounter
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.count(r.URL.Path)
		key := r.URL.Path
		if r.URL.RawQuery != "" {
			if _, ok := pages[key+"?"+r.URL.RawQuery]; ok {
				key += "?" + r.URL.RawQuery
			}
		}
		body, ok := pages[key]
		if !ok {
			http.NotFound(w, r)
			return
		}
		if target, ok := strings.CutPrefix(body, "->"); ok {
			http.Redirect(w, r, target, http.StatusFound)
			return
		}
		switch {
		case strings.HasPrefix(body, "{"):
			w.Header().Set(httpx.HeaderContentType, httpx.ContentTypeJSON)
		default:
			w.Header().Set(httpx.HeaderContentType, "text/html; charset=utf-8")
		}
		fmt.Fprint(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

// A bare /<code> that an alias turned away is checked against the canonical
// player; the same form answered by an ordinary page costs nothing more.
func TestVidmolyBareCodeReachesCanonicalPlayer(t *testing.T) {
	var reads atomic.Int32
	canonical := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reads.Add(1)
		w.Header().Set(httpx.HeaderContentType, "text/html")
		fmt.Fprint(w, `<html><title>Invented clip - VidMoly</title><script>player.setup({sources:[{file:"https://cdn.example.test/invented.mp4"}]})</script></html>`)
	}))
	defer canonical.Close()
	alias, _ := formServer(t, map[string]string{
		"/abcdef123456": "->/advert",
		"/advert":       `<html><title>Loading</title></html>`,
		"/ghijkl789012": `<html><title>An ordinary page</title></html>`,
	})
	client := testClient()
	defer client.CloseIdleConnections()
	site := vidmolyPlayer
	site.page = func(u *url.URL) (string, error) { return canonical.URL + "/embed-" + vidmolyCode(u) + ".html", nil }
	p := NewPlayer(client, &site)
	reg := &Registry{client: client, extractors: []Extractor{p}, fallback: NewDirect(client)}

	res, ex, err := reg.Extract(context.Background(), alias.URL+"/abcdef123456", Options{})
	if err != nil || ex != p || len(res.Files) != 1 || res.Files[0].URL != "https://cdn.example.test/invented.mp4" {
		t.Fatalf("bare code: extractor=%v result=%+v err=%v", ex, res, err)
	}
	before := reads.Load()
	if _, ex, _ := reg.Extract(context.Background(), alias.URL+"/ghijkl789012", Options{}); ex == p || reads.Load() != before {
		t.Fatalf("an ordinary page of the bare form went to %v after %d canonical reads", ex, reads.Load()-before)
	}
	if expectShape(t, p, "https://alias.example.test/abcdef123456", false); t.Failed() {
		t.Fatal("the bare form must stay out of the link scanner's forms")
	}
}

// The og:video link redirects to the watch page, which is the player's.
func TestSendvidMP4LinkReachesThePlayer(t *testing.T) {
	srv, _ := formServer(t, map[string]string{
		"/abcd1234.mp4": "->/abcd1234",
		"/abcd1234": `<html><head><title>Sendvid</title><meta property="og:site_name" content="Sendvid">` +
			`<meta property="og:title" content="First Clip"></head><body><video><source id="video_source" ` +
			`src="https://media.example.test/first.mp4?validto=1" type="video/mp4"></video></body></html>`,
	})
	res, ex, err := platformRegistry().Extract(context.Background(), srv.URL+"/abcd1234.mp4", Options{})
	if err != nil || ex.Name() != "sendvid" || len(res.Files) != 1 || res.Files[0].URL != "https://media.example.test/first.mp4?validto=1" {
		t.Fatalf("extractor=%v result=%+v err=%v", ex, res, err)
	}
	expectShape(t, NewPlayer(nil, &sendvidPlayer), "https://videos.example.test/abcd1234.mp4", true)
	expectShape(t, NewPlayer(nil, &sendvidPlayer), "https://videos.example.test/clips/abcd1234.mp4", false)
}

// A player or download page that came back without the player is read as
// the plain watch page the row reads anyway.
func TestPlayerRowsCheckTheCanonicalPage(t *testing.T) {
	t.Run("lulustream", func(t *testing.T) {
		srv, hits := formServer(t, map[string]string{
			// The embed answers on this domain with a page carrying no player.
			"/e/abcd1234wxyz": `<html><title>LuluStream</title><p>Loading</p></html>`,
			"/abcd1234wxyz": `<html><head><title>First Clip - LuluStream</title></head><body><h1>First Clip</h1>` +
				`<script>jwplayer("vplayer").setup({sources:[{file:"/hls/index.m3u8"}]});</script></body></html>`,
			"/hls/index.m3u8": "#EXTM3U\n#EXT-X-TARGETDURATION:10\n#EXTINF:10,\nseg-1.ts\n#EXT-X-ENDLIST\n",
		})
		res, ex, err := platformRegistry().Extract(context.Background(), srv.URL+"/e/abcd1234wxyz", Options{})
		if err != nil || ex.Name() != "lulustream" || len(res.Files) != 1 || res.Files[0].Name != "First Clip.ts" ||
			hits.get("/abcd1234wxyz") != 1 || hits.get("/e/abcd1234wxyz") != 1 {
			t.Fatalf("extractor=%v result=%+v err=%v (page reads %d, embed reads %d)", ex, res, err,
				hits.get("/abcd1234wxyz"), hits.get("/e/abcd1234wxyz"))
		}
	})
	t.Run("vids.st", func(t *testing.T) {
		srv, hits := formServer(t, map[string]string{
			"/e/12345": `<html><title>Player</title></html>`,
			"/v/12345": `<html><script>var playerConfig = {"videoUrl":"https://cdn.example.test/first.mp4","videoName":"First Clip.mp4"};</script></html>`,
		})
		res, ex, err := platformRegistry().Extract(context.Background(), srv.URL+"/e/12345", Options{})
		if err != nil || ex.Name() != "vids.st" || len(res.Files) != 1 || res.Files[0].Name != "First Clip.mp4" || hits.get("/v/12345") != 1 {
			t.Fatalf("extractor=%v result=%+v err=%v (watch page read %d times)", ex, res, err, hits.get("/v/12345"))
		}
	})
	t.Run("ok.ru", func(t *testing.T) {
		var embeds atomic.Int32
		embed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			embeds.Add(1)
			w.Header().Set(httpx.HeaderContentType, "text/html")
			fmt.Fprint(w, `<html><div data-module="OKVideo" data-options="{}"></div></html>`)
		}))
		defer embed.Close()
		site, _ := formServer(t, map[string]string{
			"/group/somegroup/video/12345": `<html><title>A group</title></html>`,
			"/dk":                          `<html><title>A legacy page</title></html>`,
			"/video/12345":                 `<html><title>An unrelated site</title></html>`,
		})
		client := testClient()
		defer client.CloseIdleConnections()
		row := okruPlayer
		row.page = func(u *url.URL) (string, error) { return embed.URL + "/videoembed/" + okruVideoID(u), nil }
		p := NewPlayer(client, &row)
		for _, link := range []string{"/group/somegroup/video/12345", "/dk?st.cmd=movieLayer&st.mvId=12345"} {
			page := probePage(context.Background(), client, mustParse(t, site.URL+link))
			if !p.Recognise(context.Background(), page) {
				t.Errorf("%s was not checked against its embed", link)
			}
		}
		if embeds.Load() != 2 {
			t.Errorf("asked the embed %d times, want 2", embeds.Load())
		}
		// The plain video form is the row's Shape and still needs its page:
		// another site's /video/<id> does not cost a request elsewhere.
		page := probePage(context.Background(), client, mustParse(t, site.URL+"/video/12345"))
		if p.Recognise(context.Background(), page) || embeds.Load() != 2 {
			t.Errorf("an unrelated /video/<id> page was claimed after %d embed reads", embeds.Load())
		}
		expectShape(t, p, "https://ok.example.test/group/somegroup/video/12345", true)
		expectShape(t, p, "https://ok.example.test/profile/someone/video/12345", true)
	})
}

// An embed is read as its key's video page.
func TestPornhubEmbedReadsItsVideoPage(t *testing.T) {
	srv, hits := formServer(t, map[string]string{
		"/embed/AbCd1234": `<html><title>Embed</title></html>`,
		"/view_video.php?viewkey=AbCd1234": `<html><head><title>First Clip - Pornhub.com</title>` +
			`<meta property="og:site_name" content="Pornhub"></head><body><script>var flashvars_1 = ` +
			`{"video_title":"First Clip","mediaDefinitions":[{"format":"mp4","quality":"720",` +
			`"videoUrl":"https://cdn.example.test/first-720.mp4"}]};</script></body></html>`,
	})
	res, ex, err := platformRegistry().Extract(context.Background(), srv.URL+"/embed/AbCd1234", Options{})
	if err != nil || ex.Name() != "pornhub" || len(res.Files) != 1 || res.Files[0].URL != "https://cdn.example.test/first-720.mp4" {
		t.Fatalf("extractor=%v result=%+v err=%v", ex, res, err)
	}
	if hits.get("/view_video.php") != 1 {
		t.Errorf("video page read %d times, want once: the validated page is reused", hits.get("/view_video.php"))
	}
	ph := NewPlayer(nil, &pornhubPlayer)
	expectShape(t, ph, "https://videos.example.test/view_video.php?viewkey=AbCd1234", true)
	// The embed's form is every player host's, so it stays a page form.
	expectShape(t, ph, "https://videos.example.test/embed/AbCd1234", false)
}

// A link of no form of the row's own that redirects into a video page —
// a random-video or short link a listing carries — is read as the page it
// ended on, which is the one recognition judged, rather than refused as
// naming no video.
func TestPlayerReadsTheVideoPageALinkRedirectedTo(t *testing.T) {
	srv, _ := formServer(t, map[string]string{
		"/video/random": "->/view_video.php?viewkey=AbCd1234",
		"/view_video.php?viewkey=AbCd1234": `<html><head><title>First Clip - Pornhub.com</title>` +
			`<meta property="og:site_name" content="Pornhub"></head><body><script>var flashvars_1 = ` +
			`{"video_title":"First Clip","mediaDefinitions":[{"format":"mp4","quality":"720",` +
			`"videoUrl":"https://cdn.example.test/first-720.mp4"}]};</script></body></html>`,
	})
	res, ex, err := platformRegistry().Extract(context.Background(), srv.URL+"/video/random", Options{})
	if err != nil || ex.Name() != "pornhub" || len(res.Files) != 1 || res.Files[0].URL != "https://cdn.example.test/first-720.mp4" {
		t.Fatalf("extractor=%v result=%+v err=%v", ex, res, err)
	}
}

// Uppercase in the forms the sites mint: a profile tab spelled with
// capitals, ids drawn from both cases.
func TestPlayerFormsTakeEitherCase(t *testing.T) {
	expectShape(t, NewPlayer(nil, &epornerPlayer), "https://videos.example.test/profile/someone/Uploaded-Videos/", true)
	expectShape(t, NewPlayer(nil, &luluPlayer), "https://videos.example.test/e/AbCd1234WxYz", true)
	expectShape(t, NewPlayer(nil, &mixdropPlayer), "https://videos.example.test/f/AbCd12Ef", true)
}

// A watch page named for the site whose player meta is missing — a video
// still processing, an embed served bare, the /s/ share form — is taken by
// the player's document describing its id.
func TestStreamableConfirmedByItsDocument(t *testing.T) {
	api, hits := formServer(t, map[string]string{
		"/videos/abcd12": `{"status":1,"percent":40,"files":{}}`,
	})
	client := testClient()
	defer client.CloseIdleConnections()
	named := `<html><head><meta property="og:site_name" content="Streamable"></head></html>`
	for _, link := range []string{"https://clips.example.test/e/abcd12", "https://clips.example.test/s/abcd12/first-clip"} {
		page := htmlPage(t, link, named)
		if !streamableConfirm(context.Background(), client, page, api.URL+"/videos/") {
			t.Errorf("%s was not confirmed by its document", link)
		}
	}
	if !streamablePage(mustParse(t, "https://clips.example.test/s/abcd12/first-clip")) {
		t.Error("the /s/<id>/<slug> form was not a watch page")
	}
	// Unnamed, or of another form, the document is never asked.
	asked := hits.get("/videos/abcd12")
	for _, page := range []*Page{
		htmlPage(t, "https://clips.example.test/e/abcd12", `<html><title>Other</title></html>`),
		htmlPage(t, "https://clips.example.test/a/b/c", named),
	} {
		if streamableConfirm(context.Background(), client, page, api.URL+"/videos/") {
			t.Errorf("%s was confirmed", page.URL)
		}
	}
	if hits.get("/videos/abcd12") != asked {
		t.Error("the document was asked about a page that does not qualify")
	}
}

// Showcase, review and manage links name a video too: recognised by its
// player page when the watch page offers no player of its own.
func TestVimeoCollectionFormsReadThePlayer(t *testing.T) {
	player, hits := formServer(t, map[string]string{
		"/video/123456": `<html><script>window.playerConfig = {"view":1}</script></html>`,
	})
	client := testClient()
	defer client.CloseIdleConnections()
	v := NewVimeo(client)
	v.player = player.URL + "/video/"
	named := `<html><head><meta property="og:site_name" content="Vimeo"></head></html>`
	for _, link := range []string{
		"https://vids.example.test/showcase/42/video/123456",
		"https://vids.example.test/someone/review/123456/abcdef0123",
		"https://vids.example.test/manage/videos/123456",
	} {
		expectShape(t, v, link, true)
		if !v.Recognise(context.Background(), htmlPage(t, link, named)) {
			t.Errorf("%s was not recognised by its player", link)
		}
	}
	if v.Recognise(context.Background(), htmlPage(t, "https://vids.example.test/showcase/42/video/123456",
		`<html><title>Another site</title></html>`)) {
		t.Error("a page not named Vimeo was taken")
	}
	if got := hits.get("/video/123456"); got != 3 {
		t.Errorf("player asked %d times, want once per page", got)
	}
}

// The player's origin Referer is honored after the no-Referer probe; the
// token is asked for once, and the transfer starts a fresh handshake.
func TestDoodstreamReadsPlayerWithItsReferer(t *testing.T) {
	srv, hits := formServer(t, map[string]string{
		"/e/abcdef123456":             `<html><title>Invented Clip</title><script>fetch('/pass_md5/session/synthetic')</script></html>`,
		"/pass_md5/session/synthetic": "https://cdn.example.test/media/",
	})
	client := testClient()
	defer client.CloseIdleConnections()
	player := NewDoodStream(client)
	reg := &Registry{client: client, extractors: []Extractor{player}, fallback: NewDirect(client)}
	res, ex, err := reg.Extract(context.Background(), srv.URL+"/e/abcdef123456", Options{})
	if err != nil || ex != player || len(res.Files) != 1 {
		t.Fatalf("extractor=%v result=%+v err=%v", ex, res, err)
	}
	if hits.get("/e/abcdef123456") != 2 || hits.get("/pass_md5/session/synthetic") != 1 {
		t.Errorf("player read %d times, token %d; want the probe, the Referer-bearing player and one token read", hits.get("/e/abcdef123456"), hits.get("/pass_md5/session/synthetic"))
	}
	// The transfer reads it afresh.
	if _, err := res.Files[0].Resolve(context.Background()); err != nil {
		t.Fatal(err)
	}
	if hits.get("/e/abcdef123456") != 3 {
		t.Errorf("the transfer read the player %d times in all, want a fresh read", hits.get("/e/abcdef123456"))
	}
}

// The older share forms are taken when the app shell answers them; the
// bare one is never offered to the link scanner.
func TestGofileLegacyForms(t *testing.T) {
	g := NewGofile(&config.Config{}, nil)
	shell := `<html><head><title>Gofile</title><script src="` + gofileScriptPath + `"></script></head></html>`
	for _, link := range []string{"https://share.example.test/d/AbC123", "https://share.example.test/AbC123", "https://share.example.test/?c=AbC123"} {
		expectRecognised(t, g, htmlPage(t, link, shell), true)
	}
	expectRecognised(t, g, htmlPage(t, "https://share.example.test/AbC123", `<html><title>Other</title></html>`), false)
	expectShape(t, g, "https://share.example.test/?c=AbC123", true)
	expectShape(t, g, "https://share.example.test/AbC123", false)
}

// A token that goes stale between a folder's pages is renewed and the
// folder asked for again, rather than kept as a part.
func TestGofileStaleTokenOnALaterPage(t *testing.T) {
	var asked atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked.Add(1)
		w.Header().Set(httpx.HeaderContentType, httpx.ContentTypeJSON)
		if r.URL.Query().Get("page") == "2" && r.Header.Get(httpx.HeaderAuthorization) == "Bearer stale" {
			fmt.Fprint(w, `{"status":"error-wrongToken"}`)
			return
		}
		n := r.URL.Query().Get("page")
		fmt.Fprintf(w, `{"status":"ok","data":{"id":"root","type":"folder","name":"Root","childrenIds":["f%s"],`+
			`"children":{"f%s":{"id":"f%s","type":"file","name":"f%s.bin","link":"https://cdn.example.test/f%s"}}},`+
			`"metadata":{"hasNextPage":%v}}`, n, n, n, n, n, n == "1")
	}))
	defer srv.Close()
	g := &Gofile{client: testClient(), fallback: "a-configured-secret", lang: "en-US", api: srv.URL}
	if _, _, err := g.fetchContent(context.Background(), "root", "stale", "", 10); err == nil || !strings.Contains(err.Error(), "token") {
		t.Fatalf("a stale token on page 2 gave %v, want the stale token itself", err)
	}
	c, end, err := g.fetchContent(context.Background(), "root", "fresh", "", 10)
	if err != nil || strings.Join(c.ChildrenIDs, ",") != "f1,f2" || end.render() != "" {
		t.Fatalf("children=%v end=%q err=%v", c, end.render(), err)
	}
}

// File-host forms the sites still hand out: the image viewer, a Drive file
// as another signed-in account sees it, slugs with dashes.
func TestFileHostForms(t *testing.T) {
	reg := testRegistry(t, nil)
	if ex, _ := reg.Known(mustParse(t, "https://img.example.test/i/abcd1234")); ex.Name() != "imagepond" {
		t.Errorf("an /i/ link is labelled %s, want the image host's", ex.Name())
	}
	bunkr := NewBunkr(nil)
	expectRecognised(t, bunkr, htmlPage(t, "https://files.example.test/i/first-image", `<div data-file-id="42"></div>`), true)
	expectRecognised(t, bunkr, htmlPage(t, "https://files.example.test/i/first-image", `<div>an image</div>`), false)
	expectRecognised(t, bunkr, htmlPage(t, "https://files.example.test/a/an-album",
		`<html><head><meta property="og:site_name" content="Bunkr"></head><body><a href="/f/first">First</a></body></html>`), true)
	expectRecognised(t, bunkr, htmlPage(t, "https://files.example.test/a/an-album",
		`<html><head><title>Another album</title></head><body><a href="/f/first">First</a></body></html>`), false)
	if !bunkrFilePage("https://files.example.test/v/first") || !bunkrFilePage("https://files.example.test/i/first") {
		t.Error("an older viewer link was not taken for a file page")
	}
	expectShape(t, NewGoogleDrive(nil), "https://docs.example.test/file/u/1/d/AbCdEfGhIjKl/view", true)
	if id := driveFileID(mustParse(t, "https://docs.example.test/file/u/1/d/AbCdEfGhIjKl/view")); id != "AbCdEfGhIjKl" {
		t.Errorf("drive id = %q", id)
	}
	expectShape(t, NewCyberdrop(nil), "https://drop.example.test/f/a-file_slug", true)
	expectShape(t, NewCyberdrop(nil), "https://drop.example.test/a/an-album", true)
}

// The API is asked only about the software's own id form: a landing page of
// another host's /d/, /f/ or /u/ costs no API guesses.
func TestPixeldrainGuessesOnlyItsIDForm(t *testing.T) {
	var api atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") {
			api.Add(1)
			http.NotFound(w, r)
			return
		}
		w.Header().Set(httpx.HeaderContentType, "text/html")
		fmt.Fprint(w, `<html><title>A landing page</title></html>`)
	}))
	defer srv.Close()
	client := testClient()
	defer client.CloseIdleConnections()
	p := NewPixeldrain(client)
	for _, link := range []string{"/d/a-long-folder-slug", "/f/abcdefghijklmn", "/u/abc"} {
		page := probePage(context.Background(), client, mustParse(t, srv.URL+link))
		if p.Recognise(context.Background(), page) {
			t.Errorf("%s was taken", link)
		}
	}
	if api.Load() != 0 {
		t.Errorf("the API was asked %d times about other hosts' forms", api.Load())
	}
}

// A file's API link, the form a page embedding it names, is a file of its
// own: followed for its form and resolved to the same file.
func TestPixeldrainAPIFileLink(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/file/AbCd1234/info":
			w.Header().Set(httpx.HeaderContentType, httpx.ContentTypeJSON)
			fmt.Fprint(w, `{"id":"AbCd1234","name":"first.bin","size":12,"mime_type":"application/octet-stream"}`)
		case "/api/file/AbCd1234":
			w.Header().Set(httpx.HeaderContentType, "application/octet-stream")
			fmt.Fprint(w, "not read")
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	reg := platformRegistry()
	if _, known := reg.Known(mustParse(t, srv.URL+"/api/file/AbCd1234")); !known {
		t.Error("the API link is not a form the link scanner follows")
	}
	res, ex, err := reg.Extract(context.Background(), srv.URL+"/api/file/AbCd1234", Options{})
	if err != nil || ex.Name() != "pixeldrain" || len(res.Files) != 1 || res.Files[0].Name != "first.bin" ||
		res.Files[0].URL != srv.URL+"/api/file/AbCd1234?download" {
		t.Fatalf("extractor=%v result=%+v err=%v", ex, res, err)
	}
}
