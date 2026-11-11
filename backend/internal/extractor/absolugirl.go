// SPDX-License-Identifier: MIT

package extractor

import (
	"crypto/md5"
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"github.com/JohanLindvall/HeapLeach/internal/util"
	"golang.org/x/net/html"
)

var absolugirlPath = regexp.MustCompile(`^/video-[A-Za-z0-9]+\.html$`)

func absolugirlShape(u *url.URL) bool { return absolugirlPath.MatchString(u.Path) }

var absolugirlPlayer = playerSite{
	name:       "absolugirl",
	shape:      absolugirlShape,
	recognises: "video pages (`/video-<id>.html`) with a Flowplayer configuration and MD5 directory construction",
	recognise: func(p *Page) bool {
		return absolugirlShape(p.Final) && absolugirlMedia(p.HTML(), p.Final) != ""
	},
	referer: pageOrigin,
	parse: func(d *playerPage) (*playerVideo, error) {
		link := absolugirlMedia(d.tree(), d.base)
		if link == "" {
			return nil, fmt.Errorf("no supported player configuration (the video may be removed)")
		}
		return &playerVideo{title: d.title(), filename: util.NameFromURL(link), candidates: []mediaCandidate{{URL: link}}}, nil
	},
}

// This legacy player hashes an ASCII directory name, then concatenates four
// page-declared components. Read only that construction, never arbitrary JS
// or a URL belonging to the preview/plugin configuration beside the clip.
func absolugirlMedia(root *html.Node, base *url.URL) string {
	for _, script := range scriptTexts(root) {
		r := jsTokens{rest: script}
		var tokens []string
		for token := r.next(); token != ""; token = r.next() {
			tokens = append(tokens, token)
		}
		joined := strings.Join(tokens, " ")
		if !strings.Contains(joined, "flowplayer (") ||
			!(strings.Contains(joined, "clip : { url : servervideo + path + repp + filee ,") ||
				strings.Contains(joined, "clip : { sources : [ { type : \"video/mp4\" , src : servervideo + path + repp + filee }")) {
			continue
		}
		values := map[string]string{}
		invalid := false
		for i := 0; i+3 < len(tokens); i++ {
			key := tokens[i]
			if key != "servervideo" && key != "path" && key != "repp" && key != "filee" {
				continue
			}
			if tokens[i+1] != "=" {
				continue
			}
			if _, exists := values[key]; exists {
				invalid = true
				break
			}
			value, ok := jsLiteral(tokens[i+2])
			if key == "repp" {
				if i+6 >= len(tokens) || tokens[i+2] != "codage" || tokens[i+3] != "(" || tokens[i+5] != ")" || tokens[i+6] != ";" {
					invalid = true
					break
				}
				value, ok = jsLiteral(tokens[i+4])
				for _, c := range value {
					if c < 32 || c > 126 {
						ok = false
					}
				}
				value = fmt.Sprintf("%x", md5.Sum([]byte(value)))
			} else if tokens[i+3] != ";" {
				ok = false
			}
			if !ok {
				invalid = true
				break
			}
			values[key] = value
		}
		if invalid || len(values) != 4 {
			continue
		}
		link := mediaPageLink(base, values["servervideo"]+values["path"]+values["repp"]+values["filee"])
		if u, err := url.Parse(link); err == nil && link != "" && strings.HasSuffix(strings.ToLower(u.Path), ".mp4") {
			return link
		}
	}
	return ""
}
