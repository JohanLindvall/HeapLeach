// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"

	"github.com/JohanLindvall/HeapLeach/internal/httpx"
	"github.com/JohanLindvall/HeapLeach/internal/util"
	"golang.org/x/net/html/atom"
)

// NoodleMagazine reads watch pages and search listings. Player links are
// signed per visit, so only titles are queued and each attempt reopens its
// own watch page, including when recovering on another route.
type NoodleMagazine struct {
	hostSet
	client *httpx.Client
}

func NewNoodleMagazine(client *httpx.Client) *NoodleMagazine {
	return &NoodleMagazine{hostSet: hostSet{"noodlemagazine.com"}, client: client}
}

func (n *NoodleMagazine) Name() string { return "noodlemagazine" }

func (n *NoodleMagazine) Extract(ctx context.Context, u *url.URL, opts Options) (*Result, error) {
	if noodleVideoID(u) != "" {
		video, err := n.readVideo(ctx, u.String())
		if err != nil {
			return nil, err
		}
		return &Result{Title: video.title, Files: []File{n.file(u.String(), video.title)}}, nil
	}
	if parts := util.PathSegments(u); len(parts) == 2 && parts[0] == "video" {
		return n.list(ctx, u, opts)
	}
	return nil, fmt.Errorf("noodlemagazine: expected /watch/<id> or /video/<query>")
}

func (n *NoodleMagazine) file(page, title string) File {
	resolve := func(ctx context.Context) (*Target, error) { return n.resolve(ctx, page) }
	return File{Name: title + ".mp4", Size: -1, Resolve: resolve, Refresh: resolve}
}

var (
	noodleWatchPath = regexp.MustCompile(`^/watch/(-?\d+_\d+)/?$`)
	noodlePlaylist  = regexp.MustCompile(`\bwindow\.playlist\s*=\s*`)
)

func noodleVideoID(u *url.URL) string {
	if m := noodleWatchPath.FindStringSubmatch(u.Path); m != nil {
		return m[1]
	}
	return ""
}

type noodleVideo struct {
	title, referer string
	media          mediaCandidate
}

func (n *NoodleMagazine) readVideo(ctx context.Context, page string) (*noodleVideo, error) {
	ResolveNote(ctx, "Reading video page")
	doc, final, err := n.client.GetPage(ctx, page, nil, nil)
	if err != nil {
		return nil, fmt.Errorf("noodlemagazine: fetch video: %w", err)
	}
	marker := noodlePlaylist.FindStringIndex(doc)
	if marker == nil {
		return nil, fmt.Errorf("noodlemagazine: no player on this page (the video may be restricted or removed)")
	}
	var playlist struct {
		Sources []struct {
			File  string    `json:"file"`
			Label flexValue `json:"label"`
			Type  string    `json:"type"`
		} `json:"sources"`
	}
	if err := json.NewDecoder(strings.NewReader(doc[marker[1]:])).Decode(&playlist); err != nil {
		return nil, fmt.Errorf("noodlemagazine: read player sources: %w", err)
	}
	var candidates []mediaCandidate
	for _, source := range playlist.Sources {
		link := noodleLink(final, source.File)
		if link == nil {
			continue
		}
		ext := strings.ToLower(path.Ext(link.Path))
		kind := strings.ToLower(source.Type)
		isHLS := kind == "hls" || kind == "application/x-mpegurl" || kind == "application/vnd.apple.mpegurl" || ext == ".m3u8"
		if !isHLS && kind != "mp4" && kind != "video/mp4" && ext != ".mp4" {
			continue
		}
		quality, _ := strconv.Atoi(source.Label.String())
		if quality == 0 {
			quality = max(qualityOf(source.Label.String()), qualityOf(link.Path))
		}
		candidates = append(candidates, mediaCandidate{URL: link.String(), Quality: quality, IsHLS: isHLS})
	}
	best, ok := bestCandidate(candidates)
	if !ok {
		return nil, fmt.Errorf("noodlemagazine: player has no usable MP4 or HLS sources")
	}
	root, err := parseHTML(doc)
	if err != nil {
		return nil, err
	}
	title := util.FirstNonEmpty(metaContent(root, "og:title"), firstText(root, atom.H1), noodleVideoID(final))
	return &noodleVideo{title: title, referer: final.String(), media: best}, nil
}

func (n *NoodleMagazine) resolve(ctx context.Context, page string) (*Target, error) {
	video, err := n.readVideo(ctx, page)
	if err != nil {
		return nil, err
	}
	target := &Target{URL: video.media.URL, Name: video.title + ".mp4", Size: -1, Headers: httpx.Referer(video.referer)}
	if video.media.IsHLS {
		ResolveNote(ctx, "Reading video playlist")
		media, err := resolveMediaPlaylist(ctx, n.client, video.media.URL, target.Headers)
		if err != nil {
			return nil, fmt.Errorf("noodlemagazine: video playlist: %w", err)
		}
		if live, why := hlsLiveEdge(media.Doc); live {
			return nil, fmt.Errorf("noodlemagazine: video playlist is still being written: %s", why)
		}
		target.Name = video.title + media.extension()
		target.Segments, target.SegmentKey = media.Segments, media.Key
		target.Audio, target.Subtitles = media.Audio, media.Subtitles
	}
	return target, nil
}

func noodleLink(base *url.URL, ref string) *url.URL {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return nil
	}
	link, err := base.Parse(ref)
	if err != nil || link.Hostname() == "" || link.User != nil || (link.Scheme != "https" && link.Scheme != "http") {
		return nil
	}
	link.Fragment = ""
	return link
}
