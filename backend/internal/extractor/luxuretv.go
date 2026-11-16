// SPDX-License-Identifier: MIT

package extractor

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"github.com/JohanLindvall/HeapLeach/internal/util"
	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

var luxuretvPath = regexp.MustCompile(`^/videos/[^/]+-([0-9]+)\.html$`)

func luxuretvShape(u *url.URL) bool { return luxuretvPath.MatchString(u.Path) }

var luxuretvPlayer = playerSite{
	name:       "luxuretv",
	shape:      luxuretvShape,
	recognises: "video pages (`/videos/<slug>-<id>.html`) with a primary HTML5 player and a matching stream ID",
	recognise: func(p *Page) bool {
		return luxuretvShape(p.Final) && luxuretvMedia(p.HTML(), p.Final) != ""
	},
	signed: true,
	parse: func(d *playerPage) (*playerVideo, error) {
		link := luxuretvMedia(d.tree(), d.final)
		if link == "" {
			return nil, fmt.Errorf("no supported video player (the video may be removed)")
		}
		return &playerVideo{
			title: d.title(), filename: util.NameFromURL(link), referer: d.final.String(),
			candidates: []mediaCandidate{{URL: link}},
		}, nil
	},
}

// Only the primary player's source carries the usable signature. Structured
// metadata repeats its unsigned path, and a separate video advertises the site.
func luxuretvMedia(root *html.Node, base *url.URL) string {
	if root == nil {
		return ""
	}
	m := luxuretvPath.FindStringSubmatch(base.Path)
	if m == nil {
		return ""
	}
	container := findFirst(root, func(n *html.Node) bool { return isElem(n, atom.Div) && attr(n, "id") == "player" })
	if container == nil {
		return ""
	}
	video := findFirst(container, func(n *html.Node) bool { return isElem(n, atom.Video) && attr(n, "id") == "thisPlayer" })
	if video == nil {
		return ""
	}
	for n := video.FirstChild; n != nil; n = n.NextSibling {
		if !isElem(n, atom.Source) {
			continue
		}
		link := mediaPageLink(base, attr(n, "src"))
		u, err := url.Parse(link)
		if err == nil && link != "" && strings.HasPrefix(u.Path, "/cf-stream/"+m[1]+"/") && strings.HasSuffix(strings.ToLower(u.Path), ".mp4") {
			return link
		}
	}
	return ""
}
