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

var bebasbokepPath = regexp.MustCompile(`^/video/[A-Za-z0-9]+/?$`)

func bebasbokepShape(u *url.URL) bool { return bebasbokepPath.MatchString(u.Path) }

var bebasbokepPlayer = playerSite{
	name:       "bebasbokep",
	shape:      bebasbokepShape,
	recognises: "video pages (`/video/<id>`) with a packed JW Player and literal rawSources renditions; also embedded by WordPress posts",
	recognise: func(p *Page) bool {
		return bebasbokepShape(p.Final) && len(bebasbokepMedia(p.HTML(), p.Final)) > 0
	},
	parse: func(d *playerPage) (*playerVideo, error) {
		candidates := bebasbokepMedia(d.tree(), d.final)
		best, ok := bestCandidate(candidates)
		if !ok {
			return nil, fmt.Errorf("no supported player sources (the video may be removed)")
		}
		return &playerVideo{title: d.title(), filename: util.NameFromURL(best.URL),
			referer: d.final.String(), candidates: candidates}, nil
	},
}

func bebasbokepMedia(root *html.Node, base *url.URL) []mediaCandidate {
	if root == nil || findFirst(root, func(n *html.Node) bool {
		return isElem(n, atom.Div) && attr(n, "id") == "jw_player"
	}) == nil {
		return nil
	}
	for _, script := range scriptTexts(root) {
		if unpacked, ok := unpackJS(script); ok {
			script = unpacked
		}
		r := jsTokens{rest: script}
		for token := r.next(); token != ""; token = r.next() {
			if token != "jwplayer" {
				continue
			}
			setup := r
			if !bebasbokepSetup(&setup) {
				continue
			}
			return bebasbokepSources(&setup, base)
		}
	}
	return nil
}

// Token matching keeps quoted examples and comments out of recognition.
func bebasbokepSetup(r *jsTokens) bool {
	for _, want := range []string{"(", "jw_player", ")", ".", "setup", "(", "{", "playlist", ":", "[", "{", "sources", ":", "(", "function", "(", ")", "{"} {
		got := r.next()
		if value, ok := jsLiteral(got); ok {
			got = value
		}
		if got != want {
			return false
		}
	}
	switch r.next() {
	case "let", "var", "const":
		return r.next() == "rawSources" && r.next() == "="
	}
	return false
}

// Read only the literal rendition array inside the primary player's setup.
// Logo, advertising and thumbnail file properties elsewhere are not sources.
// Expressions and incomplete arrays are refused without executing JavaScript.
func bebasbokepSources(r *jsTokens, base *url.URL) []mediaCandidate {
	if r.next() != "[" {
		return nil
	}
	var out []mediaCandidate
	for token := r.next(); token != "]"; token = r.next() {
		if token != "{" {
			return nil
		}
		fields := make(map[string]string)
		for key := r.next(); key != "}"; key = r.next() {
			key, ok := jsLiteral(key)
			if !ok || r.next() != ":" {
				return nil
			}
			value, ok := jsLiteral(r.next())
			if _, exists := fields[key]; !ok || exists {
				return nil
			}
			fields[key] = value
			switch r.next() {
			case "}":
				goto entry
			case ",":
			default:
				return nil
			}
		}
	entry:
		link := mediaPageLink(base, fields["file"])
		u, err := url.Parse(link)
		if err == nil && link != "" && strings.HasSuffix(strings.ToLower(u.Path), ".mp4") &&
			(strings.EqualFold(fields["type"], "video/mp4") || fields["type"] == "") {
			out = append(out, mediaCandidate{URL: link, Quality: qualityOf(fields["label"])})
		}
		switch r.next() {
		case "]":
			if r.next() == ";" {
				return out
			}
			return nil
		case ",":
		default:
			return nil
		}
	}
	if r.next() == ";" {
		return out
	}
	return nil
}
