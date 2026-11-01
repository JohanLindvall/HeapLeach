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
	"github.com/JohanLindvall/HeapLeach/internal/tools"
)

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
	// A video opened from a playlist is the one video: yt-dlp is told so,
	// and lists a playlist's own link as the playlist still.
	if !strings.Contains(string(got), "--no-playlist\n") {
		t.Fatalf("a watch link with a list would expand to the playlist: %s", got)
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

// A private track's canonical webpage_url can omit the secret that let
// the metadata helper read it. The later download needs the same input.
func TestHandoffSingleTrackKeepsSubmittedURL(t *testing.T) {
	for _, tc := range []struct {
		name, input string
	}{
		{"secret path", "https://audio.example.test/an-artist/a-recording/s-synthetic"},
		{"secret query", "https://audio.example.test/an-artist/a-recording?secret_token=s-synthetic%2Btoken"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			helper, args := metadataScript(t, `{"title":"Synthetic recording","webpage_url":"https://audio.example.test/an-artist/a-recording"}`)
			dir := filepath.Dir(helper)
			if err := os.Rename(helper, filepath.Join(dir, tools.YtDlp)); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", dir)
			tools.Reset()
			t.Cleanup(tools.Reset)
			res, err := handoffNamed(t, "soundcloud").Extract(t.Context(), mustParse(t, tc.input), Options{})
			if err != nil {
				t.Fatal(err)
			}
			if len(res.Files) != 1 || res.Files[0].External != tc.input || res.Files[0].Name != "Synthetic recording" {
				t.Fatalf("track = %+v; want the submitted URL and metadata title", res.Files)
			}
			got, err := os.ReadFile(args)
			if err != nil || !strings.HasSuffix(string(got), "--\n"+tc.input+"\n") {
				t.Fatalf("metadata helper arguments = %q, %v; want the submitted URL", got, err)
			}
		})
	}
}

// Even a one-track set must queue its entry, not the submitted set URL.
func TestHandoffSingleEntrySetUsesTrackURL(t *testing.T) {
	const track = "https://audio.example.test/an-artist/first-recording/s-track-secret"
	helper, _ := metadataScript(t, `{"_type":"url","title":"First recording","url":"`+track+`"}
{"_type":"playlist","title":"Synthetic set"}`)
	res, err := handoffNamed(t, "soundcloud").list(t.Context(), helper,
		"https://audio.example.test/an-artist/sets/a-set/s-set-secret", 2)
	if err != nil || len(res.Files) != 1 || res.Files[0].External != track || res.Title != "Synthetic set" {
		t.Fatalf("set = %+v, %v; want its listed track", res, err)
	}
}

// A single untitled item a listing site hands back is named by its page's
// slug, as one handed over unlisted is; and a failed probe names the link
// without its password.
func TestHandoffListSingleUntitledItemAndRedactedErrors(t *testing.T) {
	helper, _ := metadataScript(t, `{"id":"x8abcde","webpage_url":"https://example.test/video/x8abcde"}`)
	res, err := handoffNamed(t, "odysee").list(t.Context(), helper, "https://example.test/@a-channel/a-claim", 2)
	if err != nil || len(res.Files) != 1 || res.Title != "a-claim" || res.Files[0].Name != "a-claim" {
		t.Fatalf("video = %+v, %v", res, err)
	}
	failing := filepath.Join(t.TempDir(), "failing")
	if err := os.WriteFile(failing, []byte("#!/bin/sh\necho refused >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err = handoffNamed(t, "odysee").list(t.Context(), failing, "https://user:secret@example.test/@a-channel/a-claim", 2)
	if err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatalf("err = %v, want the link without its password", err)
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
	return newHandoff(testClient(), handoffSite{name: "rumble", why: "testing", oembed: oembed})
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
