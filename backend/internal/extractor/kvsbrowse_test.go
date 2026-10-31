// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// A category page in the theme's shape: a heading naming the category and
// the sort, one videos block with its sort controls, and a pager whose
// "next" points at "#" carrying the parameters the site's own script would
// send. This block pages by "from"; the older guess of "from_videos" is
// ignored by the platform and hands back page one again.
const kvsCategoryListing = `<!DOCTYPE html><html><head><title>Some Category - Example Tube</title></head><body>
<nav><a href="/">Home</a> <a href="/categories/">Categories</a> <a href="/latest-updates/">Latest</a></nav>
<h1>Some Category New Videos</h1>
<div id="list_videos_common_videos_list">
  <div id="list_videos_common_videos_list_sort_list" data-block-id="list_videos_common_videos_list">
    <a href="#videos" data-action="ajax" data-block-id="list_videos_common_videos_list" data-parameters="sort_by:most_viewed">Most viewed</a>
  </div>
  <div id="list_videos_common_videos_list_items">
    <a href="/video/first-clip/" class="item">First</a>
    <a href="/video/second-clip/" class="item">Second</a>
  </div>
  <div class="pagination" id="list_videos_common_videos_list_pagination"><ul>
    <li class="prev"><span>Back</span></li>
    <li class="page-current"><span class="pages-selector">1</span></li>
    <li class="page"><a href="#videos" data-action="ajax" data-block-id="list_videos_common_videos_list" data-parameters="sort_by:post_date;from:02 " class="pages-selector">2</a></li>
    <li class="next"><a href="#videos" data-action="ajax" data-block-id="list_videos_common_videos_list" data-parameters="sort_by:post_date;from:2">Next</a></li>
  </ul></div>
</div>
</body></html>`

// The last page of it, as the block request returns it.
const kvsCategoryLastPage = `<div id="list_videos_common_videos_list">
  <div id="list_videos_common_videos_list_items">
    <a href="/video/third-clip/" class="item">Third</a>
  </div>
  <div class="pagination" id="list_videos_common_videos_list_pagination"><ul>
    <li class="prev"><a href="#videos" data-action="ajax" data-block-id="list_videos_common_videos_list" data-parameters="sort_by:post_date;from:1">Back</a></li>
    <li class="page-current"><span class="pages-selector">2</span></li>
    <li class="next"><span class="pages-selector">Next</span></li>
  </ul></div>
</div>`

// The async request is built from the control, never guessed: the block it
// names, the parameters it carries, each name of a "a+b" key given the
// value, on top of whatever query the listing already had.
func TestKVSAsyncNextSendsWhatTheControlCarries(t *testing.T) {
	category := mustParseDoc(t, kvsCategoryListing)
	got := kvsAsyncNext(category, "https://tube.example.test/categories/some-category/?foo=bar")
	q := queryOf(t, got)
	for key, want := range map[string]string{
		"mode": "async", "function": "get_block", "block_id": "list_videos_common_videos_list",
		"sort_by": "post_date", "from": "2", "foo": "bar",
	} {
		if q.Get(key) != want {
			t.Errorf("category: %s = %q, want %q (in %s)", key, q.Get(key), want, got)
		}
	}
	if q.Has("from_videos") {
		t.Errorf("category: sent from_videos, which this block ignores: %s", got)
	}

	search := mustParseDoc(t, kvsScriptedListing)
	got = kvsAsyncNext(search, "https://tube.example.test/search/some-query/")
	q = queryOf(t, got)
	if q.Get("from_videos") != "2" || q.Get("from_albums") != "2" || q.Get("q") != "some-query" {
		t.Errorf("search: a \"from_videos+from_albums\" key should give both names the value: %s", got)
	}
	if q.Get("block_id") != "list_videos_videos_list_search_result" {
		t.Errorf("search: named the wrong block: %s", got)
	}

	// A control with a real href and no parameters gives nothing here; it is
	// followed as a page instead.
	member := mustParseDoc(t, kvsMemberListing)
	if got := kvsAsyncNext(member, "https://tube.example.test/members/4242/public_videos/"); got != "" {
		t.Errorf("a control with no parameters built %q, want nothing", got)
	}
}

func queryOf(t *testing.T, raw string) url.Values {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	return u.Query()
}

func TestKVSPagedListing(t *testing.T) {
	cases := map[string]string{
		"https://tube.example.test/categories/some-category/3/":      "https://tube.example.test/categories/some-category/",
		"https://tube.example.test/latest-updates/2/":                "https://tube.example.test/latest-updates/",
		"https://tube.example.test/models/someone/2/?sort_by=rating": "https://tube.example.test/models/someone/?sort_by=rating",
		// Not later pages of anything.
		"https://tube.example.test/categories/some-category/": "",
		"https://tube.example.test/videos/12345":              "", // a video by its id
		"https://tube.example.test/videos/12345/a-clip/":      "",
		"https://tube.example.test/7/":                        "",
	}
	for raw, want := range cases {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		got, ok := kvsPagedListing(u)
		if got != want || ok != (want != "") {
			t.Errorf("kvsPagedListing(%q) = %q, %v; want %q", raw, got, ok, want)
		}
	}
}

func TestKVSListingTitle(t *testing.T) {
	cases := []struct{ heading, listing, want string }{
		{"Some Category New Videos", "https://tube.example.test/categories/some-category/", "Some Category"},
		{"Someone's New Videos", "https://tube.example.test/models/someone/", "Someone"},
		{"Someone’s Videos", "https://tube.example.test/models/someone/", "Someone"},
		{"Some Tag Most Viewed Videos", "https://tube.example.test/tags/some-tag/", "Some Tag"},
		// A heading that is nothing but the sort falls back to the path.
		{"New Videos", "https://tube.example.test/latest-updates/", "latest-updates"},
		{"Top Rated Videos", "https://tube.example.test/top-rated/", "top-rated"},
		{"", "https://tube.example.test/channels/some-channel/", "some-channel"},
	}
	for _, tc := range cases {
		root := mustParseDoc(t, `<html><body><h1>`+tc.heading+`</h1></body></html>`)
		if got := kvsListingTitle(root, tc.listing); got != tc.want {
			t.Errorf("title of %q at %s = %q, want %q", tc.heading, tc.listing, got, tc.want)
		}
	}
}

func TestKVSHasListing(t *testing.T) {
	for doc, want := range map[string]bool{
		kvsCategoryListing: true,
		kvsScriptedListing: true,
		`<html><body><div id="list_videos_latest_videos_list"></div></body></html>`: true,
		`<html><body><h1>Terms of service</h1><p>...</p></body></html>`:             false,
	} {
		root := mustParseDoc(t, doc)
		if got := kvsHasListing(root); got != want {
			t.Errorf("kvsHasListing = %v, want %v for %.60q", got, want, doc)
		}
	}
}

// The whole route for everything that is neither a profile nor a search,
// against a server standing in for the install.
func TestKVSBrowse(t *testing.T) {
	var (
		mu    sync.Mutex
		asked []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		asked = append(asked, r.URL.RequestURI())
		mu.Unlock()
		q := r.URL.Query()
		switch {
		case r.URL.Path == "/categories/some-category/" && q.Get("mode") == "":
			io.WriteString(w, kvsCategoryListing)
		case r.URL.Path == "/categories/some-category/2/":
			// The theme serves a later page by path as well, which is how
			// one gets pasted in the first place.
			io.WriteString(w, kvsCategoryLastPage)
		case r.URL.Path == "/categories/some-category/" && q.Get("mode") == "async":
			// The block pages by "from" and in the sort the control named;
			// the older guess is answered the way the platform answers it,
			// with page one again.
			if q.Has("from_videos") {
				io.WriteString(w, kvsCategoryListing)
				return
			}
			if q.Get("block_id") != "list_videos_common_videos_list" || q.Get("from") != "2" || q.Get("sort_by") != "post_date" {
				http.NotFound(w, r)
				return
			}
			io.WriteString(w, kvsCategoryLastPage)
		case r.URL.Path == "/tags/":
			io.WriteString(w, `<html><body><h1>Tags</h1><div id="list_tags_tags_list">
				<a href="/tags/2024/">2024</a> <a href="/tags/other/">other</a></div></body></html>`)
		case r.URL.Path == "/tags/2024/":
			io.WriteString(w, `<html><body><h1>2024 New Videos</h1><div id="list_videos_common_videos_list">
				<a href="/video/tag-clip/">Tag clip</a></div></body></html>`)
		case r.URL.Path == "/channels/9/":
			// A channel named by a number on an install with no channel
			// index at all.
			io.WriteString(w, `<html><body><h1>9's Videos</h1><div id="list_videos_common_videos_list">
				<a href="/video/channel-clip/">Channel clip</a></div></body></html>`)
		case r.URL.Path == "/video/gone/":
			io.WriteString(w, `<html><body><p>This video has been removed.</p>
				<div id="list_videos_related_videos"><a href="/video/first-clip/">Related</a></div></body></html>`)
		case r.URL.Path == "/watch/7/":
			io.WriteString(w, kvsScriptedVideo("watched-clip"))
		case strings.HasPrefix(r.URL.Path, "/video/"):
			io.WriteString(w, kvsScriptedVideo(strings.Trim(strings.TrimPrefix(r.URL.Path, "/video/"), "/")))
		case r.URL.Path == "/terms/":
			io.WriteString(w, `<html><body><h1>Terms of service</h1><p>...</p></body></html>`)
		case r.URL.Path == "/":
			io.WriteString(w, `<html><body><div id="list_videos_most_recent_videos"><a href="/video/first-clip/">x</a></div></body></html>`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	k := NewKVS(testClient())
	extract := func(t *testing.T, path string) (*Result, error) {
		t.Helper()
		mu.Lock()
		asked = nil
		mu.Unlock()
		u := mustParse(t, srv.URL+path)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		return k.Extract(ctx, u, Options{})
	}
	requests := func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), asked...)
	}

	// A category pasted as its second page: the whole listing, from the
	// first page, paged with the parameters the control carries, and the
	// walk stops on the page that says it is last.
	t.Run("category", func(t *testing.T) {
		res, err := extract(t, "/categories/some-category/2/")
		if err != nil {
			t.Fatalf("Extract: %v", err)
		}
		if res.Title != "Some Category" {
			t.Errorf("title = %q, want the category's name", res.Title)
		}
		want := []string{"first clip.mp4", "second clip.mp4", "third clip.mp4"}
		if len(res.Files) != len(want) {
			t.Fatalf("got %d files, want %d: %+v", len(res.Files), len(want), res.Files)
		}
		for i, f := range res.Files {
			if f.Name != want[i] {
				t.Errorf("file %d = %q, want %q", i, f.Name, want[i])
			}
		}
		got := requests()
		if got[0] != "/categories/some-category/2/" || got[1] != "/categories/some-category/" {
			t.Errorf("asked %v, want the pasted page read first and the walk begun at the first page", got[:2])
		}
		var paged bool
		for _, uri := range got {
			paged = paged || strings.Contains(uri, "from=2")
			if strings.Contains(uri, "from_videos") {
				t.Errorf("guessed at the paging parameter: %s", uri)
			}
			if strings.Contains(uri, "from=3") {
				t.Errorf("asked past the page that said it was last: %s", uri)
			}
		}
		if !paged {
			t.Errorf("never asked for page two: %v", got)
		}
	})

	// The front page is refused before anything is fetched.
	t.Run("front page", func(t *testing.T) {
		_, err := extract(t, "/")
		if err == nil || !strings.Contains(err.Error(), "front page") {
			t.Errorf("err = %v, want a refusal naming the front page", err)
		}
		if got := requests(); len(got) != 0 {
			t.Errorf("fetched %v for the front page", got)
		}
	})

	// A page with neither a player nor a listing is reported as it always
	// was.
	t.Run("plain page", func(t *testing.T) {
		_, err := extract(t, "/terms/")
		if err == nil || !strings.Contains(err.Error(), "player") {
			t.Errorf("err = %v, want the no-player error", err)
		}
	})

	// A section whose own name is a number: the canonical listing turns out
	// to be the tag index and lists no videos, so the page is walked as
	// pasted.
	t.Run("numeric section", func(t *testing.T) {
		res, err := extract(t, "/tags/2024/")
		if err != nil {
			t.Fatalf("Extract: %v", err)
		}
		if len(res.Files) != 1 || res.Files[0].Name != "tag clip.mp4" {
			t.Errorf("files = %+v, want the one clip the tag lists", res.Files)
		}
		if res.Title != "2024" {
			t.Errorf("title = %q, want the tag", res.Title)
		}
		if got := requests(); got[0] != "/tags/2024/" || got[1] != "/tags/" {
			t.Errorf("asked %v, want the page as pasted, then the index, then no second fetch of the page", got)
		}
		for _, uri := range requests()[2:] {
			if uri == "/tags/2024/" {
				t.Error("fetched the pasted page twice")
			}
		}
	})

	// The same, on an install where the section's index does not exist.
	t.Run("numeric section without an index", func(t *testing.T) {
		res, err := extract(t, "/channels/9/")
		if err != nil {
			t.Fatalf("Extract: %v", err)
		}
		if len(res.Files) != 1 || res.Files[0].Name != "channel clip.mp4" {
			t.Errorf("files = %+v, want the one clip the channel lists", res.Files)
		}
		if got := requests(); got[0] != "/channels/9/" || got[1] != "/channels/" {
			t.Errorf("asked %v, want the page as pasted and then the index", got)
		}
	})

	// A removed video carries a block of related videos, which is not what
	// was asked for.
	t.Run("removed video", func(t *testing.T) {
		_, err := extract(t, "/video/gone/")
		if err == nil || !strings.Contains(err.Error(), "player") {
			t.Errorf("err = %v, want the no-player error", err)
		}
		for _, uri := range requests() {
			if uri == "/video/first-clip/" {
				t.Error("walked the related videos of a removed video")
			}
		}
	})

	// A video on a path the sniff would not recognise still resolves as a
	// video on a registered host.
	t.Run("video on an unusual path", func(t *testing.T) {
		res, err := extract(t, "/watch/7/")
		if err != nil {
			t.Fatalf("Extract: %v", err)
		}
		if len(res.Files) != 1 || res.Files[0].Name != "watched clip.mp4" {
			t.Errorf("files = %+v, want the one video", res.Files)
		}
	})
}

// A listing page shaped like the platform's, carrying what the walk depends
// on and the trap it has to survive: the stated total, the pagination the
// walk follows, video tiles — and the site's own furniture, which is linked
// exactly like the tiles are and must not be mistaken for them. One tile is
// repeated, because the platform prints a video twice when it is both recent
// and popular.
const kvsMemberListing = `<!DOCTYPE html><html><head>
<title>Someone's Profile | Example Tube</title>
</head><body>
<nav>
  <a href="/">Home</a>
  <a href="/categories/">Categories</a>
  <a href="/members/4242/favourite_videos/">Favourites</a>
  <a href="https://other.example.test/videos/1/elsewhere/">A video on another site</a>
</nav>
<div class="headline"><h2>Someone's Public Videos</h2>
  <span>Showing 1 - 3 of 5 videos</span>
</div>
<div class="thumbs-items" id="list_videos_public_videos_items">
  <a href="https://tube.example.test/videos/11/first-clip/" class="tumbpu"><span>First</span></a>
  <a href="https://tube.example.test/videos/12/second-clip/" class="tumbpu"><span>Second</span></a>
  <a href="https://tube.example.test/videos/11/first-clip/" class="tumbpu"><span>First again</span></a>
  <a href="https://tube.example.test/videos/13/third-clip/" class="tumbpu"><span>Third</span></a>
</div>
<div class="pagination"><ul class="pagination-list">
  <li class="active false"><span>1</span></li>
  <li><a class="selective" href="/members/4242/public_videos/2/">2</a></li>
  <li class="pagination-next"><a class="selective" href="/members/4242/public_videos/2/"><i class="ico-pagination-right"></i></a></li>
</ul></div>
</body></html>`

// The last page. The platform prints the pagination list without a next
// control, which is the only signal that the walk has reached the end.
const kvsMemberLastPage = `<!DOCTYPE html><html><body>
<div class="headline"><h2>Someone's Public Videos</h2>
  <span>Showing 4 - 5 of 5 videos</span>
</div>
<div class="thumbs-items" id="list_videos_public_videos_items">
  <a href="https://tube.example.test/videos/14/fourth-clip/" class="tumbpu"><span>Fourth</span></a>
</div>
<div class="pagination"><ul class="pagination-list">
  <li><a class="selective" href="/members/4242/public_videos/">1</a></li>
  <li class="active false"><span>2</span></li>
</ul></div>
</body></html>`

func TestKVSMemberPathAcceptsAnySection(t *testing.T) {
	want := "https://tube.example.test/members/4242/public_videos/"
	for _, raw := range []string{
		"https://tube.example.test/members/4242/",
		"https://tube.example.test/members/4242/public_videos/",
		// Someone pasting the wall or the favourites means "this person's
		// videos", so every section resolves to the one that is theirs.
		"https://tube.example.test/members/4242/wall/",
		"https://tube.example.test/members/4242/favourite_videos/",
	} {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		got, ok := kvsMemberPath(u)
		if !ok || got != want {
			t.Errorf("kvsMemberPath(%q) = %q, %v; want %q, true", raw, got, ok, want)
		}
	}
}

func TestKVSMemberPathRejectsEverythingElse(t *testing.T) {
	for _, raw := range []string{
		"https://tube.example.test/videos/11/a-clip/",
		"https://tube.example.test/members/",
		// A member id is numeric on this platform; a name is some other
		// site's idea of a profile and not ours to guess at.
		"https://tube.example.test/members/someone/",
		"https://tube.example.test/",
	} {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := kvsMemberPath(u); ok {
			t.Errorf("kvsMemberPath(%q) claimed a profile", raw)
		}
	}
}

// The listing is full of links that are not videos, and one that is a video
// on a different site entirely. Only the tiles count.
func TestKVSListingVideosIgnoresTheFurniture(t *testing.T) {
	root := mustParseDoc(t, kvsMemberListing)
	got := kvsListingVideos(root, "https://tube.example.test/members/4242/public_videos/")

	want := []string{
		"https://tube.example.test/videos/11/first-clip/",
		"https://tube.example.test/videos/12/second-clip/",
		"https://tube.example.test/videos/13/third-clip/",
	}
	if len(got) != len(want) {
		t.Fatalf("read %d videos, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("video %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestKVSMemberTitleIsTheMemberNotTheSection(t *testing.T) {
	root := mustParseDoc(t, kvsMemberListing)
	// "Someone's Public Videos" names a section as much as a person, and it
	// is the person a folder should be called after.
	if got := kvsMemberTitle(root); got != "Someone" {
		t.Errorf("title = %q, want %q", got, "Someone")
	}
}

func TestKVSMemberTitleFallsBackToTheHeading(t *testing.T) {
	doc := `<html><head><title>A Tube</title></head><body><h2>Latest Videos</h2></body></html>`
	root := mustParseDoc(t, doc)
	if got := kvsMemberTitle(root); got != "Latest Videos" {
		t.Errorf("title = %q, want the heading as it stands", got)
	}
}

// How the walk moves and how it stops. The next control is on the list item
// rather than on the anchor, so reading the href means descending into it.
func TestKVSNextPageFollowsThePaginationControl(t *testing.T) {
	const base = "https://tube.example.test/members/4242/public_videos/"

	root := mustParseDoc(t, kvsMemberListing)
	want := "https://tube.example.test/members/4242/public_videos/2/"
	if got := kvsNextPage(root, base); got != want {
		t.Errorf("next page = %q, want %q", got, want)
	}

	// The last page carries a pagination list but no next control, and that
	// absence is the whole stop condition — a walk that missed it would ask
	// for a page that does not exist.
	last := mustParseDoc(t, kvsMemberLastPage)
	if got := kvsNextPage(last, base); got != "" {
		t.Errorf("next page on the last page = %q, want none", got)
	}
}

// The total the listing states, which stops the walk even if a site ever
// paginates in a circle.
func TestKVSShowingTotal(t *testing.T) {
	m := kvsShowingTotal.FindStringSubmatch(kvsMemberListing)
	if m == nil || m[1] != "5" {
		t.Errorf("total = %v, want 5", m)
	}
	// Real listings say "Showing 49 - 64 of 64 videos" on a later page; the
	// total is the last number, not the first.
	m = kvsShowingTotal.FindStringSubmatch(kvsMemberLastPage)
	if m == nil || m[1] != "5" {
		t.Errorf("total on a later page = %v, want 5", m)
	}
}

// A section that is itself the member's own videos is kept rather than
// rewritten. Guessing the name wrong is not a 404 on this platform: an
// unknown member section answers with a generic block of the site's newest
// videos under the member's own URL, which reads as a listing and downloads
// somebody else's work.
func TestKVSMemberPathKeepsTheMembersOwnSection(t *testing.T) {
	cases := map[string]string{
		"https://tube.example.test/members/4242/videos/":          "https://tube.example.test/members/4242/videos/",
		"https://tube.example.test/members/4242/uploaded_videos/": "https://tube.example.test/members/4242/uploaded_videos/",
		"https://tube.example.test/members/4242/public_videos/":   "https://tube.example.test/members/4242/public_videos/",
		"https://tube.example.test/members/4242/VIDEOS/":          "https://tube.example.test/members/4242/videos/",
		// Not the member's own work, so it still falls back.
		"https://tube.example.test/members/4242/favourite_videos/": "https://tube.example.test/members/4242/public_videos/",
		"https://tube.example.test/members/4242/":                  "https://tube.example.test/members/4242/public_videos/",
	}
	for raw, want := range cases {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		got, ok := kvsMemberPath(u)
		if !ok || got != want {
			t.Errorf("kvsMemberPath(%q) = %q, %v; want %q, true", raw, got, ok, want)
		}
	}
}

// Some installs page the member listing in script and leave the "next"
// control pointing at "#", so the async form is the only way on.
func TestKVSAsyncPage(t *testing.T) {
	const listing = "https://tube.example.test/members/4242/videos/"
	got := kvsAsyncPage(listing, "list_videos_uploaded_videos", 3)
	want := listing + "?mode=async&function=get_block&block_id=list_videos_uploaded_videos&from_videos=3"
	if got != want {
		t.Errorf("kvsAsyncPage = %q, want %q", got, want)
	}
	// With no block to name there is no request to make, and the walk ends
	// rather than fetching something arbitrary.
	if got := kvsAsyncPage(listing, "", 2); got != "" {
		t.Errorf("kvsAsyncPage with no block = %q, want empty", got)
	}
}

func TestKVSListingBlock(t *testing.T) {
	root, err := parseHTML(`<html><body>
	  <div class="headline"><h1>Someone's Videos (356)</h1></div>
	  <div id="list_videos_uploaded_videos_filter_list" data-block-id="list_videos_uploaded_videos"></div>
	</body></html>`)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := kvsListingBlock(root), "list_videos_uploaded_videos"; got != want {
		t.Errorf("kvsListingBlock = %q, want %q", got, want)
	}
	bare := mustParseDoc(t, `<html><body><div class="list"></div></body></html>`)
	if got := kvsListingBlock(bare); got != "" {
		t.Errorf("kvsListingBlock on a page with no block = %q, want empty", got)
	}
}

func TestKVSSearchPath(t *testing.T) {
	cases := []struct {
		raw     string
		query   string
		listing string
	}{
		{
			raw:     "https://tube.example.test/search/some-query/",
			query:   "some-query",
			listing: "https://tube.example.test/search/some-query/",
		},
		// A later page pasted from a theme that pages by path means the
		// whole search, as a member's section means the member.
		{
			raw:     "https://tube.example.test/search/some-query/3/",
			query:   "some-query",
			listing: "https://tube.example.test/search/some-query/",
		},
		// The escaping the site wrote is the escaping the site gets back;
		// only the title is decoded.
		{
			raw:     "https://tube.example.test/search/one%2Btwo%5D/",
			query:   "one+two]",
			listing: "https://tube.example.test/search/one%2Btwo%5D/",
		},
		// The form-style search, before the theme redirects it.
		{
			raw:     "https://tube.example.test/search/?q=plain",
			query:   "plain",
			listing: "https://tube.example.test/search/plain/",
		},
	}
	for _, tc := range cases {
		u, err := url.Parse(tc.raw)
		if err != nil {
			t.Fatal(err)
		}
		query, listing, ok := kvsSearchPath(u)
		if !ok || query != tc.query || listing != tc.listing {
			t.Errorf("kvsSearchPath(%q) = %q, %q, %v; want %q, %q, true",
				tc.raw, query, listing, ok, tc.query, tc.listing)
		}
	}
}

func TestKVSSearchPathRejectsEverythingElse(t *testing.T) {
	for _, raw := range []string{
		"https://tube.example.test/search/",
		"https://tube.example.test/search/?q=%20",
		"https://tube.example.test/videos/11/a-clip/",
		"https://tube.example.test/members/4242/",
		"https://tube.example.test/",
	} {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, ok := kvsSearchPath(u); ok {
			t.Errorf("kvsSearchPath(%q) claimed a search", raw)
		}
	}
}
