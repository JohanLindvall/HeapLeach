package extractor

import (
	"os"
	"path/filepath"
	"runtime"
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
	helper, args := metadataScript(t, `{"_type":"playlist","title":"Synthetic playlist","entries":[
		{"title":"First clip","url":"https://example.test/first"},
		{"title":"Second clip","url":"https://example.test/second"},
		{"title":"Third clip","url":"https://example.test/third"}]}`)
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
	helper, _ := metadataScript(t, `{"_type":"playlist","title":"Synthetic playlist","entries":[{},null,
		{"title":"Only clip","url":"https://example.test/only"}]}`)
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
