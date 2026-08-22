package extractor

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

// A gallery page shaped like the site's, carrying every trap that decides how
// one is read: the class on the image links is the same one worn by the
// category tile above them and the related gallery below them, the thumbnail
// inside each link is a smaller size of the same picture, one picture is
// linked twice, the attributes are single-quoted here and double-quoted a few
// lines up, and the description advertises a count the page does not carry.
const pornpicsGalleryPage = `<!DOCTYPE html><html><head>
<title>A Gallery Name - PornPics.com</title>
<meta name="description" content="Watch 42 pics of A Gallery Name at PornPics.com." />
</head><body>
<nav><a class="rel-link" href="/some-category/" data-mid="239">Some category</a></nav>
<h1>A Gallery Name</h1>
<ul class="tumbs">
  <li><a class='rel-link' href='https://cdni.pornpics.test/1280/1/200/10000001/10000001_001_aaaa.jpg' data-tid="001" data-size="1440x1920">
    <img src='https://cdni.pornpics.test/460/1/200/10000001/10000001_001_aaaa.jpg' alt='A Gallery Name 1'></a></li>
  <li><a class='rel-link' href='https://cdni.pornpics.test/1280/1/200/10000001/10000001_002_bbbb.jpg' data-tid="002" data-size="1440x1920">
    <img src='https://cdni.pornpics.test/460/1/200/10000001/10000001_002_bbbb.jpg' alt='A Gallery Name 2'></a></li>
  <li><a class='rel-link' href='https://cdni.pornpics.test/1280/1/200/10000001/10000001_002_bbbb.jpg'>the same picture again</a></li>
</ul>
<section class="related">
  <a class='rel-link' href='https://www.pornpics.test/galleries/another-gallery-10000002/'>Another gallery</a>
</section>
</body></html>`

func TestPornPicsGalleryReadsTheFullSizeImagesOnly(t *testing.T) {
	u, err := url.Parse("https://www.pornpics.test/galleries/a-gallery-name-10000001/")
	if err != nil {
		t.Fatal(err)
	}

	res, err := pornpicsGallery(pornpicsGalleryPage, u)
	if err != nil {
		t.Fatalf("pornpicsGallery: %v", err)
	}
	if res.Title != "A Gallery Name" {
		t.Errorf("title = %q, want the heading rather than the document title", res.Title)
	}
	// Two pictures, whatever the description claims: the category link, the
	// related gallery and the repeated link are none of them images, and the
	// thumbnails are the same two pictures at a smaller size.
	if len(res.Files) != 2 {
		t.Fatalf("found %d files, want 2 — the advertised count of 42 is not to be trusted", len(res.Files))
	}
	for i, want := range []string{
		"https://cdni.pornpics.test/1280/1/200/10000001/10000001_001_aaaa.jpg",
		"https://cdni.pornpics.test/1280/1/200/10000001/10000001_002_bbbb.jpg",
	} {
		if res.Files[i].URL != want {
			t.Errorf("file %d url = %q, want the full size %q", i, res.Files[i].URL, want)
		}
	}
	if res.Files[0].Name != "10000001_001_aaaa.jpg" {
		t.Errorf("name = %q, want the CDN's own name", res.Files[0].Name)
	}
	// Nothing on the page states a length, and the file must not be skipped
	// on a number describing something else.
	if res.Files[0].Size != -1 || res.Files[0].SizeApprox {
		t.Errorf("size = %d approx = %v, want an unknown length",
			res.Files[0].Size, res.Files[0].SizeApprox)
	}
	// The links are unsigned and outlive any queue, so nothing is deferred.
	if res.Files[0].Resolve != nil {
		t.Error("an image was given a resolver, which it does not need")
	}
}

func TestPornPicsGalleryWithoutImagesIsAnError(t *testing.T) {
	u, _ := url.Parse("https://www.pornpics.test/galleries/gone-10000009/")
	// Only the furniture: links of the same class leading everywhere but the
	// CDN. A build that trusted the class would report two files.
	page := `<html><head><title>Gone - PornPics.com</title></head><body>
	<a class="rel-link" href="/some-category/">Some category</a>
	<a class='rel-link' href='https://www.pornpics.test/galleries/another-10000002/'>Another</a>
	</body></html>`
	if _, err := pornpicsGallery(page, u); err == nil {
		t.Fatal("a gallery holding no images resolved to something")
	}
}

// The two listing shapes, cut to what routing reads. They differ in exactly
// one thing that matters — the search page declares QUERY — and in one thing
// that must not matter: the quoting around their attributes.
const pornpicsSearchListing = `<!DOCTYPE html><html><head>
<title>A Name Nude XXX Pics - PornPics.com</title>
<script type='text/javascript'>
    var PP_PAGE_TYPE = 'search';
    var PP_SUBTYPE = 'models';
    var P_MAX = 16;
    var QUERY='a+name';
    var STRIP_PARAMS = null;
    var PP_LANG = "en";
</script></head><body>
<h1>A Name Nude Pics</h1>
<a class='rel-link' href='%[1]s/galleries/first-gallery-10000001/' data-gid='10000001'>First</a>
<a class='rel-link' href='%[1]s/galleries/second-gallery-10000002/' data-gid='10000002'>Second</a>
<a class='rel-link' href='/some-category/'>Some category</a>
</body></html>`

const pornpicsCategoryListing = `<!DOCTYPE html><html><head>
<title>Some Category XXX Porn Pics - PornPics.com</title>
<script type='text/javascript'>
    var PP_PAGE_TYPE = 'category_rotator_maps';
    var P_MAX = 100;
    var P_CID = "330";
    var PP_LANG = "en";
</script></head><body>
<h1>Some Category Pics</h1>
<a class="rel-link" href="%[1]s/galleries/first-gallery-10000001/" data-gid="10000001">First</a>
<a class="rel-link" href="%[1]s/galleries/second-gallery-10000002/" data-gid="10000002">Second</a>
<a class="rel-link" href="/another-category/">Another category</a>
</body></html>`

// A search-type page hit on its own path with ?offset= answers 200 with this
// same HTML rather than with the next block, so routing it that way returns
// its first twenty galleries and reports no trouble at all. This is the test
// that says it is not routed that way.
func TestPornPicsSearchPageIsPagedThroughTheSearchEndpoint(t *testing.T) {
	u, err := url.Parse("https://www.pornpics.com/pornstars/a-name/")
	if err != nil {
		t.Fatal(err)
	}
	root, err := parseHTML(fmt.Sprintf(pornpicsSearchListing, "https://www.pornpics.com"))
	if err != nil {
		t.Fatal(err)
	}

	got := pornpicsPagerFor(root, u)(pornpicsFirstOffset)
	want := "https://www.pornpics.com/search/srch.php?lang=en&limit=500&offset=20&q=a+name"
	if got != want {
		t.Errorf("page 1 = %q, want the search endpoint %q", got, want)
	}
	if strings.Contains(got, "/pornstars/") {
		t.Errorf("page 1 = %q, which pages the page's own path — that answers "+
			"with HTML forever and stops the listing at twenty galleries", got)
	}
}

func TestPornPicsPlainListingIsPagedThroughItsOwnPath(t *testing.T) {
	u, err := url.Parse("https://www.pornpics.com/some-category/?orientation=straight")
	if err != nil {
		t.Fatal(err)
	}
	root, err := parseHTML(fmt.Sprintf(pornpicsCategoryListing, "https://www.pornpics.com"))
	if err != nil {
		t.Fatal(err)
	}
	pager := pornpicsPagerFor(root, u)

	// The filter the URL already carried chooses what is being paged, so it
	// has to survive; offset and limit are added on top of it.
	want := "https://www.pornpics.com/some-category/?limit=500&offset=20&orientation=straight"
	if got := pager(pornpicsFirstOffset); got != want {
		t.Errorf("page 1 = %q, want %q", got, want)
	}
	// The second block starts a whole page size on, not one item on.
	want = "https://www.pornpics.com/some-category/?limit=500&offset=520&orientation=straight"
	if got := pager(pornpicsFirstOffset + pornpicsPageSize); got != want {
		t.Errorf("page 2 = %q, want %q", got, want)
	}
	if strings.Contains(pager(pornpicsFirstOffset), "srch.php") {
		t.Error("a category was paged through the search endpoint, which needs a query it has none of")
	}
}

func TestPornPicsGlobalsSurviveEitherQuoting(t *testing.T) {
	root, err := parseHTML(fmt.Sprintf(pornpicsSearchListing, "https://www.pornpics.com"))
	if err != nil {
		t.Fatal(err)
	}
	globals := pornpicsGlobals(root)
	if globals[pornpicsQueryGlobal] != "a+name" {
		t.Errorf("QUERY = %q, want the single-quoted value", globals[pornpicsQueryGlobal])
	}
	if globals[pornpicsLangGlobal] != "en" {
		t.Errorf("PP_LANG = %q, want the double-quoted value", globals[pornpicsLangGlobal])
	}
}

func TestPornPicsGalleryLinksIgnoreTheSharedClass(t *testing.T) {
	u, err := url.Parse("https://pornpics.com/some-category/")
	if err != nil {
		t.Fatal(err)
	}
	// Note the base is the bare host and the page's links are on "www.": the
	// site emits its canonical host whichever form was pasted.
	root, err := parseHTML(fmt.Sprintf(pornpicsCategoryListing, "https://www.pornpics.com"))
	if err != nil {
		t.Fatal(err)
	}

	got := pornpicsGalleryLinks(root, u)
	want := []string{
		"https://www.pornpics.com/galleries/first-gallery-10000001/",
		"https://www.pornpics.com/galleries/second-gallery-10000002/",
	}
	if len(got) != len(want) {
		t.Fatalf("found %d links %v, want the %d galleries and neither category", len(got), got, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("link %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestPornPicsIsGallery(t *testing.T) {
	cases := map[string]bool{
		"https://www.pornpics.com/galleries/a-gallery-10000001/": true,
		// Translated paths carry a language code in front, so the marker is
		// never assumed to be the first segment.
		"https://www.pornpics.com/es/galleries/a-gallery-10000001/": true,
		"https://www.pornpics.com/galleries/":                       false,
		"https://www.pornpics.com/some-category/":                   false,
		"https://www.pornpics.com/tags/some-tag/":                   false,
		"https://www.pornpics.com/pornstars/a-name/":                false,
		"https://www.pornpics.com/":                                 false,
	}
	for raw, want := range cases {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		if got := pornpicsIsGallery(u); got != want {
			t.Errorf("pornpicsIsGallery(%q) = %v, want %v", raw, got, want)
		}
	}
}

func TestPornPicsMatch(t *testing.T) {
	p := NewPornPics(nil)
	for _, raw := range []string{
		"https://www.pornpics.com/galleries/a-gallery-10000001/",
		"https://pornpics.com/some-category/",
		"https://www.pornpics.de/some-category/",
	} {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		if !p.Match(u) {
			t.Errorf("Match(%q) = false", raw)
		}
	}
	// A CDN link is already a file. Claiming it would hand an image to the
	// listing reader, which would find no galleries in it and refuse what the
	// direct extractor downloads without a thought.
	for _, raw := range []string{
		"https://cdni.pornpics.com/1280/1/200/10000001/10000001_001_aaaa.jpg",
		"https://example.test/some-category/",
	} {
		u, _ := url.Parse(raw)
		if p.Match(u) {
			t.Errorf("Match(%q) = true", raw)
		}
	}
}

// ------------------------------------------------------- the whole listing

// pornpicsSite is a stand-in for the site, serving both listing shapes and
// recording what was asked of it. What it is for is the record: the two page
// types must be paged through different endpoints, and the failure this
// guards against is a request that succeeds while fetching the wrong thing.
type pornpicsSite struct {
	mu       sync.Mutex
	requests []string
}

func (s *pornpicsSite) record(r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests = append(s.requests, r.URL.String())
}

func (s *pornpicsSite) asked(substr string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, req := range s.requests {
		if strings.Contains(req, substr) {
			return true
		}
	}
	return false
}

// askedOffset reports whether any request named that offset, read out of the
// query rather than matched as text so "20" cannot be found inside "520".
func (s *pornpicsSite) askedOffset(offset string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, req := range s.requests {
		u, err := url.Parse(req)
		if err != nil {
			continue
		}
		if got, ok := u.Query()["offset"]; ok && len(got) == 1 && got[0] == offset {
			return true
		}
	}
	return false
}

// pornpicsGalleryFor renders a gallery page holding two pictures.
func pornpicsGalleryFor(id string) string {
	return fmt.Sprintf(`<!DOCTYPE html><html><head><title>Gallery %[1]s - PornPics.com</title></head>
<body><h1>Gallery %[1]s</h1>
<a class='rel-link' href='https://cdni.pornpics.test/1280/1/200/%[1]s/%[1]s_001_aaaa.jpg'>one</a>
<a class='rel-link' href='https://cdni.pornpics.test/1280/1/200/%[1]s/%[1]s_002_bbbb.jpg'>two</a>
</body></html>`, id)
}

// pornpicsBlock renders one page of a listing the way both routes answer it.
func pornpicsBlock(root string, ids ...string) string {
	entries := make([]string, 0, len(ids))
	for _, id := range ids {
		entries = append(entries, fmt.Sprintf(
			`{"desc":"Gallery %[2]s","g_url":"%[1]s/galleries/gallery-%[2]s/","gid":"%[2]s"}`, root, id))
	}
	return "[" + strings.Join(entries, ",") + "]"
}

// pornpicsServer stands up the fake site.
func pornpicsServer(t *testing.T) (*httptest.Server, *pornpicsSite) {
	t.Helper()
	site := &pornpicsSite{}

	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	// A search-type page. Its own path ignores ?offset= entirely and answers
	// with the page again, exactly as the site does — which is what makes a
	// wrongly routed listing look like a working one.
	mux.HandleFunc("/pornstars/a-name/", func(w http.ResponseWriter, r *http.Request) {
		site.record(r)
		_, _ = io.WriteString(w, fmt.Sprintf(pornpicsSearchListing, srv.URL))
	})
	mux.HandleFunc(pornpicsSearchPath, func(w http.ResponseWriter, r *http.Request) {
		site.record(r)
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("offset") == "20" {
			_, _ = io.WriteString(w, pornpicsBlock(srv.URL, "10000003"))
			return
		}
		_, _ = io.WriteString(w, "[]")
	})

	// A plain listing, paged through its own path. Offset zero is the one
	// value that answers with HTML there, so asking for it is a bug.
	mux.HandleFunc("/some-category/", func(w http.ResponseWriter, r *http.Request) {
		site.record(r)
		offset := r.URL.Query().Get("offset")
		if offset == "" {
			_, _ = io.WriteString(w, fmt.Sprintf(pornpicsCategoryListing, srv.URL))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if offset == "20" {
			_, _ = io.WriteString(w, pornpicsBlock(srv.URL, "10000003"))
			return
		}
		_, _ = io.WriteString(w, "[]")
	})

	mux.HandleFunc("/galleries/", func(w http.ResponseWriter, r *http.Request) {
		site.record(r)
		segs := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		slug := segs[len(segs)-1]
		_, _ = io.WriteString(w, pornpicsGalleryFor(slug[strings.LastIndex(slug, "-")+1:]))
	})

	return srv, site
}

func testPornPics() *PornPics {
	return NewPornPics(httpx.New("test-agent", "en-US", 0, 5*time.Second))
}

func TestPornPicsExpandsASearchListingThroughTheSearchEndpoint(t *testing.T) {
	srv, site := pornpicsServer(t)

	u, err := url.Parse(srv.URL + "/pornstars/a-name/")
	if err != nil {
		t.Fatal(err)
	}
	res, err := testPornPics().Extract(context.Background(), u, Options{})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}

	// Two galleries off the page and one more off the endpoint, two pictures
	// each. A listing paged the wrong way would stop at four.
	if len(res.Files) != 6 {
		t.Fatalf("resolved %d files, want 6 (three galleries of two)", len(res.Files))
	}
	if !site.asked(pornpicsSearchPath + "?") {
		t.Errorf("the search endpoint was never asked; requests were %v", site.requests)
	}
	if site.asked("/pornstars/a-name/?") {
		t.Errorf("the page's own path was paged, which answers with HTML forever; requests were %v",
			site.requests)
	}
	// Each gallery lands in a folder of its own, named as the site names it.
	if res.Files[0].Dir != "Gallery 10000001" || res.Files[5].Dir != "Gallery 10000003" {
		t.Errorf("dirs = %q .. %q, want each gallery's own title",
			res.Files[0].Dir, res.Files[5].Dir)
	}
	if res.Title != "A Name Nude Pics" {
		t.Errorf("title = %q, want the listing's heading", res.Title)
	}
}

func TestPornPicsExpandsAPlainListingThroughItsOwnPath(t *testing.T) {
	srv, site := pornpicsServer(t)

	u, err := url.Parse(srv.URL + "/some-category/")
	if err != nil {
		t.Fatal(err)
	}
	res, err := testPornPics().Extract(context.Background(), u, Options{})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}

	if len(res.Files) != 6 {
		t.Fatalf("resolved %d files, want 6 (three galleries of two)", len(res.Files))
	}
	if !site.asked("/some-category/?limit=500&offset=20") {
		t.Errorf("the listing was not paged through its own path; requests were %v", site.requests)
	}
	// Offset zero on this route answers with HTML, so it is never asked for:
	// paging starts where the page's own markup left off.
	if site.askedOffset("0") {
		t.Errorf("offset zero was requested, which answers with HTML; requests were %v", site.requests)
	}
	if !site.askedOffset("520") {
		t.Errorf("the second block was not asked for a whole page size on; requests were %v",
			site.requests)
	}
	if site.asked(pornpicsSearchPath) {
		t.Errorf("a category was paged through the search endpoint; requests were %v", site.requests)
	}
}

func TestPornPicsExtractsASingleGallery(t *testing.T) {
	srv, _ := pornpicsServer(t)

	u, err := url.Parse(srv.URL + "/galleries/gallery-10000001/")
	if err != nil {
		t.Fatal(err)
	}
	res, err := testPornPics().Extract(context.Background(), u, Options{})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if len(res.Files) != 2 {
		t.Fatalf("resolved %d files, want 2", len(res.Files))
	}
	// One gallery is one job, so the title is the folder and the files need
	// no subdirectory under it.
	if res.Files[0].Dir != "" {
		t.Errorf("dir = %q, want none for a single gallery", res.Files[0].Dir)
	}
	if res.Title != "Gallery 10000001" {
		t.Errorf("title = %q, want the gallery's heading", res.Title)
	}
}

// The ceiling exists because the search endpoint, unlike the own-path route,
// clamps nothing: a broad enough query describes more galleries than anyone
// meant to ask for. Where it falls matters — a folder holding some of its
// pictures reads as a finished download.
func TestPornPicsTrimCutsOnAGalleryBoundary(t *testing.T) {
	var files []File
	for _, dir := range []string{"one", "one", "two", "two", "two", "three"} {
		files = append(files, File{Dir: dir})
	}

	// The ceiling falls inside "two", so "two" goes whole rather than in part.
	got := pornpicsTrim(files, 4)
	if len(got) != 2 || got[len(got)-1].Dir != "one" {
		t.Errorf("trimmed to %d files ending in %q, want the two of \"one\"",
			len(got), got[len(got)-1].Dir)
	}
	// A ceiling that lands on a boundary already needs no moving.
	if got := pornpicsTrim(files, 5); len(got) != 5 {
		t.Errorf("trimmed to %d files, want 5: the ceiling was already a boundary", len(got))
	}
	if got := pornpicsTrim(files, 99); len(got) != len(files) {
		t.Errorf("trimmed to %d files, want all %d", len(got), len(files))
	}
	// One gallery bigger than the whole ceiling has nowhere to cut back to,
	// and must still yield something.
	single := []File{{Dir: "one"}, {Dir: "one"}, {Dir: "one"}}
	if got := pornpicsTrim(single, 2); len(got) != 2 {
		t.Errorf("trimmed to %d files, want the ceiling's 2", len(got))
	}
}

func TestPornPicsListingWithNoGalleriesIsAnError(t *testing.T) {
	site := &pornpicsSite{}
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	// The home page: rel-link everywhere, every one of them a category.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		site.record(r)
		_, _ = io.WriteString(w, `<!DOCTYPE html><html><head><title>PornPics.com</title>
		<script>var PP_PAGE_TYPE = 'main';</script></head><body>
		<a class="rel-link" href="/one-category/">One</a>
		<a class="rel-link" href="/another-category/">Another</a>
		</body></html>`)
	})

	u, _ := url.Parse(srv.URL + "/")
	if _, err := testPornPics().Extract(context.Background(), u, Options{}); err == nil {
		t.Fatal("a page listing only categories resolved to something")
	}
}
