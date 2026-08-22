package extractor

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"

	"github.com/JohanLindvall/HeapLeach/internal/config"
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
		u, err := url.Parse("https://imgur.com" + raw)
		if err != nil {
			t.Fatal(err)
		}
		if got := imgurTargetOf(util.PathSegments(u)); got != want {
			t.Errorf("imgurTargetOf(%q) = %+v, want %+v", raw, got, want)
		}
	}
}

// TestImgurPageWalkStops pins the three ways a listing ends, because the one
// that is easy to miss is a listing that never says it has ended: the mirrored
// subreddit feeds answer a page past the end with the first page again, and a
// walk that only stopped at an empty array would page through them forever.
func TestImgurPageWalkStops(t *testing.T) {
	page := func(hashes ...string) []imgurEntry {
		out := make([]imgurEntry, 0, len(hashes))
		for _, h := range hashes {
			out = append(out, imgurEntry{Hash: h, Ext: ".jpg", Size: 1})
		}
		return out
	}

	walk := imgurPageWalk{seen: make(map[string]bool)}
	if !walk.add(page("aaa1111", "bbb2222", "", "aaa1111")) {
		t.Fatal("a first page with new rows ended the walk")
	}
	if len(walk.entries) != 2 {
		t.Fatalf("kept %d rows, want 2: the blank and the repeat are not rows", len(walk.entries))
	}
	if !walk.add(page("bbb2222", "ccc3333")) {
		t.Fatal("a page carrying one new row ended the walk")
	}
	if len(walk.entries) != 3 {
		t.Fatalf("kept %d rows, want 3", len(walk.entries))
	}
	// The whole first page again, which is how a mirrored subreddit answers
	// once it has run out.
	if walk.add(page("aaa1111", "bbb2222")) {
		t.Error("a page that repeated what was already seen did not end the walk")
	}
	if walk.add(nil) {
		t.Error("an empty page did not end the walk")
	}
}

// TestImgurPageWalkIsBounded covers the tag feeds, which simply keep answering
// with older posts. Each row naming a post costs a request to expand, so the
// ceiling has to be on rows rather than on pages.
func TestImgurPageWalkIsBounded(t *testing.T) {
	walk := imgurPageWalk{seen: make(map[string]bool)}
	for n := 0; ; n++ {
		rows := make([]imgurEntry, 60)
		for r := range rows {
			rows[r] = imgurEntry{Hash: fmt.Sprintf("p%d-%d", n, r), IsAlbum: true}
		}
		if !walk.add(rows) {
			break
		}
		if n > config.MaxListingPosts {
			t.Fatal("the walk never stopped")
		}
	}
	if len(walk.entries) < config.MaxListingPosts {
		t.Errorf("stopped at %d rows, want at least the %d ceiling",
			len(walk.entries), config.MaxListingPosts)
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

func TestImgurMatch(t *testing.T) {
	i := NewImgur(nil)
	for _, raw := range []string{
		"https://imgur.com/a/pppp111",
		"https://www.imgur.com/gallery/nans-cats-pppp111",
		"https://m.imgur.com/aaa1111",
	} {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		if !i.Match(u) {
			t.Errorf("Match(%q) = false", raw)
		}
	}
	// The media host serves the bytes; such a link needs no extractor.
	for _, raw := range []string{
		"https://i.imgur.com/aaa1111.jpeg",
		"https://example.test/a/pppp111",
	} {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		if i.Match(u) {
			t.Errorf("Match(%q) = true", raw)
		}
	}
}
