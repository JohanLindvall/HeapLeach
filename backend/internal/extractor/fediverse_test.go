package extractor

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

// Everything here is synthetic: the instances are httptest servers on
// 127.0.0.1 and the payloads are written by hand in the shape the real APIs
// answer with, traps included. What is under test is the discovery, the
// branch it leads to and the reading of each dialect's own decoys — none of
// which cares whose instance the bytes came from.

// -------------------------------------------------------------- URL shapes

// TestFediverseTargetShapes pins what routes here and what does not. Match
// cannot fetch, so this function alone decides whether a link is offered to
// nodeinfo at all: too narrow and a profile is saved as an HTML page, too
// broad and every /post/<n> on the web is sent to an instance that is not one.
func TestFediverseTargetShapes(t *testing.T) {
	tests := []struct {
		raw    string
		kind   fediKind
		handle string
		id     string
	}{
		{raw: "https://example.test/@alice", kind: fediAccount, handle: "alice"},
		{raw: "https://example.test/@alice@other.test", kind: fediAccount, handle: "alice@other.test"},
		{raw: "https://example.test/web/@alice", kind: fediAccount, handle: "alice"},
		{raw: "https://example.test/deck/@alice", kind: fediAccount, handle: "alice"},
		{raw: "https://example.test/users/alice", kind: fediAccount, handle: "alice"},
		{raw: "https://example.test/u/alice", kind: fediAccount, handle: "alice"},
		{raw: "https://example.test/c/pics", kind: fediCommunity, handle: "pics"},
		{raw: "https://example.test/c/pics@other.test", kind: fediCommunity, handle: "pics@other.test"},
		// Lemmy writes a community with a sigil of its own.
		{raw: "https://example.test/c/!pics", kind: fediCommunity, handle: "pics"},
		{raw: "https://example.test/@alice/109", kind: fediPost, handle: "alice", id: "109"},
		{raw: "https://example.test/notice/AbC123", kind: fediPost, id: "AbC123"},
		{raw: "https://example.test/notes/9abc", kind: fediPost, id: "9abc"},
		{raw: "https://example.test/post/12345", kind: fediPost, id: "12345"},
		{raw: "https://example.test/p/alice/995989826474436850", kind: fediPost,
			handle: "alice", id: "995989826474436850"},
	}
	for _, tc := range tests {
		t.Run(tc.raw, func(t *testing.T) {
			u, err := ParseURL(tc.raw)
			if err != nil {
				t.Fatal(err)
			}
			got := fediTargetOf(u)
			if got == nil {
				t.Fatal("not recognised as a fediverse link")
			}
			if got.kind != tc.kind || got.handle != tc.handle || got.id != tc.id {
				t.Errorf("kind %d handle %q id %q, want kind %d handle %q id %q",
					got.kind, got.handle, got.id, tc.kind, tc.handle, tc.id)
			}
		})
	}
}

func TestFediverseTargetRefusesOtherShapes(t *testing.T) {
	for _, raw := range []string{
		"https://example.test/",
		"https://example.test/some/file.bin",
		"https://example.test/videos/1/a-clip/",
		// A bare @ names nobody, and a name with a slash in it is a path.
		"https://example.test/@",
		"https://example.test/@alice/bob/carol",
		// Lemmy's ids are numbers; a word here is some other site's route.
		"https://example.test/post/latest-news",
		// Punctuation no account name may hold, which is what keeps other
		// sites' /@-prefixed routes out.
		"https://example.test/@alice!/1",
		"https://example.test/@alice@nodot",
		"https://example.test/users/alice/statuses/1",
	} {
		u, err := ParseURL(raw)
		if err != nil {
			t.Fatal(err)
		}
		if got := fediTargetOf(u); got != nil {
			t.Errorf("%s was taken as a %d for %q", raw, got.kind, got.handle)
		}
	}
}

// --------------------------------------------------------------- discovery

// TestFediverseNodeInfoHref covers the step that makes the whole extractor
// work without a host list. The href is not at a predictable path — the
// pixelfed-shaped entry here is the evidence — so it has to be read, and read
// from the right link.
func TestFediverseNodeInfoHref(t *testing.T) {
	instance, err := ParseURL("https://example.test/@alice")
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name  string
		links []fediNodeLink
		want  string
	}{
		{
			name:  "the only link",
			links: []fediNodeLink{{Rel: fediSchemaRel + "2.0", Href: "https://example.test/nodeinfo/2.0"}},
			want:  "https://example.test/nodeinfo/2.0",
		},
		{
			name: "the newest schema of several",
			links: []fediNodeLink{
				{Rel: fediSchemaRel + "2.0", Href: "https://example.test/nodeinfo/2.0"},
				{Rel: fediSchemaRel + "2.1", Href: "https://example.test/nodeinfo/2.1"},
				{Rel: fediSchemaRel + "1.0", Href: "https://example.test/nodeinfo/1.0"},
			},
			want: "https://example.test/nodeinfo/2.1",
		},
		{
			name: "a path nobody would have guessed",
			links: []fediNodeLink{
				{Rel: fediSchemaRel + "2.0", Href: "https://example.test/api/nodeinfo/2.0.json"},
			},
			want: "https://example.test/api/nodeinfo/2.0.json",
		},
		{
			name: "rels that are not nodeinfo's",
			links: []fediNodeLink{
				{Rel: "self", Href: "https://example.test/anything"},
				{Rel: "https://example.test/ns/schema/2.0", Href: "https://example.test/imposter"},
				{Rel: fediSchemaRel + "2.0", Href: "https://example.test/nodeinfo/2.0"},
			},
			want: "https://example.test/nodeinfo/2.0",
		},
		{
			// A discovery document is a host describing itself. Following it
			// elsewhere would let any page point us at a URL of its choosing.
			name: "a link off the instance",
			links: []fediNodeLink{
				{Rel: fediSchemaRel + "2.1", Href: "https://elsewhere.test/nodeinfo/2.1"},
				{Rel: fediSchemaRel + "2.0", Href: "https://example.test/nodeinfo/2.0"},
			},
			want: "https://example.test/nodeinfo/2.0",
		},
		{
			name: "nothing usable",
			links: []fediNodeLink{
				{Rel: fediSchemaRel + "2.0", Href: "ftp://example.test/nodeinfo"},
				{Rel: "self", Href: "https://example.test/nodeinfo/2.0"},
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := fediNodeInfoHref(tc.links, instance); got != tc.want {
				t.Errorf("followed %q, want %q", got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------- mastodon

// mastodonFixture holds one of everything that has to be handled: an ordinary
// post with two attachments, a boost, and an attachment this instance has not
// cached yet.
//
// The decoys are the point. preview_url is the same file under /small/;
// meta.original.size says "1920x1080", which is a pixel count and not a byte
// count; and the boost carries the original author's attachment in its own
// media_attachments, which is what Pleroma and Akkoma do and Mastodon does
// not.
const mastodonFixture = `[
  {
    "id": "115000000000000001",
    "account": {"id": "42", "username": "alice", "acct": "alice"},
    "media_attachments": [
      {
        "id": "1", "type": "image",
        "url": "https://files.example.test/media/000/001/original/first.jpg",
        "preview_url": "https://files.example.test/media/000/001/small/first.jpg",
        "remote_url": null,
        "meta": {"original": {"width": 1920, "height": 1080, "size": "1920x1080"}}
      },
      {
        "id": "2", "type": "video",
        "url": "https://files.example.test/media/000/002/original/second.mp4",
        "preview_url": "https://files.example.test/media/000/002/small/second.png",
        "remote_url": null,
        "meta": {"original": {"size": "1280x720", "duration": 12.5}}
      }
    ]
  },
  {
    "id": "115000000000000002",
    "account": {"id": "42", "username": "alice", "acct": "alice"},
    "reblog": {
      "id": "990000000000000009",
      "account": {"id": "99", "username": "bob", "acct": "bob@other.test"},
      "media_attachments": [
        {"id": "3", "type": "image",
         "url": "https://files.example.test/media/000/003/original/bobs.jpg",
         "preview_url": "https://files.example.test/media/000/003/small/bobs.jpg"}
      ]
    },
    "media_attachments": [
      {"id": "3", "type": "image",
       "url": "https://files.example.test/media/000/003/original/bobs.jpg",
       "preview_url": "https://files.example.test/media/000/003/small/bobs.jpg"}
    ]
  },
  {
    "id": "115000000000000003",
    "account": {"id": "42", "username": "alice", "acct": "alice"},
    "media_attachments": [
      {"id": "4", "type": "unknown", "url": null,
       "preview_url": null,
       "remote_url": "https://origin.test/media/000/004/original/remote.png"}
    ]
  }
]`

func TestMastodonFiles(t *testing.T) {
	var statuses []mastodonStatus
	if err := json.Unmarshal([]byte(mastodonFixture), &statuses); err != nil {
		t.Fatal(err)
	}

	files := mastodonFiles(statuses)
	want := []struct{ name, link string }{
		{"115000000000000001_1.jpg", "https://files.example.test/media/000/001/original/first.jpg"},
		{"115000000000000001_2.mp4", "https://files.example.test/media/000/002/original/second.mp4"},
		// The boost contributes nothing, so what follows it is the remote
		// attachment of the third status.
		{"115000000000000003_1.png", "https://origin.test/media/000/004/original/remote.png"},
	}
	if len(files) != len(want) {
		t.Fatalf("found %d files, want %d: %+v", len(files), len(want), files)
	}
	for i, file := range files {
		if file.URL != want[i].link {
			t.Errorf("file %d is %q, want %q", i, file.URL, want[i].link)
		}
		if file.Name != want[i].name {
			t.Errorf("file %d is named %q, want %q", i, file.Name, want[i].name)
		}
		// meta.original.size is "1920x1080" — dimensions. Nothing in this API
		// states a byte length, and a wrong one would tell the downloader an
		// unfinished file was complete.
		if file.Size != -1 {
			t.Errorf("file %d reports size %d, want -1: the API states no byte length",
				i, file.Size)
		}
	}
	for _, file := range files {
		if strings.Contains(file.URL, "/small/") {
			t.Errorf("%q is the preview, not the attachment", file.URL)
		}
		if strings.Contains(file.URL, "bobs.jpg") {
			t.Error("a boost's media was taken, which belongs to whoever wrote the original")
		}
	}
}

// TestFediverseExtractsAMastodonAccount drives discovery, the account lookup
// and the walk back through a timeline against a synthetic instance.
func TestFediverseExtractsAMastodonAccount(t *testing.T) {
	var asked []string
	srv := fediServer(t, "mastodon", func(w http.ResponseWriter, r *http.Request) bool {
		switch r.URL.Path {
		case "/api/v1/accounts/lookup":
			if got := r.URL.Query().Get("acct"); got != "alice" {
				t.Errorf("looked up %q, want alice", got)
			}
			writeJSON(w, `{"id":"42","username":"alice","acct":"alice"}`)
		case "/api/v1/accounts/42/statuses":
			query := r.URL.Query()
			if query.Get("only_media") != "true" {
				t.Errorf("asked without only_media: %s", r.URL.RawQuery)
			}
			asked = append(asked, query.Get("max_id"))
			writeJSON(w, mastodonPage(query.Get("max_id")))
		default:
			return false
		}
		return true
	})

	res := mustExtract(t, srv, "/@alice")
	if res.Title != "@alice@127.0.0.1" {
		t.Errorf("title = %q, want the account's canonical handle", res.Title)
	}
	// A full page, then a short one that ends the walk.
	if len(res.Files) != fediMastodonPageSize+2 {
		t.Fatalf("found %d files, want %d", len(res.Files), fediMastodonPageSize+2)
	}
	if len(asked) != 2 || asked[0] != "" || asked[1] == "" {
		t.Fatalf("paged with max_id %q, want an unqualified first page then an older one", asked)
	}
	if res.Files[0].Name != "1040_1.jpg" {
		t.Errorf("first file is %q, want the newest status's attachment", res.Files[0].Name)
	}
}

// mastodonPage answers one page of the timeline above: a full page of
// statuses with one attachment each, and then a short one whose ids carry on
// below where the first left off.
func mastodonPage(maxID string) string {
	high, count := 1040, fediMastodonPageSize
	if maxID != "" {
		high, count = 1000, 2
	}
	items := make([]string, 0, count)
	for i := range count {
		id := high - i
		items = append(items, fmt.Sprintf(`{"id":"%d",
			"account":{"id":"42","username":"alice","acct":"alice"},
			"media_attachments":[{"id":"%d","type":"image",
			  "url":"https://files.example.test/media/%d/original/pic.jpg",
			  "preview_url":"https://files.example.test/media/%d/small/pic.jpg"}]}`,
			id, id, id, id))
	}
	return "[" + strings.Join(items, ",") + "]"
}

// TestFediverseAsksPleromaByNickname covers the fallback for the server that
// has no accounts/lookup: the nickname route answers, and the timeline that
// follows is keyed by the id it returned rather than by the name.
func TestFediverseAsksPleromaByNickname(t *testing.T) {
	srv := fediServer(t, "akkoma", func(w http.ResponseWriter, r *http.Request) bool {
		switch r.URL.Path {
		case "/api/v1/accounts/lookup":
			http.NotFound(w, r) // as pleroma answers it
		case "/api/v1/accounts/alice":
			writeJSON(w, `{"id":"AqZ9","username":"alice","acct":"alice"}`)
		case "/api/v1/accounts/AqZ9/statuses":
			writeJSON(w, `[{"id":"B9dB","account":{"id":"AqZ9","acct":"alice"},
				"media_attachments":[{"id":"1","type":"image",
				  "url":"https://media.example.test/media/abc.jpg",
				  "preview_url":"https://media.example.test/media/abc.jpg"}]}]`)
		default:
			return false
		}
		return true
	})

	res := mustExtract(t, srv, "/@alice")
	if len(res.Files) != 1 || res.Files[0].Name != "B9dB_1.jpg" {
		t.Fatalf("got %+v, want the one attachment of the nickname's account", res.Files)
	}
}

// TestFediverseReportsUnknownSoftware covers the assumption of last resort: a
// server nobody here has heard of is asked in mastodon's dialect, and when
// that fails the error names the software so the failure is attributable.
func TestFediverseReportsUnknownSoftware(t *testing.T) {
	srv := fediServer(t, "somethingnew", func(http.ResponseWriter, *http.Request) bool {
		return false // no mastodon routes at all
	})

	_, err := fediExtract(t, srv, "/@alice")
	if err == nil {
		t.Fatal("a server implementing nothing produced a result")
	}
	if !strings.Contains(err.Error(), "somethingnew") {
		t.Errorf("error %q does not name the software nodeinfo reported", err)
	}
}

func TestFediverseRefusesAHostThatIsNotAnInstance(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)

	_, err := fediExtract(t, srv, "/@alice")
	if err == nil {
		t.Fatal("a host publishing no nodeinfo was accepted as an instance")
	}
	if !strings.Contains(err.Error(), "nodeinfo") {
		t.Errorf("error %q does not say what was missing", err)
	}
}

// ---------------------------------------------------------------- pixelfed

// TestFediverseExtractsAPixelfedPost covers the three things pixelfed does
// differently, each of which was found the hard way: its statuses live under
// a prefix of their own while the account lookup does not, it refuses a page
// larger than 24 outright rather than sending 24, and it serves no single
// post at all to a logged-out caller — so the post is found by asking the
// timeline for what precedes its id plus one.
func TestFediverseExtractsAPixelfedPost(t *testing.T) {
	const post = "995989826474436850"
	var asked url.Values

	srv := fediServer(t, "pixelfed", func(w http.ResponseWriter, r *http.Request) bool {
		switch r.URL.Path {
		case "/api/v1/accounts/lookup":
			writeJSON(w, `{"id":"47","username":"alice","acct":"alice"}`)
		case "/api/v1/accounts/47/statuses":
			// What pixelfed answers here, and following it lands on HTML.
			http.Redirect(w, r, "/login", http.StatusFound)
		case "/login":
			w.Header().Set(httpx.HeaderContentType, "text/html")
			_, _ = io.WriteString(w, "<html><body>Log in</body></html>")
		case "/api/pixelfed/v1/accounts/47/statuses":
			asked = r.URL.Query()
			if limit, _ := strconv.Atoi(asked.Get("limit")); limit > fediPixelfedPageSize {
				w.WriteHeader(http.StatusUnprocessableEntity)
				writeJSON(w, `{"message":"The limit may not be greater than 24."}`)
				return true
			}
			writeJSON(w, fmt.Sprintf(`[{"id":%q,
				"account":{"id":"47","username":"alice","acct":"alice"},
				"media_attachments":[
				  {"id":"1","type":"image","url":"https://cdn.example.test/one.jpg",
				   "preview_url":"https://cdn.example.test/one_thumb.jpg"},
				  {"id":"2","type":"video","url":"https://cdn.example.test/two.mp4",
				   "preview_url":"https://cdn.example.test/two_thumb.jpeg"}]}]`, post))
		default:
			return false
		}
		return true
	})

	res := mustExtract(t, srv, "/p/alice/"+post)
	if got := asked.Get("max_id"); got != "995989826474436851" {
		t.Errorf("asked for max_id %q, want the post's id plus one", got)
	}
	if len(res.Files) != 2 {
		t.Fatalf("found %d files, want the post's 2", len(res.Files))
	}
	if res.Files[0].URL != "https://cdn.example.test/one.jpg" ||
		res.Files[1].URL != "https://cdn.example.test/two.mp4" {
		t.Errorf("files are %q and %q, want the attachments rather than their previews",
			res.Files[0].URL, res.Files[1].URL)
	}
}

// ------------------------------------------------------------------- lemmy

// lemmyFixture holds the submissions a community listing mixes together: an
// image, a link to an article, a text post, a removed post, one from an
// instance too old to record a content type, and one whose thumbnail is the
// only image in sight. Every entry carries a thumbnail_url, because the
// instance mints one for everything and taking it would be the easy mistake.
const lemmyFixture = `{"posts": [
  {"post": {"id": 101, "name": "A photograph", "deleted": false, "removed": false,
    "url": "https://example.test/pictrs/image/aaaa-bbbb.jpeg",
    "thumbnail_url": "https://example.test/pictrs/image/thumb-1.jpeg",
    "url_content_type": "image/jpeg"}},
  {"post": {"id": 102, "name": "Something happened somewhere",
    "url": "https://news.example.test/2026/08/something-happened",
    "thumbnail_url": "https://example.test/pictrs/image/thumb-2.jpeg",
    "url_content_type": "text/html"}},
  {"post": {"id": 103, "name": "What do you think?", "url": null,
    "thumbnail_url": null}},
  {"post": {"id": 104, "name": "Gone", "removed": true,
    "url": "https://example.test/pictrs/image/cccc-dddd.png",
    "thumbnail_url": "https://example.test/pictrs/image/thumb-4.png",
    "url_content_type": "image/png"}},
  {"post": {"id": 105, "name": "From an older instance",
    "url": "https://other.test/pictrs/image/eeee-ffff.webp",
    "thumbnail_url": "https://example.test/pictrs/image/thumb-5.webp"}},
  {"post": {"id": 106, "name": "A clip", "url": "https://example.test/pictrs/image/gggg.mp4",
    "thumbnail_url": "https://example.test/pictrs/image/thumb-6.jpeg",
    "url_content_type": "video/mp4"}},
  {"post": {"id": 107, "name": "A song, on a video site",
    "url": "https://music.example.test/watch?v=abcdef",
    "thumbnail_url": "https://example.test/pictrs/image/thumb-7.jpeg",
    "url_content_type": "application/binary"}},
  {"post": {"id": 108, "name": "Served as bytes",
    "url": "https://other.test/pictrs/image/hhhh-iiii.png",
    "thumbnail_url": "https://example.test/pictrs/image/thumb-8.png",
    "url_content_type": "application/octet-stream"}}
]}`

func TestLemmyFiles(t *testing.T) {
	var payload struct {
		Posts []lemmyPostView `json:"posts"`
	}
	if err := json.Unmarshal([]byte(lemmyFixture), &payload); err != nil {
		t.Fatal(err)
	}

	files := lemmyFiles(payload.Posts)
	want := []struct{ name, link string }{
		{"101 A photograph.jpeg", "https://example.test/pictrs/image/aaaa-bbbb.jpeg"},
		{"105 From an older instance.webp", "https://other.test/pictrs/image/eeee-ffff.webp"},
		{"106 A clip.mp4", "https://example.test/pictrs/image/gggg.mp4"},
		// An instance that could not identify the link records
		// "application/octet-stream", so the path is what is left to judge by.
		{"108 Served as bytes.png", "https://other.test/pictrs/image/hhhh-iiii.png"},
	}
	if len(files) != len(want) {
		t.Fatalf("kept %d submissions, want %d: %+v", len(files), len(want), files)
	}
	for i, file := range files {
		if file.URL != want[i].link || file.Name != want[i].name {
			t.Errorf("file %d is %q at %q, want %q at %q",
				i, file.Name, file.URL, want[i].name, want[i].link)
		}
	}
	for _, file := range files {
		if strings.Contains(file.URL, "thumb-") {
			t.Errorf("%q is the instance's thumbnail, not the submission", file.URL)
		}
		if strings.Contains(file.URL, "/watch") {
			t.Errorf("%q is a video site's page: the instance called it "+
				"application/binary because it could not identify it", file.URL)
		}
	}
}

// TestFediverseExtractsALemmyCommunity covers the version probe as well as
// the listing: v3 is asked first because that is what is deployed, and the
// v4 fallback exists so that stays true rather than assumed.
func TestFediverseExtractsALemmyCommunity(t *testing.T) {
	for _, api := range []string{"/api/v3", "/api/v4"} {
		t.Run(api, func(t *testing.T) {
			var pages []string
			srv := fediServer(t, "lemmy", func(w http.ResponseWriter, r *http.Request) bool {
				if r.URL.Path != api+"/post/list" {
					return false // any other version 404s, as an instance's does
				}
				query := r.URL.Query()
				if got := query.Get("community_name"); got != "pics" {
					t.Errorf("asked for community %q, want pics", got)
				}
				pages = append(pages, query.Get("page"))
				writeJSON(w, lemmyFixture)
				return true
			})

			res := mustExtract(t, srv, "/c/pics")
			if res.Title != "!pics@127.0.0.1" {
				t.Errorf("title = %q, want the community's canonical handle", res.Title)
			}
			if len(res.Files) != 4 {
				t.Fatalf("found %d files, want the 4 media submissions", len(res.Files))
			}
			// The fixture is shorter than a page, so one request is the whole
			// listing.
			if len(pages) != 1 || pages[0] != "1" {
				t.Errorf("asked for pages %q, want just the first", pages)
			}
		})
	}
}

// TestFediverseStopsOnALemmyPageThatRepeats is the guard against an instance
// that ignores the page parameter — which is exactly what Lemmy 1.0 does,
// having replaced it with a cursor. Without it the walk would never end.
func TestFediverseStopsOnALemmyPageThatRepeats(t *testing.T) {
	var requests int
	srv := fediServer(t, "lemmy", func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != "/api/v3/post/list" {
			return false
		}
		requests++
		writeJSON(w, lemmyPage())
		return true
	})

	res := mustExtract(t, srv, "/c/pics")
	if len(res.Files) != fediLemmyPageSize {
		t.Fatalf("found %d files, want the one page's %d", len(res.Files), fediLemmyPageSize)
	}
	if requests != 2 {
		t.Errorf("made %d requests, want 2: one page, then the one that proved it repeats",
			requests)
	}
}

// lemmyPage answers a full page of image submissions, the same one every time.
func lemmyPage() string {
	items := make([]string, 0, fediLemmyPageSize)
	for i := range fediLemmyPageSize {
		items = append(items, fmt.Sprintf(
			`{"post":{"id":%d,"name":"Picture %d","url":"https://example.test/pictrs/image/%d.jpeg",
			  "thumbnail_url":"https://example.test/pictrs/image/thumb-%d.jpeg",
			  "url_content_type":"image/jpeg"}}`, 500+i, i, i, i))
	}
	return `{"posts":[` + strings.Join(items, ",") + `]}`
}

// ----------------------------------------------------------------- misskey

// misskeyFixture holds the two cases that differ only in what url serves: the
// first file's url is the instance's re-encoded copy, named "webpublic-",
// while name, type and size still describe the 4.8 MB original that was
// uploaded. The second is the original itself, and its size is exact.
const misskeyFixture = `[
  {
    "id": "aq80k02dcff00alo",
    "text": "two pictures",
    "user": {"id": "7rkr", "username": "alice", "host": null},
    "files": [
      {"id": "aq80jw6i0top00df", "name": "holiday.jpg", "type": "image/jpeg",
       "size": 4835694,
       "url": "https://media.example.test/webpublic-61b1888e-67d2.webp",
       "thumbnailUrl": "https://media.example.test/thumbnail-1b255408-de48.webp",
       "properties": {"width": 3060, "height": 4080}},
      {"id": "aq80jqhj0top00de", "name": "cat.jpg", "type": "image/jpeg",
       "size": 646524,
       "url": "https://media.example.test/67287b69-a713.jpg?sensitive=true",
       "thumbnailUrl": "https://media.example.test/thumbnail-f55f73e2-bbc0.webp"}
    ]
  }
]`

func TestMisskeyFiles(t *testing.T) {
	var notes []misskeyNote
	if err := json.Unmarshal([]byte(misskeyFixture), &notes); err != nil {
		t.Fatal(err)
	}

	files := misskeyFiles(notes)
	if len(files) != 2 {
		t.Fatalf("found %d files, want 2: %+v", len(files), files)
	}

	// The re-encoded copy is what url serves, so the name follows it and the
	// size — which describes the original upload, four times larger — is
	// dropped rather than reported against a different file.
	derived := files[0]
	if derived.URL != "https://media.example.test/webpublic-61b1888e-67d2.webp" {
		t.Errorf("url = %q", derived.URL)
	}
	if derived.Size != -1 {
		t.Errorf("size = %d, want -1: the reported 4835694 belongs to the original upload",
			derived.Size)
	}
	if derived.Name != "holiday.webp" {
		t.Errorf("name = %q, want the uploader's name with the served extension", derived.Name)
	}

	// The original: the size is exact, and exact is what the downloader is
	// entitled to act on.
	original := files[1]
	if original.Size != 646524 {
		t.Errorf("size = %d, want the exact 646524", original.Size)
	}
	if original.SizeApprox {
		t.Error("an exact length was marked approximate")
	}
	if original.Name != "cat.jpg" {
		t.Errorf("name = %q, want the uploader's own", original.Name)
	}
	for _, file := range files {
		if strings.Contains(file.URL, "thumbnail-") {
			t.Errorf("%q is the thumbnail, not the file", file.URL)
		}
	}
}

// TestFediverseExtractsMisskeyNotes covers the POST-only dialect end to end,
// including the untilId walk and the host field that separates a local
// account from one merely being shown.
func TestFediverseExtractsMisskeyNotes(t *testing.T) {
	var untils []string
	srv := fediServer(t, "sharkey", func(w http.ResponseWriter, r *http.Request) bool {
		body := readBody(r)
		switch r.URL.Path {
		case "/api/users/show":
			var ask struct {
				Username string  `json:"username"`
				Host     *string `json:"host"`
			}
			if err := json.Unmarshal(body, &ask); err != nil {
				panic(err)
			}
			if ask.Username != "alice" || ask.Host != nil {
				panic(fmt.Sprintf("asked for %+v, want the local alice", ask))
			}
			writeJSON(w, `{"id":"7rkr","username":"alice","host":null}`)
		case "/api/users/notes":
			var ask struct {
				UserID    string `json:"userId"`
				WithFiles bool   `json:"withFiles"`
				UntilID   string `json:"untilId"`
			}
			if err := json.Unmarshal(body, &ask); err != nil {
				panic(err)
			}
			if ask.UserID != "7rkr" || !ask.WithFiles {
				panic(fmt.Sprintf("asked %+v, want the account's notes with files", ask))
			}
			untils = append(untils, ask.UntilID)
			writeJSON(w, misskeyPage(ask.UntilID))
		default:
			return false
		}
		return true
	})

	res := mustExtract(t, srv, "/@alice")
	if res.Title != "@alice@127.0.0.1" {
		t.Errorf("title = %q", res.Title)
	}
	if len(res.Files) != fediMisskeyPageSize+1 {
		t.Fatalf("found %d files, want %d", len(res.Files), fediMisskeyPageSize+1)
	}
	if len(untils) != 2 || untils[0] != "" || untils[1] == "" {
		t.Errorf("paged with untilId %q, want a first page then an older one", untils)
	}
}

// misskeyPage answers one page of notes, each with a single file, the second
// page carrying on below where the first left off.
func misskeyPage(until string) string {
	high, count := 999, fediMisskeyPageSize
	if until != "" {
		high, count = 899, 1
	}
	items := make([]string, 0, count)
	for i := range count {
		n := high - i
		items = append(items, fmt.Sprintf(`{"id":"note%03d","text":"",
			"user":{"id":"7rkr","username":"alice","host":null},
			"files":[{"id":"file%03d","name":"pic%03d.png","size":%d,
			  "url":"https://media.example.test/%03d.png",
			  "thumbnailUrl":"https://media.example.test/thumbnail-%03d.webp"}]}`,
			n, n, n, 1000+n, n, n))
	}
	return "[" + strings.Join(items, ",") + "]"
}

// ------------------------------------------------------------------ shared

// fediServer stands up an instance that answers nodeinfo with the given
// software name and hands everything else to the dialect's own handler.
//
// The discovery document points at a path no caller would have guessed, which
// is the case that matters: pixelfed keeps its nodeinfo under /api, so the
// link has to be followed rather than assumed.
func fediServer(t *testing.T, software string, api func(http.ResponseWriter, *http.Request) bool) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/nodeinfo":
			writeJSON(w, fmt.Sprintf(
				`{"links":[{"rel":%q,"href":"%s/api/nodeinfo/2.0.json"}]}`,
				fediSchemaRel+"2.0", srv.URL))
		case "/api/nodeinfo/2.0.json":
			writeJSON(w, fmt.Sprintf(`{"version":"2.0","software":{"name":%q,"version":"1.0.0"}}`,
				software))
		default:
			if api == nil || !api(w, r) {
				http.NotFound(w, r)
			}
		}
	})
	return srv
}

func writeJSON(w http.ResponseWriter, body string) {
	w.Header().Set(httpx.HeaderContentType, httpx.ContentTypeJSON)
	_, _ = io.WriteString(w, body)
}

func readBody(r *http.Request) []byte {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		panic(err)
	}
	return body
}

// fediExtract runs the extractor against a synthetic instance.
func fediExtract(t *testing.T, srv *httptest.Server, path string) (*Result, error) {
	t.Helper()
	f := NewFediverse(httpx.New("test-agent", "en-US", 0, 5*time.Second))
	u, err := url.Parse(srv.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	if !f.Match(u) {
		t.Fatalf("%s is not matched by the fediverse extractor", u)
	}
	return f.Extract(context.Background(), u, Options{})
}

func mustExtract(t *testing.T, srv *httptest.Server, path string) *Result {
	t.Helper()
	res, err := fediExtract(t, srv, path)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	return res
}

// TestFediverseCapsATimeline pins the bound, which is the difference between
// resolving a busy account and walking a decade of it. The instance here
// never runs out of pages, as a busy one never does.
func TestFediverseCapsATimeline(t *testing.T) {
	var requests int
	srv := fediServer(t, "mastodon", func(w http.ResponseWriter, r *http.Request) bool {
		switch r.URL.Path {
		case "/api/v1/accounts/lookup":
			writeJSON(w, `{"id":"42","username":"alice","acct":"alice"}`)
		case "/api/v1/accounts/42/statuses":
			requests++
			writeJSON(w, endlessMastodonPage(requests))
		default:
			return false
		}
		return true
	})

	res := mustExtract(t, srv, "/@alice")
	if requests != config.MaxTimelinePages {
		t.Errorf("made %d requests, want the cap of %d", requests, config.MaxTimelinePages)
	}
	if len(res.Files) != config.MaxTimelinePages*fediMastodonPageSize {
		t.Errorf("found %d files, want %d", len(res.Files),
			config.MaxTimelinePages*fediMastodonPageSize)
	}
}

// endlessMastodonPage answers a full page whose ids always move on, so only
// the cap can end the walk.
func endlessMastodonPage(page int) string {
	items := make([]string, 0, fediMastodonPageSize)
	for i := range fediMastodonPageSize {
		id := strconv.Itoa(1_000_000 - page*fediMastodonPageSize - i)
		items = append(items, fmt.Sprintf(`{"id":%q,"account":{"id":"42","acct":"alice"},
			"media_attachments":[{"id":"1","type":"image",
			  "url":"https://files.example.test/media/%s/original/pic.jpg"}]}`, id, id))
	}
	return "[" + strings.Join(items, ",") + "]"
}
