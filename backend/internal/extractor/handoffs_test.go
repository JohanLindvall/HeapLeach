package extractor

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

// TestHandoffMatch pins the routing. A host list is the whole of what these
// extractors decide, so too broad a match steals a URL another extractor
// handles and too narrow a one drops it on the direct-link fallback, which
// would save the page rather than the media.
func TestHandoffMatch(t *testing.T) {
	sites := NewHandoffs(nil)

	match := func(raw string) string {
		t.Helper()
		u, err := ParseURL(raw)
		if err != nil {
			t.Fatal(err)
		}
		for _, site := range sites {
			if site.Match(u) {
				return site.Name()
			}
		}
		return ""
	}

	for raw, want := range map[string]string{
		"https://odysee.com/@channel:1/a-clip:2":      "odysee",
		"https://lbry.tv/@channel:1/a-clip:2":         "odysee",
		"https://www.dailymotion.com/video/x000000":   "dailymotion",
		"https://dai.ly/x000000":                      "dailymotion",
		"https://www.bilibili.com/video/BV00000000":   "bilibili",
		"https://b23.tv/aaaaaa":                       "bilibili",
		"https://www.nicovideo.jp/watch/sm0000000":    "niconico",
		"https://nico.ms/sm0000000":                   "niconico",
		"https://an-artist.bandcamp.com/track/a-song": "bandcamp",
		"https://bandcamp.com/a-listener":             "bandcamp",
		"https://soundcloud.com/an-artist/a-track":    "soundcloud",
		"https://m.soundcloud.com/an-artist/a-track":  "soundcloud",
		"https://www.mixcloud.com/a-user/a-show/":     "mixcloud",
		"https://rumble.com/v000000-a-clip.html":      "rumble",
		// Lookalikes: a suffix match on the bare string would take these.
		"https://notbandcamp.com/track/a-song":   "",
		"https://bandcamp.com.example.test/x":    "",
		"https://rumble.example.test/v000000-x/": "",
	} {
		if got := match(raw); got != want {
			t.Errorf("%s went to %q, want %q", raw, got, want)
		}
	}
}

// TestHandoffSitesAreUsable guards the table itself. Every field of it ends
// up in front of the user: the name opens each error and labels the host in
// the UI, and `why` completes a sentence explaining what yt-dlp is for.
func TestHandoffSitesAreUsable(t *testing.T) {
	seen := make(map[string]bool, len(handoffSites))
	for _, site := range handoffSites {
		if site.name == "" || site.why == "" || len(site.domains) == 0 {
			t.Errorf("%+v is missing a name, a reason or a domain", site)
		}
		if seen[site.name] {
			t.Errorf("two handoff hosts are both named %q", site.name)
		}
		seen[site.name] = true
	}
}

func TestHandoffName(t *testing.T) {
	for raw, want := range map[string]string{
		// A page suffix belongs to the page, not to the media.
		"https://rumble.com/v000000-a-clip.html":      "v000000-a-clip",
		"https://an-artist.bandcamp.com/track/a-song": "a-song",
		"https://www.nicovideo.jp/watch/sm0000000":    "sm0000000",
		"https://www.mixcloud.com/a-user/a-show/":     "a-show",
		// Nothing in the path to name it after.
		"https://soundcloud.com/": "soundcloud.com",
	} {
		u, err := ParseURL(raw)
		if err != nil {
			t.Fatal(err)
		}
		if got := handoffName(u); got != want {
			t.Errorf("handoffName(%s) = %q, want %q", raw, got, want)
		}
	}
}

// TestHandoffChallenged separates the failure that means "this host is shut
// to us" from the one that means "that link is not a video", since the first
// must stop the job and the second must not.
func TestHandoffChallenged(t *testing.T) {
	challenges := []error{
		&httpx.StatusError{Code: http.StatusForbidden, Status: "403 Forbidden"},
		&httpx.StatusError{Code: http.StatusServiceUnavailable, Status: "503 Service Unavailable"},
		// A 200 carrying the interstitial instead of JSON: the decode fails
		// and the body is what says why.
		errors.New(`decode json: invalid character '<' (body: <!DOCTYPE html><title>Just a moment...</title>)`),
	}
	for _, err := range challenges {
		if !handoffChallenged(err) {
			t.Errorf("%v was not recognised as a challenge", err)
		}
	}

	ordinary := []error{
		// What the oEmbed endpoint answers for a link it does not consider
		// media — a channel, a playlist, a deleted video.
		&httpx.StatusError{Code: http.StatusNotFound, Status: "404 Not Found", Body: "Media not found with that url"},
		errors.New("dial tcp: connection refused"),
	}
	for _, err := range ordinary {
		if handoffChallenged(err) {
			t.Errorf("%v was mistaken for a challenge", err)
		}
	}
}

// oembedPayload is shaped like the real answer, decoys included: the title
// must come from "title" and not from the provider or the uploader, both of
// which are strings sitting right beside it.
const oembedPayload = `{
  "type": "video",
  "version": "1.0",
  "provider_name": "Rumble",
  "provider_url": "https://rumble.com/",
  "author_name": "Some Channel",
  "title": "A clip with a name",
  "html": "<iframe src=\"https://example.test/embed/\"></iframe>",
  "thumbnail_url": "https://example.test/thumb.jpg"
}`

func testHandoff(t *testing.T, oembed string) *Handoff {
	t.Helper()
	return &Handoff{
		client: httpx.New("test-agent", "en-US", 0, 5*time.Second),
		site:   handoffSite{name: "rumble", domains: []string{"example.test"}, why: "testing", oembed: oembed},
	}
}

func TestOEmbedTitle(t *testing.T) {
	var asked string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = r.URL.Query().Get("url")
		_, _ = io.WriteString(w, oembedPayload)
	}))
	t.Cleanup(srv.Close)

	u, err := ParseURL("https://example.test/v000000-a-clip.html")
	if err != nil {
		t.Fatal(err)
	}
	title, err := testHandoff(t, srv.URL).oembedTitle(context.Background(), u)
	if err != nil {
		t.Fatalf("oembedTitle: %v", err)
	}
	if title != "A clip with a name" {
		t.Errorf("title = %q, want the video's own title", title)
	}
	// oEmbed identifies the media by the page URL, so sending anything else
	// would name whatever the endpoint fell back to.
	if asked != u.String() {
		t.Errorf("asked about %q, want %q", asked, u.String())
	}
}

// TestOEmbedTitleUnknownLinkIsNotAnError covers the link oEmbed does not
// consider media. The download is still yt-dlp's to attempt, so the item is
// queued under a name read off the URL rather than refused.
func TestOEmbedTitleUnknownLinkIsNotAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, "Media not found with that url")
	}))
	t.Cleanup(srv.Close)

	u, _ := ParseURL("https://example.test/c/a-channel")
	title, err := testHandoff(t, srv.URL).oembedTitle(context.Background(), u)
	if err != nil {
		t.Fatalf("a link oEmbed does not know refused the job: %v", err)
	}
	if title != "" {
		t.Errorf("title = %q, want none so the caller falls back to the URL", title)
	}
}

// TestOEmbedTitleChallengeStopsTheJob is the other half: an endpoint that is
// normally open to anyone answering with a challenge means the host is shut,
// and the user is told that rather than shown a bare 403.
func TestOEmbedTitleChallengeStopsTheJob(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, "<!DOCTYPE html><title>Just a moment...</title>")
	}))
	t.Cleanup(srv.Close)

	u, _ := ParseURL("https://example.test/v000000-a-clip.html")
	_, err := testHandoff(t, srv.URL).oembedTitle(context.Background(), u)
	if err == nil {
		t.Fatal("a challenged endpoint resolved to a name")
	}
	if want := "blocking automated clients"; !strings.Contains(err.Error(), want) {
		t.Errorf("error = %q, want it to say %q", err, want)
	}
}
