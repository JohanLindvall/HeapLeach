package extractor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os/exec"

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

// ytdlpMaxEntries bounds how many videos one playlist expands to.
const ytdlpMaxEntries = 500

// NewYouTube builds the YouTube extractor.
func NewYouTube() *YouTube {
	return &YouTube{hostSet: hostSet{"youtube.com", "youtu.be", "youtube-nocookie.com"}}
}

func (y *YouTube) Name() string { return "youtube" }

// Extract names the video, or every video of a playlist, leaving the actual
// download to the helper script.
func (y *YouTube) Extract(ctx context.Context, u *url.URL, _ Options) (*Result, error) {
	ytdlp, ok := tools.Find(ytdlpBinary)
	if !ok {
		return nil, fmt.Errorf("youtube: %s (%s)", tools.NotInstalled(ytdlpBinary), u.Redacted())
	}

	entries, title, err := y.probe(ctx, ytdlp, u.String())
	if err != nil {
		return nil, err
	}

	files := make([]File, 0, len(entries))
	for _, entry := range entries {
		files = append(files, File{
			Name:     entry.name,
			Size:     -1,
			External: entry.url,
		})
	}
	return &Result{Title: title, Files: files}, nil
}

// ytEntry is one video to download.
type ytEntry struct {
	url  string
	name string
}

// probe asks yt-dlp what the URL contains, without downloading anything.
func (y *YouTube) probe(ctx context.Context, ytdlp, target string) ([]ytEntry, string, error) {
	// --flat-playlist keeps a playlist probe to one request per page rather
	// than resolving every video up front.
	cmd := exec.CommandContext(ctx, ytdlp,
		"-J", "--no-warnings", "--flat-playlist", "--ignore-no-formats-error", target)
	output, err := cmd.Output()
	if err != nil {
		return nil, "", fmt.Errorf("youtube: yt-dlp could not read %s: %w", target, ytdlpError(err))
	}

	var probe struct {
		Type    string `json:"_type"`
		ID      string `json:"id"`
		Title   string `json:"title"`
		URL     string `json:"url"`
		WebURL  string `json:"webpage_url"`
		Entries []struct {
			ID     string `json:"id"`
			Title  string `json:"title"`
			URL    string `json:"url"`
			WebURL string `json:"webpage_url"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(output, &probe); err != nil {
		return nil, "", fmt.Errorf("youtube: could not read yt-dlp output: %w", err)
	}

	if probe.Type != "playlist" {
		name := util.FirstNonEmpty(probe.Title, probe.ID, "video")
		link := util.FirstNonEmpty(probe.WebURL, target)
		return []ytEntry{{url: link, name: name}}, name, nil
	}

	entries := make([]ytEntry, 0, len(probe.Entries))
	for _, entry := range probe.Entries {
		link := util.FirstNonEmpty(entry.WebURL, entry.URL)
		if link == "" && entry.ID != "" {
			link = "https://www.youtube.com/watch?v=" + entry.ID
		}
		if link == "" {
			continue
		}
		entries = append(entries, ytEntry{
			url:  link,
			name: util.FirstNonEmpty(entry.Title, entry.ID),
		})
		if len(entries) >= ytdlpMaxEntries {
			break
		}
	}
	if len(entries) == 0 {
		return nil, "", fmt.Errorf("youtube: %s lists no videos", target)
	}
	return entries, util.FirstNonEmpty(probe.Title, probe.ID, "playlist"), nil
}

// ytdlpTitle asks yt-dlp what a single video is called, without downloading
// anything. A host that leaves the transfer to yt-dlp still wants a name to
// show while the item waits its turn in the queue.
func ytdlpTitle(ctx context.Context, ytdlp, target string) (string, error) {
	cmd := exec.CommandContext(ctx, ytdlp, "-J", "--no-warnings", "--ignore-no-formats-error", target)
	output, err := cmd.Output()
	if err != nil {
		return "", ytdlpError(err)
	}
	var probe struct {
		ID    string `json:"id"`
		Title string `json:"title"`
	}
	if err := json.Unmarshal(output, &probe); err != nil {
		return "", fmt.Errorf("could not read yt-dlp output: %w", err)
	}
	return util.FirstNonEmpty(probe.Title, probe.ID), nil
}

// ytdlpError surfaces what yt-dlp printed, which is far more useful to the
// user than a bare exit status.
func ytdlpError(err error) error {
	if exit, ok := errors.AsType[*exec.ExitError](err); ok && len(exit.Stderr) > 0 {
		return fmt.Errorf("%s", util.Truncate(string(exit.Stderr), 300))
	}
	return err
}
