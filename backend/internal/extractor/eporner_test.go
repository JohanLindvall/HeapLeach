package extractor

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/httpx"
	"github.com/JohanLindvall/HeapLeach/internal/util"
)

// --------------------------------------------------------------- the fold

// The endpoint refuses the page's hash as it stands, so the fold is the one
// piece of this host that has to be exactly right. The expectations are
// worked out independently rather than recorded from this implementation:
// each group of eight hexadecimal digits is one number re-expressed in base
// 36, and the groups are joined with no padding — which is why a run of
// zeroes collapses to a single digit instead of holding its width.
func TestEpornerPlayerHash(t *testing.T) {
	cases := []struct{ in, want string }{
		{"0123456789abcdef0123456789abcdef", "bd4yv1275p7zbd4yv1275p7z"},
		{"ffffffff00000000ffffffff00000000", "1z141z301z141z30"},
		{"00000000000000000000000000000001", "0001"},
		{"ABCDEF0123456789ABCDEF0123456789", "1bo3u2p9sba151bo3u2p9sba15"},
	}
	for _, c := range cases {
		if got := epornerPlayerHash(c.in); got != c.want {
			t.Errorf("epornerPlayerHash(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	for _, bad := range []string{"", "abc", strings.Repeat("a", 31), strings.Repeat("a", 33), strings.Repeat("z", 32)} {
		if got := epornerPlayerHash(bad); got != "" {
			t.Errorf("epornerPlayerHash(%q) = %q, want it refused", bad, got)
		}
	}
}

// --------------------------------------------------------------- the paths

func TestEpornerVideoID(t *testing.T) {
	cases := []struct {
		path string
		want string
	}{
		{"/video-Aa1Bb2Cc3/some-slug/", "Aa1Bb2Cc3"},
		{"/video-Aa1Bb2Cc3/", "Aa1Bb2Cc3"},
		{"/hd-porn/Aa1Bb2Cc3/Some-Slug/", "Aa1Bb2Cc3"},
		{"/embed/Aa1Bb2Cc3/", "Aa1Bb2Cc3"},
		{"/", ""},
		{"/cat/something/", ""},
		{"/video-/", ""},
		{"/profile/Someone/uploaded-videos/", ""},
	}
	for _, c := range cases {
		u, err := url.Parse("https://www.eporner.com" + c.path)
		if err != nil {
			t.Fatal(err)
		}
		if got := epornerVideoID(util.PathSegments(u)); got != c.want {
			t.Errorf("epornerVideoID(%q) = %q, want %q", c.path, got, c.want)
		}
	}
}

func TestEpornerProfileOf(t *testing.T) {
	cases := []struct {
		path      string
		user, tab string
		ok        bool
	}{
		{"/profile/Someone/uploaded-videos/", "Someone", "uploaded-videos", true},
		{"/profile/Someone/uploaded-videos/4/", "Someone", "uploaded-videos", true},
		{"/profile/Someone/videos/", "Someone", "videos", true},
		{"/profile/Someone/UPLOADED-VIDEOS/", "Someone", "uploaded-videos", true},
		// Picture tabs list nothing this extractor can fetch.
		{"/profile/Someone/uploaded-pics/", "", "", false},
		{"/profile/Someone/pics/", "", "", false},
		{"/profile/Someone/", "", "", false},
		{"/video-Aa1Bb2Cc3/slug/", "", "", false},
	}
	for _, c := range cases {
		u, err := url.Parse("https://www.eporner.com" + c.path)
		if err != nil {
			t.Fatal(err)
		}
		user, tab, ok := epornerProfileOf(util.PathSegments(u))
		if ok != c.ok || user != c.user || tab != c.tab {
			t.Errorf("epornerProfileOf(%q) = (%q,%q,%t), want (%q,%q,%t)",
				c.path, user, tab, ok, c.user, c.tab, c.ok)
		}
	}
}

// The tab lives in the path, so it has to be carried onto every page: a walk
// that drops it lists a different tab at a plausible-looking count.
func TestEpornerProfilePage(t *testing.T) {
	u, err := url.Parse("https://de.eporner.com/profile/Someone/uploaded-videos/")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := epornerProfilePage(u, "Someone", "uploaded-videos", 1),
		"https://de.eporner.com/profile/Someone/uploaded-videos/"; got != want {
		t.Errorf("page 1 = %q, want %q", got, want)
	}
	// The language subdomain survives, and the tab with it.
	if got, want := epornerProfilePage(u, "Someone", "uploaded-videos", 3),
		"https://de.eporner.com/profile/Someone/uploaded-videos/3/"; got != want {
		t.Errorf("page 3 = %q, want %q", got, want)
	}
	if got, want := epornerProfilePage(nil, "Someone", "videos", 2),
		epornerRoot+"/profile/Someone/videos/2/"; got != want {
		t.Errorf("no base = %q, want %q", got, want)
	}
}

func TestEpornerBaseOf(t *testing.T) {
	if got, want := epornerBaseOf("https://fr.eporner.com/video-Aa1/s/"), "https://fr.eporner.com"; got != want {
		t.Errorf("base = %q, want %q", got, want)
	}
	if got, want := epornerBaseOf("::nonsense::"), epornerRoot; got != want {
		t.Errorf("unparsable base = %q, want %q", got, want)
	}
}

// ------------------------------------------------------------- renditions

func TestEpornerCandidates(t *testing.T) {
	// Labels are not always a bare resolution, and one that says only "4K"
	// still carries the number in its filename.
	raw := json.RawMessage(`{
	  "720p HD": {"labelShort":"720p","src":"https://cdn.example.test/a/1-720p.mp4"},
	  "480p":    {"labelShort":"480p","src":"https://cdn.example.test/a/1-480p.mp4"},
	  "4K":      {"labelShort":"4K","src":"https://cdn.example.test/a/1-2160p.mp4"},
	  "empty":   {"labelShort":"1080p","src":""}
	}`)
	got := epornerCandidates(raw)
	if len(got) != 3 {
		t.Fatalf("got %d candidates, want 3 (the one with no src dropped)", len(got))
	}
	best, ok := bestCandidate(got)
	if !ok {
		t.Fatal("no best candidate")
	}
	if best.Quality != 2160 {
		t.Errorf("best quality = %d, want 2160 read from the filename", best.Quality)
	}
	if !strings.HasSuffix(best.URL, "1-2160p.mp4") {
		t.Errorf("best URL = %q, want the 2160p rendition", best.URL)
	}
}

// A video offering no MP4 answers with an empty array rather than an empty
// object, and that must read as "none offered", not as a broken response.
func TestEpornerCandidatesToleratesAnEmptyArray(t *testing.T) {
	for _, raw := range []string{`[]`, `{}`, ``, `null`} {
		if got := epornerCandidates(json.RawMessage(raw)); len(got) != 0 {
			t.Errorf("epornerCandidates(%q) = %d candidates, want none", raw, len(got))
		}
	}
}

// ----------------------------------------------------------- the listing

// A profile page shaped like the site's, holding the trap this scoping
// exists for: the same tiles appear in a sidebar, and unrelated videos
// appear beside them, so taking every /video- link on the page collects
// entries the tab does not own.
const epornerProfileMarkup = `<!DOCTYPE html><html><head><title>Someone's uploaded videos - EPORNER</title></head><body>
<div class="sidebar">
  <div class="pbelin pinsert"><span>Uploaded videos (5) </span></div>
  <div class="plexcontainer">
    <div class="mb"><a href="/video-Sidebar001/first-clip/"><img alt="First Clip"></a></div>
    <div class="mb"><a href="/video-Foreign999/not-mine/"><img alt="Somebody Else"></a></div>
  </div>
  <div class="pbelin pinsert"><span>Watched videos (999) </span></div>
</div>
<div class='pbelin light plister profile-settings-header has-uploads-search'>Uploads</div>
<div class="filler"><div class="ep-cols-container"><div class="streamevents"><div class="streameventsday showAll">
  <div class="mb hdy" data-id="1001">
    <div class="mbcontent"><a href="/video-Sidebar001/first-clip/"><img alt="First Clip"></a></div>
    <div class="mbunder"><p class="mbtit"><a href="/video-Sidebar001/first-clip/">First Clip</a></p></div>
  </div>
  <div class="mb hdy" data-id="1002">
    <div class="mbcontent"><a href="/video-Second002/second-clip/"><img alt="Second Clip"></a></div>
    <div class="mbunder"><p class="mbtit"><a href="/video-Second002/second-clip/">Second Clip</a></p></div>
  </div>
  <div class="mb hdy" data-id="1003">
    <div class="mbcontent"><a href="/video-Third003/third-clip/"></a></div>
    <div class="mbunder"><p class="mbtit"><a href="/video-Third003/third-clip/">Third Clip</a></p></div>
  </div>
</div></div></div></div></body></html>`

func TestEpornerListingIgnoresWhatTheTabDoesNotOwn(t *testing.T) {
	root, err := parseHTML(epornerProfileMarkup)
	if err != nil {
		t.Fatal(err)
	}
	got := epornerListing(root, "https://www.eporner.com/profile/Someone/uploaded-videos/", map[string]bool{})
	want := []epornerVideo{
		{Page: "https://www.eporner.com/video-Sidebar001/first-clip/", Title: "First Clip"},
		{Page: "https://www.eporner.com/video-Second002/second-clip/", Title: "Second Clip"},
		{Page: "https://www.eporner.com/video-Third003/third-clip/", Title: "Third Clip"},
	}
	if len(got) != len(want) {
		t.Fatalf("collected %d videos, want %d — the sidebar's Foreign999 must not be among them: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
	for _, v := range got {
		if strings.Contains(v.Page, "Foreign999") {
			t.Errorf("a video from outside the tab was collected: %q", v.Page)
		}
	}
}

// A video already collected on an earlier page is not collected again.
func TestEpornerListingSkipsWhatWasAlreadySeen(t *testing.T) {
	root, err := parseHTML(epornerProfileMarkup)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{"Sidebar001": true, "Second002": true}
	got := epornerListing(root, "https://www.eporner.com/profile/Someone/uploaded-videos/", seen)
	if len(got) != 1 || got[0].Title != "Third Clip" {
		t.Fatalf("got %+v, want only the unseen third clip", got)
	}
}

// The count is what stops a short walk passing for a whole one, so it has to
// come from the tab's own heading — a profile also states somebody else's.
func TestEpornerDeclaredCount(t *testing.T) {
	root, err := parseHTML(epornerProfileMarkup)
	if err != nil {
		t.Fatal(err)
	}
	if got := epornerDeclaredCount(root, "uploaded-videos"); got != 5 {
		t.Errorf("uploaded-videos count = %d, want 5", got)
	}
	// "Watched videos (999)" ends in the same word and is not this tab.
	if got := epornerDeclaredCount(root, "videos"); got != 0 {
		t.Errorf("videos count = %d, want 0 — it must not match \"Watched videos\"", got)
	}
}

func TestEpornerProfileTitle(t *testing.T) {
	if got, want := epornerProfileTitle("Someone", "uploaded-videos", 5, 5), "Someone (uploaded videos)"; got != want {
		t.Errorf("complete = %q, want %q", got, want)
	}
	if got, want := epornerProfileTitle("Someone", "uploaded-videos", 3, 5),
		"Someone (uploaded videos) (partial — 3 of 5)"; got != want {
		t.Errorf("short = %q, want %q", got, want)
	}
	// With no declared total there is nothing to compare against, so the
	// job must not be accused of being short.
	if got, want := epornerProfileTitle("Someone", "videos", 3, 0), "Someone (videos)"; got != want {
		t.Errorf("no declared total = %q, want %q", got, want)
	}
}

// -------------------------------------------------------------- end to end

// epornerServer serves a video page, the player endpoint and a two-page
// profile, so the whole path can be exercised without touching the site.
func epornerServer(t *testing.T, declared int) (*httptest.Server, *int32) {
	t.Helper()
	var pageHits int32

	page := func(vid string) string {
		return `<!DOCTYPE html><html><head><title>A Made Up Clip - 5 - EPORNER</title></head><body>
<video id="EPvideo" data-vid="` + vid + `/` + vid + `.mp4"></video>
<script>var vid = '` + vid + `'; var hash = '0123456789abcdef0123456789abcdef';</script>
</body></html>`
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/xhr/video/", func(w http.ResponseWriter, r *http.Request) {
		// The endpoint only answers the folded hash.
		if got, want := r.URL.Query().Get("hash"), "bd4yv1275p7zbd4yv1275p7z"; got != want {
			http.Error(w, "bad hash "+got, http.StatusForbidden)
			return
		}
		vid := strings.TrimPrefix(r.URL.Path, "/xhr/video/")
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"vid":%q,"available":true,"code":0,"message":"","sources":{"mp4":{
		  "720p HD":{"labelShort":"720p","src":"https://cdn.example.test/%s-720p.mp4"},
		  "1080p FHD":{"labelShort":"1080p","src":"https://cdn.example.test/%s-1080p.mp4"},
		  "360p":{"labelShort":"360p","src":"https://cdn.example.test/%s-360p.mp4"}}}}`, vid, vid, vid, vid)
	})
	mux.HandleFunc("/profile/Someone/uploaded-videos/", func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/profile/Someone/uploaded-videos/":
			markup := strings.Replace(epornerProfileMarkup,
				"<span>Uploaded videos (5) </span>",
				fmt.Sprintf("<span>Uploaded videos (%d) </span>", declared), 1)
			fmt.Fprint(w, markup)
		case "/profile/Someone/uploaded-videos/2/":
			fmt.Fprint(w, `<html><body><div class="streamevents">
			  <div class="mb"><p class="mbtit"><a href="/video-Fourth004/fourth-clip/">Fourth Clip</a></p></div>
			</div></body></html>`)
		default:
			// Asking past the last page is how the walk finds its end.
			http.NotFound(w, r)
		}
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		segs := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		id := epornerVideoID(segs)
		if id == "" {
			http.NotFound(w, r)
			return
		}
		atomic.AddInt32(&pageHits, 1)
		fmt.Fprint(w, page(id))
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &pageHits
}

func epornerUnderTest(t *testing.T) *Eporner {
	t.Helper()
	return NewEporner(httpx.New("test-agent", "en-US,en;q=0.9", 1, 10*time.Second))
}

// One video, end to end: the page is read, the hash folded, the endpoint
// asked, and the best rendition chosen.
func TestEpornerVideoEndToEnd(t *testing.T) {
	srv, pageHits := epornerServer(t, 5)
	e := epornerUnderTest(t)

	u, err := url.Parse(srv.URL + "/video-Made0001Up/a-made-up-clip/")
	if err != nil {
		t.Fatal(err)
	}
	res, err := e.Extract(context.Background(), u, Options{})
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if len(res.Files) != 1 {
		t.Fatalf("got %d files, want 1", len(res.Files))
	}
	f := res.Files[0]
	if want := "A Made Up Clip - 5"; res.Title != want {
		t.Errorf("title = %q, want %q", res.Title, want)
	}
	if want := "A Made Up Clip - 5.mp4"; f.Name != want {
		t.Errorf("name = %q, want %q", f.Name, want)
	}
	if !strings.HasSuffix(f.URL, "Made0001Up-1080p.mp4") {
		t.Errorf("URL = %q, want the 1080p rendition", f.URL)
	}

	// The link is signed and short-lived, so it must be minted again when
	// the item actually starts rather than reused from extraction time.
	if f.Resolve == nil {
		t.Fatal("no resolver: a signed link would go stale in the queue")
	}
	before := atomic.LoadInt32(pageHits)
	tgt, err := f.Resolve(context.Background())
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if atomic.LoadInt32(pageHits) <= before {
		t.Error("resolving did not read the page again")
	}
	if !strings.HasSuffix(tgt.URL, "Made0001Up-1080p.mp4") {
		t.Errorf("resolved URL = %q, want the 1080p rendition", tgt.URL)
	}
}

// A whole profile tab, end to end: both pages are walked, the sidebar is
// left out, and nothing is resolved until it is asked for.
func TestEpornerProfileEndToEnd(t *testing.T) {
	srv, pageHits := epornerServer(t, 4)
	e := epornerUnderTest(t)

	u, err := url.Parse(srv.URL + "/profile/Someone/uploaded-videos/")
	if err != nil {
		t.Fatal(err)
	}
	res, err := e.Extract(context.Background(), u, Options{})
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	want := []string{"First Clip.mp4", "Second Clip.mp4", "Third Clip.mp4", "Fourth Clip.mp4"}
	if len(res.Files) != len(want) {
		t.Fatalf("got %d files, want %d: %+v", len(res.Files), len(want), res.Files)
	}
	for i, name := range want {
		if res.Files[i].Name != name {
			t.Errorf("[%d] name = %q, want %q", i, res.Files[i].Name, name)
		}
		if res.Files[i].URL != "" {
			t.Errorf("[%d] carries a URL from extraction time; it must be resolved late", i)
		}
		if res.Files[i].Resolve == nil {
			t.Errorf("[%d] has no resolver", i)
		}
	}
	// Four videos listed and four declared, so the job is whole.
	if res.Title != "Someone (uploaded videos)" {
		t.Errorf("title = %q, want it unqualified", res.Title)
	}
	// Listing must not have opened any video page.
	if got := atomic.LoadInt32(pageHits); got != 0 {
		t.Errorf("listing opened %d video pages, want 0 — they resolve on demand", got)
	}

	tgt, err := res.Files[0].Resolve(context.Background())
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if !strings.HasSuffix(tgt.URL, "Sidebar001-1080p.mp4") {
		t.Errorf("resolved URL = %q, want the first clip's 1080p rendition", tgt.URL)
	}
}

// A walk that comes back short of what the tab claims says so, rather than
// looking exactly like a complete one.
func TestEpornerProfileSaysWhenItCameBackShort(t *testing.T) {
	srv, _ := epornerServer(t, 9)
	e := epornerUnderTest(t)
	u, err := url.Parse(srv.URL + "/profile/Someone/uploaded-videos/")
	if err != nil {
		t.Fatal(err)
	}
	res, err := e.Extract(context.Background(), u, Options{})
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if want := "Someone (uploaded videos) (partial — 4 of 9)"; res.Title != want {
		t.Errorf("title = %q, want %q", res.Title, want)
	}
}

// An unavailable video is reported as such rather than as a missing file.
func TestEpornerUnavailableVideo(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/xhr/video/") {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"available":false,"code":3,"message":"This video is not available in your region","sources":{"mp4":[]}}`)
			return
		}
		fmt.Fprint(w, `<html><head><title>Gone - EPORNER</title></head><body>
<script>var vid = 'Made0001Up'; var hash = '0123456789abcdef0123456789abcdef';</script></body></html>`)
	}))
	defer srv.Close()

	u, err := url.Parse(srv.URL + "/video-Made0001Up/gone/")
	if err != nil {
		t.Fatal(err)
	}
	_, err = epornerUnderTest(t).Extract(context.Background(), u, Options{})
	if err == nil {
		t.Fatal("an unavailable video resolved without error")
	}
	if !strings.Contains(err.Error(), "not available in your region") {
		t.Errorf("error = %v, want the host's own reason carried through", err)
	}
}

// A page with no player configuration is a clear failure, not a panic.
func TestEpornerPageWithoutAPlayer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `<html><head><title>Nothing here - EPORNER</title></head><body></body></html>`)
	}))
	defer srv.Close()

	u, err := url.Parse(srv.URL + "/video-Made0001Up/gone/")
	if err != nil {
		t.Fatal(err)
	}
	_, err = epornerUnderTest(t).Extract(context.Background(), u, Options{})
	if err == nil || !strings.Contains(err.Error(), "player configuration") {
		t.Errorf("error = %v, want it to name the missing player configuration", err)
	}
}

// ---------------------------------------------------------------- titles

// The document title ends with the site's name, but titles here contain
// " - " of their own — a series numbered "- 2", "- 3". Cutting at the first
// separator, which is what the shared helper does, leaves those videos
// sharing one name, and on disk that is one file overwritten repeatedly.
func TestEpornerTitleKeepsWhatComesBeforeTheSiteName(t *testing.T) {
	cases := []struct{ doc, want string }{
		{"<html><head><title>A Clip - 5 - EPORNER</title></head><body></body></html>", "A Clip - 5"},
		{"<html><head><title>A Clip - EPORNER</title></head><body></body></html>", "A Clip"},
		{"<html><head><title>A Clip | EPORNER</title></head><body></body></html>", "A Clip"},
		// The profile pages carry a tagline behind the site name.
		{"<html><head><title>Someone's videos - EPORNER: HD Porn Tube</title></head><body></body></html>", "Someone's videos"},
		// Nothing to strip.
		{"<html><head><title>A Clip</title></head><body></body></html>", "A Clip"},
	}
	for _, c := range cases {
		if got := epornerTitle(c.doc); got != c.want {
			t.Errorf("epornerTitle(<title> in %q) = %q, want %q", c.doc, got, c.want)
		}
	}
}

// The page states its exact name in linked data, which beats reasoning about
// the document title at all. A breadcrumb list sits beside it in the same
// form, so the type has to be checked rather than taking the first name.
func TestEpornerTitlePrefersLinkedData(t *testing.T) {
	doc := `<html><head><title>Truncated Somewhere - EPORNER</title>
<script type="application/ld+json">{"@context":"https://schema.org","@type":"BreadcrumbList",
 "itemListElement":[{"@type":"ListItem","position":1,"name":"Porn Video"}]}</script>
<script type="application/ld+json">{"@context":"http://schema.org/","@type":"VideoObject",
 "name":"The Exact Name - 7","duration":"PT0H2M0S"}</script>
</head><body></body></html>`
	if got, want := epornerTitle(doc), "The Exact Name - 7"; got != want {
		t.Errorf("epornerTitle = %q, want %q", got, want)
	}
}

// Two videos in one profile differing only by a trailing number must not
// collapse onto a single filename.
func TestEpornerTitlesInASeriesStayDistinct(t *testing.T) {
	seen := map[string]bool{}
	for _, n := range []string{"", " - 2", " - 3", " - 4", " - 5"} {
		title := epornerTitle("<html><head><title>Same Base Name" + n + " - EPORNER</title></head></html>")
		if seen[title] {
			t.Fatalf("%q collides with an earlier title in the series", title)
		}
		seen[title] = true
	}
	if len(seen) != 5 {
		t.Errorf("got %d distinct titles, want 5", len(seen))
	}
}
