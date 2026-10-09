// SPDX-License-Identifier: MIT

package extractor

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

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

func TestYouTubeBoundsPlaylistAtTheHelper(t *testing.T) {
	helper, args := metadataScript(t, `{"_type":"url","title":"First clip","url":"https://example.test/first"}
{"_type":"url","title":"Second clip","url":"https://example.test/second"}
{"_type":"url","title":"Third clip","url":"https://example.test/third"}
{"_type":"playlist","title":"Synthetic playlist"}
`)
	res, err := NewYouTube().probe(t.Context(), helper, "https://example.test/playlist", 2)
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

func TestYouTubeShortPlaylistHasNoTruncationNote(t *testing.T) {
	helper, _ := metadataScript(t, `{}
null
{"_type":"url","title":"Only clip","url":"https://example.test/only"}
{"_type":"playlist","title":"Synthetic playlist"}`)
	res, err := NewYouTube().probe(t.Context(), helper, "https://example.test/playlist", 3)
	if err != nil || len(res.Files) != 1 || res.Note != "" {
		t.Fatalf("playlist = %+v, %v", res, err)
	}
}

func TestYouTubeSingleVideoAndTitle(t *testing.T) {
	helper, args := metadataScript(t, `{"title":"Synthetic clip","webpage_url":"https://example.test/clip"}`)
	res, err := NewYouTube().probe(t.Context(), helper, "https://example.test/input", 2)
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

// The probe asks for the fields it reads and nothing else. A full -J dump of
// one ordinary video came to ten megabytes, nearly all of it YouTube's
// machine-translated captions, and failed the job on the cap on helper
// output before anything was downloaded. And it says --simulate outright:
// the playlist: print switches off the simulation --print otherwise implies,
// and a probe without it downloaded the whole video into its working
// directory.
func TestYouTubeProbesPrintOnlyTheFieldsTheyRead(t *testing.T) {
	helper, args := metadataScript(t, `{"title":"Synthetic clip"}`)
	if _, err := NewYouTube().probe(t.Context(), helper, "https://example.test/clip", 2); err != nil {
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

func TestYouTubeProbeThatPrintsNothingIsAnError(t *testing.T) {
	helper, _ := metadataScript(t, "")
	if _, err := NewYouTube().probe(t.Context(), helper, "https://example.test/clip", 2); err == nil {
		t.Fatal("an empty answer from yt-dlp was taken for a video")
	}
}
