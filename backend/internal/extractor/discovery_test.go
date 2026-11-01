// SPDX-License-Identifier: MIT

package extractor

import (
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

// TestLinkScannerFollowsShortAndCanonicalLinks: short links and canonical
// single-item links lead to a supported item although no Shape names them,
// and a thread of them is followed (linkForms). The same paths on any other
// host are navigation, never a probe.
func TestLinkScannerFollowsShortAndCanonicalLinks(t *testing.T) {
	reg := testRegistry(t, nil)
	for raw, want := range map[string]string{
		"https://youtu.be/AAAAAAAAAAA":                    "youtube",
		"https://dai.ly/x8abcde":                          "dailymotion",
		"https://b23.tv/AbCd12":                           "bilibili",
		"https://on.soundcloud.com/AbCdEf":                "soundcloud",
		"https://soundcloud.com/an-artist/a-track":        "soundcloud",
		"https://soundcloud.com/an-artist/a-track/s-AbCd": "soundcloud",
		"https://www.mixcloud.com/a-user/a-show/":         "mixcloud",
		"https://streamable.com/abcd12":                   "streamable",
		"https://sendvid.com/abcd1234":                    "sendvid",
		"https://lulustream.com/abcd1234wxyz":             "lulustream",
		"https://vidmoly.to/abcdefghijkl":                 "vidmoly",
		"https://imgur.com/AbCdEfG":                       "imgur",
		"https://ibb.co/AbCdEf1":                          "chevereto",
		"https://www.sexvid.xxx/a-synthetic-clip.html":    "kvs",
		"https://www.npostart.nl/een-serie/XX_1":          "npo",
		"https://www.vrtmax.be/a-z/a-programme/":          "vrtmax",
	} {
		u := mustParse(t, raw)
		if ex, ok := reg.Known(u); !ok || ex.Name() != want {
			t.Errorf("Known(%s) = %s, %v; want %s", raw, ex.Name(), ok, want)
		}
		// A family install is labelled by install (Registry.JobLabel).
		label := map[string]string{"kvs": "sexvid", "chevereto": "imgbb"}[want]
		if label == "" {
			label = want
		}
		if got := reg.Label(u); got != label {
			t.Errorf("Label(%s) = %s, want %s", raw, got, label)
		}
		elsewhere := mustParse(t, raw)
		elsewhere.Host = "blog.example.test"
		if ex, ok := reg.Known(elsewhere); ok {
			t.Errorf("Known(%s) = %s; a bare path on another host is navigation", elsewhere, ex.Name())
		}
	}
	// The media host beside a canonical one serves files, not item pages.
	if ex, ok := reg.Known(mustParse(t, "https://i.imgur.com/AbCdEfG")); ok {
		t.Errorf("a media host's link went to %s", ex.Name())
	}
}

// TestHostLinksOnTheirOwnSiteAreNavigation: on the host's own pages a bare
// /<id> or /<user>/<name> is its account, login, genre or settings page as
// often as an item, so links named by their host are followed only from
// another site; and a file under such a path is never an item.
func TestHostLinksOnTheirOwnSiteAreNavigation(t *testing.T) {
	reg := testRegistry(t, nil)
	thread := mustParse(t, "https://forum.example.test/thread/1")
	for _, raw := range []string{
		"https://sendvid.com/account",
		"https://vidmoly.to/register",
		"https://soundcloud.com/settings/privacy",
		"https://www.mixcloud.com/genres/pop/",
	} {
		u := mustParse(t, raw)
		home := mustParse(t, raw)
		home.Path = "/"
		if ex, ok := reg.Follows(u, home); ok {
			t.Errorf("Follows(%s) from its own site = %s; want navigation", raw, ex.Name())
		}
		if _, ok := reg.Follows(u, thread); !ok {
			t.Errorf("Follows(%s) from another site = false; want followed", raw)
		}
	}
	for _, raw := range []string{"https://www.mixcloud.com/about/logo.png", "https://soundcloud.com/a-user/cover.jpg"} {
		if ex, ok := reg.Follows(mustParse(t, raw), thread); ok {
			t.Errorf("Follows(%s) = %s; a file is not an item", raw, ex.Name())
		}
	}
}

// TestPathFormsFollowedOnAnyHost: forms whose path alone says what they
// are, which were followed by host before, are Shapes now.
func TestPathFormsFollowedOnAnyHost(t *testing.T) {
	reg := testRegistry(t, nil)
	for path, want := range map[string]string{
		// A story filed under a section.
		"/sections/a-section/2026/01/02/1234567890/a-story": "npr",
		// The old embed frame, everything in the fragment.
		"/embed#!abcdefgh!key": "mega",
		// A folder named by its content id.
		"/d/0a1b2c3d-4e5f-6789-abcd-ef0123456789": "gofile",
	} {
		for _, host := range []string{"one.example.test", "another.example.test"} {
			u := mustParse(t, "https://"+host+path)
			found := false
			for _, e := range reg.extractors {
				if e.Shape(u) && e.Name() == want {
					found = true
				}
			}
			if !found {
				t.Errorf("%s: no %s Shape", u, want)
			}
		}
	}
	g := NewGofile(&config.Config{}, nil)
	shell := `<html><head><title>Gofile</title><script src="` + gofileScriptPath + `"></script></head></html>`
	expectRecognised(t, g, htmlPage(t, "https://share.example.test/d/0a1b2c3d-4e5f-6789-abcd-ef0123456789", shell), true)
}

// TestDiscoveryFormsNeverDecideRecognition: the host lists in linkforms.go,
// siteHosts among them,
// choose what the link scanner follows, never which extractor reads a page.
// No Shape or Recognise reaches them, and nothing in that file decides
// recognition.
func TestDiscoveryFormsNeverDecideRecognition(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	discovery := []string{"linkForms", "formClaims", "hostLinks", "exactHostLinks", "Known", "Follows", "claimant", "claims",
		"siteHosts", "onSiteHosts", "offHostForms", "hostAliases"}
	fset := token.NewFileSet()
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, file, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil || !recognition(fn) {
				continue
			}
			if file == "linkforms.go" {
				t.Errorf("%s: recognition declared among the discovery forms", fn.Name.Name)
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				var name string
				switch n := n.(type) {
				case *ast.SelectorExpr:
					name = n.Sel.Name
				case *ast.Ident:
					name = n.Name
				}
				if slices.Contains(discovery, name) {
					t.Errorf("%s: %s reads %s", fset.Position(n.Pos()), fn.Name.Name, name)
				}
				return true
			})
		}
	}
}

// TestPlatformFormsOnTheirOwnSiteAreNavigation: a forum's /posts/<n>,
// /members/<n>/… and the like are its own navigation. A harvest of a page
// made of them asks for the page alone and says the page may be a login,
// rather than probing each link and guessing platform APIs behind it; the
// same links on another site's page are still followed.
func TestPlatformFormsOnTheirOwnSiteAreNavigation(t *testing.T) {
	var (
		mu    sync.Mutex
		asked []string
	)
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		asked = append(asked, r.URL.Path)
		mu.Unlock()
		w.Header().Set(httpx.HeaderContentType, "text/html")
		fmt.Fprintf(w, `<html><title>A thread</title><body>
<a href="%[1]s/posts/42">post</a>
<a href="%[1]s/b/thread/42/">thread</a>
<a href="%[1]s/members/42/videos/">member</a>
<a href="%[1]s/sample/user/creator">creator</a>
</body></html>`, srv.URL)
	}))
	defer srv.Close()
	client := testClient()
	defer client.CloseIdleConnections()
	reg := NewRegistry(&config.Config{}, client)
	_, _, err := reg.Extract(t.Context(), "links:"+srv.URL+"/threads/a-thread", Options{})
	if err == nil || !strings.Contains(err.Error(), "showing a login") {
		t.Fatalf("err = %v, want the login hint", err)
	}
	if want := []string{"/threads/a-thread"}; !slices.Equal(asked, want) {
		t.Fatalf("asked %v, want %v", asked, want)
	}

	from := mustParse(t, "https://forum.example.test/threads/a-thread")
	for _, path := range []string{"/posts/42", "/b/thread/42/", "/members/42/videos/", "/sample/user/creator"} {
		if ex, ok := reg.Follows(mustParse(t, "https://www.forum.example.test"+path), from); ok {
			t.Errorf("%s on its own site followed as %s", path, ex.Name())
		}
		if _, ok := reg.Follows(mustParse(t, "https://board.example.test"+path), from); !ok {
			t.Errorf("%s on another site not followed", path)
		}
	}
}

// TestFailedProbeOfAnUnclaimedPageIsAnError: a submitted link no form
// claims, refused with an error page, is answered with that refusal rather
// than queued as a file of the page. A refusal served as media is still the
// fallback's, whose download asks as a file.
func TestFailedProbeOfAnUnclaimedPageIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "-media") {
			w.Header().Set(httpx.HeaderContentType, "video/mp4")
			w.WriteHeader(http.StatusForbidden)
			return
		}
		http.Error(w, "refused", http.StatusForbidden)
	}))
	defer srv.Close()
	client := testClient()
	defer client.CloseIdleConnections()
	reg := NewRegistry(&config.Config{}, client)
	for _, path := range []string{"/an-artist/a-track", "/AAAAAAAAAAA"} {
		res, ex, err := reg.Extract(t.Context(), srv.URL+path, Options{})
		status, ok := errors.AsType[*httpx.StatusError](err)
		if !ok || status.Code != http.StatusForbidden {
			t.Errorf("%s: %s gave %+v, %v; want the 403", path, ex.Name(), res, err)
		}
	}
	res, _, err := reg.Extract(t.Context(), srv.URL+"/a-clip-media", Options{})
	if err != nil || len(res.Files) != 1 {
		t.Fatalf("a media refusal: %+v, %v; want the file queued", res, err)
	}
}

// TestLinksWhosePagesNobodyRecognisesAreReported: a page whose supported
// links all lead to pages no extractor recognises says so, rather than that
// it links nothing supported.
func TestLinksWhosePagesNobodyRecognisesAreReported(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(httpx.HeaderContentType, "text/html")
		if r.URL.Path == "/thread" {
			fmt.Fprintf(w, `<html><title>A thread</title><body>
<a href="%[1]s/d/AbCd1">one</a> <a href="%[1]s/d/AbCd2">two</a> <a href="%[1]s/d/AbCd3">three</a>
</body></html>`, srv.URL)
			return
		}
		fmt.Fprint(w, `<html><title>An app</title><div id="app"></div></html>`)
	}))
	defer srv.Close()
	client := testClient()
	defer client.CloseIdleConnections()
	reg := NewRegistry(&config.Config{}, client)
	aliasHosts(reg, "gofile", "127.0.0.1")
	for _, raw := range []string{"links:" + srv.URL + "/thread", srv.URL + "/thread"} {
		_, _, err := reg.Extract(t.Context(), raw, Options{})
		if err == nil || !strings.Contains(err.Error(), "3 of the links") || !strings.Contains(err.Error(), "no extractor recognised their pages") {
			t.Errorf("%s: %v", raw, err)
		}
	}
}

// TestEmptyPageNamesOnlyTheOneClaimant: a page with nothing on it names the
// extractor its form points to only when one form claims it; of several,
// the first would as often be the wrong site's.
func TestEmptyPageNamesOnlyTheOneClaimant(t *testing.T) {
	reg := testRegistry(t, nil)
	d := reg.fallback.(*Direct)
	shared := mustParse(t, "https://album.example.test/a/AbCdEfG")
	if _, one := reg.claimant(shared); one {
		t.Fatal("fixture: the album form is meant to be claimed by several")
	}
	if err := d.emptyPage(shared); !strings.HasPrefix(err.Error(), "album.example.test: ") {
		t.Errorf("shared form: %v", err)
	}
	only := mustParse(t, "https://share.example.test/folder/BBBBBBBB#key")
	if err := d.emptyPage(only); !strings.HasPrefix(err.Error(), "mega: ") {
		t.Errorf("one claimant: %v", err)
	}
}

// TestFamilyJobsAreLabelledByInstall: a job a platform family read is
// labelled by its install, as each was while installs were listed; any
// other extractor labels its jobs with its own name.
func TestFamilyJobsAreLabelledByInstall(t *testing.T) {
	reg := testRegistry(t, nil)
	for _, tc := range []struct {
		ex   Extractor
		link string
		want string
	}{
		{NewKVS(nil), "https://www.tube-one.example.test/videos/1/a-clip/", "tube-one"},
		{NewPeerTube(nil), "https://tube.example.org/w/abc", "example"},
		{NewPeerTube(nil), "https://videos.an-instance.example/w/abc", "an-instance"},
		{NewPeerTube(nil), "https://peertube.tv/w/abc", "peertube"},
		{NewMega(nil), "https://share.example.test/file/AAAAAAAA#key", "mega"},
	} {
		if got := reg.JobLabel(tc.ex, mustParse(t, tc.link)); got != tc.want {
			t.Errorf("%s by %s: %q, want %q", tc.link, tc.ex.Name(), got, tc.want)
		}
	}
}

// TestFileRouteProbeAsksForHeadersOnly: an API's download route names no
// file type, and is probed as a file link is: a HEAD whose media answer is
// taken, never a GET starting the download.
func TestFileRouteProbeAsksForHeadersOnly(t *testing.T) {
	var (
		mu      sync.Mutex
		methods []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		methods = append(methods, r.Method)
		mu.Unlock()
		w.Header().Set(httpx.HeaderContentType, "application/octet-stream")
	}))
	defer srv.Close()
	client := testClient()
	defer client.CloseIdleConnections()
	p := probePage(t.Context(), client, mustParse(t, srv.URL+"/api/file/AbCd1234"))
	if p.Status != http.StatusOK || !slices.Equal(methods, []string{http.MethodHead}) {
		t.Fatalf("status %d, methods %v; want one HEAD", p.Status, methods)
	}
}

// navigationPaths are paths every kind of site uses for its own pages, and
// which some single-site extractor's Shape also names.
var navigationPaths = []string{
	"/v/abc123", "/e/abcdefghijkl", "/f/abcdefgh", "/d/AbCd1", "/a/abc12", "/c/a-channel",
	"/u/abcdefgh", "/l/abcdefgh", "/s/a-share", "/file/abc123", "/video/1234567890",
	"/watch/a-clip.html", "/view_video.php?viewkey=abc123", "/embed/abc123",
	"/video/x8abcde", "/videos/123", "/gallery/abc123", "/album/abc123", "/u/a-user",
	"/r/a-board", "/user/a-user", "/tags/a-tag", "/categories/a-category", "/music",
	"/2026/01/02/1234567890/a-story", "/programs/a-show", "/sounds/play/abc12345",
}

// TestSingleSiteFormsAreFollowedOnlyOnTheirHosts: a single-site extractor's
// Shape names paths other sites use for their own pages. Found on an
// unrelated site, its own page or another's, they are never followed as
// that extractor's; a platform's forms still may be, from another site.
func TestSingleSiteFormsAreFollowedOnlyOnTheirHosts(t *testing.T) {
	reg := testRegistry(t, nil)
	home := mustParse(t, "https://blog.example.test/")
	elsewhere := mustParse(t, "https://forum.example.test/thread/1")
	for _, path := range navigationPaths {
		u := mustParse(t, "https://blog.example.test"+path)
		for _, from := range []*url.URL{home, elsewhere} {
			if ex, ok := reg.Follows(u, from); ok {
				if _, single := siteHosts[ex.Name()]; single {
					t.Errorf("Follows(%s) from %s = %s; a single site's form on another host", u, from.Host, ex.Name())
				}
				if from == home {
					t.Errorf("Follows(%s) from its own site = %s; want navigation", u, ex.Name())
				}
			}
		}
	}
}

// TestSiteHostLinksAreFollowedFromOtherSites: on a single site's own hosts,
// a link found on another site's page is followed whatever its path, as
// when extractors were chosen by host — a band's front page, a profile, a
// programme — unless it names a file. Found on the site's own page, only its
// Shape is.
func TestSiteHostLinksAreFollowedFromOtherSites(t *testing.T) {
	reg := testRegistry(t, nil)
	thread := mustParse(t, "https://forum.example.test/thread/1")
	for raw, want := range map[string]string{
		"https://a-band.bandcamp.com/":                       "bandcamp",
		"https://www.erome.com/a-user":                       "erome",
		"https://www.svtplay.se/a-programme":                 "svtplay",
		"https://gofile.io/AbCd12":                           "gofile",
		"https://pixeldrain.com/d/AbCd":                      "pixeldrain",
		"https://soundcloud.com/an-artist":                   "soundcloud",
		"https://w.soundcloud.com/player/?url=x":             "soundcloud",
		"https://ok.ru/dk?st.cmd=movieLayer&st.mvId=1234567": "ok.ru",
		"https://a-user.rajce.idnes.cz/an-album/":            "rajce",
		"https://streamable.com/o/abc12":                     "streamable",
		"https://www.dropbox.com/l/scl/AbCd":                 "dropbox",
		"https://bunkr.cr/a/AbCd":                            "bunkr",
		"https://bunkr.co.uk/a/AbCd":                         "bunkr",
		"https://xhamster.desi/videos/a-clip-123":            "xhamster",
		"https://www.bbc.co.uk/sounds/brand/abc123":          "bbcsounds",
	} {
		u := mustParse(t, raw)
		if ex, ok := reg.Follows(u, thread); !ok || ex.Name() != want {
			t.Errorf("Follows(%s) from another site = %s, %v; want %s", raw, ex.Name(), ok, want)
		}
	}
	for _, raw := range []string{
		"https://www.bbc.co.uk/news/a-story",
		"https://soundcloud.com/an-artist/cover.jpg",
		"https://bunkr.example.test.invalid/f/AbCdEfGh",
	} {
		if ex, ok := reg.Follows(mustParse(t, raw), thread); ok {
			t.Errorf("Follows(%s) = %s; not the site's page", raw, ex.Name())
		}
	}
	if ex, ok := reg.Follows(mustParse(t, "https://www.erome.com/a-user"), mustParse(t, "https://www.erome.com/")); ok {
		t.Errorf("a profile link on the site's own page followed as %s", ex.Name())
	}
}

// TestDistinctiveFormsAreFollowedOnAnyHost: a form whose id alone names its
// service is followed on a mirror, custom domain or embed host as well.
func TestDistinctiveFormsAreFollowedOnAnyHost(t *testing.T) {
	reg := testRegistry(t, nil)
	thread := mustParse(t, "https://forum.example.test/thread/1")
	for raw, want := range map[string]string{
		"https://mirror.example.test/d/0a1b2c3d-4e5f-6789-abcd-ef0123456789": "gofile",
		"https://mirror.example.test/file/AbCdEfGh#a-key":                    "mega",
		"https://mirror.example.test/embed#!AbCdEfGh!a-key":                  "mega",
		"https://mirror.example.test/file/0123456789abcdef/a-name.zip":       "keep2share",
		"https://mirror.example.test/videoembed/1234567890":                  "ok.ru",
	} {
		if ex, ok := reg.Follows(mustParse(t, raw), thread); !ok || ex.Name() != want {
			t.Errorf("Follows(%s) = %s, %v; want %s", raw, ex.Name(), ok, want)
		}
	}
}

// TestAPagesOwnNavigationCostsNoProbes: an ordinary page whose navigation
// uses paths single-site extractors also use is read once; none of its own
// links is probed.
func TestAPagesOwnNavigationCostsNoProbes(t *testing.T) {
	var (
		mu    sync.Mutex
		asked []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		asked = append(asked, r.URL.RequestURI())
		mu.Unlock()
		w.Header().Set(httpx.HeaderContentType, "text/html")
		fmt.Fprint(w, `<html><title>A page</title><body><video src="https://media.example.test/clip.mp4"></video>`)
		for _, path := range navigationPaths {
			fmt.Fprintf(w, `<a href="%s">nav</a>`, path)
		}
		fmt.Fprint(w, `</body></html>`)
	}))
	defer srv.Close()
	client := testClient()
	defer client.CloseIdleConnections()
	reg := NewRegistry(&config.Config{}, client)
	res, _, err := reg.Extract(t.Context(), srv.URL+"/index", Options{})
	if err != nil || len(res.Files) != 1 {
		t.Fatalf("result %+v, %v; want the page's clip", res, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if want := []string{"/index"}; !slices.Equal(asked, want) {
		t.Errorf("asked %v, want only %v", asked, want)
	}
}
