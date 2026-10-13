// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
	"github.com/JohanLindvall/HeapLeach/internal/util"
	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

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
	page := &first
	res := &Result{Title: strings.Join(util.PathSegments(u)[1:], " - ")}
	limit, limitNote := opts.maxSources(), "partial — source limit"
	if opts.maxFiles() < limit {
		limit, limitNote = opts.maxFiles(), "partial — file limit"
	}
	seen, pages := make(map[string]bool), make(map[string]bool)
	for n := range config.MaxAlbumPages {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if pages[page.String()] {
			res.Note = "partial — repeated page"
			break
		}
		pages[page.String()] = true
		doc, final, err := k.client.GetPage(ctx, page.String(), httpx.Referer(first.String()), nil)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if n == 0 {
				return nil, fmt.Errorf("knuthing: fetch listing: %w", err)
			}
			res.Note = "partial — could not fetch next page"
			break
		}
		if final.String() != page.String() && pages[final.String()] {
			res.Note = "partial — repeated page"
			break
		}
		pages[final.String()] = true
		if strings.TrimRight(final.Path, "/") != strings.TrimRight(first.Path, "/") {
			return nil, fmt.Errorf("knuthing: listing redirected to another page")
		}
		if n == 0 {
			first = *final
		}
		root, err := parseHTML(doc)
		if err != nil {
			return nil, err
		}
		content := findFirst(root, func(n *html.Node) bool { return attr(n, "id") == "container_content" })
		if content == nil {
			content = root
		}
		listing := findFirst(content, func(n *html.Node) bool { return hasClass(n, "videos-latest-list") })
		if listing == nil {
			if n == 0 {
				return nil, fmt.Errorf("knuthing: no video listing on this page")
			}
			res.Note = "partial — listing is missing on next page"
			break
		}
		if n == 0 {
			res.Title = util.FirstNonEmpty(textOfFirst(content, func(n *html.Node) bool { return hasClass(n, "page_head") }), res.Title)
		}
		before := len(res.Files)
		for _, card := range findAll(listing, func(n *html.Node) bool { return hasClass(n, "video-wrapper") }) {
			var video *url.URL
			for _, a := range findAll(card, func(n *html.Node) bool { return isElem(n, atom.A) }) {
				link := knuthingLink(final, attr(a, "href"))
				if link == nil || link.Scheme != first.Scheme || !strings.EqualFold(link.Host, first.Host) {
					continue
				}
				if parts := util.PathSegments(link); len(parts) == 2 && parts[0] == "watch" {
					video = link
					break
				}
			}
			if video == nil {
				continue
			}
			key := util.FirstNonEmpty(attr(card, "data-id"), video.Path)
			if seen[key] {
				continue
			}
			seen[key] = true
			if len(res.Files) == limit {
				res.Note = limitNote
				return res, nil
			}
			title := util.FirstNonEmpty(firstText(card, atom.H4), util.NameFromURL(video.String()))
			res.Files = append(res.Files, k.file(video.String(), title))
		}
		next, note := knuthingNext(content, final, &first)
		if len(res.Files) == before && n > 0 {
			res.Note = "partial — repeated or empty page"
			break
		}
		if next == nil {
			res.Note = note
			break
		}
		if len(res.Files) == limit {
			res.Note = limitNote
			break
		}
		if n == config.MaxAlbumPages-1 {
			res.Note = "partial — page limit"
		}
		page = next
	}
	// A walk that ended while the caller cancelled is a cancellation, not a
	// partial listing: the page that ended it may have been answered before
	// its request saw the cancellation.
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(res.Files) == 0 {
		return nil, fmt.Errorf("knuthing: listing has no videos")
	}
	return res, nil
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
		link := knuthingLink(page, attr(a, "href"))
		number := 0
		if link != nil {
			number, _ = strconv.Atoi(link.Query().Get("page_id"))
		}
		if !explicit && number != current+1 {
			continue
		}
		valid := link != nil && link.Scheme == first.Scheme && strings.EqualFold(link.Host, first.Host) &&
			strings.TrimRight(link.Path, "/") == strings.TrimRight(first.Path, "/") && number == current+1
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
