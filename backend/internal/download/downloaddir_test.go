package download

import (
	"bytes"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The destination is no longer settled at startup: the UI can move it while
// the program runs. A directory named that way is no more trustworthy than
// one named on the command line, so it goes through the same checks.

func TestSetDownloadDirCreatesAndAdopts(t *testing.T) {
	m := busyManager(t)
	target := filepath.Join(t.TempDir(), "made", "on", "demand")

	if err := m.SetDownloadDir(target); err != nil {
		t.Fatalf("SetDownloadDir: %v", err)
	}
	if got := m.DownloadDir(); got != target {
		t.Errorf("DownloadDir = %q, want %q", got, target)
	}
	if info, err := os.Stat(target); err != nil || !info.IsDir() {
		t.Errorf("directory was not created: %v", err)
	}
	if got := m.Snapshot().DownloadDir; got != target {
		t.Errorf("snapshot still reports %q — the UI would show the old place", got)
	}
}

func TestSetDownloadDirRefusesTheUnusable(t *testing.T) {
	m := busyManager(t)
	before := m.DownloadDir()

	occupied := filepath.Join(t.TempDir(), "a-file")
	if err := os.WriteFile(occupied, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct{ name, path, hint string }{
		{"a file in the way", occupied, "not a directory"},
		{"nothing at all", "   ", "no download directory"},
	} {
		err := m.SetDownloadDir(tc.path)
		if err == nil || !strings.Contains(err.Error(), tc.hint) {
			t.Errorf("%s: err = %v, want mention of %q", tc.name, err, tc.hint)
		}
	}
	if got := m.DownloadDir(); got != before {
		t.Errorf("a refused change still moved the directory to %q", got)
	}
}

// A path with a leading ~ has to be expanded, exactly as the command line
// expands it — someone typing into the UI means the same thing by it.
func TestSetDownloadDirExpandsHome(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory here")
	}
	m := busyManager(t)

	// A directory under the real home would litter it, so only the
	// resolution is checked, against a path that already exists.
	if err := m.SetDownloadDir("~"); err != nil {
		t.Fatalf("SetDownloadDir(~): %v", err)
	}
	if got := m.DownloadDir(); got != home {
		t.Errorf("~ resolved to %q, want %q", got, home)
	}
}

// The point of the feature: what is queued follows the new destination.
func TestQueuedTransfersFollowTheNewDirectory(t *testing.T) {
	payload := bytes.Repeat([]byte{4}, 2048)
	srv := httptest.NewServer((&rangeServer{payload: payload}).handler())
	defer srv.Close()

	m := busyManager(t)
	moved := filepath.Join(t.TempDir(), "elsewhere")
	if err := m.SetDownloadDir(moved); err != nil {
		t.Fatal(err)
	}

	m.Start()
	if _, err := m.Add(srv.URL+"/file.bin", ""); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(filepath.Join(moved, "file.bin")); err == nil {
			got, err := os.ReadFile(filepath.Join(moved, "file.bin"))
			if err != nil || !bytes.Equal(got, payload) {
				t.Fatalf("file at the new destination is wrong: %v", err)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("nothing arrived in %s", moved)
}
