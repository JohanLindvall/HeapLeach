// SPDX-License-Identifier: MIT

package extractor

import (
	"cmp"
	"context"
	"fmt"
	"net/url"
	"strings"

	"github.com/JohanLindvall/HeapLeach/internal/httpx"
	"github.com/JohanLindvall/HeapLeach/internal/util"
)

// GoonBox's image viewer is a client-rendered app. Its public metadata API
// supplies the original separately from the medium and thumbnail renditions;
// the viewer itself can be challenged while that API still answers.
var goonboxImageShape = pathShape(`^/img/(?:[^/]+\.)?[A-Za-z0-9]+/?$`)

var goonboxSite = shareSite{
	name: "goonbox",
	recognises: "GoonBox image viewers (`/img/<id>`, also `/img/<slug>.<id>`) served by its web app; " +
		"original files are read from the public image metadata API",
	forms: []shareForm{{goonboxImageShape, func(p *Page) bool {
		return p.Named("GoonBox") && p.Script("/assets/index-") && p.Has(`id="root"`)
	}}},
	extract: goonboxExtract,
}

// NewGoonBox builds the image-viewer extractor.
func NewGoonBox(client *httpx.Client) *Share { return newShare(client, &goonboxSite) }

// goonboxImageID follows the viewer's legacy filename link handling: the
// final dot separates the slug from the same encoded id as /img/<id>.
func goonboxImageID(u *url.URL) string {
	if !goonboxImageShape(u) {
		return ""
	}
	id := util.PathSegments(u)[1]
	if dot := strings.LastIndexByte(id, '.'); dot >= 0 {
		id = id[dot+1:]
	}
	return id
}

type goonboxMetadata struct {
	Image *struct {
		ID      string `json:"encoded_id"`
		Name    string `json:"original_filename"`
		URL     string `json:"original_url"`
		Size    int64  `json:"size_bytes"`
		Deleted string `json:"deleted_at"`
	} `json:"image"`
	Deleted bool `json:"is_deleted"`
}

func goonboxExtract(ctx context.Context, s *Share, u *url.URL, _ Options) (*Result, error) {
	id := goonboxImageID(u)
	if id == "" {
		return nil, fmt.Errorf("goonbox: expected an image viewer (/img/<id>)")
	}
	origin := util.Origin(u)
	var doc goonboxMetadata
	if err := s.client.GetJSON(ctx, origin+"/api/images/"+url.PathEscape(id),
		httpx.Referer(origin+"/img/"+id), &doc); err != nil {
		return nil, fmt.Errorf("goonbox: read image %s: %w", id, err)
	}
	if doc.Deleted || doc.Image != nil && doc.Image.Deleted != "" {
		return nil, fmt.Errorf("goonbox: image %s has been removed", id)
	}
	if doc.Image == nil || doc.Image.ID != id {
		return nil, fmt.Errorf("goonbox: metadata does not describe image %s", id)
	}
	// Neither album neighbours nor a smaller rendition can stand in for a
	// missing original. Only the original's own exact byte count is useful
	// to the downloader's skip check.
	link := resolveRef(u, strings.TrimSpace(doc.Image.URL))
	media, err := url.Parse(link)
	if link == "" || err != nil || media.Host == "" || media.User != nil ||
		(media.Scheme != "http" && media.Scheme != "https") {
		return nil, fmt.Errorf("goonbox: image %s offers no original file", id)
	}
	name := cmp.Or(strings.TrimSpace(doc.Image.Name), util.NameFromURL(link), id)
	return &Result{Title: name, Files: []File{{
		Name: name, URL: link, Size: knownSize(doc.Image.Size),
		Headers: httpx.Referer(origin + "/"),
	}}}, nil
}
