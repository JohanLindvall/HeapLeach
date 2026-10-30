// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"

	"github.com/JohanLindvall/HeapLeach/internal/httpx"
	"github.com/JohanLindvall/HeapLeach/internal/util"
)

// Sendvid resolves sendvid.com videos: a watch page /<id>, or the embed
// /embed/<id>, which carries the same player.
//
// The player's one <source> names the file on a media host
// (videosN.sendvid.com), signed for a window of a few hours
// (validfrom/validto) and with a rate (rate=250k) that the media host holds
// each connection to. The signature covers all three, so the rate cannot be
// lifted, but it is per connection: a file split across several goes as
// many times faster, which the downloader does for a slow file anyway. The
// link is minted again when the transfer starts (refetchedVideo), since a
// queue can wait longer than the window.
//
// Two things on the page are not what they look like. og:video names
// /<id>.mp4, which only redirects back to the page. And the embed page's
// <title> is the site's name alone, so the title is og:title. A video that
// is gone answers 404.
type Sendvid struct {
	client *httpx.Client
}

// NewSendvid builds the sendvid extractor.
func NewSendvid(client *httpx.Client) *Sendvid {
	return &Sendvid{client: client}
}

func (s *Sendvid) Name() string { return "sendvid" }

var (
	// sendvidShape is the player, /embed/<id>.
	sendvidShape = pathShape(`^/embed/[A-Za-z0-9]{1,32}/?$`)
	// sendvidPage is the player or the bare watch page, /<id>, too common a
	// form for the link scanner.
	sendvidPage = pathShape(`^/(?:embed/)?[A-Za-z0-9]{1,32}(?:\.mp4)?/?$`)
)

// Recognises is the inventory's description.
func (s *Sendvid) Recognises() string {
	return "videos (`/<id>`, `/embed/<id>`) whose page, named Sendvid, carries the player's " +
		"`video_source`"
}

// Shape reports a player link.
func (s *Sendvid) Shape(u *url.URL) bool { return sendvidShape(u) }

// Recognise takes a page whose player carries the source Extract reads.
func (s *Sendvid) Recognise(_ context.Context, p *Page) bool {
	return sendvidPage(p.Final) && p.Named("Sendvid") && p.hasElement(func(n *html.Node) bool {
		return isElem(n, atom.Source) && attr(n, "id") == "video_source"
	})
}

// Extract resolves one video page.
func (s *Sendvid) Extract(ctx context.Context, u *url.URL, _ Options) (*Result, error) {
	id := sendvidID(u)
	if id == "" {
		return nil, fmt.Errorf("sendvid: %s names no video (expected /<id> or /embed/<id>)", u.Redacted())
	}
	page := util.Origin(u) + "/" + id
	return refetchedVideo(ctx, ".mp4", httpx.Referer(page), func(ctx context.Context) (string, string, error) {
		return s.media(ctx, page, id)
	})
}

// media reads the page's title and its current signed link.
func (s *Sendvid) media(ctx context.Context, page, id string) (title, link string, err error) {
	doc, err := fetchPage(ctx, s.client, page, nil)
	if httpx.HasStatus(err, http.StatusNotFound) {
		return "", "", fmt.Errorf("sendvid: video %s is not there — the site says it was not found or has been removed", id)
	}
	if err != nil {
		return "", "", fmt.Errorf("sendvid: fetch %s: %w", page, err)
	}
	root, err := parseHTML(doc)
	if err != nil {
		return "", "", fmt.Errorf("sendvid: %s: %w", page, err)
	}
	pageURL, _ := url.Parse(page)
	if link = resolveRef(pageURL, sendvidSource(root)); link == "" {
		return "", "", fmt.Errorf("sendvid: no video on %s (it may have been removed, or the page layout changed)", page)
	}
	title = util.FirstNonEmpty(metaContent(root, "og:title"), metaContent(root, "twitter:title"), id)
	return title, link, nil
}

// sendvidSource is the player's media address: its <source>, the one with
// the player's id first, or the <video>'s own src.
func sendvidSource(root *html.Node) string {
	if n := findFirst(root, func(n *html.Node) bool {
		return isElem(n, atom.Source) && attr(n, "id") == "video_source"
	}); n != nil {
		return attr(n, "src")
	}
	if n := findFirst(root, func(n *html.Node) bool {
		return isElem(n, atom.Source) && strings.HasPrefix(attr(n, "type"), "video/")
	}); n != nil {
		return attr(n, "src")
	}
	if n := findFirst(root, func(n *html.Node) bool { return isElem(n, atom.Video) && attr(n, "src") != "" }); n != nil {
		return attr(n, "src")
	}
	return ""
}

// sendvidID reads the video id out of /<id>, /<id>.mp4 (the og:video link)
// or /embed/<id>. The site's own pages under /help/ and /users/ have more
// segments than that.
func sendvidID(u *url.URL) string {
	segs := util.PathSegments(u)
	switch {
	case len(segs) == 2 && segs[0] == "embed":
		return sendvidValidID(segs[1])
	case len(segs) == 1:
		return sendvidValidID(strings.TrimSuffix(segs[0], ".mp4"))
	}
	return ""
}

// sendvidValidID accepts a video id: letters and digits, as the site's are.
func sendvidValidID(id string) string {
	if id == "" || len(id) > 32 {
		return ""
	}
	for _, r := range id {
		if !('a' <= r && r <= 'z' || 'A' <= r && r <= 'Z' || '0' <= r && r <= '9') {
			return ""
		}
	}
	return id
}
