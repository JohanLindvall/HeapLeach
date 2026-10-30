// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"fmt"
	"net/url"
	"path"
	"slices"
	"strconv"
	"strings"

	"github.com/JohanLindvall/HeapLeach/internal/httpx"
	"github.com/JohanLindvall/HeapLeach/internal/util"
	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// Knuthing embeds Bunny Stream players. The watch page signs the embed,
// which supplies an HLS playlist; refresh the whole chain per attempt so a
// listing can wait in the queue without spending its signatures.
type Knuthing struct {
	client *httpx.Client
}

func NewKnuthing(client *httpx.Client) *Knuthing {
	return &Knuthing{client: client}
}

func (k *Knuthing) Name() string { return "knuthing" }

var (
	// knuthingVideoShape is a watch page or the embedded player.
	knuthingVideoShape = pathShape(`^/watch/[^/]+\.html$`, `^/embed/[^/]+/?$`)
	// knuthingListShape is a category's listing, the one form distinct
	// enough to single out; knuthingListPage is any of its listings.
	knuthingListShape = pathShape(`^/videos/category/[0-9]+/?$`)
	knuthingListPage  = pathShape(`^/videos/[^/]+(?:/[^/]+)*/?$`)
)

// knuthingTheme is where the site's theme serves its scripts from.
const knuthingTheme = "/themes/knuthing/"

// Recognises is the inventory's description.
func (k *Knuthing) Recognises() string {
	return "watch pages (`/watch/<video>.html`) whose player frame or player block the KNUTHING " +
		"theme renders, and its video listings (`/videos/<section>`) by their video list"
}

// Shape reports a watch page, a player or a listing.
func (k *Knuthing) Shape(u *url.URL) bool { return knuthingVideoShape(u) || knuthingListShape(u) }

// Recognise takes a page rendered by the site's theme by what Extract
// reads on it: the player frame, or the player block that stands in for it
// before a free video is confirmed, and a listing's video list.
func (k *Knuthing) Recognise(_ context.Context, p *Page) bool {
	listing := knuthingListPage(p.Final) && !strings.HasSuffix(p.Final.Path, "/rss")
	if !knuthingVideoShape(p.Final) && !listing || !p.Script(knuthingTheme) {
		return false
	}
	if knuthingVideoShape(p.Final) {
		return p.hasElement(func(n *html.Node) bool { return hasClass(n, "bunnyframe") || hasClass(n, "player-video") })
	}
	return p.hasElement(func(n *html.Node) bool { return hasClass(n, "videos-latest-list") })
}

func (k *Knuthing) Extract(ctx context.Context, u *url.URL, opts Options) (*Result, error) {
	parts := util.PathSegments(u)
	if len(parts) == 2 && (parts[0] == "watch" || parts[0] == "embed") {
		video, err := k.readVideo(ctx, u.String())
		if err != nil {
			return nil, err
		}
		return &Result{Title: video.title, Files: []File{k.file(u.String(), video.title)}}, nil
	}
	if len(parts) >= 2 && parts[0] == "videos" && parts[len(parts)-1] != "rss" {
		return k.list(ctx, u, opts)
	}
	return nil, fmt.Errorf("knuthing: expected /watch/<video>.html, /embed/<id> or /videos/<section>")
}

func (k *Knuthing) file(page, title string) File {
	resolve := func(ctx context.Context) (*Target, error) { return k.resolve(ctx, page) }
	return File{
		Name: title + ".ts", Size: -1, Resolve: resolve,
		// Recovery must reopen this video, not its whole listing.
		Refresh: resolve,
	}
}

type knuthingVideo struct {
	title, embed, referer string
}

func (k *Knuthing) readVideo(ctx context.Context, raw string) (*knuthingVideo, error) {
	page, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	headers := httpx.Referer(util.Origin(page) + "/")
	for attempt := range 2 {
		ResolveNote(ctx, "Reading video page")
		root, final, err := fetchRoot(ctx, k.client, page, headers)
		if err != nil {
			return nil, fmt.Errorf("knuthing: fetch video page: %w", err)
		}
		frame := findFirst(root, func(n *html.Node) bool {
			return isElem(n, atom.Iframe) && hasClass(n, "bunnyframe")
		})
		if frame != nil {
			if embed := webLink(final, attr(frame, "src")); embed != nil {
				title := util.FirstNonEmpty(metaContent(root, "og:title"), firstText(root, atom.Title), util.NameFromURL(final.String()))
				return &knuthingVideo{title: title, embed: embed.String(), referer: final.String()}, nil
			}
			return nil, fmt.Errorf("knuthing: player has no usable embed URL")
		}
		// Free videos can first offer a confirmation link. Follow only the
		// link the page supplies, for this same video, and at most once.
		if attempt == 0 && final.Query().Get("limitprompt") != "true" {
			var confirm *url.URL
			for _, a := range findAll(root, func(n *html.Node) bool { return isElem(n, atom.A) }) {
				link := webLink(final, attr(a, "href"))
				if link != nil && sameSite(link, final) && link.Path == final.Path && link.Query().Get("limitprompt") == "true" {
					confirm = link
					break
				}
			}
			if confirm != nil {
				page, headers = confirm, httpx.Referer(final.String())
				continue
			}
		}
		if player := findFirst(root, func(n *html.Node) bool { return hasClass(n, "player-video") }); player != nil {
			if notice := findFirst(player, func(n *html.Node) bool { return hasClass(n, "video-processing") }); notice != nil {
				if reason := firstText(notice, atom.H5); reason != "" {
					return nil, fmt.Errorf("knuthing: %s", reason)
				}
			}
		}
		return nil, fmt.Errorf("knuthing: no accessible video player (the video may be restricted or removed)")
	}
	return nil, fmt.Errorf("knuthing: video confirmation did not open a player")
}

func (k *Knuthing) resolve(ctx context.Context, page string) (*Target, error) {
	video, err := k.readVideo(ctx, page)
	if err != nil {
		return nil, err
	}
	ResolveNote(ctx, "Reading video player")
	doc, playerURL, err := k.client.GetPage(ctx, video.embed, httpx.Referer(video.referer), nil)
	if err != nil {
		return nil, fmt.Errorf("knuthing: fetch player: %w", err)
	}
	root, err := parseHTML(doc)
	if err != nil {
		return nil, err
	}
	player := findFirst(root, func(n *html.Node) bool {
		return n.Type == html.ElementNode && (n.Data == "bunny-stream-video" || n.Data == "bunny-stream-video-ads")
	})
	if player == nil {
		return nil, fmt.Errorf("knuthing: no Bunny Stream video in the player")
	}
	link := webLink(playerURL, attr(player, "content-src"))
	if link == nil || !strings.EqualFold(path.Ext(link.Path), ".m3u8") {
		return nil, fmt.Errorf("knuthing: player has no HLS source")
	}
	headers := httpx.Referer(playerURL.String())
	ResolveNote(ctx, "Reading video playlist")
	media, err := resolveMediaPlaylist(ctx, k.client, link.String(), headers)
	if err != nil {
		return nil, fmt.Errorf("knuthing: video playlist: %w", err)
	}
	if live, why := hlsLiveEdge(media.Doc); live {
		return nil, fmt.Errorf("knuthing: video playlist is still being written: %s", why)
	}
	return media.target(link.String(), video.title+media.extension(), headers), nil
}

// Listings publish titles on their cards. Opening every watch page here
// would spend short-lived signatures and hundreds of requests to learn
// nothing needed by the queue; each file reads its own page when it starts.
func (k *Knuthing) list(ctx context.Context, u *url.URL, opts Options) (*Result, error) {
	first := *u
	first.Fragment = ""
	query := first.Query()
	query.Set("page_id", "1")
	query.Del("last_page")
	first.RawQuery = query.Encode()
	limit, limitNote := listingLimit(opts)
	links, title, note, err := walkListing(ctx, k.client, &first, listingWalk{
		name: "knuthing", title: strings.Join(util.PathSegments(u)[1:], " - "), none: "listing has no videos",
		limit: limit, limitNote: limitNote, read: knuthingCards,
	})
	if err != nil {
		return nil, err
	}
	res := &Result{Title: title, Note: note}
	for _, link := range links {
		res.Files = append(res.Files, k.file(link.url, link.title))
	}
	return res, nil
}

// knuthingCards reads the page's own video cards, not the sidebar's.
func knuthingCards(root *html.Node, page, first *url.URL) listingPage {
	content := findFirst(root, func(n *html.Node) bool { return attr(n, "id") == "container_content" })
	if content == nil {
		content = root
	}
	listing := findFirst(content, func(n *html.Node) bool { return hasClass(n, "videos-latest-list") })
	if listing == nil {
		return listingPage{missing: true}
	}
	p := listingPage{title: textOfFirst(content, func(n *html.Node) bool { return hasClass(n, "page_head") })}
	for _, card := range findAll(listing, func(n *html.Node) bool { return hasClass(n, "video-wrapper") }) {
		for _, a := range findAll(card, func(n *html.Node) bool { return isElem(n, atom.A) }) {
			link := webLink(page, attr(a, "href"))
			if link == nil || !sameSite(link, first) {
				continue
			}
			if parts := util.PathSegments(link); len(parts) == 2 && parts[0] == "watch" {
				p.links = append(p.links, listingLink{
					key: util.FirstNonEmpty(attr(card, "data-id"), link.Path), url: link.String(),
					title: util.FirstNonEmpty(firstText(card, atom.H4), util.NameFromURL(link.String())),
				})
				break
			}
		}
	}
	p.next, p.note = knuthingNext(content, page, first)
	return p
}

// Follow the listing's pager, keeping its section and filters. Numeric
// links provide a fallback for translated Next Page labels; the last-page
// shortcut never substitutes for the next page.
func knuthingNext(root *html.Node, page, first *url.URL) (*url.URL, string) {
	pager := findFirst(root, func(n *html.Node) bool { return hasClass(n, "pagination") })
	if pager == nil {
		return nil, ""
	}
	current, _ := strconv.Atoi(page.Query().Get("page_id"))
	current = max(1, current)
	var fallback *url.URL
	for _, a := range findAll(pager, func(n *html.Node) bool { return isElem(n, atom.A) }) {
		explicit := strings.EqualFold(attr(a, "title"), "Next Page") || slices.Contains(strings.Fields(attr(a, "rel")), "next")
		link := webLink(page, attr(a, "href"))
		number := 0
		if link != nil {
			number, _ = strconv.Atoi(link.Query().Get("page_id"))
		}
		if !explicit && number != current+1 {
			continue
		}
		valid := link != nil && sameSite(link, first) && samePath(link, first) && number == current+1
		if valid {
			query := link.Query()
			filters := first.Query()
			filters.Del("page_id")
			filters.Del("last_page")
			for key, values := range query {
				if key != "page_id" && key != "last_page" && !slices.Equal(values, filters[key]) {
					valid = false
				}
			}
			for key, values := range filters {
				query[key] = values
			}
			link.RawQuery = query.Encode()
		}
		if explicit {
			if !valid {
				return nil, "partial — invalid next page"
			}
			return link, ""
		}
		if valid {
			fallback = link
		}
	}
	return fallback, ""
}
