package download

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// A finished transfer must leave a line in the log at info level: the
// backend log is where a headless run's history lives, and a log that only
// records failures cannot answer what was fetched.
func TestFinishedTransferIsLogged(t *testing.T) {
	var buf bytes.Buffer
	m := &Manager{log: slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))}

	it := &Item{ID: "i1", Name: "clip.mp4", Path: "/tmp/clip.mp4"}
	it.downloaded.Store(4096)
	it.startedAt = time.Now().Add(-2 * time.Second)
	it.finishedAt = time.Now()
	m.logFinishedLocked(it, "download complete")

	line := buf.String()
	for _, want := range []string{"level=INFO", "download complete", "clip.mp4", "bytes=4096", "took="} {
		if !strings.Contains(line, want) {
			t.Errorf("log line %q is missing %q", strings.TrimSpace(line), want)
		}
	}
}

// A skipped item moved no bytes, so it says so rather than reporting a
// zero-byte download over no time at all.
func TestSkippedTransferSaysSo(t *testing.T) {
	var buf bytes.Buffer
	m := &Manager{log: slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))}

	m.logFinishedLocked(&Item{ID: "i2", Name: "clip.mp4", Skipped: true}, "download complete")

	line := strings.TrimSpace(buf.String())
	if !strings.Contains(line, "already downloaded") {
		t.Errorf("a skipped item was logged as %q", line)
	}
	if strings.Contains(line, "bytes=") {
		t.Errorf("a skipped item reported a byte count: %q", line)
	}
}
