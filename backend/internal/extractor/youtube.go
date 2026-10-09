// SPDX-License-Identifier: MIT

package extractor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"os/exec"
	"strconv"

	"github.com/JohanLindvall/HeapLeach/internal/tools"
	"github.com/JohanLindvall/HeapLeach/internal/util"
)

// YouTube hands videos to yt-dlp.
//
// Nothing here resolves a media URL, because nothing reasonably can: YouTube
// now withholds playback URLs until a caller passes its BotGuard attestation,
// on top of the signature and throttling parameters that already required
// running its player JavaScript. yt-dlp exists to keep up with exactly that,
// so this extractor only identifies the video and lets yt-dlp do the rest.
type YouTube struct{ hostSet }

// ytdlpBinary is the tool this extractor depends on, spelled once in the
// package that locates it.
const ytdlpBinary = tools.YtDlp

// NewYouTube builds the YouTube extractor.
func NewYouTube() *YouTube {
	return &YouTube{hostSet: hostSet{"youtube.com", "youtu.be", "youtube-nocookie.com"}}
}

func (y *YouTube) Name() string { return "youtube" }

// Extract names the video, or every video of a playlist, leaving the actual
// download to the helper script.
func (y *YouTube) Extract(ctx context.Context, u *url.URL, opts Options) (*Result, error) {
	ytdlp, ok := tools.Find(ytdlpBinary)
	if !ok {
		return nil, fmt.Errorf("youtube: %s (%s)", tools.NotInstalled(ytdlpBinary), u.Redacted())
	}

	return y.probe(ctx, ytdlp, u.String(), opts.maxFiles())
}

// What the probes ask yt-dlp for, one JSON object per line: each video, or
// each entry of a flat playlist, as yt-dlp reaches it, and then the playlist
// itself once its entries are done.
//
// Only the fields read are printed. -J dumps everything yt-dlp extracted,
// and for a single video that is mostly YouTube's automatic captions: a
// machine translation into each of some 180 languages, in several formats,
// each behind a long signed URL. That came to ten megabytes for one
// ordinary video, past the cap on helper output, so the probe failed
// before any download began.
//
// --simulate has to be spelled out alongside them. --print implies it only
// while every print is at the default stage, and the playlist: one is a
// later stage, which turns it off: the probe then downloads, in its
// working directory, the video it was only asked to name.
const (
	ytdlpVideoFields    = "%(.{_type,id,title,url,webpage_url})j"
	ytdlpPlaylistFields = "playlist:%(.{_type,id,title})j"
)

// ytdlpEntry is one line of that output.
type ytdlpEntry struct {
	Type   string `json:"_type"`
	ID     string `json:"id"`
	Title  string `json:"title"`
	URL    string `json:"url"`
	WebURL string `json:"webpage_url"`
}

// ytdlpEntries reads a probe's lines, in the order yt-dlp printed them.
func ytdlpEntries(output []byte) ([]ytdlpEntry, error) {
	var entries []ytdlpEntry
	for line := range bytes.Lines(output) {
		if line = bytes.TrimSpace(line); len(line) == 0 {
			continue
		}
		var entry ytdlpEntry
		if err := json.Unmarshal(line, &entry); err != nil {
			return nil, fmt.Errorf("could not read yt-dlp output: %w", err)
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

// probe asks yt-dlp what the URL contains, without downloading anything.
func (y *YouTube) probe(ctx context.Context, ytdlp, target string, limit int) (*Result, error) {
	// --flat-playlist keeps a playlist probe to one request per page rather
	// than resolving every video up front. Ask for one extra entry to detect
	// truncation, and bound the listing at its source instead of reading an
	// entire channel only to discard everything past the cap.
	end := limit
	if end < math.MaxInt {
		end++
	}
	output, err := tools.Probe(ctx, ytdlp,
		"--simulate", "--no-warnings", "--flat-playlist", "--ignore-no-formats-error",
		"--print", ytdlpVideoFields, "--print", ytdlpPlaylistFields,
		"--playlist-end", strconv.Itoa(end), "--", target)
	if err != nil {
		return nil, fmt.Errorf("youtube: yt-dlp could not read %s: %w", target, err)
	}
	lines, err := ytdlpEntries(output)
	if err != nil {
		return nil, fmt.Errorf("youtube: %w", err)
	}

	var playlist *ytdlpEntry
	var entries []ytdlpEntry
	for _, line := range lines {
		if line.Type == "playlist" {
			playlist = &line
		} else {
			entries = append(entries, line)
		}
	}
	if playlist == nil {
		if len(entries) == 0 {
			return nil, fmt.Errorf("youtube: yt-dlp said nothing about %s", target)
		}
		video := entries[0]
		name := util.FirstNonEmpty(video.Title, video.ID, "video")
		link := util.FirstNonEmpty(video.WebURL, target)
		return &Result{Title: name, Files: []File{{External: link, Name: name, Size: -1}}}, nil
	}

	res := &Result{Title: util.FirstNonEmpty(playlist.Title, playlist.ID, "playlist")}
	for _, entry := range entries {
		link := util.FirstNonEmpty(entry.WebURL, entry.URL)
		if link == "" && entry.ID != "" {
			link = "https://www.youtube.com/watch?v=" + entry.ID
		}
		if link == "" {
			continue
		}
		res.Files = append(res.Files, File{
			External: link,
			Name:     util.FirstNonEmpty(entry.Title, entry.ID, "video"),
			Size:     -1,
		})
		if len(res.Files) >= limit {
			break
		}
	}
	if len(res.Files) == 0 {
		return nil, fmt.Errorf("youtube: %s lists no videos", target)
	}
	if len(entries) > limit {
		res.Note = "partial — playlist limit reached"
	}
	return res, nil
}

// ytdlpTitle asks yt-dlp what a single video is called, without downloading
// anything. A host that leaves the transfer to yt-dlp still wants a name to
// show while the item waits its turn in the queue.
func ytdlpTitle(ctx context.Context, ytdlp, target string) (string, error) {
	output, err := tools.Probe(ctx, ytdlp,
		"--simulate", "--no-warnings", "--no-playlist", "--ignore-no-formats-error",
		"--print", ytdlpVideoFields, "--", target)
	if err != nil {
		return "", err
	}
	entries, err := ytdlpEntries(output)
	if err != nil {
		return "", err
	}
	if len(entries) == 0 {
		return "", nil
	}
	return util.FirstNonEmpty(entries[0].Title, entries[0].ID), nil
}

// ytdlpError surfaces what yt-dlp printed, which is far more useful to the
// user than a bare exit status.
func ytdlpError(err error) error {
	if exit, ok := errors.AsType[*exec.ExitError](err); ok && len(exit.Stderr) > 0 {
		return fmt.Errorf("%s", util.Truncate(string(exit.Stderr), 300))
	}
	return err
}
