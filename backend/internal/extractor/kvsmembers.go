package extractor

import (
	"context"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
	"github.com/JohanLindvall/HeapLeach/internal/util"
	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// A member's public videos on a Kernel Video Sharing install.
//
// The platform lists them at /members/<id>/public_videos/, fifteen at a time,
// and pages through them with its own asynchronous block loader rather than
// with an ordinary URL: appending ?mode=async&function=get_block&block_id=...
// &from=<n> returns just the listing fragment. The paged path a reader would
// guess at — /public_videos/2/ — is a 404.
//
// Two things decide the shape of the walk below.
//
// Asking for a page past the end returns the FIRST page again, byte for byte,
// rather than an empty one. So the stop condition cannot be "the page was
// empty"; it is "the page added nothing new", which is what bunkr's albums
// and fapello's listings already do here for the same reason. The listing
// also states its own total — "Showing 1 - 15 of 15 videos" — which is read
// where it can be, as a second and cheaper way to know when to stop.
//
// The block id is read off the first page rather than compiled in. It is
// derivable from the section name, but the container is right there in the
// markup and taking it from the page means an install that names its blocks
// differently still works.
//
// This is only offered on the hosts registered as KVS. The direct-link
// sniffer deliberately does not look at /members/ paths: that is far too
// ordinary a URL shape to spend a request on for every unrecognised site,
// where /videos/<id>/<slug>/ is specific enough to be worth the guess.

// kvsMemberSection is the listing this fetches. A profile has several —
// favourites, albums, friends — and only a member's own public videos are
// unambiguously theirs and reachable without an account.
const kvsMemberSection = "public_videos"

// kvsListingBlock finds the container the platform puts its listing in, whose
// id names the block to ask for the next page of.
var kvsListingBlock = regexp.MustCompile(`id="(list_videos_[a-z0-9_]+)_items"`)

// kvsShowingTotal reads the "Showing 1 - 15 of 15 videos" line, which is how
// the listing states how far it goes.
var kvsShowingTotal = regexp.MustCompile(`Showing\s+\d+\s*-\s*\d+\s+of\s+(\d+)`)

// kvsMemberPath reports whether a URL names a member profile, and returns the
// canonical public-videos listing for it.
//
// Any section is accepted and redirected to the public videos: someone who
// pastes a profile, or its wall, or its favourites, means "this person's
// videos", and the sections that are not that are either unreachable without
// an account or not the member's own work.
func kvsMemberPath(u *url.URL) (listing string, ok bool) {
	segs := util.PathSegments(u)
	if len(segs) < 2 || segs[0] != "members" {
		return "", false
	}
	if _, err := strconv.Atoi(segs[1]); err != nil {
		return "", false
	}
	return fmt.Sprintf("%s/members/%s/%s/", util.Origin(u), segs[1], kvsMemberSection), true
}

// kvsMember resolves a profile into one file per public video.
func kvsMember(ctx context.Context, client *httpx.Client, listing, label string) (*Result, error) {
	pages, title, err := kvsMemberPages(ctx, client, listing, label)
	if err != nil {
		return nil, err
	}
	if len(pages) == 0 {
		return nil, fmt.Errorf("%s: no public videos on %s "+
			"(the member may have none, or may keep them private)", label, listing)
	}

	// Every video needs its own page read for a media URL, so they are
	// fetched several at a time and a video that will not load is skipped
	// rather than failing a profile of hundreds.
	files := FanOut(ctx, pages, func(ctx context.Context, page string) ([]File, error) {
		u, err := ParseURL(page)
		if err != nil {
			return nil, err
		}
		res, err := kvsExtract(ctx, client, u, label)
		if err != nil {
			return nil, err
		}
		return res.Files, nil
	})
	if len(files) == 0 {
		return nil, fmt.Errorf("%s: none of the %d videos on %s could be resolved",
			label, len(pages), listing)
	}
	return &Result{Title: title, Files: files}, nil
}

// kvsMemberPages walks the listing and collects every video page it names.
func kvsMemberPages(ctx context.Context, client *httpx.Client, listing, label string) ([]string, string, error) {
	var (
		pages []string
		title string
		block string
		total int
		seen  = make(map[string]bool)
	)

	for page := 1; page <= config.MaxAlbumPages; page++ {
		target := listing
		if page > 1 {
			if block == "" {
				break // the first page named no block, so there is no second
			}
			target = fmt.Sprintf("%s?mode=async&function=get_block&block_id=%s&sort_by=&from=%d",
				listing, url.QueryEscape(block), page)
		}

		doc, err := client.GetString(ctx, target, httpx.Referer(listing))
		if err != nil {
			if page == 1 {
				return nil, "", fmt.Errorf("%s: fetch %s: %w", label, listing, err)
			}
			break // a later page failing still leaves the earlier ones
		}
		root, err := parseHTML(doc)
		if err != nil {
			break
		}
		if page == 1 {
			title = kvsMemberTitle(root, doc)
			if m := kvsListingBlock.FindStringSubmatch(doc); m != nil {
				block = m[1]
			}
			if m := kvsShowingTotal.FindStringSubmatch(doc); m != nil {
				total, _ = strconv.Atoi(m[1])
			}
		}

		added := 0
		for _, link := range kvsListingVideos(root, listing) {
			if seen[link] {
				continue
			}
			seen[link] = true
			pages = append(pages, link)
			added++
		}
		// Past the end the platform serves the first page again rather than
		// an empty one, so "nothing new" is the only reliable stop.
		if added == 0 {
			break
		}
		if total > 0 && len(pages) >= total {
			break
		}
		if len(pages) >= config.MaxListingFiles {
			break
		}
	}
	return pages, title, nil
}

// kvsListingVideos reads the video pages a listing links to.
func kvsListingVideos(root *html.Node, base string) []string {
	baseURL, err := url.Parse(base)
	if err != nil {
		return nil
	}
	var out []string
	for _, a := range findAll(root, func(n *html.Node) bool { return isElem(n, atom.A) }) {
		link := resolveRef(baseURL, attr(a, "href"))
		if link == "" {
			continue
		}
		ref, err := url.Parse(link)
		if err != nil || !strings.EqualFold(ref.Host, baseURL.Host) {
			continue
		}
		// The listing sits among the site's own furniture, so the video
		// path shape is what separates a thumbnail from a menu item — the
		// same test the direct-link sniffer uses.
		if kvsVideoPath(ref) {
			out = append(out, link)
		}
	}
	return util.Dedupe(out)
}

// kvsMemberTitle names the job after the member, which is what a folder full
// of their videos should be called.
func kvsMemberTitle(root *html.Node, doc string) string {
	heading := strings.TrimSpace(firstText(root, atom.H2))
	// The heading reads "<name>'s Public Videos"; the possessive is the
	// name, and the rest is a section label nobody wants in a folder name.
	if name, _, ok := strings.Cut(heading, "'s "); ok && name != "" {
		return name
	}
	return util.FirstNonEmpty(heading, trimSiteSuffix(firstTitleOf(doc)))
}
