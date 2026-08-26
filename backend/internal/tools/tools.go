// Package tools locates the optional external programs the downloader uses
// when they are available.
//
// A binary sitting next to the running executable wins over one on PATH, so
// the static builds `make dependencies` drops in ./bin are picked up without
// touching the system, and without depending on how the service was launched.
package tools

import (
	"os"
	"path/filepath"
	"runtime"
	"sync"
)

// The helper binaries the downloader can put to work. Named here, in the
// package that locates them, so the download engine and the extractors spell
// them identically.
const (
	YtDlp   = "yt-dlp"
	FFmpeg  = "ffmpeg"
	FFprobe = "ffprobe"
)

var (
	mu       sync.Mutex
	resolved = map[string]string{}
)

// Find returns the path to a named tool, preferring one installed alongside
// the running binary. The answer is cached, misses included, so a job that
// needs a tool nobody has installed does not walk PATH once per file.
// Recheck drops the misses when that assumption stops holding.
func Find(name string) (string, bool) {
	mu.Lock()
	defer mu.Unlock()

	if path, ok := resolved[name]; ok {
		return path, path != ""
	}
	path := locate(name)
	resolved[name] = path
	return path, path != ""
}

// Reset clears the cache. Only tests need this.
func Reset() {
	mu.Lock()
	defer mu.Unlock()
	resolved = map[string]string{}
}

// Recheck forgets the tools that were not found, so the next Find looks for
// them again.
//
// A miss is the one cached answer that goes stale: being told a helper is
// missing is exactly what sends someone off to install it, and until now the
// running service went on insisting it was absent. Retrying is when they
// expect that to have been noticed, so that is where this is called from.
// What was found stays found — a path that resolved once does not stop
// existing, and re-walking PATH for it would buy nothing.
func Recheck() {
	mu.Lock()
	defer mu.Unlock()
	for name, path := range resolved {
		if path == "" {
			delete(resolved, name)
		}
	}
}

func locate(name string) string {
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	// Alongside the binary first.
	if exe, err := os.Executable(); err == nil {
		if resolvedExe, err := filepath.EvalSymlinks(exe); err == nil {
			exe = resolvedExe
		}
		candidate := filepath.Join(filepath.Dir(exe), name)
		if executable(candidate) {
			return candidate
		}
	}
	// Then the usual search path.
	if path, err := lookPath(name); err == nil {
		return path
	}
	return ""
}

// executable reports whether a path is a runnable file.
func executable(path string) bool {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return false
	}
	return runtime.GOOS == "windows" || info.Mode().Perm()&0o111 != 0
}

// lookPath is exec.LookPath, kept behind a variable so tests can steer it.
var lookPath = defaultLookPath
