// SPDX-License-Identifier: MIT

package download

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/extractor"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
	"github.com/JohanLindvall/HeapLeach/internal/tools"
)

// A playlist's subtitles arrive as WebVTT: as one file, the way most hosts
// serve them, or as a run of segments, each a WebVTT file of its own whose
// header says where its cues sit on the video's timeline. Each track's are
// fetched at the start of the attempt, while their links are freshest,
// merged into one WebVTT file beside the part (mergeWebVTT), and joined into
// the MP4 as text tracks with the audio (mux.go).
//
// A subtitle is a bonus to the video, never a condition of it. A track that
// cannot be fetched or read is left out, said so in the log, and the video
// goes on without it.

// subtitleFile is one subtitle track fetched for the join.
type subtitleFile struct {
	path  string
	track extractor.Track
	// offset is what to add to the file's cue times to put them on the
	// video's timeline, and mapped whether the WebVTT said where they sit.
	// An unmapped file's cues count from the start of the video, which only
	// the video can say (mux.go asks it).
	offset float64
	mapped bool
}

// subtitlePart names the WebVTT file of an item's i-th subtitle track. It
// keeps the ".part" ending, which no library watching the folder takes for
// a subtitle.
func subtitlePart(part string, i int) string {
	return strings.TrimSuffix(part, ".part") + ".sub" + strconv.Itoa(i+1) + ".part"
}

// fetchSubtitles fetches and merges each subtitle track, returning those
// that could be. Nothing is fetched when ffmpeg, which joins them, is
// missing. A file left from an earlier attempt that this one could not
// replace is removed, so that the join never takes one that may belong to
// another track.
func (m *Manager) fetchSubtitles(ctx context.Context, it *Item, part string, tracks []extractor.Track,
	headers httpx.Header, files transferFiles) []subtitleFile {
	if len(tracks) == 0 {
		return nil
	}
	if _, ok := tools.Find(tools.FFmpeg); !ok {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, config.SubtitleFetchTimeout)
	defer cancel()

	m.note(it, "fetching "+plural(len(tracks), "subtitle track"))
	defer m.note(it, "")
	var out []subtitleFile
	for i, track := range tracks {
		path := subtitlePart(part, i)
		merged, err := m.fetchWebVTT(ctx, it, track, headers)
		if err == nil {
			err = writeFile(files, path, merged.data)
		}
		if err != nil {
			_ = files.remove(path)
			m.log.Info("subtitle track left out", "item", it.ID, "track",
				cmp.Or(track.Name, track.Language, strconv.Itoa(i+1)), "err", err)
			continue
		}
		out = append(out, subtitleFile{path: path, track: track, offset: merged.offset, mapped: merged.mapped})
	}
	return out
}

// writeFile writes a whole file under a temporary name and renames it into
// place, so that a file found there is always a whole one.
func writeFile(files transferFiles, path string, data []byte) error {
	tmp := path + ".tmp"
	if err := files.write(tmp, data); err != nil {
		_ = files.remove(tmp)
		return err
	}
	return files.rename(tmp, path)
}

// fetchWebVTT fetches one track's WebVTT files, several at a time, and
// merges them.
func (m *Manager) fetchWebVTT(ctx context.Context, it *Item, track extractor.Track, headers httpx.Header) (webVTT, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var (
		fetched atomic.Int64
		wg      sync.WaitGroup
		mu      sync.Mutex
		failure error
	)
	parts := make([][]byte, len(track.Segments))
	slots := make(chan struct{}, config.SubtitleFetchConcurrency)
	for i, segment := range track.Segments {
		select {
		case slots <- struct{}{}:
		case <-ctx.Done():
		}
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-slots }()
			data, err := m.fetchSegment(ctx, it.limiter, segment, headers, &fetched)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if failure == nil {
					failure = fmt.Errorf("WebVTT file %d of %d: %w", i+1, len(track.Segments), err)
				}
				cancel()
				return
			}
			parts[i] = data
		}()
	}
	wg.Wait()
	if failure != nil {
		return webVTT{}, failure
	}
	if err := ctx.Err(); err != nil {
		return webVTT{}, err
	}
	return mergeWebVTT(parts)
}

// webVTT is a track's WebVTT files merged into one.
type webVTT struct {
	data []byte
	// offset is what to add to data's cue times to put them on the media
	// timeline, when mapped; see subtitleFile.
	offset float64
	mapped bool
}

// mpegTSClock is the 90 kHz clock X-TIMESTAMP-MAP counts in, and
// mpegTSWrap where its 33-bit counter wraps.
const (
	mpegTSClock = 90000
	mpegTSWrap  = 1 << 33
)

// errNotWebVTT is a subtitle file that is not WebVTT at all.
var errNotWebVTT = errors.New("not a WebVTT file")

// mergeWebVTT joins a track's WebVTT files into one: a plain "WEBVTT"
// header, and the cues of all of them, in order.
//
// Each file of a segmented track says where its cues sit with an
// X-TIMESTAMP-MAP, which can differ from one file to the next: the cues are
// moved onto the first such file's timeline, and the offset returned puts
// that onto the media's. A file without one is taken to share the first's.
// Segmenters repeat a cue that spans the boundary in both files, so a cue
// already given, the same times and text, is not given again. Everything
// else is dropped, none of which survives into an MP4's text track anyway:
// identifiers, which would repeat across files; notes; and style and region
// blocks, which also leave ffmpeg reading no cues at all from the file.
func mergeWebVTT(files [][]byte) (webVTT, error) {
	var (
		seen = make(map[string]bool)
		cues []string
		ref  *timestampMap
	)
	for i, raw := range files {
		text := strings.TrimPrefix(string(raw), "\ufeff")
		text = strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n")
		blocks := webVTTBlocks(text)
		if len(blocks) == 0 || !webVTTSignature(blocks[0]) {
			return webVTT{}, fmt.Errorf("WebVTT file %d of %d: %w", i+1, len(files), errNotWebVTT)
		}
		mapping := parseTimestampMap(blocks[0])
		if ref == nil && mapping != nil {
			ref = mapping
		}
		var shift float64
		if mapping != nil {
			shift = mapping.since(ref)
		}
		for _, block := range blocks[1:] {
			first, _, _ := strings.Cut(block, "\n")
			if webVTTKeyword(first, "STYLE") || webVTTKeyword(first, "REGION") || webVTTKeyword(first, "NOTE") {
				continue
			}
			cue, ok := shiftCue(block, shift)
			if !ok || seen[cue] {
				continue
			}
			seen[cue] = true
			cues = append(cues, cue)
		}
	}
	if len(cues) == 0 {
		return webVTT{}, errors.New("no cues")
	}

	var b strings.Builder
	b.WriteString("WEBVTT\n\n")
	for _, block := range cues {
		b.WriteString(block)
		b.WriteString("\n\n")
	}
	merged := webVTT{data: []byte(b.String())}
	if ref != nil {
		merged.offset, merged.mapped = ref.offset(), true
	}
	return merged, nil
}

// webVTTBlocks splits a WebVTT file into its blocks: the runs of lines
// between blank ones.
func webVTTBlocks(text string) []string {
	var blocks []string
	for block := range strings.SplitSeq(text, "\n\n") {
		if block = strings.Trim(block, "\n"); strings.TrimSpace(block) != "" {
			blocks = append(blocks, block)
		}
	}
	return blocks
}

// webVTTSignature reports whether a header block opens as WebVTT's must.
func webVTTSignature(header string) bool {
	return webVTTKeyword(header, "WEBVTT")
}

// webVTTKeyword reports whether a line is the keyword alone or the keyword
// followed by a space, a tab or a line's end, as WebVTT's block keywords are.
func webVTTKeyword(line, keyword string) bool {
	rest, ok := strings.CutPrefix(line, keyword)
	return ok && (rest == "" || rest[0] == ' ' || rest[0] == '\t' || rest[0] == '\n')
}

// timestampMap is a WebVTT file's X-TIMESTAMP-MAP: the cue time LOCAL is
// at MPEGTS on the media's 90 kHz clock.
type timestampMap struct {
	mpegTS int64
	local  float64
}

// offset is what to add to the file's cue times to put them on the media
// timeline, in seconds.
func (t *timestampMap) offset() float64 {
	return float64(t.mpegTS)/mpegTSClock - t.local
}

// since is how far this file's cue times sit from ref's on the media
// timeline: what to add to a cue of this file to put it on ref's. The 90 kHz
// counter wraps every 26½ hours, and a later file's that has wrapped is
// counted on from ref's rather than back from it.
func (t *timestampMap) since(ref *timestampMap) float64 {
	ticks := t.mpegTS - ref.mpegTS
	if ticks < -mpegTSWrap/2 {
		ticks += mpegTSWrap
	}
	return float64(ticks)/mpegTSClock - (t.local - ref.local)
}

// parseTimestampMap reads the X-TIMESTAMP-MAP of a header block, nil when
// there is none or it cannot be read.
func parseTimestampMap(header string) *timestampMap {
	for line := range strings.SplitSeq(header, "\n") {
		rest, ok := strings.CutPrefix(strings.TrimSpace(line), "X-TIMESTAMP-MAP=")
		if !ok {
			continue
		}
		var (
			mapping           timestampMap
			haveTS, haveLocal bool
		)
		for field := range strings.SplitSeq(rest, ",") {
			name, value, _ := strings.Cut(strings.TrimSpace(field), ":")
			switch name {
			case "MPEGTS":
				n, err := strconv.ParseInt(value, 10, 64)
				mapping.mpegTS, haveTS = n%mpegTSWrap, err == nil
			case "LOCAL":
				t, ok := parseCueTime(value)
				mapping.local, haveLocal = t, ok
			}
		}
		if haveTS && haveLocal {
			return &mapping
		}
	}
	return nil
}

// shiftCue moves a cue block's timing by shift seconds, dropping its
// identifier. It reports false for a block that is not a cue, and for a cue
// that would end before the video starts.
func shiftCue(block string, shift float64) (string, bool) {
	lines := strings.Split(block, "\n")
	timing := 0
	if !strings.Contains(lines[0], "-->") {
		timing = 1 // an identifier first
	}
	if timing >= len(lines) || !strings.Contains(lines[timing], "-->") {
		return "", false
	}
	startText, rest, _ := strings.Cut(lines[timing], "-->")
	endText, settings, _ := strings.Cut(strings.TrimSpace(rest), " ")
	start, okStart := parseCueTime(strings.TrimSpace(startText))
	end, okEnd := parseCueTime(strings.TrimSpace(endText))
	if !okStart || !okEnd {
		return "", false
	}
	start, end = max(start+shift, 0), end+shift
	if end <= start {
		return "", false
	}
	line := formatCueTime(start) + " --> " + formatCueTime(end)
	if settings = strings.TrimSpace(settings); settings != "" {
		line += " " + settings
	}
	return strings.Join(append([]string{line}, lines[timing+1:]...), "\n"), true
}

// parseCueTime reads a WebVTT timestamp, hh:mm:ss.ttt or mm:ss.ttt, in
// seconds.
func parseCueTime(text string) (float64, bool) {
	clock, fraction, ok := strings.Cut(text, ".")
	if !ok || len(fraction) != 3 {
		return 0, false
	}
	millis, err := strconv.Atoi(fraction)
	if err != nil {
		return 0, false
	}
	parts := strings.Split(clock, ":")
	if len(parts) < 2 || len(parts) > 3 {
		return 0, false
	}
	var seconds float64
	for _, part := range parts {
		n, err := strconv.Atoi(part)
		if err != nil || n < 0 {
			return 0, false
		}
		seconds = seconds*60 + float64(n)
	}
	return seconds + float64(millis)/1000, true
}

// formatCueTime writes seconds as a WebVTT timestamp, hh:mm:ss.ttt.
func formatCueTime(seconds float64) string {
	millis := int64(math.Round(seconds * 1000))
	return fmt.Sprintf("%02d:%02d:%02d.%03d", millis/3600000, millis/60000%60, millis/1000%60, millis%1000)
}
