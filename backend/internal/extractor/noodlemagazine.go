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
	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// NoodleMagazine reads watch pages and search listings. Player links are
// signed per visit, so only titles are queued and each attempt reopens its
// own watch page, including when recovering on another route.
type NoodleMagazine struct {
	client *httpx.Client
}

func NewNoodleMagazine(client *httpx.Client) *NoodleMagazine {
	return &NoodleMagazine{client: client}
}

func (n *NoodleMagazine) Name() string { return "noodlemagazine" }

// noodleListShape is a search listing, /video/<query>.
var noodleListShape = pathShape(`^/video/[^/]+/?$`)

// Recognises is the inventory's description.
func (n *NoodleMagazine) Recognises() string {
	return "watch pages (`/watch/<id>`) named Noodlemagazine carrying the player's `window.playlist`, " +
		"and search listings (`/video/<query>`) of its video cards"
}

// Shape reports a watch page or a listing.
func (n *NoodleMagazine) Shape(u *url.URL) bool { return noodleVideoID(u) != "" || noodleListShape(u) }

// Recognise takes a watch page by the playlist Extract reads, and a
// listing by the card list it walks, rendered with the site's search
// script.
func (n *NoodleMagazine) Recognise(_ context.Context, p *Page) bool {
	if noodleVideoID(p.Final) != "" {
		return noodlePlaylist.MatchString(p.Body) && p.Named("noodlemagazine")
	}
	return noodleListShape(p.Final) && p.Script("/static/js/search.") &&
		p.hasElement(func(n *html.Node) bool { return attr(n, "id") == "list_videos" }) &&
		p.hasElement(func(n *html.Node) bool { return hasClass(n, "item_link") })
}

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
	pageURL, err := url.Parse(page)
	if err != nil {
		return nil, fmt.Errorf("noodlemagazine: %w", err)
	}
	doc, final, err := fetchDocument(ctx, n.client, pageURL, nil)
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
	headers := httpx.Referer(video.referer)
	if !video.media.IsHLS {
		return &Target{URL: video.media.URL, Name: video.title + ".mp4", Size: -1, Headers: headers}, nil
	}
	ResolveNote(ctx, "Reading video playlist")
	media, err := resolveMediaPlaylist(ctx, n.client, video.media.URL, headers)
	if err != nil {
		return nil, fmt.Errorf("noodlemagazine: video playlist: %w", err)
	}
	if live, why := hlsLiveEdge(media.Doc); live {
		return nil, fmt.Errorf("noodlemagazine: video playlist is still being written: %s", why)
	}
	return media.target(video.media.URL, video.title+media.extension(), headers), nil
}

func (n *NoodleMagazine) list(ctx context.Context, u *url.URL, opts Options) (*Result, error) {
	first := *u
	first.Fragment = ""
	query := first.Query()
	query.Del("p")
	first.RawQuery = query.Encode()
	limit, limitNote := listingLimit(opts)
	links, title, note, err := walkListing(ctx, n.client, &first, listingWalk{
		name: "noodlemagazine", title: strings.ReplaceAll(util.PathSegments(u)[1], "+", " "),
		none: "no videos found for this search", limit: limit, limitNote: limitNote, read: noodleCards,
	})
	if err != nil {
		return nil, err
	}
	res := &Result{Title: title, Note: note}
	for _, link := range links {
		res.Files = append(res.Files, n.file(link.url, link.title))
	}
	return res, nil
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
			title := util.FirstNonEmpty(textOfFirst(card, func(n *html.Node) bool { return hasClass(n, "title") }), attr(card, "data-video-title-base"), id)
			p.links = append(p.links, listingLink{key: id, url: link.String(), title: title})
			break
		}
	}
	p.next, p.note = noodleNext(root, page)
	return p
}

// The "more" control supplies the zero-based page index. GET renders the
// same results as the player's POST, with a fresh pager; preserve the search
// path's escaping and every filter when advancing it.
func noodleNext(root *html.Node, page *url.URL) (*url.URL, string) {
	control := findFirst(root, func(n *html.Node) bool {
		return hasClass(n, "more") && !hasAttr(n, "hidden") && attr(n, "data-action") == ""
	})
	if control == nil {
		return nil, ""
	}
	query := page.Query()
	current, _ := strconv.Atoi(query.Get("p"))
	next, err := strconv.Atoi(attr(control, "data-page"))
	if err != nil || next <= 0 || next != current+1 {
		return nil, "partial — invalid next page"
	}
	link := *page
	query.Set("p", strconv.Itoa(next))
	link.RawQuery = query.Encode()
	return &link, ""
}
