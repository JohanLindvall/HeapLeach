// SPDX-License-Identifier: MIT

package extractor

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"path"
	"strings"

	"github.com/JohanLindvall/HeapLeach/internal/httpx"
	"github.com/JohanLindvall/HeapLeach/internal/util"
	"golang.org/x/net/html"
)

type darkGramAlbum struct {
	Title string `json:"title"`
	Items []struct {
		ID       string `json:"id"`
		Kind     string `json:"kind"`
		HLSURL   string `json:"hls_url"`
		PhotoURL string `json:"photo_url"`
	} `json:"items"`
}

// darkGramResult reads the album the DarkGram player mounts from data-album.
// HTML parsing unescapes the attribute; JSON decoding handles escaped slashes.
// Only the first player is read, as with mediaPageElements: another player can
// be a recommendation, while the items within one album belong together.
// Recognising a broken player is an error, not a reason to save the HTML.
func darkGramResult(client *httpx.Client, page *url.URL, root *html.Node, opts Options) (*Result, error) {
	player := findFirst(root, func(n *html.Node) bool {
		return n.Type == html.ElementNode && hasClass(n, "darkgram-album")
	})
	if player == nil {
		return nil, nil
	}
	var album darkGramAlbum
	if err := json.Unmarshal([]byte(attr(player, "data-album")), &album); err != nil {
		return nil, fmt.Errorf("darkgram: invalid album data: %w", err)
	}
	title := cmp.Or(mediaPageTitle(root), strings.TrimSpace(album.Title), util.NameFromURL(page.String()), page.Hostname())
	res := &Result{Title: title}
	headers := httpx.Referer(page.String())
	seen := make(map[string]bool)
	unavailable := 0
	for i, item := range album.Items {
		var ref string
		switch item.Kind {
		case "video":
			ref = item.HLSURL
		case "photo":
			ref = item.PhotoURL
		}
		// A thumbnail is never a substitute for a missing original. Keep
		// only fetchable URLs, resolving relatives after any page redirect.
		u := webLink(page, ref)
		if u == nil {
			unavailable++
			continue
		}
		link := u.String()
		key := item.Kind + "/" + cmp.Or(item.ID, link)
		if seen[key] {
			continue
		}
		seen[key] = true
		if len(res.Files) == opts.maxFiles() {
			res.Note = fileLimitNote
			break
		}
		// Use the original position so a missing item or a file cap never
		// renames the files that follow it on a later run.
		stem := fmt.Sprintf("%s - %03d", title, i+1)
		file := File{URL: link, Size: -1, Headers: headers}
		if item.Kind == "photo" {
			ext := strings.ToLower(path.Ext(u.Path))
			switch ext {
			case ".jpg", ".jpeg", ".png", ".webp", ".gif", ".avif":
			default:
				ext = ".jpg" // The player's /photo endpoint has no extension.
			}
			file.Name = stem + ext
		} else {
			file.Name = stem + ".ts"
			// Keep the player's playlist endpoint. Read its renditions,
			// segments and key at each attempt, so nothing signed by the
			// storage behind it expires while the album waits in the queue.
			file.Resolve = func(ctx context.Context) (*Target, error) {
				media, err := resolveMediaPlaylist(ctx, client, link, headers)
				if err != nil {
					return nil, fmt.Errorf("darkgram: video playlist: %w", err)
				}
				if live, why := hlsLiveEdge(media.Doc); live {
					return nil, fmt.Errorf("darkgram: video playlist is still being written: %s", why)
				}
				return media.target(link, stem+media.extension(), headers), nil
			}
		}
		res.Files = append(res.Files, file)
	}
	if len(res.Files) == 0 {
		return nil, fmt.Errorf("darkgram: album has no downloadable media")
	}
	res.Note = joinNotes(res.Note, countNote(unavailable, "album items unavailable"))
	return res, nil
}
