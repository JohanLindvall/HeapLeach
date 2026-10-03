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
