// SPDX-License-Identifier: MIT

package extractor

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"

	"github.com/JohanLindvall/HeapLeach/internal/httpx"
	"github.com/JohanLindvall/HeapLeach/internal/util"
	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

var (
	// noodleListShape is a search listing, /video/<query>.
	noodleListShape = pathShape(`^/video/[^/]+/?$`)
	noodleWatchPath = regexp.MustCompile(`^/watch/(-?\d+_\d+)/?$`)
	noodlePlaylist  = regexp.MustCompile(`\bwindow\.playlist\s*=\s*`)
)

// Watch pages and search listings. Player links are signed per visit, so
// the row peeks: only titles are queued, and each attempt reopens its own
// watch page, including when recovering on another route. The media
// referer is the watch page as finally served.
var noodlePlayer = playerSite{
	finished: true,
	name:     "noodlemagazine",
	shape:    func(u *url.URL) bool { return noodleVideoID(u) != "" || noodleListShape(u) },
	// A watch page by the playlist read reads, and a listing by the card
	// list it walks, rendered with the site's search script.
	recognise: func(p *Page) bool {
		if noodleVideoID(p.Final) != "" {
			return noodlePlaylist.MatchString(p.Body) && p.Named("noodlemagazine")
		}
		return noodleListShape(p.Final) && p.Script("/static/js/search.") &&
			p.hasElement(func(n *html.Node) bool { return attr(n, "id") == "list_videos" }) &&
			p.hasElement(func(n *html.Node) bool { return hasClass(n, "item_link") })
	},
	recognises: "watch pages (`/watch/<id>`) named Noodlemagazine carrying the player's `window.playlist`, " +
		"and search listings (`/video/<query>`) of its video cards",
	page: func(u *url.URL) (string, error) {
		if noodleVideoID(u) != "" {
			return u.String(), nil
		}
		return "", errors.New("expected /watch/<id> or /video/<query>")
	},
	// The page carries the sources, so reading it is what tells a video
	// that plays from one that does not; the signed links go unused.
	peek: func(ctx context.Context, client *httpx.Client, page string) (string, error) {
		video, err := noodleRead(ctx, client, page)
		if err != nil {
			return "", err
		}
		return video.title, nil
	},
	read: noodleRead,
	listing: &playerListing{
		shape: func(u *url.URL) bool {
			parts := util.PathSegments(u)
			return len(parts) == 2 && parts[0] == "video"
		},
		walk: noodleWalk,
	},
}

func noodleVideoID(u *url.URL) string {
	if m := noodleWatchPath.FindStringSubmatch(u.Path); m != nil {
		return m[1]
	}
	return ""
}

// noodleRead reads the watch page's playlist: its MP4 and HLS sources.
func noodleRead(ctx context.Context, client *httpx.Client, page string) (*playerVideo, error) {
	ResolveNote(ctx, "Reading video page")
	pageURL, err := url.Parse(page)
	if err != nil {
		return nil, err
	}
	d, err := readDocument(ctx, client, pageURL, nil)
	if err != nil {
		return nil, fmt.Errorf("fetch video: %w", err)
	}
	doc, final := d.body, d.final
	marker := noodlePlaylist.FindStringIndex(doc)
	if marker == nil {
		return nil, fmt.Errorf("no player on this page (the video may be restricted or removed)")
	}
	var playlist struct {
		Sources []struct {
			File  string    `json:"file"`
			Label flexValue `json:"label"`
			Type  string    `json:"type"`
		} `json:"sources"`
	}
	if err := json.NewDecoder(strings.NewReader(doc[marker[1]:])).Decode(&playlist); err != nil {
		return nil, fmt.Errorf("read player sources: %w", err)
	}
	var candidates []mediaCandidate
	for _, source := range playlist.Sources {
		link := webLink(final, source.File)
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
	if _, ok := bestCandidate(candidates); !ok {
		return nil, fmt.Errorf("player has no usable MP4 or HLS sources")
	}
	// The tree recognition built when it read the page's name, if this is
	// the probed page; at transfer time the page is fresh and parsed here.
	root, err := d.parse()
	if err != nil {
		return nil, err
	}
	return &playerVideo{
		title:      cmp.Or(metaContent(root, "og:title"), firstText(root, atom.H1), noodleVideoID(final)),
		candidates: candidates,
		referer:    final.String(),
	}, nil
}

// noodleWalk starts a search at its first page, keeping its filters.
func noodleWalk(u *url.URL) (*url.URL, listingWalk) {
	first := *u
	first.Fragment = ""
	query := first.Query()
	query.Del("p")
	first.RawQuery = query.Encode()
	return &first, listingWalk{
		title: strings.ReplaceAll(util.PathSegments(u)[1], "+", " "),
		none:  "no videos found for this search", read: noodleCards,
	}
}

// noodleCards reads a search page's cards. An exhausted search serves
// unrelated "Watching now" cards in a class=list_videos container: only
// id=list_videos is this search.
func noodleCards(root *html.Node, page, _ *url.URL) listingPage {
	listing := findFirst(root, func(n *html.Node) bool { return attr(n, "id") == "list_videos" })
	if listing == nil {
		empty := findFirst(root, func(n *html.Node) bool { return attr(n, "data-i18n") == "nothing_found" }) != nil
		return listingPage{end: empty, missing: !empty}
	}
	p := listingPage{title: firstText(root, atom.H1)}
	for _, card := range findAll(listing, func(n *html.Node) bool { return hasClass(n, "item") }) {
		for _, a := range findAll(card, func(n *html.Node) bool { return isElem(n, atom.A) && hasClass(n, "item_link") }) {
			link := webLink(page, attr(a, "href"))
			if link == nil || !sameSite(link, page) || noodleVideoID(link) == "" {
				continue
			}
			id := noodleVideoID(link)
			title := cmp.Or(textOfFirst(card, func(n *html.Node) bool { return hasClass(n, "title") }), attr(card, "data-video-title-base"), id)
			p.links = append(p.links, listingLink{key: id, url: link.String(), title: title})
			break
		}
	}
	p.next, p.badNext = noodleNext(root, page)
	return p
}

// The "more" control supplies the zero-based page index. GET renders the
// same results as the player's POST, with a fresh pager; preserve the search
// path's escaping and every filter when advancing it.
// A control naming any page but the next one is an invalid next page.
func noodleNext(root *html.Node, page *url.URL) (next *url.URL, invalid bool) {
	control := findFirst(root, func(n *html.Node) bool {
		return hasClass(n, "more") && !hasAttr(n, "hidden") && attr(n, "data-action") == ""
	})
	if control == nil {
		return nil, false
	}
	query := page.Query()
	current, _ := strconv.Atoi(query.Get("p"))
	number, err := strconv.Atoi(attr(control, "data-page"))
	if err != nil || number <= 0 || number != current+1 {
		return nil, true
	}
	link := *page
	query.Set("p", strconv.Itoa(number))
	link.RawQuery = query.Encode()
	return &link, false
}
