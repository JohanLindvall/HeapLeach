// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

// The fixtures are written by hand in the shape the API answers with, on a
// reserved domain. Two things in them are traps rather than decoration: an
// item's URL ends in the storage UUID, which is not its id, so a name taken
// from the path would be the wrong one; and the /original=true/ segment is
// the transform that selects the full-size file, so it has to survive into
// the download URL untouched.

const civitaiListingPage = `{
  "items": [
    {
      "id": 111,
      "url": "https://image.civitai.example/keyaaaa/11111111-1111-1111-1111-111111111111/original=true/11111111-1111-1111-1111-111111111111.jpeg",
      "hash": "U1234567",
      "width": 832, "height": 1216,
      "nsfwLevel": "Soft", "nsfw": true, "browsingLevel": 2,
      "postId": 4242, "meta": null, "username": "SomeCreator",
      "stats": {"likeCount": 3}
    },
    {
      "id": 222,
      "url": "https://image.civitai.example/keyaaaa/22222222-2222-2222-2222-222222222222/original=true/22222222-2222-2222-2222-222222222222.mp4",
      "type": "video",
      "nsfwLevel": "X", "nsfw": true,
      "postId": 4242, "username": "SomeCreator"
    },
    {
      "id": 333,
      "url": "https://image.civitai.example/keyaaaa/33333333-3333-3333-3333-333333333333/original=true/no-extension-here",
      "postId": 4242, "username": "SomeCreator"
    },
    {
      "id": 444,
      "url": "",
      "postId": 4242, "username": "SomeCreator"
    }
  ],
  "metadata": {"nextCursor": "3|1744401394648"}
}`

// TestCivitaiTargetOf pins which shapes on the site this extractor claims.
//
// The absent ones matter as much as the present: /collections/<id> must never
// be claimed, because the parameter behind it is ignored and answers with the
// front page, and /api/download/... belongs to the direct extractor, which
// can actually fetch it.
func TestCivitaiTargetOf(t *testing.T) {
	tests := []struct {
		raw      string
		selector string
		value    string
	}{
		{"https://gallery.example.test/images/12345", civitaiByImage, "12345"},
		{"https://gallery.example.test/images/12345?period=AllTime", civitaiByImage, "12345"},
		{"https://gallery.example.test/posts/6789", civitaiByPost, "6789"},
		{"https://gallery.example.test/posts/6789/a-title-slug", civitaiByPost, "6789"},
		{"https://gallery.example.test/user/SomeCreator", civitaiByUser, "SomeCreator"},
		{"https://gallery.example.test/user/SomeCreator/images", civitaiByUser, "SomeCreator"},
		{"https://gallery.example.test/user/SomeCreator/posts", civitaiByUser, "SomeCreator"},
		{"https://gallery.example.test/user/SomeCreator/videos", civitaiByUser, "SomeCreator"},
		// The model endpoint, which is the other route: no selector.
		{"https://gallery.example.test/models/4201", "", "4201"},
		{"https://gallery.example.test/models/4201/a-model-name", "", "4201"},
		// Claimed by nobody here.
		{"https://gallery.example.test/collections/99", "", ""},
		{"https://gallery.example.test/api/download/models/555", "", ""},
		{"https://gallery.example.test/user/SomeCreator/models", "", ""},
		{"https://gallery.example.test/models/train", "", ""},
		{"https://gallery.example.test/images", "", ""},
		{"https://gallery.example.test/", "", ""},
		{"https://image.civitai.example/keyaaaa/uuid/original=true/x.jpeg", "", ""},
	}

	c := NewCivitai(nil)
	for _, tt := range tests {
		u, err := url.Parse(tt.raw)
		if err != nil {
			t.Fatal(err)
		}
		got := civitaiTargetOf(u)
		if tt.value == "" {
			if got != nil {
				t.Errorf("%s was claimed as %+v, want no match", tt.raw, got)
			}
			if c.Shape(u) {
				t.Errorf("Shape(%s) = true, want false", tt.raw)
			}
			continue
		}
		if got == nil {
			t.Errorf("%s was not claimed, want %s=%s", tt.raw, tt.selector, tt.value)
			continue
		}
		if got.selector != tt.selector || got.value != tt.value {
			t.Errorf("%s -> %q=%q, want %q=%q", tt.raw, got.selector, got.value, tt.selector, tt.value)
		}
		// A post is read when submitted, but is no shape: /posts/<n> is everywhere.
		if c.Shape(u) != (tt.selector != civitaiByPost) {
			t.Errorf("Shape(%s) = %v", tt.raw, c.Shape(u))
		}
	}
}

// TestCivitaiRecognise asks the API the extraction reads, on the page's own
// origin, and only for a page of the right form that names the site or could
// not be read at all.
func TestCivitaiRecognise(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set(httpx.HeaderContentType, "application/json")
		switch {
		case r.URL.Path == "/api/v1/images" && r.URL.Query().Get("imageId") == "12345":
			_, _ = io.WriteString(w, `{"items":[{"id":12345,"url":"https://media.example.test/a.jpeg"}],"metadata":{}}`)
		case r.URL.Path == "/api/v1/models/4201":
			_, _ = io.WriteString(w, `{"id":4201,"name":"A Model","modelVersions":[]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c := NewCivitai(httpx.New("test-agent", "en-US", 0, 5*time.Second))
	challenged := func(raw string) *Page {
		p := fixturePage(t, raw, 0, "", "")
		p.Challenged = true
		return p
	}
	named := `<html><head><title>An Image | Civitai</title></head><body></body></html>`

	image := challenged(srv.URL + "/images/12345")
	for range 2 {
		if !c.Recognise(t.Context(), image) {
			t.Fatal("a challenged image page the API knows was not recognised")
		}
	}
	if hits.Load() != 1 {
		t.Errorf("asked the API %d times for one page, want once", hits.Load())
	}
	if !c.Recognise(t.Context(), htmlPage(t, srv.URL+"/models/4201", named)) {
		t.Error("a named model page the API knows was not recognised")
	}
	// The right form and the name, but the API knows nothing of it.
	if c.Recognise(t.Context(), htmlPage(t, srv.URL+"/images/999", named)) {
		t.Error("an image the API does not know was recognised")
	}
	// An ordinary page of the right form costs no request at all.
	before := hits.Load()
	if c.Recognise(t.Context(), htmlPage(t, srv.URL+"/images/12345", `<html><title>A photo blog</title></html>`)) || hits.Load() != before {
		t.Error("an ordinary page of the right form was recognised or cost a request")
	}
	// An item page titled after the item and its uploader alone: the app's
	// data island and bundles are what lead to the API, which then decides.
	app := `<html><head><title>Image posted by Someone</title><script src="/_next/static/chunks/a.js"></script></head>` +
		`<body><script id="__NEXT_DATA__" type="application/json">{}</script></body></html>`
	if !c.Recognise(t.Context(), htmlPage(t, srv.URL+"/images/12345", app)) {
		t.Error("an unnamed app page the API knows was not recognised")
	}
	before = hits.Load()
	if c.Recognise(t.Context(), htmlPage(t, srv.URL+"/images/12345",
		`<html><head><script src="/_next/static/chunks/a.js"></script></head><body></body></html>`)) || hits.Load() != before {
		t.Error("another Next.js page of the right form was recognised or cost a request")
	}
	// The API answering, for a page of another form.
	if c.Recognise(t.Context(), challenged(srv.URL+"/collections/12345")) {
		t.Error("a collection was recognised")
	}
}

// TestCivitaiQueryLiftsMatureFiltering guards the parameter the whole
// extractor's completeness rests on, and the rule that only one selector is
// ever sent.
func TestCivitaiQueryLiftsMatureFiltering(t *testing.T) {
	raw := civitaiQuery("https://civitai.example/api/v1/images",
		&civitaiTarget{selector: civitaiByUser, value: "Some Creator"})

	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("built an unparsable query %q: %v", raw, err)
	}
	query := u.Query()
	if query.Get("nsfw") != civitaiNSFW {
		t.Errorf("nsfw = %q, want %q — without it the listing silently hides mature items",
			query.Get("nsfw"), civitaiNSFW)
	}
	if query.Get("limit") != "100" {
		t.Errorf("limit = %q, want the listing's own maximum", query.Get("limit"))
	}
	if query.Get(civitaiByUser) != "Some Creator" {
		t.Errorf("username = %q, want it escaped into the query", query.Get(civitaiByUser))
	}
	for _, unwanted := range []string{civitaiByImage, civitaiByPost, "collectionId", "modelId"} {
		if _, present := query[unwanted]; present {
			t.Errorf("%s was sent; only one selector belongs in a query, and that is not "+
				"one the listing honours", unwanted)
		}
	}
}

// civitaiServer stands in for the listing endpoint. Each call to next hands
// out one canned response; every request's raw query is recorded so a test
// can check what was actually asked.
type civitaiServer struct {
	srv      *httptest.Server
	mu       sync.Mutex
	queries  []string
	requests atomic.Int32
}

// asked returns the raw query of the nth request made.
func (cs *civitaiServer) asked(n int) string {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	if n >= len(cs.queries) {
		return ""
	}
	return cs.queries[n]
}

// newCivitaiServer serves pages in order. A page is either a status code with
// the site's own overload body, or a JSON document; "%s" in a document is
// replaced with the server's own base URL so a cursor can point back at it.
func newCivitaiServer(t *testing.T, pages []civitaiResponse) *civitaiServer {
	t.Helper()
	cs := &civitaiServer{}
	cs.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := int(cs.requests.Add(1)) - 1
		cs.mu.Lock()
		cs.queries = append(cs.queries, r.URL.RawQuery)
		cs.mu.Unlock()
		page := pages[min(n, len(pages)-1)]
		if page.status != 0 {
			w.WriteHeader(page.status)
			_, _ = io.WriteString(w, `{"error":"Image search is temporarily overloaded — please retry."}`)
			return
		}
		_, _ = io.WriteString(w, strings.ReplaceAll(page.body, "%s", cs.srv.URL))
	}))
	t.Cleanup(cs.srv.Close)
	return cs
}

type civitaiResponse struct {
	status int
	body   string
}

// civitaiTestClient never retries on its own, so what a test observes is this
// extractor's own handling of an overloaded search rather than the shared
// client's.
func civitaiTestClient() *Civitai {
	return &Civitai{client: httpx.New("test-agent", "en-US", 0, 5*time.Second)}
}

// civitaiCursorPage is a page that points at the next one, carrying the cursor and
// the filters the way the site's own nextPage does.
func civitaiCursorPage(ids []int, cursor string) string {
	items := make([]string, 0, len(ids))
	for _, id := range ids {
		items = append(items, fmt.Sprintf(
			`{"id":%d,"url":"https://image.civitai.example/k/%d/original=true/uuid-%d.jpeg","username":"SomeCreator"}`,
			id, id, id))
	}
	meta := "{}"
	if cursor != "" {
		meta = fmt.Sprintf(`{"nextCursor":%q,"nextPage":"%%s/api/v1/images?limit=100&nsfw=X&username=SomeCreator&cursor=%s"}`,
			cursor, url.QueryEscape(cursor))
	}
	return fmt.Sprintf(`{"items":[%s],"metadata":%s}`, strings.Join(items, ","), meta)
}

// TestCivitaiWalkFollowsTheCursorAndDropsTheRepeat covers both halves of
// paging: the cursor link is used as handed over, and the item a boundary
// repeats is counted once.
func TestCivitaiWalkFollowsTheCursorAndDropsTheRepeat(t *testing.T) {
	cs := newCivitaiServer(t, []civitaiResponse{
		{body: civitaiCursorPage([]int{1, 2, 3}, "3|1744401394648")},
		// The cursor's own item comes round again at the head of page two.
		{body: civitaiCursorPage([]int{3, 4}, "")},
	})

	items, note, err := civitaiTestClient().walk(context.Background(),
		cs.srv.URL+"/api/v1/images?limit=100&nsfw=X&username=SomeCreator", Options{})
	if err != nil || note != "" {
		t.Fatalf("walk: %v, note %q", err, note)
	}

	var got []int64
	for _, item := range items {
		got = append(got, item.ID)
	}
	if len(got) != 4 || got[0] != 1 || got[3] != 4 {
		t.Errorf("collected %v, want 1..4 once each in listing order", got)
	}
	if n := cs.requests.Load(); n != 2 {
		t.Fatalf("made %d requests, want one per page", n)
	}
	// Followed as given: the cursor is there, and so is the browsing level
	// the site folded into its own link.
	second := cs.asked(1)
	if !strings.Contains(second, "cursor=3%7C1744401394648") || !strings.Contains(second, "nsfw=X") {
		t.Errorf("page two was asked for as %q, want the site's own link unaltered", second)
	}
}

// TestCivitaiWalkWaitsOutTheOverload covers the 503 the search backend
// answers with while it is busy. It carries no Retry-After, so the wait is
// ours, and the page it interrupted still has to arrive.
func TestCivitaiWalkWaitsOutTheOverload(t *testing.T) {
	cs := newCivitaiServer(t, []civitaiResponse{
		{body: civitaiCursorPage([]int{1, 2}, "2|999")},
		{status: http.StatusServiceUnavailable},
		{body: civitaiCursorPage([]int{3, 4}, "")},
	})

	items, note, err := civitaiTestClient().walk(context.Background(),
		cs.srv.URL+"/api/v1/images?limit=100&nsfw=X&username=SomeCreator", Options{})
	if err != nil || note != "" {
		t.Fatalf("walk: %v, note %q", err, note)
	}
	if len(items) != 4 {
		t.Errorf("collected %d items, want all 4 — the overload is transient, not the end", len(items))
	}
}

// TestCivitaiWalkSaysAnOverloadCutItShort is the other half of the same
// behaviour, and the more important one: a search that stays overloaded must
// not look like a listing that ended. Half a gallery reported as a whole one
// is a failure nobody can see, so it is marked partial; an overloaded first
// page has nothing to show and fails.
func TestCivitaiWalkSaysAnOverloadCutItShort(t *testing.T) {
	cs := newCivitaiServer(t, []civitaiResponse{
		{body: civitaiCursorPage([]int{1, 2}, "2|999")},
		{status: http.StatusServiceUnavailable},
	})

	items, note, err := civitaiTestClient().walk(context.Background(),
		cs.srv.URL+"/api/v1/images?limit=100&nsfw=X&username=SomeCreator", Options{})
	if err != nil || len(items) != 2 || note != "partial — the image search stayed overloaded" {
		t.Fatalf("a listing cut short by an overload returned %d items, note %q, err %v", len(items), note, err)
	}
	if n := cs.requests.Load(); n < 3 {
		t.Errorf("gave up after %d requests, want the overload retried", n)
	}

	cs = newCivitaiServer(t, []civitaiResponse{{status: http.StatusServiceUnavailable}})
	if items, _, err := civitaiTestClient().walk(context.Background(),
		cs.srv.URL+"/api/v1/images?limit=100&nsfw=X&username=SomeCreator", Options{}); err == nil {
		t.Fatalf("an overloaded first page returned %d items and no error", len(items))
	}
}

func TestCivitaiNextPageStaysOnTheHost(t *testing.T) {
	const here = "https://civitai.com/api/v1/images?limit=100&nsfw=X&username=SomeCreator"
	tests := map[string]string{
		"":                     "",
		"::not a url":          "",
		here + "&cursor=1%7C2": here + "&cursor=1%7C2",
		"https://elsewhere.example.test/api/v1/images?cursor=1": "",
	}
	for raw, want := range tests {
		if got := civitaiNextPage(here, raw); got != want {
			t.Errorf("civitaiNextPage(%q) = %q, want %q", raw, got, want)
		}
	}
}

// TestCivitaiFilesAreNamedByIDAndFetchedAtFullSize pins what a listing item
// becomes: the id names the file, the URL is untouched, and nothing about
// this host needs the machinery the awkward ones do.
func TestCivitaiFilesAreNamedByIDAndFetchedAtFullSize(t *testing.T) {
	var page civitaiListing
	if err := json.Unmarshal([]byte(civitaiListingPage), &page); err != nil {
		t.Fatalf("fixture does not decode: %v", err)
	}

	want := []struct {
		name string
		ext  string
	}{
		{"111.jpeg", ".jpeg"},
		{"222.mp4", ".mp4"},
		{"333", ""},
	}
	for i, item := range page.Items[:3] {
		file := civitaiFile(item)
		if file.Name != want[i].name {
			t.Errorf("name = %q, want %q — the path's last segment is the storage id, not the image's",
				file.Name, want[i].name)
		}
		if file.URL != item.URL {
			t.Errorf("url = %q, want the listing's own %q", file.URL, item.URL)
		}
		if !strings.Contains(file.URL, "/original=true/") {
			t.Errorf("url = %q dropped the transform that selects the full-size file", file.URL)
		}
		if file.Size != -1 || file.SizeApprox {
			t.Errorf("size = %d approx = %v, want an unknown length", file.Size, file.SizeApprox)
		}
		if file.Resolve != nil || file.Cipher != nil || file.Segments != nil || file.External != "" {
			t.Errorf("%s was given machinery it does not need: the links are unsigned and "+
				"videos are progressive", file.Name)
		}
	}

	// The fourth item has no URL at all, and only the walk drops it, so the
	// listing decode must at least keep it recognisable.
	if page.Items[3].URL != "" {
		t.Error("the fixture's empty-URL item decoded to something")
	}
}

// TestCivitaiTitlePrefersTheSitesOwnCasing checks the job name, which is also
// the folder a multi-file job lands in.
func TestCivitaiTitlePrefersTheSitesOwnCasing(t *testing.T) {
	items := []civitaiImage{{ID: 1, Username: "SomeCreator"}}
	tests := []struct {
		target civitaiTarget
		items  []civitaiImage
		want   string
	}{
		{civitaiTarget{civitaiByUser, "somecreator"}, items, "SomeCreator"},
		{civitaiTarget{civitaiByUser, "somecreator"}, nil, "somecreator"},
		{civitaiTarget{civitaiByPost, "4242"}, items, "SomeCreator post 4242"},
		{civitaiTarget{civitaiByPost, "4242"}, nil, "civitai-post-4242"},
		{civitaiTarget{civitaiByImage, "111"}, items, "SomeCreator 111"},
		{civitaiTarget{civitaiByImage, "111"}, nil, "civitai-111"},
	}
	for _, tt := range tests {
		if got := civitaiTitle(&tt.target, tt.items); got != tt.want {
			t.Errorf("title for %s=%s = %q, want %q", tt.target.selector, tt.target.value, got, tt.want)
		}
	}
}

// A model's payload, cut to what is read. The trap here is that its images
// carry no id field — the listing's does — and that the same image is offered
// by two versions.
const civitaiModelPayload = `{
  "id": 4201,
  "name": "A Model Name",
  "type": "Checkpoint",
  "nsfw": true,
  "modelVersions": [
    {
      "id": 90001, "name": "v2.0",
      "images": [
        {"url": "https://image.civitai.example/k/aaaa/original=true/128374653.jpeg", "type": "image", "nsfwLevel": "Soft"},
        {"url": "https://image.civitai.example/k/bbbb/original=true/128374654.jpeg", "type": "image"}
      ]
    },
    {
      "id": 90000, "name": "v1.0",
      "images": [
        {"url": "https://image.civitai.example/k/aaaa/original=true/128374653.jpeg", "type": "image"},
        {"url": "https://image.civitai.example/k/cccc/original=true/128374655.mp4", "type": "video"},
        {"url": ""}
      ]
    }
  ]
}`

// TestCivitaiModelFilesGroupByVersion covers the endpoint the image listing
// cannot stand in for, and the naming that has to work without an id field.
func TestCivitaiModelFilesGroupByVersion(t *testing.T) {
	var model civitaiModel
	if err := json.Unmarshal([]byte(civitaiModelPayload), &model); err != nil {
		t.Fatalf("fixture does not decode: %v", err)
	}

	files := civitaiModelFiles(&model)
	if len(files) != 3 {
		t.Fatalf("got %d files, want 3: the image both versions show is one file, "+
			"and the empty URL is none", len(files))
	}

	want := []struct{ dir, name string }{
		{"v2.0", "128374653.jpeg"},
		{"v2.0", "128374654.jpeg"},
		// Kept under the version that offered it first.
		{"v1.0", "128374655.mp4"},
	}
	for i, file := range files {
		if file.Dir != want[i].dir || file.Name != want[i].name {
			t.Errorf("file %d = %s/%s, want %s/%s", i, file.Dir, file.Name, want[i].dir, want[i].name)
		}
	}
}
