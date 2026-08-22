package download

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"os"
	"sync"

	"github.com/JohanLindvall/HeapLeach/internal/httpx"
	"github.com/JohanLindvall/HeapLeach/internal/util"
)

// Playlist transfers: files that arrive as an ordered list of parts rather
// than as one rangeable resource.
//
// Adaptive streams work this way. There is no length to divide up and no
// offsets to seek to, so the segmented engine does not apply; instead the
// parts are fetched several at a time and appended strictly in order. The
// extractor picks a rendition carrying audio and video together, so simply
// joining them yields something playable — no remuxing required.

// playlistState records how far a playlist transfer got, so an interrupted
// one resumes instead of refetching hundreds of segments.
type playlistState struct {
	Index int   `json:"index"`
	Bytes int64 `json:"bytes"`
	Total int   `json:"total"`
}

func playlistStatePath(part string) string { return part + ".playlist" }

func loadPlaylistState(part string, total int) *playlistState {
	raw, err := os.ReadFile(playlistStatePath(part))
	if err != nil {
		return nil
	}
	var st playlistState
	if err := json.Unmarshal(raw, &st); err != nil {
		return nil
	}
	// A playlist that has changed length is a different stream.
	if st.Total != total || st.Index < 0 || st.Index > total || st.Bytes < 0 {
		return nil
	}
	return &st
}

func savePlaylistState(part string, st playlistState) error {
	raw, err := json.Marshal(st)
	if err != nil {
		return err
	}
	tmp := playlistStatePath(part) + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, playlistStatePath(part))
}

func clearPlaylistState(part string) {
	_ = os.Remove(playlistStatePath(part))
	_ = os.Remove(playlistStatePath(part) + ".tmp")
}

// fetchedSegment is one part on its way to the assembler.
type fetchedSegment struct {
	index int
	data  []byte
	err   error
}

// transferPlaylist fetches every segment and appends them in order.
func (m *Manager) transferPlaylist(ctx context.Context, it *Item, part, name string) (string, error) {
	m.mu.Lock()
	segments := append([]string(nil), it.Segments...)
	headers := maps.Clone(it.Headers)
	workers := m.streams
	m.mu.Unlock()

	if len(segments) == 0 {
		return "", errors.New("playlist has no segments")
	}
	if workers < 1 {
		workers = 1
	}

	start, written := 0, int64(0)
	if saved := loadPlaylistState(part, len(segments)); saved != nil {
		start, written = saved.Index, saved.Bytes
	}

	flags := os.O_CREATE | os.O_WRONLY
	if start == 0 {
		flags |= os.O_TRUNC
	}
	file, err := os.OpenFile(part, flags, 0o644)
	if err != nil {
		return "", fmt.Errorf("open %s: %w", name, err)
	}
	defer file.Close()

	// Trim anything written past the last recorded segment boundary: those
	// bytes belong to a segment that was only partly saved.
	if err := file.Truncate(written); err != nil {
		return "", fmt.Errorf("prepare %s: %w", name, err)
	}

	m.setProgress(it, written)
	m.setSegmentProgress(it, start, len(segments))

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	results := make(chan fetchedSegment, workers)
	slots := make(chan struct{}, workers)

	var wg sync.WaitGroup
	go func() {
		for i := start; i < len(segments); i++ {
			select {
			case slots <- struct{}{}:
			case <-ctx.Done():
				break
			}
			if ctx.Err() != nil {
				break
			}
			wg.Add(1)
			go func(index int) {
				defer wg.Done()
				data, err := m.fetchSegment(ctx, segments[index], headers)
				select {
				case results <- fetchedSegment{index: index, data: data, err: err}:
				case <-ctx.Done():
				}
			}(i)
		}
		wg.Wait()
		close(results)
	}()

	it.streams.Store(int32(workers))
	defer it.streams.Store(0)

	// Segments arrive out of order; hold them until their turn comes.
	pending := make(map[int][]byte, workers*2)
	next := start

	for outcome := range results {
		if outcome.err != nil {
			cancel()
			//nolint:revive // drain so the producer goroutines can exit
			for range results {
			}
			return "", fmt.Errorf("segment %d of %d: %w", outcome.index+1, len(segments), outcome.err)
		}
		pending[outcome.index] = outcome.data

		for {
			data, ok := pending[next]
			if !ok {
				break
			}
			delete(pending, next)

			if _, err := file.WriteAt(data, written); err != nil {
				cancel()
				for range results {
				}
				return "", fmt.Errorf("write %s: %w", name, err)
			}
			written += int64(len(data))
			next++
			it.downloaded.Store(written)
			<-slots // a written segment frees its slot, bounding memory

			if next%playlistStateEvery == 0 || next == len(segments) {
				_ = savePlaylistState(part, playlistState{Index: next, Bytes: written, Total: len(segments)})
			}
			m.setSegmentProgress(it, next, len(segments))
		}
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if next != len(segments) {
		return "", fmt.Errorf("joined %d of %d segments", next, len(segments))
	}

	if err := file.Close(); err != nil {
		return "", fmt.Errorf("write %s: %w", name, err)
	}
	m.mu.Lock()
	it.Size = written
	m.mu.Unlock()
	clearPlaylistState(part)
	return name, nil
}

// playlistStateEvery is how often progress is checkpointed, in segments.
const playlistStateEvery = 20

// fetchSegment retrieves one part in full.
//
// A playlist part arrives as one whole response rather than a stream, so
// the throttle is consulted around the fetch instead of inside it: once to
// wait out a pause before starting, and once afterwards to pay for the
// bytes. The rate is therefore right on average across a playlist rather
// than smooth within a single part.
func (m *Manager) fetchSegment(ctx context.Context, rawURL string, headers httpx.Header) ([]byte, error) {
	for attempt := 0; ; attempt++ {
		if _, err := m.throttle.take(ctx, 1); err != nil {
			return nil, err
		}
		req, err := m.client.NewRequest(ctx, http.MethodGet, rawURL, nil)
		if err != nil {
			return nil, err
		}
		for header, value := range headers {
			req.Header.Set(header, value)
		}
		req.Header.Set(httpx.HeaderAccept, httpx.AcceptAny)

		data, err := m.client.Bytes(req)
		if err == nil {
			m.chargeThrottle(ctx, len(data))
			return data, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if attempt >= m.cfg.MaxRetries || !retryableTransfer(err) {
			return nil, err
		}
		if err := util.SleepCtx(ctx, transferRetryDelay(attempt)); err != nil {
			return nil, err
		}
	}
}

// setSegmentProgress publishes how far through a playlist a transfer is,
// which is the only meaningful progress when the total length is unknown
// until the last segment has been fetched.
func (m *Manager) setSegmentProgress(it *Item, done, total int) {
	m.mu.Lock()
	it.SegmentsDone, it.SegmentsTotal = done, total
	m.mu.Unlock()
}
