package extractor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

// An album page as the site renders it: each item appears twice — once for
// the inline player, once for the lightbox — images lazy-load through
// data-src with a thumbnail in src, and the page's own furniture shares the
// markup. The parser has to fold the repeats and keep only the media.
const eromeAlbumPage = `<html><head><title>An Album - EroMe</title></head><body>
  <h1 class="album-title-page">An Album</h1>
  <video><source src="https://media.example.test/v/clip1.mp4"></video>
  <div><img class="img-back" data-src="https://media.example.test/i/pic1.jpg"
            src="https://media.example.test/thumbs/thumb_pic1.jpg"></div>
  <video><source src="https://media.example.test/v/clip1.mp4"></video>
  <img src="https://media.example.test/logo.svg">
  <img class="img-front" src="https://media.example.test/i/pic2.png">
</body></html>`

func TestEromeAlbum(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/a/AAAA" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(eromeAlbumPage))
	}))
	t.Cleanup(srv.Close)

	e := NewErome(httpx.New("test-agent", "en-US", 0, 5*time.Second))
	u, _ := ParseURL(srv.URL + "/a/AAAA")
	res, err := e.Extract(context.Background(), u, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Title != "An Album" {
		t.Errorf("title = %q, want the page's own heading", res.Title)
	}
	var names []string
	for _, f := range res.Files {
		names = append(names, f.Name)
		if f.Headers[httpx.HeaderReferer] == "" {
			t.Errorf("%s carries no referer; the CDN checks one", f.Name)
		}
	}
	want := "clip1.mp4 pic1.jpg pic2.png"
	if got := strings.Join(names, " "); got != want {
		t.Errorf("files = %q, want %q (repeats folded, logo and thumbnail dropped)", got, want)
	}
}

func TestIsEromeMedia(t *testing.T) {
	cases := map[string]bool{
		"https://media.example.test/v/clip.mp4":     true,
		"https://media.example.test/i/pic.webp":     true,
		"https://media.example.test/thumb_x.jpg":    false, // a thumbnail
		"https://media.example.test/player.js":      false, // not media at all
		"https://media.example.test/no-extension":   false,
		"relative/path.mp4":                         false, // no host to fetch from
		"https://media.example.test/thumbs/....mp4": true,  // thumb in the path is fine; only the name decides
	}
	for raw, want := range cases {
		if got := isEromeMedia(raw); got != want {
			t.Errorf("isEromeMedia(%q) = %v, want %v", raw, got, want)
		}
	}
}

// A profile page as the site renders one: the creator's own albums, the
// site's furniture linking to its sections, a link to another creator, and
// the same album linked twice — once from the thumbnail and once from its
// caption, which is how the markup actually reads.
const eromeProfilePageOne = `<html><head><title>creator - EroMe</title></head><body>
  <h1>creator</h1>
  <nav><a href="/">Home</a><a href="/explore">Explore</a><a href="/login">Log in</a></nav>
  <a href="/creator?t=posts">Posts</a><a href="/creator?t=reposts">Reposts</a>
  <div class="album"><a href="/a/AAAA1111"><img src="https://s1.erome.com/1/AAAA1111/t.jpg"></a>
    <a href="/a/AAAA1111">A Day Out</a></div>
  <div class="album"><a href="/a/BBBB2222"><img src="https://s1.erome.com/1/BBBB2222/t.jpg"></a></div>
  <div class="album"><a href="https://www.erome.com/a/CCCC3333">Absolute link</a></div>
  <aside><a href="/othercreator">Another creator</a><a href="/a/">Malformed</a></aside>
</body></html>`

const eromeProfilePageTwo = `<html><body>
  <div class="album"><a href="/a/DDDD4444">One more</a></div>
  <div class="album"><a href="/a/AAAA1111">Already seen on page one</a></div>
</body></html>`

// The tab is what separates a creator's own albums from the ones they have
// reposted, so it has to survive being paged — otherwise page two onward
// silently walks a different listing.
func TestEromeProfilePageKeepsTheTab(t *testing.T) {
	u, err := url.Parse("https://www.erome.com/creator?t=reposts")
	if err != nil {
		t.Fatal(err)
	}

	// The first page is asked for exactly as the user gave it.
	if got := eromeProfilePage(u, 1); got.String() != u.String() {
		t.Errorf("page 1 = %q, want the profile URL untouched %q", got.String(), u.String())
	}

	second := eromeProfilePage(u, 2)
	query := second.Query()
	if got := query.Get("page"); got != "2" {
		t.Errorf("page = %q, want 2", got)
	}
	if got := query.Get("t"); got != "reposts" {
		t.Errorf("t = %q, want the tab carried over to page 2", got)
	}

	// A profile with no tab stays that way rather than gaining one.
	plain, _ := url.Parse("https://www.erome.com/creator")
	third := eromeProfilePage(plain, 3)
	if got := third.Query(); got.Get("page") != "3" || got.Has("t") {
		t.Errorf("query = %v, want only a page number", got)
	}
}

func TestEromeAlbumLinks(t *testing.T) {
	base, err := url.Parse("https://www.erome.com/creator?t=posts")
	if err != nil {
		t.Fatal(err)
	}
	root, err := parseHTML(eromeProfilePageOne)
	if err != nil {
		t.Fatal(err)
	}

	seen := map[string]bool{}
	got := eromeAlbumLinks(root, base, seen)
	want := []string{
		"https://www.erome.com/a/AAAA1111",
		"https://www.erome.com/a/BBBB2222",
		"https://www.erome.com/a/CCCC3333",
	}
	if len(got) != len(want) {
		t.Fatalf("collected %v, want the three albums %v", got, want)
	}
	for i, link := range want {
		if got[i] != link {
			t.Errorf("[%d] = %q, want %q (the page's own order)", i, got[i], link)
		}
	}

	// The second page contributes only what the first did not, which is what
	// tells the walk it has reached the end.
	next, err := parseHTML(eromeProfilePageTwo)
	if err != nil {
		t.Fatal(err)
	}
	second := eromeAlbumLinks(next, base, seen)
	if len(second) != 1 || second[0] != "https://www.erome.com/a/DDDD4444" {
		t.Errorf("page two contributed %v, want only the album page one lacked", second)
	}

	// A page that repeats what is already held ends the walk.
	if again := eromeAlbumLinks(next, base, seen); len(again) != 0 {
		t.Errorf("a repeated page contributed %v, want nothing", again)
	}
}

// Two albums sharing a title would otherwise share a folder: their files
// would interleave, and the profile would report one album fewer than it
// holds. Only the repeats are qualified, so every other album keeps the
// folder it has always had and a re-run skips rather than re-fetches it.
func TestEromeFoldersDisambiguateRepeatedTitles(t *testing.T) {
	albums := []eromeAlbum{
		{title: "A Day Out", id: "AAAA1111"},
		{title: "Summer Series", id: "BBBB2222"},
		{title: "A Day Out", id: "CCCC3333"},
		{title: "", id: "DDDD4444"},
		{title: "A Day Out", id: "EEEE5555"},
	}
	got := eromeFolders(albums)
	want := []string{
		"A Day Out AAAA1111",
		"Summer Series",
		"A Day Out CCCC3333",
		"DDDD4444",
		"A Day Out EEEE5555",
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("[%d] folder = %q, want %q", i, got[i], want[i])
		}
	}

	// Every folder is distinct, which is the whole point.
	distinct := map[string]bool{}
	for _, name := range got {
		if distinct[name] {
			t.Errorf("folder %q is used twice", name)
		}
		distinct[name] = true
	}
	if len(distinct) != len(albums) {
		t.Errorf("%d folders for %d albums", len(distinct), len(albums))
	}
}

// The ordinary case has to stay ordinary: a profile whose titles are all its
// own is filed under exactly those titles.
func TestEromeFoldersLeaveUniqueTitlesAlone(t *testing.T) {
	albums := []eromeAlbum{
		{title: "A Day Out", id: "AAAA1111"},
		{title: "Summer Series", id: "BBBB2222"},
	}
	for i, got := range eromeFolders(albums) {
		if got != albums[i].title {
			t.Errorf("[%d] folder = %q, want the album's own title %q", i, got, albums[i].title)
		}
	}
}

// A listing the host cut short must not pass for a complete one. Asking past
// the last page is how the walk finds its end, so a page that will not load
// is ordinarily just that — but a rate limit means the opposite, and the
// title is the only place a Result can say so.
func TestEromeProfileTitleAdmitsARateLimit(t *testing.T) {
	if got := eromeProfileTitle("creator", false); got != "creator" {
		t.Errorf("title = %q, want the profile's own name untouched", got)
	}
	got := eromeProfileTitle("creator", true)
	if got == "creator" {
		t.Fatal("a listing cut short by the host was reported as a whole profile")
	}
	if !strings.Contains(got, "creator") {
		t.Errorf("title = %q, want it to still name the profile", got)
	}
	if !strings.Contains(got, "rate limited") {
		t.Errorf("title = %q, want it to say why it is partial", got)
	}
}

// A profile page for the walk tests, whose album links name the site
// outright: the walk resolves each href against the page it came from, and
// what makes a link an album is that it is on erome. A fixture served from a
// stand-in would otherwise resolve its relative links against that stand-in
// and be discarded, which is the same rule keeping other creators' links out
// of a profile.
const eromeWalkPage = `<html><head><title>creator - EroMe</title></head><body>
  <h1>creator</h1>
  <a href="/explore">Explore</a>
  <a href="https://www.erome.com/a/AAAA1111">One</a>
  <a href="https://www.erome.com/a/BBBB2222">Two</a>
  <a href="https://www.erome.com/a/CCCC3333">Three</a>
</body></html>`

// The walk over a profile's pages, fed by a stand-in: no album is opened
// here, so what is under test is only how the walk reads what it is given.
func eromeWalkServer(t *testing.T, handler http.HandlerFunc) (*Erome, string) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return NewErome(httpx.New("test-agent", "en-US", 0, 5*time.Second)), srv.URL
}

// A later page that will not load is ordinarily the end of the listing, and
// the walk is right to stop there and keep what it has.
func TestEromeProfileAlbumsStopsAtTheEnd(t *testing.T) {
	e, base := eromeWalkServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") == "2" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(eromeWalkPage))
	})
	u, _ := ParseURL(base + "/creator?t=posts")

	albums, title, limited, err := e.profileAlbums(context.Background(), u)
	if err != nil {
		t.Fatal(err)
	}
	if len(albums) != 3 {
		t.Errorf("collected %d albums, want the 3 page one lists", len(albums))
	}
	if title != "creator" {
		t.Errorf("title = %q, want the profile's own heading", title)
	}
	if limited {
		t.Error("an ordinary end of listing was reported as a rate limit")
	}
}

// A rate limit is the opposite: the host has said come back, the client has
// already waited it out as far as it goes, and what came back is part of a
// profile rather than the whole of it. The walk has to say so, or the caller
// cannot tell a short profile from a truncated one.
func TestEromeProfileAlbumsReportsARateLimit(t *testing.T) {
	e, base := eromeWalkServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") == "2" {
			// The host naming its own interval is believed, so this keeps
			// the test to the behaviour rather than to the waiting.
			w.Header().Set(httpx.HeaderRetryAfter, "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = w.Write([]byte(eromeWalkPage))
	})
	u, _ := ParseURL(base + "/creator?t=posts")

	albums, _, limited, err := e.profileAlbums(context.Background(), u)
	if err != nil {
		t.Fatalf("a rate limit on page two lost the pages already walked: %v", err)
	}
	if len(albums) != 3 {
		t.Errorf("collected %d albums, want page one's 3 kept", len(albums))
	}
	if !limited {
		t.Fatal("a rate limit was read as the end of the listing")
	}
}

// The first page is different: nothing has been collected yet, so there is
// no partial answer to hand back and the failure is the answer.
func TestEromeProfileAlbumsFailsOnTheFirstPage(t *testing.T) {
	e, base := eromeWalkServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(httpx.HeaderRetryAfter, "0")
		w.WriteHeader(http.StatusTooManyRequests)
	})
	u, _ := ParseURL(base + "/creator?t=posts")

	if _, _, _, err := e.profileAlbums(context.Background(), u); err == nil {
		t.Fatal("a profile whose first page never loaded was reported as empty rather than failed")
	}
}

// A deleted album answers 410 with a full page of the site's own furniture.
// The status error would quote the first 200 characters of that markup at
// the user, which says nothing the code has not already said.
func TestEromeDeletedAlbumIsReportedPlainly(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusGone)
		_, _ = w.Write([]byte(`<!DOCTYPE html><html lang="en"><head>` +
			`<meta charset="utf-8"><meta name="RATING" content="RTA-5042-1996-1400-1577-RTA">` +
			`</head><body><h1>Album deleted</h1></body></html>`))
	}))
	t.Cleanup(srv.Close)

	e := NewErome(httpx.New("test-agent", "en-US", 0, 5*time.Second))
	u, _ := ParseURL(srv.URL + "/a/AAAA")
	_, err := e.Extract(context.Background(), u, Options{})
	if err == nil {
		t.Fatal("a deleted album was accepted")
	}
	if !strings.Contains(err.Error(), "deleted") {
		t.Errorf("error %q does not say the album is gone", err)
	}
	if strings.Contains(err.Error(), "<") {
		t.Errorf("error quotes the page's markup: %q", err)
	}
}
