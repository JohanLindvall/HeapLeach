// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"fmt"
	"net/url"
	"path"
	"strconv"
	"strings"

	"github.com/JohanLindvall/HeapLeach/internal/httpx"
	"github.com/JohanLindvall/HeapLeach/internal/util"
	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// Kernel Video Sharing installs.
//
// KVS is sold as a turnkey tube site rather than run as one, so a great many
// hosts serve the identical player over the identical page: one flashvars
// object, with the media URL scrambled by the page's own license code. The
// arithmetic that undoes that lives in kvs.go and none of it is per-host, so
// one extractor covers every install, and it knows none of them by name.
//
// An install is recognised by the software's own page structure, which is
// what this extractor then reads: the licensed player (its configuration
// carrying a license code, or the player script), or the asynchronous
// listing controls every listing renders. A site that moves domain, or one
// nobody ever listed, is recognised the same way.
type KVS struct {
	client *httpx.Client
}

// kvsMaxAlternates bounds the numbered alternate renditions a page is read
// for. Installs list a handful at most; the bound is against a malformed
// page, not against any real one.
const kvsMaxAlternates = 6

// kvsLabel names every install: the software, since the extractor is not
// any one site's.
const kvsLabel = "kvs"

// NewKVS builds the Kernel Video Sharing extractor.
func NewKVS(client *httpx.Client) *KVS { return &KVS{client: client} }

func (k *KVS) Name() string { return kvsLabel }

// Recognises is the inventory's description.
func (k *KVS) Recognises() string {
	return "Kernel Video Sharing pages — videos, member profiles, searches and listings — carrying " +
		"the licensed player's configuration or script, or its `list_videos` blocks (asynchronous " +
		"controls, or a container and its `_items` list)"
}

// Shape reports the URL forms worth a probe: a video page under a /videos/
// or /video/ segment, and a member profile. Listings and searches are
// recognised once fetched, but their paths are every site's.
func (k *KVS) Shape(u *url.URL) bool {
	if kvsVideoPath(u) {
		return true
	}
	_, member := kvsMemberPath(u)
	return member
}

// Recognise takes any page of an install below its root: the structure the
// extraction reads is the evidence, whatever the path, since installs rename
// their sections.
func (k *KVS) Recognise(_ context.Context, p *Page) bool {
	if len(p.Segments()) == 0 || !p.HasAny(kvsMarkers...) {
		return false
	}
	root := p.HTML()
	return root != nil && kvsFingerprint(root, p.Body)
}

// kvsMarkers are the literals every part of the fingerprint contains, one
// of which the body must carry before the document is walked for it.
var kvsMarkers = []string{"license_code", "kt_player", "list_videos"}

// kvsFingerprint reports the platform's own structure. A generic flashvars
// object or a videos container is insufficient: KVS identifies itself
// through the licensed player, its asynchronous block controls (which carry
// both the block id and the platform's parameters), or a block rendered as
// the platform renders one: its container and the items list named after it.
func kvsFingerprint(root *html.Node, doc string) bool {
	if vars, err := parseFlashvars(doc); err == nil && vars["license_code"] != "" {
		return true
	}
	// An inline call is looked for only where the document has one at all,
	// rather than by reading out every script's text.
	inline := strings.Contains(doc, "kt_player(")
	return findFirst(root, func(n *html.Node) bool {
		if n.DataAtom == atom.Script {
			return strings.Contains(attr(n, "src"), "/kt_player.") || inline && strings.Contains(textOf(n), "kt_player(")
		}
		if id := attr(n, "id"); strings.HasPrefix(id, "list_videos_") {
			// The platform renders a block as its container and, inside
			// it, the items list named after it: a theme that drops the
			// asynchronous controls still keeps the pair.
			items := id + "_items"
			return findFirst(n, func(c *html.Node) bool { return c != n && attr(c, "id") == items }) != nil
		}
		return attr(n, "data-action") == "ajax" &&
			strings.HasPrefix(attr(n, "data-block-id"), "list_videos") && attr(n, "data-parameters") != ""
	}) != nil
}

// Extract resolves a video page, a member's whole public catalogue,
// everything a search turns up, or any other listing the install lays out —
// a category, a model, a tag, a channel, the site's own latest list.
func (k *KVS) Extract(ctx context.Context, u *url.URL, opts Options) (*Result, error) {
	var l kvsListing
	if listing, ok := kvsMemberPath(u); ok {
		l = kvsListingForMember(listing)
	} else if query, listing, ok := kvsSearchPath(u); ok {
		l = kvsListingForSearch(query, listing)
	} else {
		return kvsBrowse(ctx, k.client, u, opts.maxFiles())
	}
	l.limit = opts.maxFiles()
	if l.url == u.String() {
		if doc, _, ok := probedDocument(ctx, u); ok {
			l.first = doc
		}
	}
	return kvsListingResult(ctx, k.client, l)
}

// kvsExtract fetches a video page and resolves it. Everything needed is
// inline in the page, so one request resolves a video.
func kvsExtract(ctx context.Context, client *httpx.Client, u *url.URL) (*Result, error) {
	doc, _, err := fetchDocument(ctx, client, u, httpx.Referer(util.Origin(u)+"/"))
	if err != nil {
		return nil, fmt.Errorf("%s: fetch %s: %w", kvsLabel, u.Redacted(), err)
	}
	return kvsResult(doc, u)
}

// kvsResult turns a fetched page into a result, and is where the host-shaped
// error messages come from. Kept apart from the fetch so it can be tested
// against a page rather than a site.
func kvsResult(doc string, u *url.URL) (*Result, error) {
	vars, err := parseFlashvars(doc)
	if err != nil {
		return nil, fmt.Errorf("%s: %w (the video may be private or removed, "+
			"or this host may not run the player this handles)", kvsLabel, err)
	}

	renditions, err := kvsRenditions(vars)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", kvsLabel, err)
	}
	// KVS serves plain files. An install that has moved to an adaptive
	// stream is reported rather than handled: saving the playlist under the
	// video's name would produce a few kilobytes that look like a finished
	// download and play as nothing.
	direct := make([]mediaCandidate, 0, len(renditions))
	for _, c := range renditions {
		if !c.IsHLS {
			direct = append(direct, c)
		}
	}
	best, ok := bestCandidate(direct)
	if !ok {
		if len(renditions) > 0 {
			return nil, fmt.Errorf("%s: this page offers only an adaptive stream, "+
				"which this host is not expected to and so is not handled", kvsLabel)
		}
		return nil, fmt.Errorf("%s: no media URL in the player configuration", kvsLabel)
	}

	title := util.FirstNonEmpty(
		vars["video_title"],
		pageTitle(doc),
		strings.Trim(u.Path, "/"),
	)

	return &Result{
		Title: title,
		Files: []File{{
			Name:    title + kvsExtension(vars, best.URL),
			URL:     best.URL,
			Size:    -1,
			Headers: httpx.Referer(u.String()),
		}},
	}, nil
}

// kvsRenditions collects and unscrambles every rendition a player
// configuration lists.
//
// video_url is the main one; alternates are numbered video_alt_url,
// video_alt_url2, ... and each may carry a matching _text label naming its
// resolution. Which of them is actually the largest varies by install, so
// they are all read and the labels decide, rather than trusting the order.
func kvsRenditions(vars map[string]string) ([]mediaCandidate, error) {
	license := vars["license_code"]
	var (
		out      []mediaCandidate
		firstErr error
	)
	for i := 0; i <= kvsMaxAlternates; i++ {
		key := "video_url"
		if i > 0 {
			key = "video_alt_url"
			if i > 1 {
				key += strconv.Itoa(i)
			}
		}
		scrambled := vars[key]
		if scrambled == "" {
			continue
		}
		direct, err := kvsRealURL(scrambled, license)
		if err != nil {
			// One rendition that will not unscramble is not fatal while
			// another still might; the first failure is kept in case none do.
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		out = append(out, mediaCandidate{
			URL:     direct,
			Quality: kvsQuality(vars, key, direct),
			IsHLS:   strings.Contains(direct, ".m3u8"),
		})
	}
	if len(out) == 0 && firstErr != nil {
		return nil, firstErr
	}
	return out, nil
}

// kvsHDQuality is what a rendition flagged high-definition without stating a
// resolution ranks as: above any standard-definition label, which is what
// the flag distinguishes it from.
const kvsHDQuality = 720

// kvsQuality ranks one rendition.
//
// The label is tried first and the file name second, since installs label
// some renditions with a resolution and leave others to the name. What
// neither covers is a label that is a word — "HQ", "HD" — and that is how
// some installs mark their best file: the main rendition labelled "HQ" and
// flagged video_url_hd, beside an alternate labelled "240p". Read as a
// number, the word is nothing, so the 240p won and every download from those
// installs came down at the lowest quality on offer. The word and the _hd
// flag both mean high definition, so either ranks the rendition as such.
func kvsQuality(vars map[string]string, key, direct string) int {
	label := vars[key+"_text"]
	if q := qualityOf(label); q > 0 {
		return q
	}
	if q := qualityOf(direct); q > 0 {
		return q
	}
	switch strings.ToUpper(strings.TrimSpace(label)) {
	case "HQ", "HD", "FHD", "FULL HD":
		return kvsHDQuality
	}
	if vars[key+"_hd"] == "1" {
		return kvsHDQuality
	}
	return 0
}

// kvsExtension picks the file extension the player advertises, falling back
// to the one in the URL path.
func kvsExtension(vars map[string]string, direct string) string {
	if postfix := vars["postfix"]; strings.HasPrefix(postfix, ".") {
		return postfix
	}
	return linkExt(direct, ".mp4")
}

// kvsVideoPath reports whether a path looks like a KVS video page: a /videos/
// segment somewhere above a final segment that is a slug rather than a file.
func kvsVideoPath(u *url.URL) bool {
	segs := util.PathSegments(u)
	if len(segs) < 2 {
		return false
	}
	if path.Ext(segs[len(segs)-1]) != "" {
		return false // a file, not a page
	}
	for _, seg := range segs[:len(segs)-1] {
		if seg == "videos" || seg == "video" {
			return true
		}
	}
	return false
}
