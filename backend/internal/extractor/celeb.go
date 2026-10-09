// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"path"
	"strconv"
	"strings"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
	"github.com/JohanLindvall/HeapLeach/internal/util"
	"golang.org/x/net/html"
)

// Celeb reads creator image galleries and individual images. The first page
// carries Inertia props in data-page; subsequent pages use the same media
// records from the creator API. src is the full-size image, while thumbnail
// and the creator's avatar are separate fields that must not be collected.
type Celeb struct {
	hostSet
	client *httpx.Client
}

// The site's player base64-decodes media URLs, then XORs them with this
// repeating key. This is URL obfuscation, not an expiring signature: keep
// the decoded /api/v1/image/ URL and let it redirect when the file starts.
const celebURLKey = "ilovemyself"

func NewCeleb(client *httpx.Client) *Celeb {
	return &Celeb{hostSet: hostSet{"celeb.st"}, client: client}
}

func (c *Celeb) Name() string { return "celeb" }

type celebImage struct {
	ID  int64  `json:"id"`
	Src string `json:"src"`
}

type celebPage struct {
	Component string `json:"component"`
	Props     struct {
		Creator struct {
			Name       string `json:"name"`
			Slug       string `json:"slug"`
			ImageCount int    `json:"imageCount"`
		} `json:"creator"`
		MediaType   string       `json:"mediaType"`
		Pages       int          `json:"pages"`
		CurrentPage int          `json:"currentPage"`
		Media       []celebImage `json:"media"`
		MediaID     int64        `json:"mediaId"`
		MediaURL    string       `json:"mediaUrl"`
	} `json:"props"`
}

func (c *Celeb) Extract(ctx context.Context, u *url.URL, opts Options) (*Result, error) {
	segs := util.PathSegments(u)
	if len(segs) < 2 || len(segs) > 4 || segs[0] != "creator" || (len(segs) >= 3 && segs[2] != "images") {
		return nil, fmt.Errorf("celeb: expected /creator/<name>/images or /creator/<name>/images/<id>")
	}
	var imageID int64
	if len(segs) == 4 {
		var err error
		imageID, err = strconv.ParseInt(segs[3], 10, 64)
		if err != nil || imageID <= 0 {
			return nil, fmt.Errorf("celeb: invalid image id")
		}
	}

	pageURL := *u
	pageURL.Fragment = ""
	if len(segs) == 2 {
		pageURL.Path = strings.TrimRight(pageURL.Path, "/") + "/images"
		pageURL.RawPath = ""
	}
	// Pasting a later page still means the whole gallery. Keep the selected
	// sort order, which the API needs on every subsequent request too.
	query := pageURL.Query()
	query.Del("page")
	pageURL.RawQuery = query.Encode()
	page := pageURL.String()
	doc, err := c.client.GetString(ctx, page, nil)
	if err != nil {
		return nil, fmt.Errorf("celeb: fetch gallery: %w", err)
	}
	data, err := celebPageData(doc)
	if err != nil {
		return nil, err
	}
	p := data.Props
	res := &Result{Title: util.FirstNonEmpty(p.Creator.Name, segs[1])}
	if imageID != 0 {
		if data.Component != "Creator/MediaObject" || p.MediaID != imageID {
			return nil, fmt.Errorf("celeb: image is missing from the page")
		}
		file, err := (celebImage{ID: p.MediaID, Src: p.MediaURL}).file(page)
		if err != nil {
			return nil, err
		}
		res.Files = []File{file}
		return res, nil
	}
	if data.Component != "Creator/Media" || p.MediaType != "media" || p.CurrentPage != 1 {
		return nil, fmt.Errorf("celeb: page does not contain an image gallery")
	}

	seen := make(map[int64]bool)
	items := p.Media
	pages := max(1, p.Pages)
	limit := opts.maxFiles()
	for pageNo := 1; pageNo <= pages; pageNo++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		before := len(res.Files)
		for _, item := range items {
			if seen[item.ID] {
				continue
			}
			if len(res.Files) == limit {
				res.Note = "partial — file limit"
				break
			}
			file, err := item.file(page)
			if err != nil {
				return nil, err
			}
			seen[item.ID] = true
			res.Files = append(res.Files, file)
		}
		if res.Note != "" {
			break
		}
		// A repeated or unexpectedly empty page must not loop or pass for
		// a complete gallery. The count below makes the shortfall visible.
		if len(res.Files) == before {
			res.Note = "partial — pagination stopped"
			break
		}
		if pageNo == pages {
			break
		}
		if len(res.Files) == limit {
			res.Note = "partial — file limit"
			break
		}
		if pageNo == config.MaxAlbumPages {
			res.Note = "partial — page limit"
			break
		}
		api := util.Origin(&pageURL) + "/api/v1/creator/" +
			url.PathEscape(util.FirstNonEmpty(p.Creator.Slug, segs[1])) + "/media"
		params := url.Values{"page": {strconv.Itoa(pageNo + 1)}, "mediaType": {"media"}}
		if sort := query.Get("sort"); sort != "" {
			params.Set("sort", sort)
		}
		items = nil
		if err := c.client.GetJSON(ctx, api+"?"+params.Encode(), httpx.Referer(page), &items); err != nil {
			return nil, fmt.Errorf("celeb: fetch image page %d: %w", pageNo+1, err)
		}
	}
	if len(res.Files) == 0 {
		return nil, fmt.Errorf("celeb: no images found")
	}
	if p.Creator.ImageCount > len(res.Files) {
		res.Note = fmt.Sprintf("%d of %d images", len(res.Files), p.Creator.ImageCount)
	}
	return res, nil
}

func celebPageData(doc string) (*celebPage, error) {
	root, err := parseHTML(doc)
	if err != nil {
		return nil, err
	}
	node := findFirst(root, func(n *html.Node) bool { return hasAttr(n, "data-page") })
	if node == nil {
		return nil, fmt.Errorf("celeb: no gallery data on the page")
	}
	var data celebPage
	if err := json.Unmarshal([]byte(attr(node, "data-page")), &data); err != nil {
		return nil, fmt.Errorf("celeb: decode gallery data: %w", err)
	}
	return &data, nil
}

func (m celebImage) file(referer string) (File, error) {
	if m.ID <= 0 {
		return File{}, fmt.Errorf("celeb: image has no id")
	}
	link, err := celebImageURL(m.Src)
	if err != nil {
		return File{}, fmt.Errorf("celeb: image %d: %w", m.ID, err)
	}
	ext := path.Ext(link.Path)
	if ext == "" {
		ext = ".jpg"
	}
	return File{
		// The URL's basename is an opaque token. The image's own id is
		// shorter, unique and stable across gallery sorts and revisits.
		Name:    strconv.FormatInt(m.ID, 10) + ext,
		URL:     link.String(),
		Size:    -1,
		Headers: httpx.Referer(referer),
	}, nil
}

func celebImageURL(raw string) (*url.URL, error) {
	if !strings.HasPrefix(raw, "https://") && !strings.HasPrefix(raw, "http://") {
		decoded, err := base64.RawStdEncoding.DecodeString(strings.TrimRight(raw, "="))
		if err != nil {
			return nil, fmt.Errorf("invalid encoded image URL: %w", err)
		}
		for i := range decoded {
			decoded[i] ^= celebURLKey[i%len(celebURLKey)]
		}
		raw = string(decoded)
	}
	link, err := url.Parse(raw)
	if err != nil || (link.Scheme != "http" && link.Scheme != "https") || link.Host == "" || link.User != nil {
		return nil, fmt.Errorf("invalid image URL")
	}
	return link, nil
}
