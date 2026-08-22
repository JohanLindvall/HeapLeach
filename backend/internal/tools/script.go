package tools

import (
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
)

//go:embed scripts/yt-download.sh
var ytDownloadScript []byte

// ScriptName is the download helper's filename. A copy next to the binary
// takes precedence over the built-in one, so the recipe can be adjusted
// without rebuilding.
const ScriptName = "yt-download.sh"

// YouTubeScript returns a path to the download helper, writing out the
// built-in copy when none is installed alongside the binary. The returned
// cleanup removes anything it created.
func YouTubeScript() (path string, cleanup func(), err error) {
	if installed, ok := Find(ScriptName); ok {
		return installed, func() {}, nil
	}

	file, err := os.CreateTemp("", "down-yt-*.sh")
	if err != nil {
		return "", nil, fmt.Errorf("stage download script: %w", err)
	}
	name := file.Name()
	remove := func() { _ = os.Remove(name) }

	if _, err := file.Write(ytDownloadScript); err != nil {
		_ = file.Close()
		remove()
		return "", nil, fmt.Errorf("stage download script: %w", err)
	}
	if err := file.Close(); err != nil {
		remove()
		return "", nil, fmt.Errorf("stage download script: %w", err)
	}
	if err := os.Chmod(name, 0o700); err != nil {
		remove()
		return "", nil, fmt.Errorf("stage download script: %w", err)
	}
	return name, remove, nil
}

// ScriptDir is where an override copy of the script would live.
func ScriptDir() string {
	if exe, err := os.Executable(); err == nil {
		return filepath.Dir(exe)
	}
	return "."
}
