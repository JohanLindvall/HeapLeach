// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

// TestHandoffSitesAreUsable guards the table itself. Every field of it ends
// up in front of the user: the name opens each error and labels the host in
// the UI, and `why` completes a sentence explaining what yt-dlp is for.
func TestHandoffSitesAreUsable(t *testing.T) {
	seen := make(map[string]bool, len(handoffSites))
	for _, site := range handoffSites {
		if site.name == "" || site.why == "" || site.recognise == nil || site.shape == nil || site.recognises == "" {
			t.Errorf("%s is missing a name, a reason, a form or a recogniser", site.name)
		}
		if seen[site.name] {
			t.Errorf("two handoff hosts are both named %q", site.name)
		}
		seen[site.name] = true
	}
}

// handoffNamed is the registered handoff of that name.
func handoffNamed(t *testing.T, name string) *Handoff {
	t.Helper()
	for _, e := range NewHandoffs(nil) {
		if e.Name() == name {
			return e.(*Handoff)
		}
	}
	t.Fatalf("no handoff %s", name)
	return nil
}

// TestHandoffExpand pins which sites list a collection one item per entry:
// each has a collection form of its own, and yt-dlp lists it flat.
func TestHandoffExpand(t *testing.T) {
	var got []string
	for _, site := range handoffSites {
		if site.expand {
			got = append(got, site.name)
		}
	}
	want := []string{"youtube", "odysee", "dailymotion", "soundcloud"}
	if !slices.Equal(got, want) {
		t.Errorf("expanding sites = %v, want %v", got, want)
	}
}

func metadataScript(t *testing.T, json string) (string, string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fixture uses a POSIX shell")
	}
	dir := t.TempDir()
	script, args := filepath.Join(dir, "metadata"), filepath.Join(dir, "args")
	t.Setenv("HEAPLEACH_TEST_METADATA", json)
	t.Setenv("HEAPLEACH_TEST_ARGS", args)
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$HEAPLEACH_TEST_ARGS\"\nprintf '%s' \"$HEAPLEACH_TEST_METADATA\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return script, args
}

func TestHandoffListBoundsPlaylistAtTheHelper(t *testing.T) {
	helper, args := metadataScript(t, `{"_type":"url","title":"First clip","url":"https://example.test/first"}
{"_type":"url","title":"Second clip","url":"https://example.test/second"}
{"_type":"url","title":"Third clip","url":"https://example.test/third"}
{"_type":"playlist","title":"Synthetic playlist"}
`)
	res, err := handoffNamed(t, "youtube").list(t.Context(), helper, "https://example.test/playlist", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Files) != 2 || res.Note == "" || res.Title != "Synthetic playlist" || res.Files[1].Name != "Second clip" {
		t.Fatalf("playlist = %+v", res)
	}
	got, err := os.ReadFile(args)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "--playlist-end\n3\n--\nhttps://example.test/playlist\n") {
		t.Fatalf("unbounded or unprotected helper arguments: %s", got)
	}
}

func TestHandoffListShortPlaylistHasNoTruncationNote(t *testing.T) {
	helper, _ := metadataScript(t, `{}
null
{"_type":"url","title":"Only clip","url":"https://example.test/only"}
{"_type":"playlist","title":"Synthetic playlist"}`)
	res, err := handoffNamed(t, "youtube").list(t.Context(), helper, "https://example.test/playlist", 3)
	if err != nil || len(res.Files) != 1 || res.Note != "" {
		t.Fatalf("playlist = %+v, %v", res, err)
	}
}

// An entry naming no page of its own is reached through the site's watch
// page where the site has one, and skipped where it does not.
func TestHandoffListEntryByID(t *testing.T) {
	const listing = `{"_type":"url","id":"abcdefghijk","title":"Bare clip"}
{"_type":"playlist","title":"Synthetic playlist"}`
	helper, _ := metadataScript(t, listing)
	res, err := handoffNamed(t, "youtube").list(t.Context(), helper, "https://example.test/playlist", 3)
	if err != nil || len(res.Files) != 1 || res.Files[0].External != handoffNamed(t, "youtube").site.watch+"abcdefghijk" {
		t.Fatalf("playlist = %+v, %v", res, err)
	}
	if _, err := handoffNamed(t, "dailymotion").list(t.Context(), helper, "https://example.test/playlist", 3); err == nil {
		t.Fatal("an entry with no page was listed on a site with no watch page")
	}
}

func TestHandoffListSingleVideoAndTitle(t *testing.T) {
	helper, args := metadataScript(t, `{"title":"Synthetic clip","webpage_url":"https://example.test/clip"}`)
	res, err := handoffNamed(t, "youtube").list(t.Context(), helper, "https://example.test/input", 2)
	if err != nil || len(res.Files) != 1 || res.Files[0].External != "https://example.test/clip" || res.Note != "" {
		t.Fatalf("video = %+v, %v", res, err)
	}
	if title, err := ytdlpTitle(t.Context(), helper, "--untrusted-input"); err != nil || title != "Synthetic clip" {
		t.Fatalf("title = %q, %v", title, err)
	}
	got, err := os.ReadFile(args)
	if err != nil || !strings.Contains(string(got), "--no-playlist\n") || !strings.HasSuffix(string(got), "--\n--untrusted-input\n") {
		t.Fatalf("title helper arguments = %q, %v", got, err)
	}
}

// The probes ask for the fields they read and nothing else. A full -J dump
// of one ordinary video came to ten megabytes, nearly all of it
// machine-translated captions, and failed the job on the cap on helper
// output before anything was downloaded. And they say --simulate outright:
// the playlist: print switches off the simulation --print otherwise implies,
// and a probe without it downloaded the whole video into its working
// directory.
func TestHandoffProbesPrintOnlyTheFieldsTheyRead(t *testing.T) {
	helper, args := metadataScript(t, `{"title":"Synthetic clip"}`)
	if _, err := handoffNamed(t, "youtube").list(t.Context(), helper, "https://example.test/clip", 2); err != nil {
		t.Fatal(err)
	}
	probe, err := os.ReadFile(args)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ytdlpTitle(t.Context(), helper, "https://example.test/clip"); err != nil {
		t.Fatal(err)
	}
	title, err := os.ReadFile(args)
	if err != nil {
		t.Fatal(err)
	}
	for name, got := range map[string]string{"probe": string(probe), "title": string(title)} {
		lines := strings.Split(got, "\n")
		if slices.Contains(lines, "-J") || slices.Contains(lines, "--dump-single-json") || !slices.Contains(lines, "--print") {
			t.Errorf("%s arguments = %q; want selected fields printed, not the whole info dump", name, got)
		}
		if !slices.Contains(lines, "--simulate") {
			t.Errorf("%s arguments = %q; without --simulate the probe downloads the video", name, got)
		}
	}
}

func TestHandoffListThatPrintsNothingIsAnError(t *testing.T) {
	helper, _ := metadataScript(t, "")
	if _, err := handoffNamed(t, "youtube").list(t.Context(), helper, "https://example.test/clip", 2); err == nil {
		t.Fatal("an empty answer from yt-dlp was taken for a video")
	}
}

func TestHandoffName(t *testing.T) {
	for raw, want := range map[string]string{
		// A page suffix belongs to the page, not to the media.
		"https://rumble.com/v000000-a-clip.html":      "v000000-a-clip",
		"https://an-artist.example.test/track/a-song": "a-song",
		"https://www.nicovideo.jp/watch/sm0000000":    "sm0000000",
		"https://www.mixcloud.com/a-user/a-show/":     "a-show",
		// Nothing in the path to name it after.
		"https://soundcloud.com/": "soundcloud.com",
	} {
		u := mustParse(t, raw)
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
		client: testClient(),
		site:   handoffSite{name: "rumble", why: "testing", oembed: oembed},
	}
}

func TestOEmbedTitle(t *testing.T) {
	var asked string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = r.URL.Query().Get("url")
		_, _ = io.WriteString(w, oembedPayload)
	}))
	t.Cleanup(srv.Close)

	u := mustParse(t, "https://example.test/v000000-a-clip.html")
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
