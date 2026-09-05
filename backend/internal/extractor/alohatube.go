package extractor

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"github.com/JohanLindvall/HeapLeach/internal/httpx"
	"github.com/JohanLindvall/HeapLeach/internal/tools"
	"github.com/JohanLindvall/HeapLeach/internal/util"
	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// AlohaTube hands a video back to whoever actually hosts it.
//
// Aloha Tube is an index rather than a host, the way yandex's previews are:
// a view page is its own furniture wrapped around an <iframe> pointing at
// the tube the video really lives on. So there is nothing here to download,
// and the work is to find that frame and step aside.
//
// Where it differs from yandex is what it steps aside to. A frame on a host
// this build already knows is resolved by that host's own extractor, which
// is strictly better than handing it to yt-dlp: the extractor may be the
// only thing that works. The frame this was written for points at drtuber,
// whose media endpoint yt-dlp asks in a way it no longer answers — so the
// external downloader fails on precisely the link the registry resolves.
// Only a frame on a host nothing here claims is passed out to yt-dlp.
//
// The view URL names a source of its own in a "u=" parameter, and it is
// deliberately not used: it pointed at a second wrapper rather than at the
// video, so the frame the page actually plays is the more direct answer.
type AlohaTube struct {
	hostSet
	client   *httpx.Client
	registry *Registry
}

const alohaRoot = "https://www.alohatube.com"

// NewAlohaTube builds the alohatube extractor. It resolves what it finds
// through the registry it is part of, so it is handed that registry rather
// than building one.
func NewAlohaTube(client *httpx.Client, registry *Registry) *AlohaTube {
	return &AlohaTube{hostSet: hostSet{"alohatube.com"}, client: client, registry: registry}
}

func (a *AlohaTube) Name() string { return "alohatube" }

// Extract finds the embedded player and resolves it where it lives.
func (a *AlohaTube) Extract(ctx context.Context, u *url.URL, opts Options) (*Result, error) {
	doc, err := a.client.GetString(ctx, u.String(), httpx.Referer(alohaRoot+"/"))
	if err != nil {
		return nil, fmt.Errorf("alohatube: fetch %s: %w", u.Redacted(), err)
	}
	frames := alohaFrames(doc, u)
	if len(frames) == 0 {
		return nil, fmt.Errorf("alohatube: %s embeds no player "+
			"(the video may have been removed, or the page layout changed)", u.Redacted())
	}

	// A supported host first, wherever it sits in the page: the ordering
	// below is a guess about which frame is the player, and a frame we can
	// name is better evidence than a guess.
	for _, frame := range frames {
		parsed, err := url.Parse(frame)
		if err != nil {
			continue
		}
		if ex, ok := a.registry.Known(parsed); ok {
			return ex.Extract(ctx, parsed, opts)
		}
	}

	ytdlp, ok := tools.Find(ytdlpBinary)
	if !ok {
		return nil, fmt.Errorf("alohatube: %s embeds a player from %s, which no extractor "+
			"here claims — yt-dlp is what fetches from those, and %s",
			u.Redacted(), alohaHostOf(frames[0]), tools.NotInstalled(ytdlpBinary))
	}
	title, err := ytdlpTitle(ctx, ytdlp, frames[0])
	if err != nil {
		return nil, fmt.Errorf("alohatube: the page embeds %s, which could not be read: %w",
			frames[0], err)
	}
	return &Result{Title: title, Files: []File{{
		Name:     title + ".mp4",
		Size:     -1,
		External: frames[0],
	}}}, nil
}

// alohaFrames lists the page's off-site frames, likeliest player first.
//
// A view page carries advertising frames alongside the player, and they are
// off-site too, so being a frame is not enough to identify the video. The
// player is the one offering fullscreen — an ad has no use for it — which
// separates them on the page this was written against without depending on
// an id or a class name that a redesign would take away.
func alohaFrames(doc string, page *url.URL) []string {
	root, err := parseHTML(doc)
	if err != nil {
		return nil
	}
	var players, others []string
	for _, n := range findAll(root, func(n *html.Node) bool { return isElem(n, atom.Iframe) }) {
		link := alohaOffsite(resolveRef(page, attr(n, "src")))
		if link == "" {
			continue
		}
		if hasAttr(n, "allowfullscreen") {
			players = append(players, link)
		} else {
			others = append(others, link)
		}
	}
	return append(players, others...)
}

// alohaOffsite keeps a frame only if it leads away from the index, since the
// page frames some of its own furniture the same way.
func alohaOffsite(link string) string {
	u, err := url.Parse(link)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return ""
	}
	if util.HostMatches(u.Host, "alohatube.com") {
		return ""
	}
	return link
}

// alohaHostOf names a frame's host for an error message.
func alohaHostOf(link string) string {
	if u, err := url.Parse(link); err == nil && u.Host != "" {
		return strings.ToLower(u.Hostname())
	}
	return link
}
