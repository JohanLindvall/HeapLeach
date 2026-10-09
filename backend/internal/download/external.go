// SPDX-License-Identifier: MIT

package download

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/JohanLindvall/HeapLeach/internal/config"
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

	ytdlp, ok := tools.Find(tools.YtDlp)
	if !ok {
		return errors.New(tools.NotInstalled(tools.YtDlp))
	}

	cmd := tools.CommandContext(ctx, script, source, dir)
	cmd.Env = append(os.Environ(), "YTDLP="+ytdlp)
	// ffmpeg is optional: without it the script asks for a single already
	// muxed stream instead of a pair it could not join.
	if ffmpeg, ok := tools.Find(tools.FFmpeg); ok {
		cmd.Env = append(cmd.Env, "FFMPEG="+ffmpeg)
	}
	// So is a JavaScript runtime, but less so every month: YouTube signs its
	// media URLs from player code that has to be run to be answered, and
	// yt-dlp has deprecated extracting without one. It finds deno on PATH by
	// itself; passing a path is what reaches a copy sitting next to the
	// binary, where `make dependencies` puts it.
	if deno, ok := tools.Find(tools.Deno); ok {
		cmd.Env = append(cmd.Env, "DENO="+deno)
	}
	// yt-dlp fetches each stream to a file of its own and joins them at the
	// end, and every one of those files carries a media extension: a library
	// watching the destination saw a dubbed video's twenty streams and its
	// half-written merge as twenty-one videos. So they are kept in a hidden
	// directory beside the destination, on the same filesystem so that the
	// finished file arrives by rename, and only that file comes out of it.
	// Named for the page, so an interrupted download finds its pieces again.
	parts := filepath.Join(externalParts, partSuffix(source))
	cmd.Env = append(cmd.Env, "PARTS="+filepath.Join(dir, parts))
	// Let os/exec own its stdout copy, so WaitDelay can close a pipe a
	// helper's child inherited after the parent exits. Draining StdoutPipe
	// before calling Wait would never start that deadline.
	stdout, output := io.Pipe()
	defer stdout.Close()
	defer output.Close()
	cmd.Stdout = output
	stderr := util.BoundedBuffer{Limit: config.ErrorBodySample}
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start downloader: %w", err)
	}

	it.streams.Store(1)
	defer it.streams.Store(0)

	progress := make(chan string, 1)
	go func() { progress <- m.readExternalProgress(it, stdout) }()
	err = cmd.Wait()
	output.Close()
	produced := <-progress

	if err != nil {
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
	clearExternalParts(dir, parts)
	return nil
}

// externalParts is the hidden directory, beside the destination, that an
// external download keeps its pieces in while it runs.
const externalParts = ".heapleach"

// clearExternalParts removes a finished download's pieces, and the hidden
// directory itself once nothing else is in it: another download into the
// same folder may be using it. A failed one keeps its pieces for the next
// attempt to carry on from. The removal goes through a root opened on the
// destination, so a symlink in place of the directory cannot aim it
// anywhere else.
func clearExternalParts(dir, parts string) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return
	}
	defer root.Close()
	_ = root.RemoveAll(parts)
	_ = root.Remove(externalParts)
}

// readExternalProgress folds the helper's output into the item's state and
// returns the path it reported writing.
func (m *Manager) readExternalProgress(it *Item, stdout io.Reader) string {
	var produced string

	// yt-dlp reports one file at a time, and a merged download is two of
	// them: the video stream and then the audio, each counting from zero.
	// Passed straight through, the item would show whichever is in flight —
	// finishing at "5 MB of 5 MB" for a download of 154. So the streams
	// already done are carried in base and every report is added to it.
	var (
		stream   string
		base     int64
		streamed int64
	)

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		switch {
		case strings.HasPrefix(line, "PROGRESS "):
			done, total, id, ok := parseProgress(strings.TrimPrefix(line, "PROGRESS "))
			if !ok {
				continue
			}
			if newExternalStream(id, stream, done, streamed) {
				base += streamed
				streamed = 0
			}
			stream = id
			streamed = done

			it.downloaded.Store(base + done)
			if total > 0 {
				m.mu.Lock()
				it.Size = base + total
				m.mu.Unlock()
			}
		case strings.HasPrefix(line, "FILE "):
			produced = strings.TrimSpace(strings.TrimPrefix(line, "FILE "))
		}
	}
	// A line too long to scan ends the loop early. The helper still has to
	// be drained, or it blocks on a full pipe and Wait never returns.
	if err := scanner.Err(); err != nil {
		m.log.Debug("external progress unreadable", "item", it.ID, "err", err)
		_, _ = io.Copy(io.Discard, stdout)
	}
	return produced
}

// newExternalStream reports whether this line belongs to a different file
// from the last one.
//
// The format id says so outright, and is what the shipped helper reports. A
// copy of that script installed beside the binary overrides the built-in one
// and may predate the field, so a counter that has collapsed to a fraction
// of where it was stands in: fetching several fragments at once makes the
// figure wobble backwards by a fraction of a percent, which this is well
// clear of, while a new stream restarts it from almost nothing.
func newExternalStream(id, previous string, done, streamed int64) bool {
	if id != "" || previous != "" {
		return id != previous
	}
	return streamed > 0 && done < streamed-done
}

// parseProgress reads a "<downloaded> <total> [<format-id>]" line, tolerating
// the "NA" yt-dlp emits when a total is not yet known.
func parseProgress(fields string) (done, total int64, stream string, ok bool) {
	parts := strings.Fields(fields)
	if len(parts) == 0 {
		return 0, 0, "", false
	}
	done, ok = parseByteCount(parts[0])
	if !ok {
		return 0, 0, "", false
	}
	if len(parts) > 1 {
		// Only on success: a discarded ok would let a refused value through
		// as the total, which is the shape of the bug this replaced.
		if n, valid := parseByteCount(parts[1]); valid {
			total = n
		}
	}
	// The format id is last and optional, so an older helper script that
	// does not report one still parses.
	if len(parts) > 2 && parts[2] != "NA" {
		stream = parts[2]
	}
	return done, total, stream, true
}

// parseByteCount reads a count yt-dlp may state as an integer or as a float.
//
// A fragmented download has no length to report — the manifest names parts,
// not bytes — so yt-dlp answers with a running estimate averaged over the
// fragments so far, and that arrives as "129456458.22222222". Read as an
// integer it is not a number at all, which is how a whole download came to
// report its size as the first estimate that happened to land on a whole
// byte: every honest one after it was discarded, leaving "158 MB / 712 B",
// a bar pinned at full and no ETA.
//
// The fractional part is an artefact of the averaging rather than a quantity,
// so it is truncated. Anything not a finite, non-negative number is refused
// rather than folded to zero, since zero here means "no total yet".
func parseByteCount(s string) (int64, bool) {
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		if n < 0 {
			return 0, false
		}
		return n, true
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) || f < 0 || f >= 0x1p63 {
		return 0, false
	}
	return int64(f), true
}
