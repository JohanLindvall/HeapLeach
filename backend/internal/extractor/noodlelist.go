// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
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

func (n *NoodleMagazine) list(ctx context.Context, u *url.URL, opts Options) (*Result, error) {
	first := *u
	first.Fragment = ""
	query := first.Query()
	query.Del("p")
	first.RawQuery = query.Encode()
	page := &first
	res := &Result{Title: strings.ReplaceAll(util.PathSegments(u)[1], "+", " ")}
	limit, limitNote := opts.maxSources(), "partial — source limit"
	if opts.maxFiles() < limit {
		limit, limitNote = opts.maxFiles(), "partial — file limit"
	}
	seen := make(map[string]bool)
	for index := range config.MaxAlbumPages {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		ResolveNote(ctx, fmt.Sprintf("Reading listing page %d", index+1))
		doc, final, err := n.client.GetPage(ctx, page.String(), httpx.Referer(first.String()), nil)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if index == 0 {
				return nil, fmt.Errorf("noodlemagazine: fetch listing: %w", err)
			}
			res.Note = "partial — could not fetch next page"
			break
		}
		if strings.TrimRight(final.Path, "/") != strings.TrimRight(first.Path, "/") ||
			(index > 0 && (final.Scheme != first.Scheme || !strings.EqualFold(final.Host, first.Host) || final.RawQuery != page.RawQuery)) {
			if index == 0 {
				return nil, fmt.Errorf("noodlemagazine: listing redirected to another page")
			}
			res.Note = "partial — next page redirected to another listing"
			break
		}
		if index == 0 {
			first = *final
		}
		root, err := parseHTML(doc)
		if err != nil {
			return nil, err
		}
		// An exhausted search serves unrelated "Watching now" cards in a
		// class=list_videos container. Only id=list_videos is this search.
		listing := findFirst(root, func(n *html.Node) bool { return attr(n, "id") == "list_videos" })
		if listing == nil {
			empty := findFirst(root, func(n *html.Node) bool { return attr(n, "data-i18n") == "nothing_found" }) != nil
			if empty {
				break
			}
			if index == 0 {
				return nil, fmt.Errorf("noodlemagazine: no video listing on this page")
			}
			res.Note = "partial — listing is missing on next page"
			break
		}
		if index == 0 {
			res.Title = util.FirstNonEmpty(firstText(root, atom.H1), res.Title)
		}
		before := len(res.Files)
		for _, card := range findAll(listing, func(n *html.Node) bool { return hasClass(n, "item") }) {
			var video *url.URL
			for _, a := range findAll(card, func(n *html.Node) bool { return isElem(n, atom.A) && hasClass(n, "item_link") }) {
				link := noodleLink(final, attr(a, "href"))
				if link != nil && link.Scheme == final.Scheme && strings.EqualFold(link.Host, final.Host) && noodleVideoID(link) != "" {
					video = link
					break
				}
			}
			if video == nil {
				continue
			}
			id := noodleVideoID(video)
			if seen[id] {
				continue
			}
			seen[id] = true
			if len(res.Files) == limit {
				res.Note = limitNote
				return res, nil
			}
			title := util.FirstNonEmpty(textOfFirst(card, func(n *html.Node) bool { return hasClass(n, "title") }), attr(card, "data-video-title-base"), id)
			res.Files = append(res.Files, n.file(video.String(), title))
		}
		if len(res.Files) == before {
			res.Note = "partial — repeated or empty page"
			break
		}
		next, note := noodleNext(root, final)
		if next == nil {
			res.Note = note
			break
		}
		if len(res.Files) == limit {
			res.Note = limitNote
			break
		}
		if index == config.MaxAlbumPages-1 {
			res.Note = "partial — page limit"
		}
		page = next
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(res.Files) == 0 {
		return nil, fmt.Errorf("noodlemagazine: no videos found for this search")
	}
	return res, nil
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
