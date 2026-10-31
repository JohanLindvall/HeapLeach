// SPDX-License-Identifier: MIT

package download

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/extractor"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

// Playlist transfers: files that arrive as an ordered list of parts rather
// than as one rangeable resource.
//
// Adaptive streams work this way. There is no length to divide up and no
// offsets to seek to, so the segmented engine does not apply; instead the
// parts are fetched several at a time and appended strictly in order. Where
// the playlist has a rendition carrying audio and video together the
// extractor picks it, and simply joining its parts yields something
// playable. Where every rendition keeps its audio apart, as CMAF's do, each
// audio track is joined into a part file of its own, and mux.go joins them
// all into one file when the last part lands.

// playlistState records how far a playlist transfer got, so an interrupted
// one resumes instead of refetching hundreds of segments.
type playlistState struct {
	Index int   `json:"index"`
	Bytes int64 `json:"bytes"`
	Total int   `json:"total"`
	// Track is the audio track the part holds (playlistTrack.id), empty for
	// the item's own segments.
	Track string `json:"track,omitempty"`
}

func playlistStatePath(part string) string { return part + ".playlist" }

func loadPlaylistState(part string, total int, files transferFiles) *playlistState {
	st := readSidecar[playlistState](files, playlistStatePath(part))
	// A playlist that has changed length is a different stream.
	if st == nil || st.Total != total || st.Index < 0 || st.Index > total || st.Bytes < 0 {
		return nil
	}
	return st
}

func savePlaylistState(part string, st playlistState, files transferFiles) error {
	return writeSidecar(files, playlistStatePath(part), st)
}

func clearPlaylistState(part string, files transferFiles) {
	clearSidecar(files, playlistStatePath(part))
}

// playlistTrack is one list of segments joined into a part file of its own:
// the item's own segments, or one of the audio tracks a playlist keeps apart
// from its video (Item.Audio), whose parts sit beside the item's. An audio
// track the playlist says the video's own segments carry has nothing to
// fetch, and no part: it is listed for the join (mux.go).
type playlistTrack struct {
	part     string
	segments []string
	key      *extractor.SegmentKey
	// audio is the track's description, nil for the item's own segments.
	audio *extractor.Track
	// start is where a packed audio track's timeline starts, which the join
	// has to tell ffmpeg (packedAudioStart); zero for any other.
	start float64
}

// playlistTracksLocked lists what a playlist transfer fetches: the item's
// own segments, then each audio track. The caller holds mu.
func playlistTracksLocked(it *Item, part string) []playlistTrack {
	tracks := make([]playlistTrack, 0, 1+len(it.Audio))
	tracks = append(tracks, playlistTrack{part: part, segments: it.Segments, key: it.SegmentKey})
	for i, audio := range it.Audio {
		t := playlistTrack{segments: audio.Segments, key: audio.Key, audio: &audio}
		if len(audio.Segments) > 0 {
			t.part = audioPart(part, i)
		}
		tracks = append(tracks, t)
	}
	return tracks
}

// audioPart names the part file of an item's i-th audio track. It keeps the
// ".part" ending, which no library watching the folder takes for media.
func audioPart(part string, i int) string {
	return strings.TrimSuffix(part, ".part") + ".audio" + strconv.Itoa(i+1) + ".part"
}

// id is what a track's checkpoint is held to. A playlist's tracks commonly
// have the same number of segments, so a part resumed from the checkpoint of
// another language would carry both.
func (t playlistTrack) id() string {
	if t.audio == nil {
		return ""
	}
	return t.audio.Language + "/" + t.audio.Name
}

// state is the track's checkpoint, if there is one and it is this track's.
func (t playlistTrack) state(files transferFiles) *playlistState {
	st := loadPlaylistState(t.part, len(t.segments), files)
	if st == nil || st.Track != t.id() {
		return nil
	}
	return st
}

// describe names a track in an error: nothing for the item's own segments,
// which the error already names.
func (t playlistTrack) describe(err error) error {
	if t.audio == nil {
		return err
	}
	label := cmp.Or(t.audio.Name, t.audio.Language, "unnamed")
	return fmt.Errorf("the %q audio track: %w", label, err)
}

// trackJoin is one track being joined into its part file.
type trackJoin struct {
	playlistTrack
	file    *os.File
	secret  []byte // the AES-128 key, fetched
	start   int    // the first segment this attempt fetches
	next    int    // the next segment to write
	written int64
	pending map[int][]byte // fetched out of turn, waiting for it
}

// fetchedSegment is one part on its way to the assembler.
type fetchedSegment struct {
	track, index int
	data         []byte
	err          error
}

// transferPlaylist fetches every segment and appends them in order: the
// item's own into its part file, and each audio track kept apart from them
// into one of its own.
//
// The tracks are fetched together, segment i of every track before segment
// i+1 of any, from one pool of connections. Every track's links were signed
// together, so none is left to expire while another downloads ahead of it;
// and the segment count the item shows as its progress then moves with the
// time the transfer takes, where one track after another would race through
// three of four tracks' segments in the first fifth of it.
func (m *Manager) transferPlaylist(ctx context.Context, it *Item, part, name string, files transferFiles) (string, error) {
	m.mu.Lock()
	tracks := playlistTracksLocked(it, part)
	for i := range tracks {
		tracks[i].segments = slices.Clone(tracks[i].segments)
	}
	subtitleTracks := slices.Clone(it.Subtitles)
	headers := maps.Clone(it.Headers)
	workers := m.streams
	m.mu.Unlock()

	if len(tracks[0].segments) == 0 {
		return "", errors.New("playlist has no segments")
	}

	// Subtitles first, while their links are freshest, and before the stall
	// watchdog starts: they have a time limit of their own, and a host slow
	// to serve them has not stalled the video.
	subtitles := m.fetchSubtitles(ctx, it, part, subtitleTracks, headers, files)
	m.mu.Lock()
	it.subtitleFiles = subtitles
	m.mu.Unlock()

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Watch the key requests as well as the media parts: both use streaming
	// bodies, so headers followed by silence otherwise wait indefinitely.
	// The item's counter only advances when a whole part lands in order;
	// raw fetched bytes also recognise a large part arriving slowly.
	var (
		fetched atomic.Int64
		stalled atomic.Bool
	)
	go watchForStall(ctx, fetched.Load, cancel, &stalled, m.stallTimeout(), m.throttle.isPaused)
	fail := func(err error) (string, error) {
		return "", annotateTransfer(name, err, &stalled, m.stallTimeout())
	}
	if workers < 1 {
		workers = 1
	}

	joins := make([]*trackJoin, 0, len(tracks))
	var (
		done, total int
		written     int64
		first, last = math.MaxInt, 0
	)
	for _, t := range tracks {
		if t.part == "" {
			continue // carried in the video's own segments
		}
		j := &trackJoin{playlistTrack: t, pending: make(map[int][]byte)}
		joins = append(joins, j)
		// An encrypted playlist's key, fetched once per attempt — it is
		// signed like the segments, and a resolver may just have minted it
		// afresh.
		if t.key != nil {
			var err error
			if j.secret, err = m.fetchSegment(ctx, it.limiter, t.key.URI, headers, &fetched); err != nil {
				return fail(t.describe(fmt.Errorf("fetch the key for %s: %w", name, err)))
			}
		}
		if saved := t.state(files); saved != nil {
			j.start, j.written = saved.Index, saved.Bytes
			// The checkpoint is only as good as the file it describes. A
			// part that is gone or shorter than recorded would otherwise be
			// padded with zeros by the truncate below and finished as if
			// whole.
			if fi, err := files.stat(t.part); err != nil || fi.Size() < j.written {
				j.start, j.written = 0, 0
			}
		}
		j.next = j.start

		flags := os.O_CREATE | os.O_WRONLY
		if j.start == 0 {
			flags |= os.O_TRUNC
		}
		file, err := files.open(t.part, flags, 0o644)
		if err != nil {
			return "", fmt.Errorf("open %s: %w", name, t.describe(err))
		}
		defer file.Close()
		j.file = file
		// Trim anything written past the last recorded segment boundary:
		// those bytes belong to a segment that was only partly saved.
		if err := file.Truncate(j.written); err != nil {
			return "", fmt.Errorf("prepare %s: %w", name, t.describe(err))
		}

		done += j.start
		total += len(t.segments)
		written += j.written
		if j.start < len(t.segments) {
			first = min(first, j.start)
		}
		last = max(last, len(t.segments))
	}

	m.setProgress(it, written)
	m.setSegmentProgress(it, done, total)

	results := make(chan fetchedSegment, workers)
	slots := make(chan struct{}, workers)

	var wg sync.WaitGroup
	go func() {
		defer close(results)
		defer wg.Wait()
		for index := first; index < last; index++ {
			for track, j := range joins {
				if index < j.start || index >= len(j.segments) {
					continue
				}
				select {
				case slots <- struct{}{}:
				case <-ctx.Done():
					return
				}
				wg.Add(1)
				go func() {
					defer wg.Done()
					data, err := m.fetchSegment(ctx, it.limiter, j.segments[index], headers, &fetched)
					if err == nil && j.secret != nil && index >= j.key.Clear {
						data, err = decryptSegment(data, j.secret, segmentIV(j.key, index))
					}
					if err == nil && index > 0 {
						data = withoutPackedAudioTag(data)
					}
					select {
					case results <- fetchedSegment{track: track, index: index, data: data, err: err}:
					case <-ctx.Done():
					}
				}()
			}
		}
	}()

	it.streams.Store(int32(workers))
	defer it.streams.Store(0)

	// Segments arrive out of order; each is held until its turn comes in
	// its own track. A slot is given back only once its segment is written,
	// which bounds what is held, and cannot deadlock: of the segments
	// holding slots, the one sent for first has its predecessor written.
	served := false
	for outcome := range results {
		j := joins[outcome.track]
		if outcome.err != nil {
			cancel()
			//nolint:revive // drain so the producer goroutines can exit
			for range results {
			}
			return fail(j.describe(fmt.Errorf("segment %d of %d: %w", outcome.index+1, len(j.segments), outcome.err)))
		}
		j.pending[outcome.index] = outcome.data

		for {
			data, ok := j.pending[j.next]
			if !ok {
				break
			}
			delete(j.pending, j.next)

			if _, err := j.file.WriteAt(data, j.written); err != nil {
				cancel()
				for range results {
				}
				return "", fmt.Errorf("write %s: %w", name, j.describe(err))
			}
			j.written += int64(len(data))
			written += int64(len(data))
			j.next++
			done++
			if !served {
				// The first part to land says this host is serving, which
				// is what a later 503 from it is measured against. Marked
				// once: a playlist has thousands of these.
				served = true
				m.hostGate.serving(m.itemHost(it))
			}
			it.downloaded.Store(written)
			<-slots // a written segment frees its slot, bounding memory

			if j.next%config.PlaylistStateEvery == 0 || j.next == len(j.segments) {
				_ = savePlaylistState(j.part, playlistState{Index: j.next, Bytes: j.written,
					Total: len(j.segments), Track: j.id()}, files)
			}
			m.setSegmentProgress(it, done, total)
		}
	}
	if err := ctx.Err(); err != nil {
		return fail(err)
	}
	for _, j := range joins {
		if j.next != len(j.segments) {
			return "", j.describe(fmt.Errorf("joined %d of %d segments", j.next, len(j.segments)))
		}
		if err := j.file.Close(); err != nil {
			return "", fmt.Errorf("write %s: %w", name, j.describe(err))
		}
	}
	m.mu.Lock()
	it.Size = written
	m.mu.Unlock()
	return name, nil
}

// fetchSegment retrieves one part in full, retrying on its own so a single
// dropped connection does not sink the whole playlist.
func (m *Manager) fetchSegment(ctx context.Context, limiter *throttle, rawURL string, headers httpx.Header, fetched *atomic.Int64) ([]byte, error) {
	for attempt := 0; ; attempt++ {
		data, err := m.fetchSegmentOnce(ctx, limiter, rawURL, headers, fetched)
		if err == nil {
			return data, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if err := m.retryAfter(ctx, attempt, err); err != nil {
			return nil, err
		}
	}
}

// fetchSegmentOnce streams one part into memory.
//
// The body is read through the shared throttle, so a pause parks mid-part
// and a rate cap holds inside a part rather than only between parts, and
// every read is counted into fetched so the stall watchdog sees a large part
// arriving slowly as the progress it is. The body is read directly rather
// than through Client.Bytes, whose in-memory cap is sized for pages and
// would silently truncate a media part larger than it.
func (m *Manager) fetchSegmentOnce(ctx context.Context, limiter *throttle, rawURL string, headers httpx.Header, fetched *atomic.Int64) ([]byte, error) {
	target, first, last, ranged := extractor.SegmentRange(rawURL)
	req, err := m.client.NewRequest(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	headers.Apply(req)
	req.Header.Set(httpx.HeaderAccept, httpx.AcceptAny)
	if ranged {
		req.Header.Set(httpx.HeaderRange, fmt.Sprintf("bytes=%d-%d", first, last))
	}

	resp, err := m.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	switch {
	case ranged && resp.StatusCode == http.StatusPartialContent:
		if err := segmentRangeAnswered(resp, first, last); err != nil {
			return nil, fmt.Errorf("%s: %w", req.URL.Redacted(), err)
		}
	case ranged && resp.StatusCode == http.StatusOK:
		return nil, fmt.Errorf("%s: asked for bytes %d-%d: %w", req.URL.Redacted(), first, last, errRangeIgnored)
	case resp.StatusCode != http.StatusOK:
		return nil, httpx.NewStatusError(resp)
	}
	if err := rejectWebPage(resp, "segment.ts"); err != nil {
		return nil, err
	}

	var buf bytes.Buffer
	if resp.ContentLength > 0 && resp.ContentLength <= config.MaxSegmentBytes {
		buf.Grow(int(resp.ContentLength))
	}
	// One byte past the ceiling, so reaching it is detectable rather than
	// arriving as a part that is quietly short.
	body := io.TeeReader(
		io.LimitReader(m.throttled(ctx, limiter, resp.Body), config.MaxSegmentBytes+1),
		countInto(fetched))
	if _, err := buf.ReadFrom(body); err != nil {
		return nil, fmt.Errorf("read %s: %w", req.URL.Redacted(), err)
	}
	if int64(buf.Len()) > config.MaxSegmentBytes {
		return nil, fmt.Errorf("%s: part exceeds %d bytes, which no real one does",
			req.URL.Redacted(), int64(config.MaxSegmentBytes))
	}
	if ranged && int64(buf.Len()) != last-first+1 {
		return nil, fmt.Errorf("%s: asked for %d bytes, got %d", req.URL.Redacted(), last-first+1, buf.Len())
	}
	return buf.Bytes(), nil
}

// errRangeIgnored is a host answering a part's range with the whole file.
// Read, it would be fetched whole once for every part of a playlist, a
// thousand times over; and asking again gets the same answer.
var errRangeIgnored = errors.New("the host answered with the whole file")

// segmentRangeAnswered checks that a partial answer is the part asked for:
// a Content-Range starting elsewhere is some other part of the file.
func segmentRangeAnswered(resp *http.Response, first, last int64) error {
	header := resp.Header.Get(httpx.HeaderContentRange)
	if header == "" {
		return nil // the length is checked once the body is read
	}
	r, ok := parseContentRange(header)
	if !ok || r.first < 0 {
		return fmt.Errorf("unreadable Content-Range %q", header)
	}
	if r.first != first || r.last != last {
		return fmt.Errorf("asked for bytes %d-%d, got %d-%d", first, last, r.first, r.last)
	}
	return nil
}

// countInto is a writer that only tallies what passes through it.
func countInto(n *atomic.Int64) io.Writer {
	return writerFunc(func(p []byte) (int, error) {
		n.Add(int64(len(p)))
		return len(p), nil
	})
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

// setSegmentProgress publishes how far through a playlist a transfer is,
// which is the only meaningful progress when the total length is unknown
// until the last segment has been fetched.
func (m *Manager) setSegmentProgress(it *Item, done, total int) {
	m.mu.Lock()
	it.SegmentsDone, it.SegmentsTotal = done, total
	m.mu.Unlock()
}
