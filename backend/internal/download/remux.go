package download

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/tools"
)

// Joined MPEG-TS segments are already playable, so remuxing is a convenience
// rather than a requirement: it only rewraps the same streams into an .mp4
// container. Nothing is re-encoded, so it is quick and lossless, and when
// ffmpeg is not installed the .ts file is simply kept as-is.

// remuxTimeout bounds the rewrap. Copying streams is fast even for a long
// programme, so a stuck ffmpeg should not hold a worker indefinitely.
const remuxTimeout = 30 * time.Minute

// remuxToMP4 rewraps a transport stream as MP4, returning the path to use.
// Any failure leaves the original untouched and returns it.
func (m *Manager) remuxToMP4(ctx context.Context, path string) string {
	if !strings.EqualFold(filepath.Ext(path), ".ts") {
		return path
	}
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		m.log.Debug("ffmpeg not installed; keeping the transport stream", "path", filepath.Base(path))
		return path
	}

	target := strings.TrimSuffix(path, filepath.Ext(path)) + ".mp4"
	unique, err := UniquePath(filepath.Dir(target), filepath.Base(target))
	if err != nil {
		return path
	}

	ctx, cancel := context.WithTimeout(ctx, remuxTimeout)
	defer cancel()

	// Conversion is only worth doing if it is lossless, so every attempt is
	// a stream copy: nothing is ever re-encoded.
	//
	// What comes over is the best video, plus the best audio and subtitle
	// track of each language, so a recording that shipped in several
	// languages stays usable in all of them without also carrying the
	// duplicate encodes of each. Working that out needs a look inside, so
	// the file is probed first; without a probe ffmpeg's own pick of one
	// video and one audio track is used instead.
	maps := simpleMaps()
	mapsWithoutSubs := simpleMapsWithoutSubtitles()
	hasSubtitles := true
	if ffprobe, ok := tools.Find("ffprobe"); ok {
		if streams, err := probeMedia(ctx, ffprobe, path); err == nil {
			video, audio, subtitles := selectStreams(streams)
			if len(video)+len(audio) > 0 {
				maps = mapArgs(video, audio, subtitles)
				mapsWithoutSubs = mapArgs(video, audio, nil)
				hasSubtitles = len(subtitles) > 0
			}
		}
	}

	// Two things can still refuse to copy: AAC in a transport stream uses a
	// framing MP4 does not, which aac_adtstoasc fixes but which rejects
	// audio that is not AAC; and transport-stream subtitles are usually
	// bitmap formats MP4 cannot hold at all. So the attempts step down,
	// dropping the subtitles last. If none work the streams cannot enter
	// MP4 untouched, and the transport stream is kept exactly as it is.
	aacFix := []string{"-bsf:a", "aac_adtstoasc"}
	attempts := [][]string{
		append(append([]string{}, maps...), aacFix...),
		maps,
	}
	if hasSubtitles {
		attempts = append(attempts,
			append(append([]string{}, mapsWithoutSubs...), aacFix...),
			mapsWithoutSubs)
	}

	var (
		output []byte
		runErr error
	)
	for _, step := range attempts {
		args := []string{"-nostdin", "-hide_banner", "-loglevel", "error", "-y", "-i", path}
		args = append(args, step...)
		args = append(args, "-movflags", "+faststart", unique)

		output, runErr = exec.CommandContext(ctx, ffmpeg, args...).CombinedOutput()
		if runErr == nil {
			break
		}
		_ = os.Remove(unique)
		if ctx.Err() != nil {
			return path
		}
	}
	if runErr != nil {
		m.log.Debug("not losslessly convertible; keeping the transport stream",
			"path", filepath.Base(path), "err", runErr,
			"ffmpeg", strings.TrimSpace(truncateOutput(string(output))))
		return path
	}

	if info, err := os.Stat(unique); err != nil || info.Size() == 0 {
		_ = os.Remove(unique)
		return path
	}

	_ = os.Remove(path)
	m.log.Debug("rewrapped as mp4", "path", filepath.Base(unique))
	return unique
}

// simpleMaps is the fallback when the file could not be probed: let ffmpeg
// pick one video and one audio track, and take subtitles if there are any.
func simpleMaps() []string {
	return []string{"-map", "0:v:0?", "-map", "0:a:0?", "-map", "0:s?", "-c", "copy"}
}

// mapArgs turns chosen stream indexes into ffmpeg arguments.
func mapArgs(video, audio, subtitles []int) []string {
	args := make([]string, 0, 2*(len(video)+len(audio)+len(subtitles))+2)
	for _, group := range [][]int{video, audio, subtitles} {
		for _, index := range group {
			args = append(args, "-map", "0:"+strconv.Itoa(index))
		}
	}
	return append(args, "-c", "copy")
}

// simpleMapsWithoutSubtitles is the unprobed fallback for the retry that
// gives up on subtitles MP4 cannot hold.
func simpleMapsWithoutSubtitles() []string {
	return []string{"-map", "0:v:0?", "-map", "0:a:0?", "-c", "copy"}
}

// truncateOutput keeps an ffmpeg diagnostic short enough to log.
func truncateOutput(s string) string {
	const limit = 300
	if len(s) <= limit {
		return s
	}
	return s[:limit] + "..."
}
