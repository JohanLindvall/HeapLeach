// SPDX-License-Identifier: MIT

package extractor

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/JohanLindvall/HeapLeach/internal/httpx"
	"github.com/JohanLindvall/HeapLeach/internal/util"
)

// A post page shaped like the site's, cut down to what the extractor reads.
//
// The traps are all here on purpose. The assignment is a JavaScript string
// literal, so the title's apostrophe arrives as \', which JSON rejects; the
// caption carries a \u escape at the outer layer and a doubled one at the
// inner; image_count is 0 although five entries follow it; the animated
// entry's size describes the GIF rather than the MP4 that will be fetched
// instead; one entry has been removed and now points at the placeholder;
// one repeats an earlier one; and the page's own card images name neither
// the files nor even one of them, being a poster frame and a thumbnail.
const imgurPostPage = `<!DOCTYPE html><html><head>
<title>Nan&#39;s cats - Album on Imgur</title>
<meta name="twitter:image" data-react-helmet="true" content="https://i.imgur.com/zzz9999h.jpg">
<meta property="og:image" data-react-helmet="true" content="https://i.imgur.com/zzz9999.jpeg?fbplay">
</head><body>
<script>window.postDataJSON="{\"id\":\"pppp111\",\"title\":\"Nan\'s cats\",\"image_count\":0,\"cover_id\":\"aaa1111\",\"media\":[{\"id\":\"aaa1111\",\"url\":\"https://i.imgur.com/aaa1111.jpeg\",\"size\":2267304,\"ext\":\"jpeg\",\"type\":\"image\",\"metadata\":{\"description\":\"width=3072\\u0026height=4080\"}},{\"id\":\"bbb2222\",\"url\":\"https://i.imgur.com/bbb2222.gif\",\"size\":115434148,\"ext\":\"gif\",\"type\":\"image\"},{\"id\":\"aaa1111\",\"url\":\"https://i.imgur.com/aaa1111.jpeg\",\"size\":2267304,\"ext\":\"jpeg\",\"type\":\"image\"},{\"id\":\"ccc3333\",\"url\":\"https://i.imgur.com/ccc3333.mp4\",\"size\":17088971,\"ext\":\"mp4\",\"type\":\"video\"},{\"id\":\"ddd4444\",\"url\":\"https://i.imgur.com/removed.png\",\"size\":503,\"ext\":\"png\",\"type\":\"image\"}]}"</script>
<div id="root"></div></body></html>`

// A page for an id that is a file rather than a post. The card image is the
// "huge" thumbnail — the same id with a letter appended — and picking it would
// yield a twentieth of the file while looking entirely correct.
const imgurMediaPage = `<!DOCTYPE html><html><head>
<title>Imgur</title>
<meta name="twitter:image" data-react-helmet="true" content="https://i.imgur.com/aaa1111h.jpg">
<meta property="og:image:width" content="600">
<meta property="og:image" data-react-helmet="true" content="https://i.imgur.com/aaa1111.jpeg?fb">
</head><body><div id="root"></div></body></html>`

// The site's own listing pages are ordinary server-rendered documents whose
// card image is the imgur logo, on the static host rather than the media one.
const imgurListingShell = `<!DOCTYPE html><html><head>
<title>Imgur: The magic of the Internet</title>
<meta property="og:image" content="https://s.imgur.com/images/logo-1200-630.png">
</head><body><a href="/gallery/pppp111">a post</a></body></html>`

func TestImgurPostReadsEveryMediaEntry(t *testing.T) {
	post, err := imgurPostFrom(imgurPostPage)
	if err != nil {
		t.Fatalf("imgurPostFrom: %v", err)
	}
	// The apostrophe arrives escaped the way JavaScript escapes it, which is
	// not the way JSON does.
	if post.Title != "Nan's cats" {
		t.Errorf("title = %q, want the apostrophe intact", post.Title)
	}

	files := imgurFilesOf(post.Media)
	// Five entries: one repeated, one removed, three to download. image_count
	// says 0 and is rightly ignored.
	if len(files) != 3 {
		t.Fatalf("got %d files, want 3", len(files))
	}

	first := files[0]
	if first.URL != "https://i.imgur.com/aaa1111.jpeg" {
		t.Errorf("url = %q, want the link exactly as the site wrote it", first.URL)
	}
	if first.Name != "aaa1111.jpeg" {
		t.Errorf("name = %q, want the media id and its extension", first.Name)
	}
	// The post data states an exact length, so it must be usable as one.
	if first.Size != 2267304 || first.SizeApprox {
		t.Errorf("size = %d approx = %v, want the exact stated length", first.Size, first.SizeApprox)
	}

	// The animated entry is fetched as MP4, and its stated length described
	// the GIF, so no length may be carried over.
	gif := files[1]
	if gif.URL != "https://i.imgur.com/bbb2222.mp4" || gif.Name != "bbb2222.mp4" {
		t.Errorf("animated entry = %q named %q, want the mp4 rendition", gif.URL, gif.Name)
	}
	if gif.Size != -1 {
		t.Errorf("size = %d, want -1: the stated 115434148 is the GIF's, not the MP4's", gif.Size)
	}

	if files[2].URL != "https://i.imgur.com/ccc3333.mp4" || files[2].Size != 17088971 {
		t.Errorf("video entry = %q size %d, want it untouched with its exact length",
			files[2].URL, files[2].Size)
	}
	for _, f := range files {
		if f.URL == "https://i.imgur.com/removed.png" {
			t.Error("the removed placeholder was queued as a file")
		}
		// The page's card images name a poster frame that is in no media
		// entry, so reading them on a post page would show up here.
		if strings.Contains(f.URL, "zzz9999") {
			t.Errorf("%s came from the page's card image rather than its media list", f.URL)
		}
	}
}

// TestImgurPostDataSurvivesItsEscaping pins the two layers separately, since a
// change to either would look like the other from the outside.
func TestImgurPostDataSurvivesItsEscaping(t *testing.T) {
	literal, err := imgurPostLiteral(imgurPostPage)
	if err != nil {
		t.Fatalf("imgurPostLiteral: %v", err)
	}
	// Scanning stops at the literal's own closing quote, not at the first one
	// it meets, and the JavaScript-only escape is gone.
	if literal[0] != '"' || literal[len(literal)-1] != '"' {
		t.Fatalf("literal is not quoted: %.40q...", literal)
	}
	if want := `\'`; strings.Contains(literal, want) {
		t.Errorf("literal still carries %s, which JSON rejects", want)
	}

	post, err := imgurPostFrom(imgurPostPage)
	if err != nil {
		t.Fatalf("imgurPostFrom: %v", err)
	}
	// A \u escape at the outer layer and a doubled one at the inner must both
	// arrive as the character they name, whichever layer wrote them.
	if post.ID != "pppp111" {
		t.Errorf("id = %q", post.ID)
	}
	if len(post.Media) != 5 {
		t.Fatalf("got %d media entries, want all 5 including the removed one", len(post.Media))
	}
}

// TestImgurPostDataAbsentIsNotAFailure covers the page the site serves for an
// id that is a file rather than a post: there is no post data, and that is the
// signal to read the file's own page instead.
func TestImgurPostDataAbsentIsNotAFailure(t *testing.T) {
	if _, err := imgurPostFrom(imgurMediaPage); !errors.Is(err, errNoImgurPost) {
		t.Errorf("err = %v, want errNoImgurPost", err)
	}
	// A page that names the global and then carries something unreadable is a
	// genuine failure, not an absence. Conflating the two would report a
	// parse regression as "this id holds nothing".
	for what, doc := range map[string]string{
		"a literal that never closes": `<html><body><script>window.postDataJSON="{\"id\":` +
			`</script></body></html>`,
		"the global assigned something that is not a string": `<html><body>` +
			`<script>window.postDataJSON=null</script></body></html>`,
		"a literal holding something that is not a post": `<html><body>` +
			`<script>window.postDataJSON="[1,2,3]"</script></body></html>`,
	} {
		_, err := imgurPostFrom(doc)
		if err == nil || errors.Is(err, errNoImgurPost) {
			t.Errorf("%s: err = %v, want a failure distinct from errNoImgurPost", what, err)
		}
	}
}

func TestImgurMediaFromPrefersTheFileOverTheThumbnail(t *testing.T) {
	link, err := imgurMediaFrom(imgurMediaPage)
	if err != nil {
		t.Fatalf("imgurMediaFrom: %v", err)
	}
	if link != "https://i.imgur.com/aaa1111.jpeg" {
		t.Errorf("link = %q, want the og:image with its crawler tag dropped "+
			"(the twitter:image is a thumbnail)", link)
	}
}

func TestImgurMediaFromRejectsWhatIsNotAFile(t *testing.T) {
	tests := map[string]string{
		"the site's own listing shell, whose card image is the logo": imgurListingShell,
		"a page with no card image at all":                           `<html><head><title>Imgur</title></head><body></body></html>`,
		"a file that has been removed": `<html><head><meta property="og:image" ` +
			`content="https://i.imgur.com/removed.png"></head><body></body></html>`,
	}
	for what, doc := range tests {
		if link, err := imgurMediaFrom(doc); err == nil {
			t.Errorf("%s resolved to %q, want an error", what, link)
		}
	}
}

func TestImgurPreferMP4(t *testing.T) {
	tests := []struct {
		link    string
		want    string
		swapped bool
	}{
		{"https://i.imgur.com/aaa1111.gif", "https://i.imgur.com/aaa1111.mp4", true},
		{"https://i.imgur.com/aaa1111.GIF", "https://i.imgur.com/aaa1111.mp4", true},
		{"https://i.imgur.com/aaa1111.jpeg", "https://i.imgur.com/aaa1111.jpeg", false},
		{"https://i.imgur.com/aaa1111.mp4", "https://i.imgur.com/aaa1111.mp4", false},
		// A "gif" that is only part of the name is not one.
		{"https://i.imgur.com/my.gif.jpeg", "https://i.imgur.com/my.gif.jpeg", false},
	}
	for _, tt := range tests {
		got, swapped := imgurPreferMP4(tt.link)
		if got != tt.want || swapped != tt.swapped {
			t.Errorf("imgurPreferMP4(%q) = %q, %v; want %q, %v",
				tt.link, got, swapped, tt.want, tt.swapped)
		}
	}
}

func TestImgurTargetOf(t *testing.T) {
	tests := map[string]imgurTarget{
		"/a/pppp111":                 {ID: "pppp111"},
		"/gallery/pppp111":           {ID: "pppp111"},
		"/gallery/nans-cats-pppp111": {ID: "pppp111"},
		"/t/cats/nans-cats-pppp111":  {ID: "pppp111"},
		"/r/cats/pppp111":            {ID: "pppp111"},
		"/aaa1111":                   {ID: "aaa1111"},
		"/t/cats":                    {Listing: "/t/cats", Title: "tag cats"},
		"/r/cats":                    {Listing: "/r/cats", Title: "subreddit cats"},
		"/user/somebody":             {Listing: "/user/somebody/submitted", Title: "somebody"},
		"/user/somebody/submitted":   {Listing: "/user/somebody/submitted", Title: "somebody"},
		"/user/somebody/posts":       {Listing: "/user/somebody/submitted", Title: "somebody"},
		"/user/somebody/favorites":   {Listing: "/user/somebody/favorites", Title: "somebody favorites"},
		"/a/pppp111/anything":        {ID: "pppp111"},
		"/":                          {},
		"/search/x/y":                {},
	}
	for raw, want := range tests {
		u := mustParse(t, "https://imgur.com"+raw)
		if got := imgurTargetOf(util.PathSegments(u)); got != want {
			t.Errorf("imgurTargetOf(%q) = %+v, want %+v", raw, got, want)
		}
	}
}

// TestImgurListingEntriesStop pins the ways a listing ends, because the one
// that is easy to miss is a listing that never says it has ended: the
// mirrored subreddit feeds answer a page past the end with the first page
// again, and a walk that only stopped at an empty array would page through
// them forever. A tag feed never ends, and the listing cap bounds it.
func TestImgurListingEntriesStop(t *testing.T) {
	row := func(hash string) string { return `{"hash":"` + hash + `","ext":".jpg","size":1}` }
	pages := map[string]string{
		"0": row("aaa1111") + "," + row("bbb2222") + `,{"hash":""},` + row("aaa1111"),
		"1": row("bbb2222") + "," + row("ccc3333"),
	}
	for _, tc := range []struct {
		name  string
		after string // what every page after the second answers
		limit int
		rows  int
		note  string
	}{
		{name: "empty page", after: "", rows: 3},
		{name: "first page again", after: pages["0"], rows: 3},
		{name: "capped", after: pages["0"], limit: 2, rows: 2, note: "partial — source limit"},
		// A page that fails is no end the listing states: what was read
		// stands, and the listing is said to have been cut.
		{name: "later page fails", after: "fail", rows: 3, note: "partial — could not fetch next page"},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			page := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/t/cats/page/"), "/hit.json")
			body, ok := pages[page]
			if !ok {
				body = tc.after
			}
			if body == "fail" {
				w.WriteHeader(http.StatusBadGateway)
				return
			}
			_, _ = w.Write([]byte(`{"data":[` + body + `]}`))
		}))
		i := NewImgur(testClient())
		entries, note, err := i.listingEntries(t.Context(), nil, srv.URL, "/t/cats", Options{limits: Limits{Sources: tc.limit}})
		srv.Close()
		if err != nil || len(entries) != tc.rows || note != tc.note {
			t.Errorf("%s: rows=%d note=%q err=%v", tc.name, len(entries), note, err)
		}
	}
}

// TestImgurEntryMedia pins which listing rows can be turned into a file
// without reading a page, since getting that wrong either wastes a request per
// row or queues a link to a post as though it were an image.
func TestImgurEntryMedia(t *testing.T) {
	tests := []struct {
		name  string
		entry imgurEntry
		want  File
		ok    bool
	}{{
		name:  "a row that is itself the media carries its own exact length",
		entry: imgurEntry{Hash: "aaa1111", Ext: ".mp4", Size: 4418960},
		want:  File{Name: "aaa1111.mp4", URL: "https://i.imgur.com/aaa1111.mp4", Size: 4418960},
		ok:    true,
	}, {
		name:  "an album row names a post, which has to be read",
		entry: imgurEntry{Hash: "pppp111", Ext: ".jpg", IsAlbum: true},
		ok:    false,
	}, {
		name:  "a media row with no length gives none",
		entry: imgurEntry{Hash: "bbb2222", Ext: ".jpg"},
		want:  File{Name: "bbb2222.jpg", URL: "https://i.imgur.com/bbb2222.jpg", Size: -1},
		ok:    true,
	}, {
		name:  "an animated row is fetched as mp4 and loses the GIF's length",
		entry: imgurEntry{Hash: "ccc3333", Ext: ".gif", Size: 115434148},
		want:  File{Name: "ccc3333.mp4", URL: "https://i.imgur.com/ccc3333.mp4", Size: -1},
		ok:    true,
	}, {
		name:  "an extension written without its dot still builds a link",
		entry: imgurEntry{Hash: "ddd4444", Ext: "jpg", Size: 10},
		want:  File{Name: "ddd4444.jpg", URL: "https://i.imgur.com/ddd4444.jpg", Size: 10},
		ok:    true,
	}, {
		name:  "a row naming no file at all is not one",
		entry: imgurEntry{Hash: "eee5555"},
		ok:    false,
	}}

	for _, tt := range tests {
		got, ok := tt.entry.media()
		if ok != tt.ok {
			t.Errorf("%s: ok = %v, want %v", tt.name, ok, tt.ok)
			continue
		}
		if !ok {
			continue
		}
		if got.Name != tt.want.Name || got.URL != tt.want.URL || got.Size != tt.want.Size {
			t.Errorf("%s: got %q %q %d, want %q %q %d", tt.name,
				got.Name, got.URL, got.Size, tt.want.Name, tt.want.URL, tt.want.Size)
		}
		if got.SizeApprox {
			t.Errorf("%s: a listing row's length is exact, not approximate", tt.name)
		}
	}
}

func TestImgurRecognise(t *testing.T) {
	i := NewImgur(nil)
	expectRecognised(t, i, htmlPage(t, "https://pics.example.test/gallery/nans-cats-pppp111", imgurPostPage), true)
	expectRecognised(t, i, htmlPage(t, "https://pics.example.test/a/pppp111", imgurPostPage), true)
	// A bare id is no shape, but a page carrying the post data, or a single
	// file whose card image is the very file the id names, is the site's.
	expectRecognised(t, i, htmlPage(t, "https://pics.example.test/pppp111", imgurPostPage), true)
	expectRecognised(t, i, htmlPage(t, "https://pics.example.test/aaa1111", imgurMediaPage), true)
	// The right form and the name, but the card image is the site's logo.
	expectRecognised(t, i, htmlPage(t, "https://pics.example.test/aaa1111", imgurListingShell), false)
	expectRecognised(t, i, htmlPage(t, "https://pics.example.test/a/aaa1111", imgurListingShell), false)
	// The post data on a page of another form.
	expectRecognised(t, i, htmlPage(t, "https://pics.example.test/about/team/x", imgurPostPage), false)
	// A listing is only taken once its JSON answers, which needs a client.
	expectRecognised(t, i, htmlPage(t, "https://pics.example.test/user/someone", imgurListingShell), false)

	for raw, want := range map[string]bool{
		"https://pics.example.test/a/pppp111":                     true,
		"https://pics.example.test/gallery/nans-cats-pppp111":     true,
		"https://pics.example.test/t/cats":                        true,
		"https://pics.example.test/r/cats/pppp111":                true,
		"https://pics.example.test/user/someone/favorites":        true,
		"https://pics.example.test/aaa1111":                       false,
		"https://pics.example.test/":                              false,
		"https://pics.example.test/user/someone/favorites/extra/": false,
	} {
		expectShape(t, i, raw, want)
	}
}

// A listing is recognised by the JSON the extractor walks, asked of the
// page's own origin once however many times the page is offered.
func TestImgurRecognisesAListingByItsJSON(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.URL.Path == "/user/someone/submitted/page/0/hit.json" {
			w.Header().Set(httpx.HeaderContentType, "application/json")
			_, _ = w.Write([]byte(`{"data":[{"hash":"pppp111","is_album":true}]}`))
			return
		}
		w.Header().Set(httpx.HeaderContentType, "text/html")
		_, _ = w.Write([]byte(imgurListingShell))
	}))
	defer srv.Close()
	i := NewImgur(testClient())
	shell := strings.Replace(imgurListingShell, "<title>", `<meta property="og:site_name" content="Imgur"><title>`, 1)
	p := htmlPage(t, srv.URL+"/user/someone", shell)
	for range 2 {
		if !i.Recognise(t.Context(), p) {
			t.Fatal("a listing whose JSON answers was not recognised")
		}
	}
	if hits.Load() != 1 {
		t.Errorf("asked %d times, want once", hits.Load())
	}
	if i.Recognise(t.Context(), htmlPage(t, srv.URL+"/t/cats", shell)) {
		t.Error("a listing whose JSON does not answer was recognised")
	}
	before := hits.Load()
	if i.Recognise(t.Context(), htmlPage(t, srv.URL+"/user/someone", "<html><title>A forum</title></html>")) || hits.Load() != before {
		t.Error("a page that does not name the site was recognised or cost a request")
	}
}

// A bare media page submitted as itself is recognised by its meta, which
// parses it, and its file is then read off that same tree: nothing is asked
// (there is no client to ask with), the shared tree is left as it was, and
// the file is the one the string reader finds.
func TestImgurSingleMediaReadsTheProbedTree(t *testing.T) {
	i := NewImgur(nil)
	p := probedPage(imgurRoot+"/aaa1111", imgurMediaPage)
	if !i.Recognise(t.Context(), p) {
		t.Fatal("fixture not recognised")
	}
	root := p.HTML()
	nodes := countNodes(root)
	files, title, err := i.singleMedia(withPage(t.Context(), p), "aaa1111")
	if err != nil {
		t.Fatal(err)
	}
	want, err := imgurMediaFrom(imgurMediaPage)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0].URL != want || files[0].Size != -1 || title != files[0].Name {
		t.Errorf("files %+v title %q, want one file at %s of unknown size", files, title, want)
	}
	if p.HTML() != root || countNodes(root) != nodes {
		t.Error("the shared tree changed")
	}
}
