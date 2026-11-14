// SPDX-License-Identifier: MIT

package extractor

import (
	"cmp"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
	"github.com/JohanLindvall/HeapLeach/internal/util"
	"golang.org/x/net/html"
)

var txxxShape = pathShape(`^/embed/[0-9]+/?$`)

var txxxPlayer = playerSite{
	name:       "txxx",
	shape:      txxxShape,
	recognises: "embedded video players (`/embed/<id>/`) with the TXXX constants and embed script",
	recognise: func(p *Page) bool {
		return txxxShape(p.Final) && p.Script("/static/js/embed.js") &&
			txxxPageID(p.HTML()) == pathID(p.Final, false, "embed")
	},
	read:   txxxRead,
	signed: true,
}

// The embed's constants identify the video, including on a redirected
// domain. Read a literal assignment, not an example or comment in the page.
func txxxPageID(root *html.Node) string {
	for _, script := range scriptTexts(root) {
		r := jsTokens{rest: script}
		for token := r.next(); token != ""; token = r.next() {
			if token != "window" || r.next() != "." || r.next() != "constants" || r.next() != "=" {
				continue
			}
			var state struct {
				Project string `json:"project_name"`
				Query   struct {
					VideoID string `json:"video_id"`
				} `json:"query"`
			}
			if json.NewDecoder(strings.NewReader(r.rest)).Decode(&state) == nil && strings.HasPrefix(state.Project, "txxx.") {
				return state.Query.VideoID
			}
		}
	}
	return ""
}

func txxxRead(ctx context.Context, client *httpx.Client, page string) (*playerVideo, error) {
	u, err := url.Parse(page)
	if err != nil {
		return nil, err
	}
	doc, err := readDocument(ctx, client, u, nil)
	if err != nil {
		return nil, videoPageError{err}
	}
	id := txxxPageID(doc.tree())
	n, err := strconv.ParseUint(id, 10, 64)
	if err != nil || n == 0 || !txxxShape(doc.final) || id != pathID(doc.final, false, "embed") {
		return nil, fmt.Errorf("no video in the embed configuration (the video may be removed)")
	}
	origin := util.Origin(doc.final)
	headers := httpx.Referer(doc.final.String())
	// The metadata API shards IDs by million and thousand. These are
	// protocol path components, not limits on the number of videos read.
	metadata := fmt.Sprintf("%s/api/json/video/%d/%d/%d/%s.json", origin,
		config.TxxxMetadataLifetime, n/1000000*1000000, n/1000*1000, id)
	var info struct {
		Video struct {
			ID      string `json:"video_id"`
			Title   string `json:"title"`
			Private string `json:"is_private"`
		} `json:"video"`
	}
	if err := client.GetJSON(ctx, metadata, headers, &info); err != nil {
		return nil, fmt.Errorf("video metadata: %w", err)
	}
	if info.Video.ID != id {
		return nil, fmt.Errorf("video metadata is missing or names another video")
	}
	if info.Video.Private == "1" {
		return nil, fmt.Errorf("this video is private")
	}
	endpoint := fmt.Sprintf("%s/api/videofile.php?video_id=%s&lifetime=%d", origin, id, config.TxxxFileLifetime)
	var raw json.RawMessage
	if err := client.GetJSON(ctx, endpoint, headers, &raw); err != nil {
		return nil, fmt.Errorf("video sources: %w", err)
	}
	entries, err := txxxSources(raw)
	if err != nil {
		return nil, err
	}
	var candidates []mediaCandidate
	for _, entry := range entries {
		decoded, err := txxxDecode(entry.URL)
		if err != nil {
			continue
		}
		link := mediaPageLink(doc.final, decoded)
		media, err := url.Parse(link)
		if err != nil || link == "" || !strings.HasSuffix(strings.ToLower(strings.TrimRight(media.Path, "/")), ".mp4") {
			continue
		}
		candidates = append(candidates, mediaCandidate{URL: link, Quality: qualityOf(entry.Format)})
	}
	best, ok := bestCandidate(candidates)
	if !ok {
		return nil, fmt.Errorf("no playable video in the source response")
	}
	return &playerVideo{title: cmp.Or(info.Video.Title, "txxx-"+id), candidates: candidates,
		filename: util.NameFromURL(best.URL), referer: doc.final.String()}, nil
}

type txxxSource struct {
	URL    string `json:"video_url"`
	Format string `json:"format"`
}

// The player accepts a list, a numerically keyed object, or an encoded JSON
// string. Preserve list/key order: the player uses its first source by default.
func txxxSources(raw json.RawMessage) ([]txxxSource, error) {
	var encoded string
	if json.Unmarshal(raw, &encoded) == nil {
		decoded, err := txxxDecode(encoded)
		if err != nil {
			return nil, fmt.Errorf("invalid encoded source response: %w", err)
		}
		raw = json.RawMessage(decoded)
	}
	var entries []txxxSource
	if json.Unmarshal(raw, &entries) == nil {
		return entries, nil
	}
	var keyed map[string]txxxSource
	if json.Unmarshal(raw, &keyed) == nil && len(keyed) > 0 {
		for i := 0; i < len(keyed); i++ {
			entry, ok := keyed[strconv.Itoa(i)]
			if !ok {
				return nil, fmt.Errorf("invalid source response keys")
			}
			entries = append(entries, entry)
		}
		return entries, nil
	}
	return nil, fmt.Errorf("invalid or unavailable video sources")
}

// The player's base164 alphabet substitutes Cyrillic lookalikes and three
// punctuation marks, then applies JavaScript unescape (which preserves '+').
func txxxDecode(encoded string) (string, error) {
	encoded = strings.NewReplacer("А", "A", "В", "B", "С", "C", "Е", "E", "М", "M",
		".", "+", ",", "/", "~", "=").Replace(encoded)
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", err
	}
	return url.PathUnescape(string(decoded))
}
