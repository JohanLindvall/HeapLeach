package extractor

import (
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Shared helpers for tube sites.
//
// These all work the same way: the player configuration sits in the page,
// listing the same video at several qualities, and the links carry a
// signature that expires. So the page is parsed for candidates, the best is
// taken, and resolution is deferred to download time so a link cannot go
// stale while the item waits in the queue.

// mediaCandidate is one rendition of a video.
type mediaCandidate struct {
	URL     string
	Quality int
	IsHLS   bool
}

// qualityPattern finds a vertical resolution in a URL or label: "720p",
// "-1080p.mp4", "quality=480".
var qualityPattern = regexp.MustCompile(`(\d{3,4})\s*[pP]\b|\b(\d{3,4})p`)

// qualityOf reads a resolution out of a string, or 0 when there is none.
func qualityOf(text string) int {
	if m := qualityPattern.FindStringSubmatch(text); m != nil {
		for _, group := range m[1:] {
			if group == "" {
				continue
			}
			if n, err := strconv.Atoi(group); err == nil {
				return n
			}
		}
	}
	return 0
}

// bestCandidate picks the highest quality, preferring a plain file over an
// adaptive playlist at the same resolution: a single file downloads in
// parallel and resumes, where a playlist has to be fetched piece by piece.
func bestCandidate(candidates []mediaCandidate) (mediaCandidate, bool) {
	usable := candidates[:0:0]
	for _, c := range candidates {
		if c.URL != "" {
			usable = append(usable, c)
		}
	}
	if len(usable) == 0 {
		return mediaCandidate{}, false
	}
	sort.SliceStable(usable, func(i, j int) bool {
		if usable[i].Quality != usable[j].Quality {
			return usable[i].Quality > usable[j].Quality
		}
		return !usable[i].IsHLS && usable[j].IsHLS
	})
	return usable[0], true
}

// mediaURLPattern finds direct media links embedded in a page.
var mediaURLPattern = regexp.MustCompile(`https?://[^"'\\\s<>]+?\.(?:mp4|m3u8)(?:\?[^"'\\\s<>]*)?`)

// mediaLinksIn collects the media URLs a page carries, keeping only those
// served by a host related to the site itself.
func mediaLinksIn(doc string, hostHints ...string) []mediaCandidate {
	seen := make(map[string]bool)
	var found []mediaCandidate

	for _, raw := range mediaURLPattern.FindAllString(doc, -1) {
		link := strings.ReplaceAll(raw, `\/`, "/")
		if seen[link] {
			continue
		}
		if len(hostHints) > 0 && !matchesAnyHost(link, hostHints) {
			continue
		}
		seen[link] = true
		found = append(found, mediaCandidate{
			URL:     link,
			Quality: qualityOf(link),
			IsHLS:   strings.Contains(link, ".m3u8"),
		})
	}
	return found
}

func matchesAnyHost(link string, hints []string) bool {
	for _, hint := range hints {
		if strings.Contains(link, hint) {
			return true
		}
	}
	return false
}
