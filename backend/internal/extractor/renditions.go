// SPDX-License-Identifier: MIT

package extractor

import (
	"cmp"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// Renditions: the same video offered at several qualities, and choosing
// among them.
//
// A player page lists the same video at several qualities, and its links
// carry a signature that expires. So the page is parsed for candidates, the
// best is taken, and resolution is deferred to download time so a link
// cannot go stale while the item waits in the queue. A ladder of named
// qualities, where a site names rather than numbers them, is read with
// rankIn.

// mediaCandidate is one rendition of a video.
type mediaCandidate struct {
	URL     string
	Quality int
	IsHLS   bool
	// Size is the rendition's exact length in bytes, where the source
	// states one; zero or less states none.
	Size int64
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
// Of two otherwise equal, the larger is taken; past that, the first.
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
	slices.SortStableFunc(usable, func(a, b mediaCandidate) int {
		return cmp.Or(
			cmp.Compare(b.Quality, a.Quality), // highest first
			cmpBool(a.IsHLS, b.IsHLS),         // a plain file before a playlist
			cmp.Compare(b.Size, a.Size),
		)
	})
	return usable[0], true
}

// cmpBool orders false before true, so a ranking can prefer the candidates
// that have a property by comparing b to a, and the ones without it by
// comparing a to b — the same way cmp.Compare reads for numbers.
func cmpBool(a, b bool) int {
	switch {
	case a == b:
		return 0
	case b:
		return -1
	default:
		return 1
	}
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

// rankIn is name's place in order, compared without regard to case, or
// unknown when order does not hold it. Where an unknown name lands is each
// caller's to say: last (len(order)) for an ordering that still takes it,
// or -1 for one read as an allow-list or with the best at the end.
func rankIn(order []string, name string, unknown int) int {
	if i := slices.IndexFunc(order, func(known string) bool { return strings.EqualFold(name, known) }); i >= 0 {
		return i
	}
	return unknown
}
