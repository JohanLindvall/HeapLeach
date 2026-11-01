// SPDX-License-Identifier: MIT

package extractor

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

// What each site's extractor does with the links its hosts claim
// (hostClaim): one claimed first is read with no page attached, so nothing
// it does may lean on the probe; one claimed as a fallback is handed the
// site's own page that nothing recognised, and says why in the site's
// words rather than reading the page's related links as the item.

// claimedRegistry is a registry of ex alone, whose links on a fixture
// server's host are claimed as ex's hosts' are.
func claimedRegistry(ex Extractor) *Registry {
	client := testClient()
	reg := &Registry{client: client, fallback: NewDirect(client)}
	reg.extractors = []Extractor{ex}
	aliasClaims(reg, ex.Name(), "127.0.0.1")
	return reg
}

// pageRefusingSite serves api's answers by path and refuses every other
// request the way a challenged front end does, recording what was asked.
type pageRefusingSite struct {
	srv *httptest.Server

	mu    sync.Mutex
	asked []string
}

func newPageRefusingSite(t *testing.T, api map[string]string) *pageRefusingSite {
	t.Helper()
	s := &pageRefusingSite{}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.asked = append(s.asked, r.URL.RequestURI())
		s.mu.Unlock()
		if body, ok := api[r.URL.Path]; ok {
			if strings.HasPrefix(body, "<") {
				w.Header().Set(httpx.HeaderContentType, "text/html")
			} else {
				w.Header().Set(httpx.HeaderContentType, httpx.ContentTypeJSON)
			}
			_, _ = io.WriteString(w, body)
			return
		}
		w.Header().Set(httpx.HeaderContentType, "text/html")
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `<html><head><title>Just a moment...</title></head><body>Checking your browser</body></html>`)
	}))
	t.Cleanup(s.srv.Close)
	return s
}

// wasAsked reports whether path was requested.
func (s *pageRefusingSite) wasAsked(path string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, asked := range s.asked {
		if u, err := url.Parse(asked); err == nil && u.Path == path {
			return true
		}
	}
	return false
}

// TestHostClaimedFirstReadsWithoutThePage: every extractor claimed first
// reads its links through what it always read — an API, a player, a
// document — with no page attached, whatever the page would have said: a
// challenge, a CAPTCHA, a shell with no markers. Its own refusals reach the
// job in its own words.
func TestHostClaimedFirstReadsWithoutThePage(t *testing.T) {
	t.Run("vimeo watch link read from its player", func(t *testing.T) {
		// The watch page would answer a CAPTCHA; the player is all
		// Extract reads.
		srv, _ := vimeoServer(t, map[string]string{"123456": vimeoPage(1, "A Clip",
			`{"files": {"progressive": [{"url": "https://media.example.test/a.mp4", "height": 720}]}}`)})
		res, ex, err := claimedRegistry(vimeoWith(srv)).Extract(t.Context(), srv.URL+"/123456", Options{})
		if err != nil || ex.Name() != "vimeo" || len(res.Files) != 1 || res.Title != "A Clip" {
			t.Fatalf("%v from %v: %v; want the player's one file", res, ex, err)
		}
	})
	t.Run("vimeo player refused says the video is not public", func(t *testing.T) {
		srv, _ := vimeoServer(t, nil)
		for _, path := range []string{"/123456", "/video/123456"} {
			_, ex, err := claimedRegistry(vimeoWith(srv)).Extract(t.Context(), srv.URL+path, Options{})
			if ex == nil || ex.Name() != "vimeo" || !errors.Is(err, errVimeoNotPublic) {
				t.Errorf("%s: %v from %v, want vimeo saying the video is not public", path, err, ex)
			}
		}
	})
	t.Run("bitchute embed read through the API", func(t *testing.T) {
		stub := &bitchuteStub{}
		b := stub.start(t)
		res, ex, err := claimedRegistry(b).Extract(t.Context(), stub.root+"/embed/AAAABBBBCCCC/", Options{})
		if err != nil || ex.Name() != "bitchute" || len(res.Files) != 1 {
			t.Fatalf("%v from %v: %v; want the API's file", res, ex, err)
		}
		if stub.calls["/embed/AAAABBBBCCCC/"] != 0 {
			t.Error("the embed page was fetched although the API answered")
		}
	})
	t.Run("streamable embed and gone video read from the player's document", func(t *testing.T) {
		// The link's own host is never asked: nothing listens there.
		player, hits := streamableServer(t, 0, streamableReadyDoc)
		res, ex, err := claimedRegistry(player).Extract(t.Context(), "http://127.0.0.1:9/e/abcd12", Options{})
		if err != nil || ex.Name() != "streamable" || len(res.Files) != 1 || hits.Load() == 0 {
			t.Fatalf("%v from %v: %v; want the document's file", res, ex, err)
		}
		gone, _ := streamableServer(t, http.StatusNotFound, "")
		if _, ex, err := claimedRegistry(gone).Extract(t.Context(), "http://127.0.0.1:9/abcd12", Options{}); ex == nil ||
			ex.Name() != "streamable" || err == nil || !strings.HasPrefix(err.Error(), "streamable: ") {
			t.Errorf("%v from %v, want streamable's own refusal", err, ex)
		}
	})
	t.Run("4chan pruned thread read from the API", func(t *testing.T) {
		site := newPageRefusingSite(t, nil)
		f := NewFourChan(testClient())
		f.api = site.srv.URL + "/api"
		_, ex, err := claimedRegistry(f).Extract(t.Context(), site.srv.URL+"/a/thread/1", Options{})
		if ex == nil || ex.Name() != "4chan" || err == nil || !strings.HasPrefix(err.Error(), "4chan: fetch thread a/1") {
			t.Errorf("%v from %v, want 4chan's own refusal", err, ex)
		}
		if site.wasAsked("/a/thread/1") {
			t.Error("the thread page was probed")
		}
	})
	t.Run("redgifs removed clip is the API's answer", func(t *testing.T) {
		site := newPageRefusingSite(t, map[string]string{"/v2/auth/temporary": `{"token":"t"}`})
		r := NewRedGifs(testClient())
		r.api = site.srv.URL + "/v2"
		_, ex, err := claimedRegistry(r).Extract(t.Context(), site.srv.URL+"/watch/goneclip", Options{})
		if ex == nil || ex.Name() != "redgifs" || err == nil || !strings.HasPrefix(err.Error(), "redgifs: goneclip") {
			t.Errorf("%v from %v, want redgifs reporting the API's refusal", err, ex)
		}
		if site.wasAsked("/watch/goneclip") {
			t.Error("the watch page was probed")
		}
	})
	t.Run("imgur single-media post and bare album read through the gallery", func(t *testing.T) {
		site := newPageRefusingSite(t, map[string]string{
			"/gallery/pppp111": imgurPostPage,
			"/gallery/qqqq222": imgurPostPage,
		})
		i := NewImgur(testClient())
		i.root = site.srv.URL
		for _, path := range []string{"/a/pppp111", "/qqqq222"} {
			res, ex, err := claimedRegistry(i).Extract(t.Context(), site.srv.URL+path, Options{})
			if err != nil || ex.Name() != "imgur" || len(res.Files) == 0 {
				t.Errorf("%s: %v from %v: %v; want the post read through /gallery/<id>", path, res, ex, err)
			}
			if site.wasAsked(path) {
				t.Errorf("%s: the link's own page was probed", path)
			}
		}
	})
	t.Run("civitai image read through the API", func(t *testing.T) {
		site := newPageRefusingSite(t, map[string]string{
			"/api/v1/images": `{"items":[{"id":12345,"url":"https://media.example.test/a.jpeg"}],"metadata":{}}`,
		})
		c := NewCivitai(testClient())
		c.api = site.srv.URL + "/api/v1"
		res, ex, err := claimedRegistry(c).Extract(t.Context(), site.srv.URL+"/images/12345", Options{})
		if err != nil || ex.Name() != "civitai" || len(res.Files) != 1 {
			t.Fatalf("%v from %v: %v; want the API's image", res, ex, err)
		}
		if site.wasAsked("/images/12345") {
			t.Error("the image page was probed")
		}
	})
	t.Run("archive.org missing and darkened items in its own words", func(t *testing.T) {
		site := newPageRefusingSite(t, map[string]string{
			"/metadata/typo-id": `{}`,
			"/metadata/dark-id": `{"is_dark":true}`,
		})
		a := NewArchiveOrg(nil, testClient())
		a.metadata = site.srv.URL + "/metadata/"
		for path, want := range map[string]string{
			"/download/typo-id": "there is no item called",
			"/details/dark-id":  "has been darkened",
		} {
			_, ex, err := claimedRegistry(a).Extract(t.Context(), site.srv.URL+path, Options{})
			if ex == nil || ex.Name() != "archive.org" || err == nil || !strings.Contains(err.Error(), want) {
				t.Errorf("%s: %v from %v, want archive.org saying %q", path, err, ex, want)
			}
		}
	})
	t.Run("foolfuuka missing thread in the archive's words", func(t *testing.T) {
		site := newPageRefusingSite(t, map[string]string{"/_/api/chan/thread/": `{"error":"Thread not found."}`})
		_, ex, err := claimedRegistry(NewFoolFuuka(testClient())).Extract(t.Context(), site.srv.URL+"/a/thread/1/", Options{})
		if ex == nil || ex.Name() != "foolfuuka" || err == nil || !strings.Contains(err.Error(), "Thread not found") {
			t.Errorf("%v from %v, want the archive's own reason", err, ex)
		}
		if site.wasAsked("/a/thread/1/") {
			t.Error("the challenged thread page was probed")
		}
	})
	t.Run("kemono post read through the API", func(t *testing.T) {
		site := newPageRefusingSite(t, map[string]string{
			"/api/v1/patreon/user/123/profile":  `{"id":"123","service":"patreon","name":"Someone","post_count":1}`,
			"/api/v1/patreon/user/123/post/456": `{"post":{"id":"456","title":"A Post","file":{"name":"a.jpg","path":"/aa/bb/aabb.jpg"},"attachments":[]}}`,
		})
		res, ex, err := claimedRegistry(NewKemono(testClient())).Extract(t.Context(), site.srv.URL+"/patreon/user/123/post/456", Options{})
		if err != nil || ex.Name() != "kemono" || len(res.Files) != 1 {
			t.Fatalf("%v from %v: %v; want the post's file", res, ex, err)
		}
	})
	t.Run("peertube video read through the API", func(t *testing.T) {
		site := newPageRefusingSite(t, map[string]string{"/api/v1/videos/abcdefghijk": peerTubeBothListsVideo})
		res, ex, err := claimedRegistry(NewPeerTube(testClient())).Extract(t.Context(), site.srv.URL+"/w/abcdefghijk", Options{})
		if err != nil || ex.Name() != "peertube" || len(res.Files) == 0 {
			t.Fatalf("%v from %v: %v; want the video's file", res, ex, err)
		}
		if site.wasAsked("/w/abcdefghijk") {
			t.Error("the watch page was probed")
		}
	})
}

// TestKnownBoardsReadAsWhatTheyRun: a known board's link is read as the
// software it runs, with nothing guessed: no page fetched, no family
// asked about, and a Philomena board's "Everything" filter the one known
// for it rather than looked up.
func TestKnownBoardsReadAsWhatTheyRun(t *testing.T) {
	site := newPageRefusingSite(t, map[string]string{
		"/api/v1/json/filters/system": `{"filters":[{"id":1,"name":"Everything"}]}`,
		"/api/v1/json/search/images":  `{"images":[{"id":7,"view_url":"https://media.example.test/7.png","format":"png"}],"total":1}`,
	})
	b := NewBooru(testClient())
	b.boards = map[string]booruBoard{"127.0.0.1": {api: apiPhilomena, filterID: "56027"}}
	res, ex, err := claimedRegistry(b).Extract(t.Context(), site.srv.URL+"/search?q=a_tag", Options{})
	if err != nil || ex.Name() != "booru" || len(res.Files) != 1 {
		t.Fatalf("%v from %v: %v; want the board's search", res, ex, err)
	}
	var filters []string
	site.mu.Lock()
	for _, asked := range site.asked {
		if u, err := url.Parse(asked); err == nil && u.Path == "/api/v1/json/search/images" {
			filters = append(filters, u.Query().Get("filter_id"))
		}
	}
	site.mu.Unlock()
	if len(filters) == 0 || filters[0] != "56027" {
		t.Errorf("searched under filters %v, want the board's known one", filters)
	}
	for _, path := range []string{"/search", "/api/v1/json/filters/system"} {
		if site.wasAsked(path) {
			t.Errorf("%s was asked, though the board is known", path)
		}
	}
	for host, want := range map[string]booruBoard{
		"derpibooru.org": {api: apiPhilomena, filterID: "56027"},
		"ponybooru.org":  {api: apiPhilomena, filterID: "3"},
		"furbooru.org":   {api: apiPhilomena, filterID: "2"},
		"e621.net":       {api: apiE621},
		"a.booru.org":    {api: apiGelbooru01},
	} {
		if got, ok := NewBooru(nil).board(mustParse(t, "https://"+host+"/")); !ok || got != want {
			t.Errorf("%s: %+v, %v; want %+v", host, got, ok, want)
		}
	}
	// Every board the host claims names is a known one.
	for host := range familyInstalls[config.FamilyBooru] {
		if _, ok := booruBoards[host]; !ok {
			t.Errorf("%s is claimed as a board but not known as one", host)
		}
	}
}

// TestBooruGuessIsAskedOncePerOrigin: /post/<n> is szurubooru's post, and
// every forum's and aggregator's post too. A site that is no board is
// asked about it once, not once per link a page of its posts carries.
func TestBooruGuessIsAskedOncePerOrigin(t *testing.T) {
	var mu sync.Mutex
	guesses := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/posts/" {
			mu.Lock()
			guesses++
			mu.Unlock()
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	b := NewBooru(testClient())
	for n := range 3 {
		if b.Recognise(t.Context(), htmlPage(t, fmt.Sprintf("%s/post/%d", srv.URL, n+1), "<html><body>A forum post</body></html>")) {
			t.Fatal("a forum's post was taken for a board's")
		}
	}
	if guesses != 1 {
		t.Errorf("the board's API was guessed at %d times, want once for the origin", guesses)
	}
}

// TestHostClaimedFallbackRowsSayWhy: a page on a player's or a gallery's
// own host that nothing recognised — removed, private, expired — or that
// answered 404 is the site's own, and its extractor says why: never the
// generic fallback, which would read the page's related links as the item,
// nor the bare status of the probe. A player's 404 is the video gone, in
// its row's words.
func TestHostClaimedFallbackRowsSayWhy(t *testing.T) {
	rows := []struct {
		name, path, related string
		// player rows say a 404 page is the video gone.
		player bool
	}{
		{"pornhub", "/view_video.php?viewkey=ph0123abcd", "/view_video.php?viewkey=phRelated%02d", true},
		{"xhamster", "/videos/a-clip-abc123", "/videos/other-clip-Related%02d", true},
		{"tnaflix", "/amateur/a-clip/video12345", "/amateur/Related/video9999%02d", true},
		{"pornone", "/amateur/a-clip/1234567", "/amateur/Related/99999%02d", true},
		{"eporner", "/video-AbCd1234/a-clip/", "/video-Related%02d/other/", true},
		{"doodstream", "/e/abcdef123456", "/e/Related%02dxx", true},
		{"mixdrop", "/e/abcdef123", "/e/Related%02dxx", true},
		{"streamtape", "/v/abcdef123", "/v/Related%02dxx", true},
		{"lulustream", "/e/abcdef123456", "/e/Related%02dxxx", true},
		{"vids.st", "/v/12345", "/v/99%02d?Related", true},
		{"sendvid", "/embed/abc123", "/embed/Related%02d", true},
		{"kvs", "/videos/12345/a-clip/", "/videos/999%02d/Related/", false},
		{"kvs", "/a-clip.html", "/related-%02d.html?Related", false},
		{"chevereto", "/image/AbCd1", "/image/Related%02d", false},
		{"bandcamp", "/album/an-album", "/album/related-%02d?Related", false},
		{"rajce", "/user/album/an-album", "/user/album/Related%02d", false},
		{"erome", "/a/AbCd123", "/a/Related%02d", false},
		{"abclisten", "/listen/programs/a-show/an-episode/1234567", "/listen/programs/Related/x/%02d", false},
		{"npr", "/2024/01/02/1234567/a-story", "/2024/01/02/999%02d/Related", false},
	}
	for _, status := range []int{http.StatusOK, http.StatusNotFound} {
		for _, row := range rows {
			t.Run(fmt.Sprintf("%s %s %d", row.name, row.path, status), func(t *testing.T) {
				var (
					mu    sync.Mutex
					asked []string
				)
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					mu.Lock()
					asked = append(asked, r.URL.RequestURI())
					mu.Unlock()
					w.Header().Set(httpx.HeaderContentType, "text/html")
					w.WriteHeader(status)
					fmt.Fprint(w, `<html><head><title>Not available</title></head><body><p>This was removed.</p>`)
					for i := range 5 {
						fmt.Fprintf(w, `<a href="`+row.related+`">related</a>`, i)
					}
					fmt.Fprint(w, `</body></html>`)
				}))
				t.Cleanup(srv.Close)
				reg := NewRegistry(&config.Config{}, testClient())
				aliasClaims(reg, row.name, "127.0.0.1")
				aliasHosts(reg, row.name, "127.0.0.1")
				res, ex, err := reg.Extract(t.Context(), srv.URL+row.path, Options{})
				// A KVS install is named in its errors by its host's first
				// label, as its jobs are labelled.
				prefix := row.name
				if row.name == "kvs" {
					prefix = hostLabel("127.0.0.1")
				}
				if err == nil || ex == nil || ex.Name() != row.name || !strings.HasPrefix(err.Error(), prefix+": ") {
					t.Fatalf("%v from %v: %v; want %s's own error", res, ex, err, row.name)
				}
				if status == http.StatusNotFound && row.player && !strings.Contains(err.Error(), "is not there") {
					t.Errorf("%v, want the video reported gone", err)
				}
				mu.Lock()
				defer mu.Unlock()
				for _, path := range asked {
					if strings.Contains(path, "elated") {
						t.Fatalf("a related link was read: %v", asked)
					}
				}
			})
		}
	}
}

// TestOKRuBlockedVideoIsTheEmbedsAnswer: an ok.ru watch page on its host
// with no player — the author blocked, the video gone — is read through
// its embed, whose status the site words, and its related video is left
// alone.
func TestOKRuBlockedVideoIsTheEmbedsAnswer(t *testing.T) {
	var mu sync.Mutex
	var asked []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		asked = append(asked, r.URL.Path)
		mu.Unlock()
		w.Header().Set(httpx.HeaderContentType, "text/html")
		if strings.HasPrefix(r.URL.Path, "/videoembed/") {
			fmt.Fprint(w, `<html><body><div data-module="OKVideo" data-options="{&quot;flashvars&quot;:{&quot;metadata&quot;:&quot;{\&quot;movie\&quot;:{\&quot;status\&quot;:\&quot;BLOCKED\&quot;,\&quot;statusText\&quot;:\&quot;The author is blocked\&quot;}}&quot;}}"></div></body></html>`)
			return
		}
		fmt.Fprint(w, `<html><body><p>The author is blocked.</p><a href="/video/987654321">related</a></body></html>`)
	}))
	t.Cleanup(srv.Close)
	site := okruPlayer
	site.page = func(u *url.URL) (string, error) { return srv.URL + "/videoembed/" + okruVideoID(u), nil }
	_, ex, err := claimedRegistry(NewPlayer(testClient(), &site)).Extract(t.Context(), srv.URL+"/video/123456789", Options{})
	if ex == nil || ex.Name() != "ok.ru" || err == nil || !strings.HasPrefix(err.Error(), "ok.ru: ") {
		t.Fatalf("%v from %v, want ok.ru's own refusal", err, ex)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, path := range asked {
		if strings.Contains(path, "987654321") {
			t.Fatalf("the related video was read: %v", asked)
		}
	}
}

// TestDoodPagesOnItsHosts: on the host's own domains, the host's
// unbranded page for a removed video is reported as removed, and a body
// with no markup, whatever its sentence, is a refusal of the address; a
// code in capitals is the host's form as one in lower case is.
func TestDoodPagesOnItsHosts(t *testing.T) {
	for body, want := range map[string]string{
		`<!doctype html><html><head><title>Video not found</title></head><body><h1>Not Found</h1></body></html>`: "removed",
		"This address may not watch this video right now.":                                                       "refused the player",
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set(httpx.HeaderContentType, "text/html")
			fmt.Fprint(w, body)
		}))
		reg := NewRegistry(&config.Config{}, testClient())
		aliasClaims(reg, "doodstream", "127.0.0.1")
		_, ex, err := reg.Extract(t.Context(), srv.URL+"/e/AbCdEf123456", Options{})
		srv.Close()
		if ex == nil || ex.Name() != "doodstream" || err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: %v from %v, want doodstream saying %q", body, err, ex, want)
		}
		if want == "refused the player" {
			if _, refused := errors.AsType[*RefusedError](err); !refused {
				t.Errorf("%v is not a refusal of the address", err)
			}
		}
	}
	for _, raw := range []string{"https://dood.example.test/e/AbCdEf123456", "https://dood.example.test/d/ABCDEF123456"} {
		if !doodShape(mustParse(t, raw)) {
			t.Errorf("%s is not of the host's form", raw)
		}
	}
}

// TestFediverseLinkThatRedirectsIsStillTheAccount: an account or post
// link whose page sends a logged-out visitor to the sign-in page, a
// remote-interaction page or another path is still the account or post,
// read through the API; it is never saved as the page.
func TestFediverseLinkThatRedirectsIsStillTheAccount(t *testing.T) {
	srv := fediServer(t, "mastodon", func(w http.ResponseWriter, r *http.Request) bool {
		switch r.URL.Path {
		case "/@alice", "/@alice@remote.example.test":
			http.Redirect(w, r, "/redirect/accounts/1", http.StatusFound)
		case "/redirect/accounts/1", "/login":
			w.Header().Set(httpx.HeaderContentType, "text/html")
			fmt.Fprint(w, `<html><head><title>Leaving</title></head><body>Sign in to continue</body></html>`)
		case "/api/v1/accounts/lookup":
			writeJSON(w, `{"id":"42","username":"alice","acct":"alice"}`)
		case "/api/v1/accounts/42/statuses":
			writeJSON(w, mastodonPage(r.URL.Query().Get("max_id")))
		default:
			return false
		}
		return true
	})
	for _, path := range []string{"/@alice", "/@alice@remote.example.test"} {
		res, ex, err := probeRegistry().Extract(t.Context(), srv.URL+path, Options{})
		if err != nil || ex.Name() != "fediverse" || len(res.Files) == 0 {
			t.Errorf("%s: %v from %v: %v; want the account read through the API", path, res, ex, err)
		}
	}
}

// TestPeerTubeLinkThatRedirectsIsStillTheVideo: a video link whose page
// sends a logged-out visitor to the sign-in page is still the video.
func TestPeerTubeLinkThatRedirectsIsStillTheVideo(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/w/abcdefghijk", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/login", http.StatusFound)
	})
	mux.HandleFunc("/login", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set(httpx.HeaderContentType, "text/html")
		fmt.Fprint(w, `<html><head><title>Login</title></head></html>`)
	})
	mux.HandleFunc("/api/v1/config", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, `{"serverVersion":"7.0.0"}`)
	})
	mux.HandleFunc("/api/v1/videos/abcdefghijk", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, peerTubeBothListsVideo)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	res, ex, err := probeRegistry().Extract(t.Context(), srv.URL+"/w/abcdefghijk", Options{})
	if err != nil || ex.Name() != "peertube" || len(res.Files) == 0 {
		t.Fatalf("%v from %v: %v; want the video read through the API", res, ex, err)
	}
}

// TestKVSMemberProfileOnAnInstallIsItsPublicVideos: a member's profile on
// a known install, whose page shows none of the platform's markers, is the
// member's public videos, walked as a listing.
func TestKVSMemberProfileOnAnInstallIsItsPublicVideos(t *testing.T) {
	pages := map[string]sitePage{}
	srv, _ := fakeSite(t, pages)
	onSite := func(doc string) sitePage {
		return sitePage{kind: "text/html", body: strings.ReplaceAll(doc, "https://tube.example.test", srv.URL)}
	}
	pages["/members/4242/"] = onSite(`<html><head><title>Someone</title></head><body><p>A bio.</p></body></html>`)
	pages["/members/4242/public_videos/"] = onSite(kvsMemberListing)
	pages["/members/4242/public_videos/2/"] = onSite(kvsMemberLastPage)
	for _, video := range []string{"11/first-clip", "12/second-clip", "13/third-clip", "14/fourth-clip"} {
		pages["/videos/"+video+"/"] = onSite(kvsPage())
	}
	reg := NewRegistry(&config.Config{}, testClient())
	aliasClaims(reg, "kvs", "127.0.0.1")
	res, ex, err := reg.Extract(t.Context(), srv.URL+"/members/4242/", Options{})
	if err != nil || ex.Name() != "kvs" || len(res.Files) != 4 {
		t.Fatalf("%v from %v: %v; want the member's four public videos", res, ex, err)
	}
}

// TestPBSVideoWithATrailingSegment: /video/<slug>/<extra>/ is the video
// the slug names, read as /video/<slug>/ is, but not a form to follow.
func TestPBSVideoWithATrailingSegment(t *testing.T) {
	u := mustParse(t, "https://www.pbs.org/video/an-episode-abc1/extra/")
	if target := pbsTargetOf(u); target == nil || target.slug != "an-episode-abc1" {
		t.Errorf("target %+v, want the slug", target)
	}
	if pbsShape(u) {
		t.Error("a video's sub-page is followed as a link")
	}
	if ex := testRegistry(t, nil).hostClaim(u, claimFallback); ex == nil || ex.Name() != "pbs" {
		t.Errorf("claimed by %v, want pbs", ex)
	}
}

// TestRajceStorageVideoIsTheHostClaims: a pasted video on the storage
// hosts, which names no file type, is the file, fetched with the site as
// its referer and named with its MP4 extension.
func TestRajceStorageVideoIsTheHostClaims(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(httpx.HeaderContentType, "video/mp4")
		w.WriteHeader(http.StatusForbidden)
	}))
	t.Cleanup(srv.Close)
	reg := NewRegistry(&config.Config{}, testClient())
	aliasClaims(reg, "rajce", "127.0.0.1")
	res, ex, err := reg.Extract(t.Context(), srv.URL+"/storage/video/123", Options{})
	if err != nil || ex.Name() != "rajce" || len(res.Files) != 1 {
		t.Fatalf("%v from %v: %v; want the one file", res, ex, err)
	}
	file := res.Files[0]
	if res.Title != "123.mp4" || file.Name != "123.mp4" || file.URL != srv.URL+"/storage/video/123" ||
		file.Headers[httpx.HeaderReferer] != rajceRoot+"/" {
		t.Errorf("title %q, file %+v; want 123.mp4 with the unchanged link and site as referer", res.Title, file)
	}
}

// TestBroadcasterAndAPILinksAreClaimedFirst: links that a broadcaster or
// an API-driven site resolves from the link alone — embeds, document API
// links, programmes — are its own before any probe, so a page with no
// markers, an error page or a challenge changes nothing.
func TestBroadcasterAndAPILinksAreClaimedFirst(t *testing.T) {
	reg := NewRegistry(&config.Config{}, testClient())
	for raw, want := range map[string]string{
		"https://auvio.rtbf.be/embed/media/123":                         "rtbf",
		"https://www.arte.tv/embeds/de/123456-000-A":                    "arte",
		"https://www.arte.tv/de/videos/123456-000-A/a-programme/":       "arte",
		"https://zdf-prod-futura.zdf.de/mediathekV2/document/a-doc-100": "zdf",
		"https://www.svtplay.se/video/a1b2c3d/a-programme":              "svtplay",
		"https://tv.nrk.no/program/ABCD12345678":                        "nrk",
		"https://www.dr.dk/drtv/se/a-programme_12345":                   "drtv",
		"https://www.bbc.co.uk/sounds/play/p0abcd12":                    "bbcsounds",
		"https://www.bitchute.com/embed/AbCd123/":                       "bitchute",
		"https://vimeo.com/123456":                                      "vimeo",
		"https://imgur.com/a/AbCd1":                                     "imgur",
		"https://imgur.com/AbCd1":                                       "imgur",
		"https://archive.org/download/an-item":                          "archive.org",
		"https://desuarchive.org/a/thread/1/":                           "foolfuuka",
		"https://derpibooru.org/search?q=a_tag":                         "booru",
		"https://www.nicovideo.jp/user/1/mylist/2":                      "niconico",
	} {
		ex, page := reg.identify(t.Context(), mustParse(t, raw))
		if ex.Name() != want || page != nil {
			t.Errorf("%s: %s, probed %v; want %s with no probe", raw, ex.Name(), page != nil, want)
		}
	}
}
