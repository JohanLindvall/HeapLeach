// SPDX-License-Identifier: MIT

//go:build unix

package download

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/tools"
)

func installExternalFixture(t *testing.T, script string) {
	t.Helper()
	dir := t.TempDir()
	for name, body := range map[string]string{tools.ScriptName: script, tools.YtDlp: "exit 0\n"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	tools.Reset()
	t.Cleanup(tools.Reset)
}

func TestExternalHelperProgressAndFinishedFile(t *testing.T) {
	installExternalFixture(t, `printf hello > "$2/result.bin"
printf 'PROGRESS 5 5\nFILE %s/result.bin\n' "$2"
`)
	m := progressManager(t, 0)
	it := &Item{External: "https://example.test/clip", Name: "Synthetic clip"}
	if err := m.transferExternal(t.Context(), it, m.DownloadDir(), ""); err != nil {
		t.Fatal(err)
	}
	if it.Name != "result.bin" || it.downloaded.Load() != 5 || it.Size != 5 || it.Path != "result.bin" {
		t.Fatalf("external result = %+v", it)
	}
}

func TestExternalHelperCannotLeaveProgressPipeOpenForever(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "child-pid")
	t.Setenv("HEAPLEACH_TEST_CHILD_PID", pidFile)
	installExternalFixture(t, `sleep 30 &
printf '%s' "$!" > "$HEAPLEACH_TEST_CHILD_PID"
exit 0
`)
	t.Cleanup(func() {
		if data, err := os.ReadFile(pidFile); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil {
				if child, err := os.FindProcess(pid); err == nil {
					_ = child.Kill()
				}
			}
		}
	})
	m := progressManager(t, 0)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	it := &Item{External: "https://example.test/clip"}
	err := m.transferExternal(ctx, it, m.DownloadDir(), "")
	if !errors.Is(err, exec.ErrWaitDelay) {
		t.Fatalf("inherited progress pipe did not hit the helper wait deadline: %v", err)
	}
}

// yt-dlp's pieces carry media extensions, so a library watching the
// destination took each stream and the half-written merge for a video of its
// own. They are kept in a hidden directory beside it, and only the finished
// file is left once the download is done.
func TestExternalPiecesStayHiddenAndGoWhenDone(t *testing.T) {
	installExternalFixture(t, `case "$PARTS" in "$2"/.*) ;; *) echo "pieces not hidden beside the destination: $PARTS" >&2; exit 1 ;; esac
mkdir -p "$PARTS" && printf video > "$PARTS/clip.f401.mp4" && printf audio > "$PARTS/clip.f251-0.webm"
printf joined > "$2/clip.mkv"
printf 'PROGRESS 6 6\nFILE %s/clip.mkv\n' "$2"
`)
	m := progressManager(t, 0)
	it := &Item{External: "https://example.test/clip", Name: "Synthetic clip"}
	if err := m.transferExternal(t.Context(), it, m.DownloadDir(), ""); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(m.DownloadDir())
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	if len(names) != 1 || names[0] != "clip.mkv" {
		t.Fatalf("destination holds %q, want only the finished file", names)
	}
}

// A failed attempt keeps its pieces, and the next one is pointed at the
// same place, so it carries on rather than starting over.
func TestExternalPiecesSurviveAFailedAttempt(t *testing.T) {
	installExternalFixture(t, `mkdir -p "$PARTS" && printf video > "$PARTS/clip.f401.mp4"
echo "connection reset" >&2
exit 1
`)
	m := progressManager(t, 0)
	it := &Item{External: "https://example.test/clip"}
	if err := m.transferExternal(t.Context(), it, m.DownloadDir(), ""); err == nil {
		t.Fatal("a failing helper reported success")
	}
	if found, _ := filepath.Glob(filepath.Join(m.DownloadDir(), externalParts, "*", "clip.f401.mp4")); len(found) != 1 {
		t.Fatalf("pieces after a failed attempt = %q, want them kept", found)
	}

	installExternalFixture(t, `test -f "$PARTS/clip.f401.mp4" || { echo "the next attempt was not given the pieces" >&2; exit 1; }
printf joined > "$2/clip.mkv"
printf 'FILE %s/clip.mkv\n' "$2"
`)
	if err := m.transferExternal(t.Context(), it, m.DownloadDir(), ""); err != nil {
		t.Fatal(err)
	}
}

// The script itself hands the directory to yt-dlp as its temp path, which is
// what keeps the pieces out of the destination.
func TestDownloadScriptGivesYtDlpTheHiddenTempPath(t *testing.T) {
	bin := t.TempDir()
	args := filepath.Join(t.TempDir(), "args")
	t.Setenv("HEAPLEACH_TEST_ARGS", args)
	if err := os.WriteFile(filepath.Join(bin, tools.YtDlp), []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$HEAPLEACH_TEST_ARGS\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	tools.Reset()
	t.Cleanup(tools.Reset)

	m := progressManager(t, 0)
	it := &Item{External: "https://example.test/clip"}
	// The fake reports no file, so the transfer fails; only what it was
	// asked to do matters here.
	_ = m.transferExternal(t.Context(), it, m.DownloadDir(), "")
	got, err := os.ReadFile(args)
	if err != nil {
		t.Fatal(err)
	}
	want := "--paths\ntemp:" + filepath.Join(m.DownloadDir(), externalParts, partSuffix(it.External)) + "\n"
	if !strings.Contains(string(got), want) {
		t.Fatalf("yt-dlp was run with %q, want the temp path %q", got, want)
	}
}
