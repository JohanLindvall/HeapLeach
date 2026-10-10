// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"github.com/JohanLindvall/HeapLeach/internal/httpx"
	"github.com/JohanLindvall/HeapLeach/internal/util"
	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// FileJump serves public file shares. The download button is a stable
// redirector that signs the storage URL on every request, so keep the button
// URL itself: saving its redirect target would expire while queued.
type FileJump struct {
	hostSet
	client *httpx.Client
}

func NewFileJump(client *httpx.Client) *FileJump {
	return &FileJump{hostSet: hostSet{"filejump.com"}, client: client}
}

func (f *FileJump) Name() string { return "filejump" }

func (f *FileJump) Extract(ctx context.Context, u *url.URL, _ Options) (*Result, error) {
	parts := util.PathSegments(u)
	if len(parts) < 2 || parts[0] != "s" || len(parts) > 3 ||
		(len(parts) == 3 && parts[2] != "download" && parts[2] != "content") {
		return nil, fmt.Errorf("filejump: expected a public share (/s/<id>)")
	}
	page := *u
	page.Path, page.RawPath, page.Fragment = "/s/"+parts[1], "", ""
	doc, err := f.client.GetString(ctx, page.String(), httpx.Referer(util.Origin(&page)+"/"))
	if err != nil {
		return nil, fmt.Errorf("filejump: fetch share: %w", err)
	}
	root, err := parseHTML(doc)
	if err != nil {
		return nil, err
	}
	var download string
	for _, a := range findAll(root, func(n *html.Node) bool { return isElem(n, atom.A) }) {
		if hasAttr(a, "disabled") || attr(a, "aria-disabled") == "true" {
			continue
		}
		link, err := page.Parse(strings.TrimSpace(attr(a, "href")))
		if err != nil || link.User != nil || link.Scheme != page.Scheme ||
			!strings.EqualFold(link.Host, page.Host) || link.Path != page.Path+"/download" {
			continue
		}
		link.Fragment = ""
		download = link.String()
		break
	}
	if download == "" {
		return nil, fmt.Errorf("filejump: no public download link (the share may be restricted, expired or password protected)")
	}
	name := util.FirstNonEmpty(textOfFirst(root, func(n *html.Node) bool { return hasClass(n, "title") }),
		trimSiteSuffix(firstText(root, atom.Title)), parts[1])
	meta := textOfFirst(root, func(n *html.Node) bool { return hasClass(n, "meta") })
	size := parseHumanSize(strings.ReplaceAll(meta, ",", ""))
	return &Result{Title: name, Files: []File{{
		Name: name, URL: download, Size: size, SizeApprox: size >= 0,
		Headers: httpx.Referer(page.String()),
	}}}, nil
}
