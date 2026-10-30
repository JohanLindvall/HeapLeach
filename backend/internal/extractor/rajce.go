// SPDX-License-Identifier: MIT

package extractor

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"

	"github.com/JohanLindvall/HeapLeach/internal/httpx"
	"github.com/JohanLindvall/HeapLeach/internal/util"
	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// Rajce reads an album's embedded settings, which contain its entire media
// list. Images and videos have separate URLs; a video's image is its poster.
type Rajce struct {
	client *httpx.Client
}

func NewRajce(client *httpx.Client) *Rajce {
	return &Rajce{client: client}
}

func (r *Rajce) Name() string { return "rajce" }

var rajceSettings = regexp.MustCompile(`\b(?:var|let|const)\s+settings\s*=\s*`)

// rajceShape is an album: /<user>/album/<slug>, optionally with one of its
// items after it.
var rajceShape = pathShape(`^/[^/]+/album/[^/]+(?:/\d+)?/?$`)

// Recognises is the inventory's description.
func (r *Rajce) Recognises() string {
	return "albums (`/<user>/album/<slug>`) whose page assigns the album's settings, media list included, in a script"
}

// Shape reports an album link.
func (r *Rajce) Shape(u *url.URL) bool { return rajceShape(u) }

// Recognise takes an album page whose script assigns the settings object
// rajceAlbumData decodes, with its legacy_media list.
func (r *Rajce) Recognise(_ context.Context, p *Page) bool {
	return rajceShape(p.Final) && rajceSettings.MatchString(p.Body) && p.Has(`"legacy_media"`)
}

type rajceAlbum struct {
	Title string        `json:"album_name"`
	Count int           `json:"media_count"`
	Media []rajceMedium `json:"legacy_media"`
}

type rajceMedium struct {
	ID       flexValue `json:"id"`
	Type     string    `json:"type"`
	IsVideo  bool      `json:"is_video"`
	Filename string    `json:"filename"`
	ImageURL string    `json:"image_url"`
	VideoURL string    `json:"video_url"`
	Version  int       `json:"version"`
}

func (r *Rajce) Extract(ctx context.Context, u *url.URL, opts Options) (*Result, error) {
	doc, page, err := fetchDocument(ctx, r.client, u, nil)
	if err != nil {
		return nil, fmt.Errorf("rajce: fetch album: %w", err)
	}
	album, err := rajceAlbumData(doc)
	if err != nil {
		return nil, err
	}
	page.Fragment, page.RawFragment = "", ""
	res := &Result{Title: cmp.Or(strings.TrimSpace(album.Title), util.NameFromURL(page.String()))}
	headers := httpx.Referer(page.String())
	seen := make(map[string]bool)
	unavailable := 0
	var notes []string
	for _, medium := range album.Media {
		file, ok := medium.file(page)
		if !ok {
			unavailable++
			continue
		}
		id := medium.ID.String()
		if seen["url:"+file.URL] || (id != "" && seen["id:"+id]) {
			continue
		}
		if len(res.Files) == opts.maxFiles() {
			notes = append(notes, fileLimitNote)
			break
		}
		seen["url:"+file.URL] = true
		if id != "" {
			seen["id:"+id] = true
		}
		file.Headers = headers
		res.Files = append(res.Files, file)
	}
	if len(res.Files) == 0 {
		return nil, fmt.Errorf("rajce: album has no downloadable photos or videos (it may be empty or restricted)")
	}
	notes = append(notes, cappedNote(len(res.Files), album.Count, "files"))
	res.Note = joinNotes(append(notes, countNote(unavailable, "album items unavailable"))...)
	return res, nil
}

func rajceAlbumData(doc string) (*rajceAlbum, error) {
	root, err := parseHTML(doc)
	if err != nil {
		return nil, err
	}
	for _, node := range findAll(root, func(n *html.Node) bool { return isElem(n, atom.Script) }) {
		if node.FirstChild == nil {
			continue
		}
		// Script text must retain its whitespace: collapsing it would change
		// filenames and titles inside the JSON strings as well.
		script := node.FirstChild.Data
		marker := rajceSettings.FindStringIndex(script)
		if marker == nil {
			continue
		}
		var album rajceAlbum
		// Decode one JSON value so the following JavaScript is left alone.
		if err := json.NewDecoder(strings.NewReader(script[marker[1]:])).Decode(&album); err != nil {
			return nil, fmt.Errorf("rajce: read album data: %w", err)
		}
		if album.Media != nil {
			return &album, nil
		}
	}
	return nil, fmt.Errorf("rajce: no album media on this page (expected /<user>/album/<slug>; the album may be private, password protected or removed)")
}

func (m rajceMedium) file(page *url.URL) (File, bool) {
	video := m.IsVideo || m.Type == "video"
	ref := m.ImageURL
	if video {
		ref = m.VideoURL
	} else if m.Type != "photo" {
		return File{}, false
	}
	u := webLink(page, ref)
	if u == nil {
		return File{}, false
	}
	name := cmp.Or(m.Filename, util.NameFromURL(u.String()))
	if name == "" {
		return File{}, false
	}
	if video {
		// filename can be a JPEG poster or an extensionless media id. The
		// separate video_url serves MP4 bytes in both cases.
		name = strings.TrimSuffix(name, path.Ext(name)) + ".mp4"
	} else if m.Version > 0 {
		query := u.Query()
		query.Set("ver", strconv.Itoa(m.Version))
		u.RawQuery = query.Encode()
	}
	return File{Name: name, URL: u.String(), Size: -1}, true
}
