package tools

import (
	"errors"
	"os"
	"os/exec"
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

func TestScriptDirIsWhereTheBinaryLives(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Skip("no executable path on this platform")
	}
	if got, want := ScriptDir(), filepath.Dir(exe); got != want {
		t.Errorf("ScriptDir = %q, want %q", got, want)
	}
}

func TestNotInstalledNamesTheToolAndTheRemedy(t *testing.T) {
	msg := NotInstalled(YtDlp)
	for _, want := range []string{YtDlp, "make dependencies", "PATH"} {
		if !strings.Contains(msg, want) {
			t.Errorf("%q does not mention %q", msg, want)
		}
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

// The script turns DENO into the flag yt-dlp wants, and asks for nothing
// when there is no runtime to point at.
//
// yt-dlp finds deno on PATH unaided, so this only matters for the copy the
// service keeps next to its binary — which is the copy `make dependencies`
// installs, and the one PATH does not reach.
func TestDownloadScriptPassesOnAJSRuntime(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the download helper is a shell script")
	}
	dir := t.TempDir()

	// A stand-in for yt-dlp that reports the arguments it was handed.
	ytdlp := filepath.Join(dir, "yt-dlp-stub")
	stub := "#!/bin/sh\nfor a in \"$@\"; do echo \"$a\"; done\n"
	if err := os.WriteFile(ytdlp, []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}

	script := filepath.Join(dir, ScriptName)
	if err := os.WriteFile(script, ytDownloadScript, 0o700); err != nil {
		t.Fatal(err)
	}

	// A bare environment: an inherited DENO or FFMPEG would decide the
	// answer before the test did.
	run := func(extra ...string) string {
		t.Helper()
		cmd := exec.Command(script, "https://example.test/watch", dir)
		cmd.Env = append([]string{"PATH=" + os.Getenv("PATH"), "YTDLP=" + ytdlp}, extra...)
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("running the helper: %v", err)
		}
		return string(out)
	}

	if got := run(); strings.Contains(got, "--js-runtimes") {
		t.Errorf("nothing to point at, but the script asked for a runtime anyway:\n%s", got)
	}

	deno := filepath.Join(dir, "deno")
	got := run("DENO=" + deno)
	if !strings.Contains(got, "--js-runtimes") || !strings.Contains(got, "deno:"+deno) {
		t.Errorf("DENO was set, but the script did not pass it on:\n%s", got)
	}
}

// runDownloadScript runs the helper against a stand-in yt-dlp and returns the
// arguments it was handed. The stub answers the language probe from `langs`
// and echoes its arguments for the download call, which is how the format
// the script settled on can be read back.
func runDownloadScript(t *testing.T, langs string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the download helper is a shell script")
	}
	dir := t.TempDir()

	ytdlp := filepath.Join(dir, "yt-dlp-stub")
	// --simulate is what separates the two calls: the probe passes it and
	// the download does not. Keying on --print would match both, since the
	// download uses it to report the finished file.
	stub := "#!/bin/sh\n" +
		"for a in \"$@\"; do\n" +
		"  if [ \"$a\" = \"--simulate\" ]; then echo '" + langs + "'; exit 0; fi\n" +
		"done\n" +
		"for a in \"$@\"; do echo \"$a\"; done\n"
	if err := os.WriteFile(ytdlp, []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}

	script := filepath.Join(dir, ScriptName)
	if err := os.WriteFile(script, ytDownloadScript, 0o700); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(script, "https://example.test/watch", dir)
	// FFMPEG set, since merging is what any of this depends on.
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "YTDLP=" + ytdlp, "FFMPEG=/usr/bin/true"}
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("running the helper: %v", err)
	}
	return string(out)
}

// A dubbed release is the case this exists for: the default selector takes
// the single best audio, so a video whose original track is Spanish loses the
// English dub beside it. Each language is asked for by name instead.
func TestDownloadScriptAsksForEveryAudioLanguage(t *testing.T) {
	// The shape yt-dlp reports: one entry per format, so languages repeat.
	got := runDownloadScript(t, "['ar', 'en-US', 'pt-BR', 'ar', 'es-US', 'en-US']")

	for _, want := range []string{
		"ba[language=ar]", "ba[language=en-US]", "ba[language=es-US]", "ba[language=pt-BR]",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("selector does not ask for %s:\n%s", want, got)
		}
	}
	// Named once each, however many formats carried them.
	if n := strings.Count(got, "ba[language=ar]"); n != 1 {
		t.Errorf("Arabic asked for %d times, want once", n)
	}
	// Merging drops the per-track tags without this, leaving every language
	// labelled the same and the choice they were fetched for unavailable.
	if !strings.Contains(got, "--embed-metadata") {
		t.Error("several languages merged without the metadata pass that labels them")
	}
	// And more than one is only kept if this is asked for.
	if !strings.Contains(got, "--audio-multistreams") {
		t.Error("the extra languages would be downloaded and then discarded")
	}
}

// One language needs no naming, and must not pay for a second pass over the
// finished file to relabel a track that is already right.
func TestDownloadScriptLeavesASingleLanguageAlone(t *testing.T) {
	got := runDownloadScript(t, "['en', 'en', 'en']")

	if strings.Contains(got, "language=") {
		t.Errorf("a single-language video was asked for by name:\n%s", got)
	}
	if !strings.Contains(got, "bv*+ba/b") {
		t.Errorf("selector = %s, want the plain pair", got)
	}
	if strings.Contains(got, "--embed-metadata") {
		t.Error("a second pass over the file was run with nothing to relabel")
	}
}

// A probe that answers with nothing usable must not stop the download: one
// language working beats no download at all.
func TestDownloadScriptFallsBackWhenTheProbeSaysNothing(t *testing.T) {
	for _, langs := range []string{"", "[]", "['none', 'None']", "garbage output"} {
		got := runDownloadScript(t, langs)
		if !strings.Contains(got, "bv*+ba/b") {
			t.Errorf("probe %q gave selector:\n%s\nwant the fallback", langs, got)
		}
		if strings.Contains(got, "--embed-metadata") {
			t.Errorf("probe %q asked for a metadata pass it has no languages for", langs)
		}
	}
}
