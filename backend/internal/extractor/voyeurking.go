// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/JohanLindvall/HeapLeach/internal/httpx"
	"github.com/JohanLindvall/HeapLeach/internal/util"
	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// VoyeurKing indexes videos hosted by Keep2Share and FileBoom. Listing pages
// carry the detail links; each detail page's React Router data names its file.
// Keep the host's File intact so its resolver, free-download pacing and
// proxy route are chosen by the downloader when the transfer starts.
type VoyeurKing struct {
	client   *httpx.Client
	registry *Registry
}

func NewVoyeurKing(client *httpx.Client, registry *Registry) *VoyeurKing {
	return &VoyeurKing{client: client, registry: registry}
}

func (v *VoyeurKing) Name() string { return "voyeurking" }

// voyeurKingVideoShape is a video's page, /video/<slug>.
var voyeurKingVideoShape = pathShape(`^/video/[^/.]+/?$`)

// voyeurKingRouterContext is the React Router data every page streams.
const voyeurKingRouterContext = "window.__reactRouterContext.streamController.enqueue("

// Recognises is the inventory's description.
func (v *VoyeurKing) Recognises() string {
	return "video pages (`/video/<slug>`) and category and collection listings (`/categories/<name>`, " +
		"`/collection/<slug>`) whose React Router data names the `routes/video.$slug`, `routes/categories` or " +
		"`routes/collection` route"
}

// Shape reports a video or a listing.
func (v *VoyeurKing) Shape(u *url.URL) bool { return voyeurKingVideoShape(u) || voyeurKingListing(u) }

// Recognise takes a page streaming the React Router data video decodes,
// for the route that matches its form: the video route for a video, the
// category or collection route for a listing, whose <main> list walks.
func (v *VoyeurKing) Recognise(_ context.Context, p *Page) bool {
	if !p.Has(voyeurKingRouterContext) {
		return false
	}
	switch {
	case voyeurKingVideoShape(p.Final):
		return p.Has("routes/video.$slug")
	case voyeurKingListing(p.Final):
		return p.Has("<main") && p.HasAny("routes/categories", "routes/collection")
	}
	return false
}

func (v *VoyeurKing) Extract(ctx context.Context, u *url.URL, opts Options) (*Result, error) {
	segs := util.PathSegments(u)
	if len(segs) == 2 && segs[0] == "video" {
		return v.video(ctx, u, opts)
	}
	if !voyeurKingListing(u) {
		return nil, fmt.Errorf("voyeurking: expected /categories/<name>, /collection/<slug> or /video/<slug>")
	}
	// A pasted later page means the entire listing, with its chosen sort.
	first := *u
	first.Path = "/" + segs[0] + "/" + segs[1]
	first.RawPath, first.Fragment = "", ""
	query := first.Query()
	query.Del("page")
	first.RawQuery = query.Encode()
	sources, title, note, err := v.list(ctx, &first, opts.maxSources())
	if err != nil {
		return nil, err
	}
	e := expandSources(ctx, v.registry, sources, opts)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(e.files) == 0 {
		return nil, noneRead("voyeurking", len(sources), "videos", first.Redacted(), e.refusals, e.failed)
	}
	return &Result{Title: title, Note: strings.Join(listingNotes(note, e, len(sources), "videos"), "; "), Files: e.files}, nil
}

func voyeurKingListing(u *url.URL) bool {
	segs := util.PathSegments(u)
	if len(segs) < 2 || (segs[0] != "categories" && segs[0] != "collection") {
		return false
	}
	if len(segs) == 2 {
		return true
	}
	if len(segs) == 4 && segs[2] == "page" {
		page, err := strconv.Atoi(segs[3])
		return err == nil && page > 0
	}
	return false
}

func (v *VoyeurKing) list(ctx context.Context, first *url.URL, limit int) (sources []string, title, note string, err error) {
	section := util.PathSegments(first)
	links, title, note, err := walkListing(ctx, v.client, first, listingWalk{
		name: "voyeurking", title: section[1], none: "listing has no videos",
		limit: limit, limitNote: "partial — source limit",
		read: func(root *html.Node, page, first *url.URL) listingPage {
			return voyeurKingCards(root, page, first, section)
		},
	})
	for _, link := range links {
		sources = append(sources, link.url)
	}
	return sources, title, note, err
}

// voyeurKingCards reads a listing page's <main>: its video links, and its
// rel=next link within the same listing type and slug.
func voyeurKingCards(root *html.Node, page, first *url.URL, section []string) listingPage {
	main := findFirst(root, func(node *html.Node) bool { return isElem(node, atom.Main) })
	if main == nil {
		return listingPage{missing: true}
	}
	p := listingPage{title: firstText(main, atom.H1)}
	invalid := false
	for _, a := range findAll(main, func(node *html.Node) bool { return isElem(node, atom.A) }) {
		link := webLink(page, attr(a, "href"))
		isNext := strings.Contains(" "+attr(a, "rel")+" ", " next ")
		if link == nil || !sameSite(link, first) {
			invalid = invalid || isNext
			continue
		}
		segs := util.PathSegments(link)
		if isNext {
			if voyeurKingListing(link) && segs[0] == section[0] && segs[1] == section[1] {
				p.next = link
			} else {
				invalid = true
			}
		}
		if len(segs) != 2 || segs[0] != "video" {
			continue
		}
		// The thumbnail and title both link to the same page. Tracking
		// queries and fragments do not make them different videos.
		link.Path = strings.TrimRight(link.Path, "/")
		link.RawPath, link.RawQuery = "", ""
		p.links = append(p.links, listingLink{key: link.String(), url: link.String()})
	}
	p.badNext = p.next == nil && invalid
	return p
}

func (v *VoyeurKing) video(ctx context.Context, u *url.URL, opts Options) (*Result, error) {
	doc, _, err := fetchDocument(ctx, v.client, u, nil)
	if err != nil {
		return nil, fmt.Errorf("voyeurking: fetch video: %w", err)
	}
	data, err := voyeurKingPageData(doc)
	if err != nil {
		return nil, err
	}
	loader := data.field(data.field(0, "loaderData"), "routes/video.$slug")
	video := data.field(loader, "video")
	if data.text(data.field(video, "slug")) != util.PathSegments(u)[1] {
		return nil, fmt.Errorf("voyeurking: video data does not match the requested page")
	}
	link, err := ParseURL(data.text(data.field(video, "file")))
	if err != nil {
		return nil, fmt.Errorf("voyeurking: video has no download file")
	}
	// The file host is told by what its page shows, like any other link:
	// a link only shaped like one of theirs is not good enough.
	res, ex, err := v.registry.follow(ctx, link.String(), opts)
	if _, k2s := ex.(*Keep2Share); err == nil && !k2s {
		err = fmt.Errorf("voyeurking: video file is not hosted by K2S or FileBoom")
	}
	if err != nil {
		return nil, err
	}
	res.Title = util.FirstNonEmpty(data.text(data.field(video, "title")), res.Title)
	return res, nil
}

// React Router serialises a flat JSON table into quoted enqueue calls.
// Both object keys ("_12") and values refer to table indices. Follow only
// loaderData -> the video route -> video -> file: scanning every URL would
// also collect player assets, previews and recommendations. No script runs.
type voyeurKingData []json.RawMessage

func voyeurKingPageData(doc string) (voyeurKingData, error) {
	root, err := parseHTML(doc)
	if err != nil {
		return nil, err
	}
	var stream strings.Builder
	for _, script := range findAll(root, func(n *html.Node) bool { return isElem(n, atom.Script) }) {
		var body string
		for node := script.FirstChild; node != nil; node = node.NextSibling {
			if node.Type == html.TextNode {
				body += node.Data
			}
		}
		for {
			_, rest, found := strings.Cut(body, "window.__reactRouterContext.streamController.enqueue(")
			if !found {
				break
			}
			var chunk string
			decoder := json.NewDecoder(strings.NewReader(rest))
			if err := decoder.Decode(&chunk); err != nil {
				return nil, fmt.Errorf("voyeurking: decode page data: %w", err)
			}
			stream.WriteString(chunk)
			body = rest[decoder.InputOffset():]
		}
	}
	var data voyeurKingData
	if err := json.NewDecoder(strings.NewReader(stream.String())).Decode(&data); err != nil {
		return nil, fmt.Errorf("voyeurking: missing or invalid video data: %w", err)
	}
	return data, nil
}

func (d voyeurKingData) field(index int, name string) int {
	if index < 0 || index >= len(d) {
		return -1
	}
	var object map[string]int
	if json.Unmarshal(d[index], &object) != nil {
		return -1
	}
	for key, value := range object {
		if !strings.HasPrefix(key, "_") {
			continue
		}
		i, err := strconv.Atoi(key[1:])
		if err == nil && d.text(i) == name {
			return value
		}
	}
	return -1
}

func (d voyeurKingData) text(index int) string {
	var value string
	if index >= 0 && index < len(d) {
		_ = json.Unmarshal(d[index], &value)
	}
	return value
}
