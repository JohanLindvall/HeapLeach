// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"github.com/JohanLindvall/HeapLeach/internal/httpx"
	"github.com/JohanLindvall/HeapLeach/internal/util"
	"golang.org/x/net/html/atom"
)

// LuluStream resolves lulustream's video pages, on each of the domains it
// answers on.
//
// The page carries a JW Player whose setup is packed (p,a,c,k,e,d), and what
// it hands the player is an HLS master playlist signed for eight hours and
// tied to the address that asked. Behind it is one rendition, video and audio
// together, in transport-stream segments encrypted with AES-128 — so the
// playlist is followed down to its segments and key, and the downloader
// decrypts each segment as it lands. Nothing is handed to an external tool.
//
// The page is read again when a file's turn comes, segments and key with
// it: a queue longer than the signature would otherwise start failing partway
// down, and a key fetched at extraction time could be stale by then too.
type LuluStream struct {
	client *httpx.Client
}

// NewLuluStream builds the lulustream extractor.
func NewLuluStream(client *httpx.Client) *LuluStream {
	return &LuluStream{client: client}
}

func (l *LuluStream) Name() string { return "lulustream" }

var (
	// luluShape is a player or a download page.
	luluShape = pathShape(`^/[ed]/[0-9a-z]{12}/?$`)
	// luluPage is either, or the bare watch page, /<id>, too common a form
	// for the link scanner.
	luluPage = pathShape(`^/(?:[ed]/)?[0-9a-z]{12}/?$`)
)

// Recognises is the inventory's description.
func (l *LuluStream) Recognises() string {
	return "videos (`/<id>`, `/e/<id>`, `/d/<id>`) whose page names LuluStream and carries its " +
		"player's HLS source"
}

// Shape reports a player or a download page.
func (l *LuluStream) Shape(u *url.URL) bool { return luluShape(u) }

// Recognise takes a page carrying the player source Extract reads, packed
// or not, under the site's name.
func (l *LuluStream) Recognise(_ context.Context, p *Page) bool {
	if !luluPage(p.Final) || !p.HasAny("LuluStream", "LuluVdo", "Luluvdo") {
		return false
	}
	if luluSource.MatchString(p.Body) {
		return true
	}
	setup, ok := unpackJS(p.Body)
	return ok && luluSource.MatchString(setup)
}

// luluSource finds the playlist the player is given in its unpacked setup.
var luluSource = regexp.MustCompile(`file\s*:\s*"(https?://[^"]+\.m3u8[^"]*)"`)

// Extract resolves one video page.
func (l *LuluStream) Extract(ctx context.Context, u *url.URL, _ Options) (*Result, error) {
	id := luluID(u)
	if id == "" {
		return nil, fmt.Errorf("lulustream: %s names no video (expected /<id>)", u.Redacted())
	}
	// The plain page, whichever shape was pasted: it is served on every
	// domain, where the embed and download pages redirect on some of them.
	page := util.Origin(u) + "/" + id
	headers := httpx.Referer(util.Origin(u) + "/")

	first, err := l.read(ctx, page, headers)
	if err != nil {
		return nil, err
	}
	return &Result{Title: first.title, Files: []File{{
		Name:       first.name(),
		URL:        first.playlist,
		Size:       -1,
		Headers:    headers,
		Segments:   first.media.Segments,
		SegmentKey: first.media.Key,
		Audio:      first.media.Audio,
		Subtitles:  first.media.Subtitles,
		Resolve: func(ctx context.Context) (*Target, error) {
			fresh, err := l.read(ctx, page, headers)
			if err != nil {
				return nil, err
			}
			return &Target{
				URL:        fresh.playlist,
				Name:       fresh.name(),
				Size:       -1,
				Headers:    headers,
				Segments:   fresh.media.Segments,
				SegmentKey: fresh.media.Key,
				Audio:      fresh.media.Audio,
				Subtitles:  fresh.media.Subtitles,
			}, nil
		},
	}}}, nil
}

// luluVideo is what one reading of a page found.
type luluVideo struct {
	title    string
	playlist string
	media    *hlsMedia
}

// name is the file's name: the title and the container the segments are.
func (v *luluVideo) name() string {
	return v.title + v.media.extension()
}

// read fetches the page, unpacks the player's setup and follows its
// playlist down to the segments.
func (l *LuluStream) read(ctx context.Context, page string, headers httpx.Header) (*luluVideo, error) {
	doc, err := fetchPage(ctx, l.client, page, headers)
	if err != nil {
		return nil, fmt.Errorf("lulustream: fetch %s: %w", page, err)
	}
	setup, ok := unpackJS(doc)
	if !ok {
		setup = doc // an unpacked page is read as it is
	}
	found := luluSource.FindStringSubmatch(setup)
	if found == nil {
		return nil, fmt.Errorf("lulustream: no player source on %s (the video may have been removed)", page)
	}

	media, err := resolveMediaPlaylist(ctx, l.client, found[1], headers)
	if err != nil {
		return nil, fmt.Errorf("lulustream: %s: %w", page, err)
	}
	return &luluVideo{title: luluTitle(doc, page), playlist: found[1], media: media}, nil
}

// luluTitle is the video's own name, from the page's heading — the document
// title carries the site's name after it.
func luluTitle(doc, page string) string {
	var heading string
	if root, err := parseHTML(doc); err == nil {
		heading = strings.TrimSpace(firstText(root, atom.H1))
	}
	return util.FirstNonEmpty(heading, pageTitle(doc), util.NameFromURL(page))
}

// luluID reads the video id out of any of the URL shapes the site uses:
// /<id>, and the embed and download pages /e/<id> and /d/<id>.
func luluID(u *url.URL) string {
	segs := util.PathSegments(u)
	switch {
	case len(segs) >= 2 && (segs[0] == "e" || segs[0] == "d"):
		return segs[1]
	case len(segs) == 1:
		return segs[0]
	}
	return ""
}
