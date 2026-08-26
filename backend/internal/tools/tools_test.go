package tools

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// stubLookPath steers the PATH half of the search and restores it afterwards.
func stubLookPath(t *testing.T, fn func(string) (string, error)) {
	t.Helper()
	orig := lookPath
	lookPath = fn
	t.Cleanup(func() { lookPath = orig; Reset() })
	Reset()
}

func TestFindFallsBackToPath(t *testing.T) {
	stubLookPath(t, func(name string) (string, error) {
		if strings.HasPrefix(name, "sometool") {
			return "/usr/bin/" + name, nil
		}
		return "", errors.New("not found")
	})

	path, ok := Find("sometool")
	if !ok || !strings.HasPrefix(path, "/usr/bin/sometool") {
		t.Errorf("Find = %q, %v", path, ok)
	}
	if _, ok := Find("absent"); ok {
		t.Error("a tool nowhere to be found reported as present")
	}
}

func TestFindCachesItsAnswer(t *testing.T) {
	var calls int
	stubLookPath(t, func(name string) (string, error) {
		calls++
		return "", errors.New("not found")
	})

	Find("cached")
	Find("cached")
	if calls != 1 {
		t.Errorf("lookup ran %d times, want once — a job of many files asks repeatedly", calls)
	}

	// Reset is what lets a test — or a future reload — look again.
	Reset()
	Find("cached")
	if calls != 2 {
		t.Errorf("Reset should clear the cache, lookups = %d", calls)
	}
}

func TestRecheckForgetsOnlyTheMisses(t *testing.T) {
	var calls int
	stubLookPath(t, func(name string) (string, error) {
		calls++
		if name == "installed" {
			return "/usr/bin/installed", nil
		}
		return "", errors.New("not found")
	})

	if _, ok := Find("installed"); !ok {
		t.Fatal("Find did not see the tool the stub offers")
	}
	if _, ok := Find("missing"); ok {
		t.Fatal("Find reported a tool the stub does not offer")
	}
	if calls != 2 {
		t.Fatalf("setup took %d lookups, want 2", calls)
	}

	Recheck()

	// The one that was found is still cached, so nothing is looked up again.
	if _, ok := Find("installed"); !ok || calls != 2 {
		t.Errorf("a hit was thrown away: ok = %v, lookups = %d, want true and 2", ok, calls)
	}
	// The one that was not is looked for afresh — which is the whole point:
	// it may have been installed since it was last missed.
	if _, ok := Find("missing"); ok || calls != 3 {
		t.Errorf("a miss was not rechecked: ok = %v, lookups = %d, want false and 3", ok, calls)
	}
}

// A tool installed after the service gave up on it is the case this exists
// for, so it is worth stating outright rather than only in the negative.
func TestRecheckSeesAToolInstalledSince(t *testing.T) {
	present := false
	stubLookPath(t, func(name string) (string, error) {
		if present {
			return "/usr/bin/" + name, nil
		}
		return "", errors.New("not found")
	})

	if _, ok := Find("latecomer"); ok {
		t.Fatal("the tool is not there yet")
	}

	present = true
	if _, ok := Find("latecomer"); ok {
		t.Error("without a recheck the cached miss should still stand")
	}

	Recheck()
	if path, ok := Find("latecomer"); !ok || path != "/usr/bin/latecomer" {
		t.Errorf("after Recheck: Find = %q, %v — want the installed path", path, ok)
	}
}

func TestExecutable(t *testing.T) {
	dir := t.TempDir()

	runnable := filepath.Join(dir, "runnable")
	if err := os.WriteFile(runnable, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	plain := filepath.Join(dir, "plain")
	if err := os.WriteFile(plain, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}

	if !executable(runnable) {
		t.Error("an executable file reported as not runnable")
	}
	if runtime.GOOS != "windows" && executable(plain) {
		t.Error("a mode-0644 file reported as runnable")
	}
	if executable(dir) {
		t.Error("a directory reported as runnable")
	}
	if executable(filepath.Join(dir, "missing")) {
		t.Error("a missing path reported as runnable")
	}
}

func TestYouTubeScriptStagesTheEmbeddedCopy(t *testing.T) {
	// Nothing installed anywhere: the embedded script is written out.
	stubLookPath(t, func(string) (string, error) { return "", errors.New("not found") })

	path, cleanup, err := YouTubeScript()
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(body) == 0 || !strings.HasPrefix(string(body), "#!") {
		t.Errorf("staged script does not look like a script: %.40q", body)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o100 == 0 {
		t.Error("staged script is not executable")
	}

	cleanup()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("cleanup left the staged script behind")
	}
}

func TestYouTubeScriptPrefersAnInstalledCopy(t *testing.T) {
	stubLookPath(t, func(name string) (string, error) {
		if name == ScriptName {
			return "/opt/heapleach/" + ScriptName, nil
		}
		return "", errors.New("not found")
	})

	path, cleanup, err := YouTubeScript()
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if path != "/opt/heapleach/"+ScriptName {
		t.Errorf("an installed copy must win over the embedded one, got %q", path)
	}
}
