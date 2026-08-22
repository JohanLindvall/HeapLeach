package download

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/JohanLindvall/HeapLeach/internal/tools"
	"github.com/JohanLindvall/HeapLeach/internal/util"
)

// External downloads: pages where reaching the media needs more than an
// HTTP request, and a purpose-built tool already solves it.
//
// The work is done by a helper script rather than inline here, so the whole
// recipe — format selection, merging, retries — sits in one readable place
// and can be changed without rebuilding. The script reports progress on
// stdout, which is folded back into the normal item state so these
// downloads look and behave like any other in the UI.

// transferExternal runs the helper script for one page and records where it
// put the file.
func (m *Manager) transferExternal(ctx context.Context, it *Item, dir, rel string) error {
	m.mu.Lock()
	source := it.External
	m.mu.Unlock()

	script, cleanup, err := tools.YouTubeScript()
	if err != nil {
		return err
	}
	defer cleanup()

	ytdlp, ok := tools.Find("yt-dlp")
	if !ok {
		return fmt.Errorf("yt-dlp is not installed — run `make dependencies` to fetch it " +
			"into the folder holding heapleach, or put it on PATH")
	}

	cmd := exec.CommandContext(ctx, script, source, dir)
	cmd.Env = append(os.Environ(), "YTDLP="+ytdlp)
	// ffmpeg is optional: without it the script asks for a single already
	// muxed stream instead of a pair it could not join.
	if ffmpeg, ok := tools.Find("ffmpeg"); ok {
		cmd.Env = append(cmd.Env, "FFMPEG="+ffmpeg)
	}
	// The helper spawns yt-dlp, which spawns ffmpeg; cancelling has to take
	// the whole group down, not just the script.
	setProcessGroup(cmd)
	cmd.Cancel = func() error { return killGroup(cmd) }

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start downloader: %w", err)
	}

	it.streams.Store(1)
	defer it.streams.Store(0)

	// Drained before Wait, which closes the pipe out from under a reader.
	produced := m.readExternalProgress(it, stdout)

	if err := cmd.Wait(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if detail := strings.TrimSpace(stderr.String()); detail != "" {
			return fmt.Errorf("%s", util.Truncate(detail, 300))
		}
		return fmt.Errorf("downloader failed: %w", err)
	}
	if produced == "" {
		return fmt.Errorf("the downloader did not report a finished file")
	}

	info, err := os.Stat(produced)
	if err != nil {
		return fmt.Errorf("downloader reported %s, which is not there: %w", filepath.Base(produced), err)
	}
	if converted := m.remuxToMP4(ctx, produced); converted != produced {
		produced = converted
		if info, err = os.Stat(produced); err != nil {
			return fmt.Errorf("converted file is missing: %w", err)
		}
	}

	m.mu.Lock()
	it.Name = filepath.Base(produced)
	it.Size = info.Size()
	m.mu.Unlock()
	it.downloaded.Store(info.Size())
	m.setPath(it, filepath.Join(rel, filepath.Base(produced)))
	return nil
}

// readExternalProgress folds the helper's output into the item's state and
// returns the path it reported writing.
func (m *Manager) readExternalProgress(it *Item, stdout io.Reader) string {
	var produced string

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		switch {
		case strings.HasPrefix(line, "PROGRESS "):
			done, total, ok := parseProgress(strings.TrimPrefix(line, "PROGRESS "))
			if !ok {
				continue
			}
			it.downloaded.Store(done)
			if total > 0 {
				m.mu.Lock()
				it.Size = total
				m.mu.Unlock()
			}
		case strings.HasPrefix(line, "FILE "):
			produced = strings.TrimSpace(strings.TrimPrefix(line, "FILE "))
		}
	}
	return produced
}

// parseProgress reads a "<downloaded> <total>" pair, tolerating the "NA"
// yt-dlp emits when a total is not yet known.
func parseProgress(fields string) (done, total int64, ok bool) {
	parts := strings.Fields(fields)
	if len(parts) == 0 {
		return 0, 0, false
	}
	done, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return 0, 0, false
	}
	if len(parts) > 1 {
		total, _ = strconv.ParseInt(parts[1], 10, 64)
	}
	return done, total, true
}
