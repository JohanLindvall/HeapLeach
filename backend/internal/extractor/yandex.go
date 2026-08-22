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

// Yandex resolves the video preview pages of its search.
//
// Yandex hosts none of this. A preview page is a viewer wrapped around
// somebody else's video, and the page says whose: it links the source
// alongside the player. So the work here is to find that link and stand
// aside — the video itself belongs to whichever host it came from, and is
// handed to the external downloader, which knows far more of them than this
// program ever will.
//
// Worth knowing when this breaks: the preview is search output, so it is
// rebuilt whenever the search page is, and the anchor below is the only part
// of it this depends on.
type Yandex struct {
	client *httpx.Client
}

// yandexSourceClass marks the anchor naming the site a video came from.
const yandexSourceClass = "VideoViewer-Source"

// NewYandex builds the yandex video extractor.
func NewYandex(client *httpx.Client) *Yandex { return &Yandex{client: client} }

func (y *Yandex) Name() string { return "yandex" }

// Match covers the many country domains the same search is served on.
func (y *Yandex) Match(u *url.URL) bool {
	host := strings.ToLower(u.Hostname())
	if !strings.HasPrefix(host, "yandex.") && !strings.Contains(host, ".yandex.") {
		return false
	}
	return strings.Contains(u.Path, "/video/")
}

// Extract finds the video's real home and hands it over.
func (y *Yandex) Extract(ctx context.Context, u *url.URL, _ Options) (*Result, error) {
	ytdlp, ok := tools.Find(ytdlpBinary)
	if !ok {
		return nil, fmt.Errorf("yandex: yt-dlp is not installed — a preview only points at "+
			"the site holding the video, and yt-dlp is what fetches from those; run "+
			"`make dependencies`, or put it on PATH (%s)", u.Redacted())
	}

	doc, err := y.client.GetString(ctx, u.String(), httpx.Referer(util.Origin(u)+"/"))
	if err != nil {
		return nil, fmt.Errorf("yandex: fetch %s: %w", u.Redacted(), err)
	}
	source, err := yandexSource(doc, u)
	if err != nil {
		return nil, err
	}

	title, err := ytdlpTitle(ctx, ytdlp, source)
	if err != nil {
		return nil, fmt.Errorf("yandex: the preview points at %s, which could not be read: %w",
			source, err)
	}
	return &Result{Title: title, Files: []File{{
		Name:     title + ".mp4",
		Size:     -1,
		External: source,
	}}}, nil
}

// yandexSource reads the link to the site a preview's video really lives on.
func yandexSource(doc string, u *url.URL) (string, error) {
	root, err := parseHTML(doc)
	if err != nil {
		return "", fmt.Errorf("yandex: parse %s: %w", u.Redacted(), err)
	}

	for _, a := range findAll(root, func(n *html.Node) bool {
		return isElem(n, atom.A) && strings.Contains(attr(n, "class"), yandexSourceClass)
	}) {
		if link := yandexOffsite(resolveRef(u, attr(a, "href"))); link != "" {
			return link, nil
		}
	}
	return "", fmt.Errorf("yandex: %s names no source video "+
		"(the preview may have expired, or the page layout changed)", u.Redacted())
}

// yandexOffsite keeps a link only if it leads away from yandex, since the
// viewer's own furniture is linked the same way.
func yandexOffsite(link string) string {
	u, err := url.Parse(link)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return ""
	}
	host := strings.ToLower(u.Hostname())
	if strings.HasPrefix(host, "yandex.") || strings.Contains(host, ".yandex.") ||
		strings.Contains(host, "yastatic.") {
		return ""
	}
	return link
}
