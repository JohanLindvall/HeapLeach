// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
	"github.com/JohanLindvall/HeapLeach/internal/util"
	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// VoyeurKing indexes videos hosted by Keep2Share. Listing pages carry the
// detail links; each detail page's React Router data names its K2S file.
// Keep the host's File intact so its resolver, free-download pacing and
// proxy route are chosen by the downloader when the transfer starts.
type VoyeurKing struct {
	hostSet
	client   *httpx.Client
	registry *Registry
}

func NewVoyeurKing(client *httpx.Client, registry *Registry) *VoyeurKing {
	return &VoyeurKing{hostSet: hostSet{"voyeurking.com"}, client: client, registry: registry}
}

func (v *VoyeurKing) Name() string { return "voyeurking" }

func (v *VoyeurKing) Extract(ctx context.Context, u *url.URL, opts Options) (*Result, error) {
	segs := util.PathSegments(u)
	if len(segs) == 2 && segs[0] == "video" {
		return v.video(ctx, u, opts)
	}
	if !voyeurKingCategory(u) {
		return nil, fmt.Errorf("voyeurking: expected /categories/<name> or /video/<slug>")
	}
	// A pasted later page means the entire category, with its chosen sort.
	first := *u
	first.Path = "/categories/" + segs[1]
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
		return nil, fmt.Errorf("voyeurking: no accessible K2S files in %d videos", len(sources))
	}
	var notes []string
	if note != "" {
		notes = append(notes, note)
	}
	if e.full {
		notes = append(notes, "partial — file limit")
	}
	if e.used < len(sources) {
		notes = append(notes, fmt.Sprintf("%d of %d videos resolved", e.used, len(sources)))
	}
	return &Result{Title: title, Note: strings.Join(notes, "; "), Files: e.files}, nil
}

func voyeurKingCategory(u *url.URL) bool {
	segs := util.PathSegments(u)
	if len(segs) == 2 {
		return segs[0] == "categories"
	}
	if len(segs) == 4 && segs[0] == "categories" && segs[2] == "page" {
		page, err := strconv.Atoi(segs[3])
		return err == nil && page > 0
	}
	return false
}

func (v *VoyeurKing) list(ctx context.Context, first *url.URL, limit int) (sources []string, title, note string, err error) {
	seen, pages := make(map[string]bool), make(map[string]bool)
	page := first
	title = util.PathSegments(first)[1]
	for n := range config.MaxAlbumPages {
		if pages[page.String()] {
			return sources, title, "partial — repeated page", nil
		}
		pages[page.String()] = true
		doc, fetchErr := v.client.GetString(ctx, page.String(), httpx.Referer(first.String()))
		if fetchErr != nil {
			if ctx.Err() != nil {
				return nil, "", "", ctx.Err()
			}
			if n == 0 {
				return nil, "", "", fmt.Errorf("voyeurking: fetch category: %w", fetchErr)
			}
			return sources, title, "partial — could not fetch next page", nil
		}
		root, parseErr := parseHTML(doc)
		if parseErr != nil {
			return nil, "", "", parseErr
		}
		main := findFirst(root, func(node *html.Node) bool { return isElem(node, atom.Main) })
		if main == nil {
			if n > 0 {
				return sources, title, "partial — category listing is missing on next page", nil
			}
			return nil, "", "", fmt.Errorf("voyeurking: category listing is missing")
		}
		if n == 0 {
			title = util.FirstNonEmpty(firstText(main, atom.H1), title)
		}
		before := len(sources)
		var next *url.URL
		for _, a := range findAll(main, func(node *html.Node) bool { return isElem(node, atom.A) }) {
			link, linkErr := page.Parse(strings.TrimSpace(attr(a, "href")))
			isNext := strings.Contains(" "+attr(a, "rel")+" ", " next ")
			if linkErr != nil || link.User != nil || link.Scheme != first.Scheme || !strings.EqualFold(link.Host, first.Host) {
				if isNext {
					note = "partial — invalid next page"
				}
				continue
			}
			link.Fragment = ""
			segs := util.PathSegments(link)
			if isNext {
				if voyeurKingCategory(link) && segs[1] == util.PathSegments(first)[1] {
					next = link
				} else {
					note = "partial — invalid next page"
				}
			}
			if len(segs) != 2 || segs[0] != "video" {
				continue
			}
			// The thumbnail and title both link to the same page. Tracking
			// queries and fragments do not make them different videos.
			link.Path = strings.TrimRight(link.Path, "/")
			link.RawPath, link.RawQuery = "", ""
			if seen[link.String()] {
				continue
			}
			seen[link.String()] = true
			if len(sources) == limit {
				return sources, title, "partial — source limit", nil
			}
			sources = append(sources, link.String())
		}
		if next == nil {
			return sources, title, note, nil
		}
		if len(sources) == before {
			return sources, title, "partial — repeated or empty page", nil
		}
		if len(sources) == limit {
			return sources, title, "partial — source limit", nil
		}
		page = next
	}
	return sources, title, "partial — page limit", nil
}

func (v *VoyeurKing) video(ctx context.Context, u *url.URL, opts Options) (*Result, error) {
	doc, err := v.client.GetString(ctx, u.String(), nil)
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
		return nil, fmt.Errorf("voyeurking: video has no K2S file")
	}
	ex, known := v.registry.Known(link)
	if !known || ex.Name() != "keep2share" {
		return nil, fmt.Errorf("voyeurking: video file is not hosted by K2S")
	}
	res, _, err := v.registry.Extract(ctx, link.String(), opts)
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
