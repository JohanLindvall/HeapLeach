// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"

	"github.com/JohanLindvall/HeapLeach/internal/httpx"
	"github.com/JohanLindvall/HeapLeach/internal/tools"
	"github.com/JohanLindvall/HeapLeach/internal/util"
	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// Pointer resolves a page whose only job is to point at a video hosted
// elsewhere: a search preview naming its source, an index framing another
// tube's player. Nothing here is downloaded from the page itself; the work
// is to find where it points and step aside.
//
// What it steps aside to is the extractor that recognises the link, which
// is strictly better than handing it to yt-dlp: the extractor may be the
// only thing that works. A frame this was written for points at a tube
// whose media endpoint yt-dlp asks in a way it no longer answers, so the
// external downloader fails on precisely the link the registry resolves.
// Each link is followed strictly, so one nobody recognises — an advert, a
// player only yt-dlp knows — is passed over rather than saved as a page,
// and only when none is recognised is the first handed to yt-dlp. A site
// whose sources are anyone's videos (pointerSite.retry) also hands a
// source to yt-dlp when the extractor that recognised it fails on it.
type Pointer struct {
	about
	client   *httpx.Client
	registry *Registry
	site     pointerSite
}

// pointerSite is one site whose pages point elsewhere.
type pointerSite struct {
	about
	// recognise reports whether a probed page is the site's and points
	// somewhere.
	recognise func(p *Page) bool
	// links reads the off-site links the page points at, likeliest first.
	links func(root *html.Node, page *url.URL) []string
	// missing completes "<page> ..." when the page points nowhere.
	missing string
	// retry hands a source over to yt-dlp when the extractor that
	// recognised it failed on it, where the site's sources are anyone's
	// videos and yt-dlp may read one the native extractor cannot. Unset,
	// the native extractor's error stands: it is the only thing that works
	// on the site's sources.
	retry bool
}

// pointerSites are the sites recognised here.
var pointerSites = []pointerSite{
	{
		// A preview is search output: a viewer wrapped around somebody
		// else's video, with the source linked beside the player. It is
		// rebuilt whenever the search page is, and the source anchor is the
		// only part of it read here.
		name:  "yandex",
		shape: yandexShape,
		recognise: func(p *Page) bool {
			// The same on every country domain the search is served on.
			return yandexShape(p.Final) && p.Script(yandexBundle) && p.Has(yandexSourceClass)
		},
		recognises: "video search previews (`/video/preview/<id>`, `/video/search?filmId=<id>`, their touch " +
			"forms) built by the Yandex search front end, naming the source video in its viewer",
		links:   yandexSources,
		missing: "names no source video (the preview may have expired, or the page layout changed)",
		retry:   true,
	},
	{
		// A view page frames the tube the video really lives on. Its URL
		// names a source of its own in a "u=" parameter, deliberately not
		// used: it pointed at a second wrapper rather than at the video. A
		// view that redirects straight to the video is never this site's:
		// the page probed is the video's own, and whoever recognises that
		// takes it.
		name:  "alohatube",
		shape: alohaShape,
		recognise: func(p *Page) bool {
			if !alohaShape(p.Final) || !p.Has("<iframe", "allowfullscreen") || !p.Named("Aloha Tube") {
				return false
			}
			return p.hasElement(func(n *html.Node) bool {
				return isElem(n, atom.Iframe) && hasAttr(n, "allowfullscreen") &&
					alohaOffsite(resolveRef(p.Final, attr(n, "src"))) != ""
			})
		},
		recognises: "Aloha Tube view pages (`/ktm/view.cgi?gid=<id>`) that frame another site's player " +
			"(an off-site `<iframe allowfullscreen>`), resolved where the video lives",
		links:   alohaFrames,
		missing: "embeds no player (the video may have been removed, or the page layout changed)",
	},
}

// The forms and markers the pointer sites are recognised by.
var (
	// yandexShape is a preview, /video/preview/<id>, or a search or preview
	// naming its film in the query, each also under /video/touch/.
	yandexShape = func(u *url.URL) bool {
		return yandexPreview(u) || yandexFilm(u) && u.Query().Get("filmId") != ""
	}
	yandexPreview = pathShape(`^/video/(?:touch/)?preview/[0-9]+/?$`)
	yandexFilm    = pathShape(`^/video/(?:touch/)?(?:search|preview)/?$`)
	// alohaShape is a view page, /ktm/view.cgi?gid=<id>.
	alohaShape = func(u *url.URL) bool {
		return u.EscapedPath() == "/ktm/view.cgi" && u.Query().Get("gid") != ""
	}
)

const (
	// yandexSourceClass marks the anchor naming the site a video came from.
	yandexSourceClass = "VideoViewer-Source"
	// yandexBundle is the path the search front end's scripts are served
	// under.
	yandexBundle = "/fiji-static/"
)

// NewPointer builds the extractor for the named site, resolving what it
// finds through registry, or nil for a name not in the table. Each is
// registered where its form sits among the others', since a form shared
// with another host goes to whichever comes first.
func NewPointer(client *httpx.Client, registry *Registry, name string) *Pointer {
	for _, site := range pointerSites {
		if site.name == name {
			return &Pointer{about: site.about, client: client, registry: registry, site: site}
		}
	}
	return nil
}

// Recognise reports whether the probed page is the site's.
func (p *Pointer) Recognise(_ context.Context, page *Page) bool { return p.site.recognise(page) }

// Extract finds where the page points and resolves it there.
func (p *Pointer) Extract(ctx context.Context, u *url.URL, opts Options) (*Result, error) {
	root, page, err := fetchRoot(ctx, p.client, u, httpx.Referer(util.Origin(u)+"/"))
	if err != nil {
		return nil, fmt.Errorf("%s: fetch %s: %w", p.name, u.Redacted(), err)
	}
	links := p.site.links(root, page)
	if len(links) == 0 {
		return nil, fmt.Errorf("%s: %s %s", p.name, u.Redacted(), p.site.missing)
	}

	// A link some extractor here recognises first, wherever it sits in the
	// page: the ordering is a guess about which link is the video, and a
	// link we can read is better evidence than a guess. A challenged page
	// nobody claimed is passed over the same way, since yt-dlp may still
	// get through where a plain fetch did not.
	//
	// Where the site retries, a source whose extractor failed on it is
	// handed to yt-dlp, as one video; the extractor's error stands when
	// yt-dlp is missing or cannot read it either.
	for _, link := range links {
		res, ex, err := p.registry.follow(ctx, link, opts)
		if errors.Is(err, errUnrecognised) || err != nil && ex == p.registry.fallback {
			continue
		}
		if err != nil && p.site.retry && ctx.Err() == nil {
			if ytdlp, ok := tools.Find(ytdlpBinary); ok {
				if handed, herr := p.handOver(ctx, ytdlp, link); herr == nil {
					return handed, nil
				}
			}
		}
		return res, err
	}

	ytdlp, ok := tools.Find(ytdlpBinary)
	if !ok {
		return nil, fmt.Errorf("%s: %s points at %s, which no extractor here claims — "+
			"yt-dlp is what fetches from those, and %s",
			p.name, u.Redacted(), pointerHost(links[0]), tools.NotInstalled(ytdlpBinary))
	}
	return p.handOver(ctx, ytdlp, links[0])
}

// handOver queues the one video at link for yt-dlp, named by what yt-dlp
// calls it. A link naming a playlist beside its video is the video
// (ytdlpTitle asks with --no-playlist), as the download reads it.
func (p *Pointer) handOver(ctx context.Context, ytdlp, link string) (*Result, error) {
	title, err := ytdlpTitle(ctx, ytdlp, link)
	if err != nil {
		return nil, fmt.Errorf("%s: the page points at %s, which could not be read: %w",
			p.name, link, err)
	}
	return &Result{Title: title, Files: []File{{
		Name:     title + ".mp4",
		Size:     -1,
		External: link,
	}}}, nil
}

// offsiteLink keeps a link only if it is a web link leading away from the
// site, as judged by home, since a page links its own furniture the same
// way it links the video.
func offsiteLink(link string, home func(host string) bool) string {
	u, err := url.Parse(link)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return ""
	}
	if home(strings.ToLower(u.Hostname())) {
		return ""
	}
	return link
}

// pointerHost names a link's host for an error message.
func pointerHost(link string) string {
	if u, err := url.Parse(link); err == nil && u.Host != "" {
		return strings.ToLower(u.Hostname())
	}
	return link
}

// yandexSources reads the links to the site a preview's video really lives
// on: the viewer's source anchors that lead away from the search.
func yandexSources(root *html.Node, page *url.URL) []string {
	var out []string
	for _, a := range findAll(root, func(n *html.Node) bool {
		return isElem(n, atom.A) && strings.Contains(attr(n, "class"), yandexSourceClass)
	}) {
		if link := yandexOffsite(resolveRef(page, attr(a, "href"))); link != "" && !slices.Contains(out, link) {
			out = append(out, link)
		}
	}
	return out
}

// yandexOffsite keeps a link only if it leads away from the search and its
// static hosts.
func yandexOffsite(link string) string {
	return offsiteLink(link, func(host string) bool {
		return strings.HasPrefix(host, "yandex.") || strings.Contains(host, ".yandex.") ||
			strings.Contains(host, "yastatic.")
	})
}

// alohaFrames lists the page's off-site frames, likeliest player first.
//
// A view page carries advertising frames alongside the player, and they are
// off-site too, so being a frame is not enough to identify the video. The
// player is the one offering fullscreen — an ad has no use for it — which
// separates them on the page this was written against without depending on
// an id or a class name that a redesign would take away.
func alohaFrames(root *html.Node, page *url.URL) []string {
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

// alohaOffsite keeps a frame only if it leads away from the index.
func alohaOffsite(link string) string {
	return offsiteLink(link, func(host string) bool { return util.HostMatches(host, "alohatube.com") })
}
