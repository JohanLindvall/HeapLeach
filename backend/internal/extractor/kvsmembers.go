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
// The platform lists them at /members/<id>/public_videos/, forty-eight at a
// time, and pages them at /public_videos/<n>/. The walk follows the listing's
// own "next" anchor rather than building those paths itself, which is both
// simpler and the only version that cannot be wrong about where the pages
// stop: the last one carries no next link at all.
//
// That deserves a note, because the platform offers a second mechanism that
// looks right and is not. Appending ?mode=async&function=get_block&block_id=
// ...&from=<n> returns the listing as a bare fragment, which is what the
// site's own scripts use — but the from parameter is ignored on a member
// listing. Every page comes back as page one, byte for byte, and a walk built
// on it stops at the first repeat having quietly collected only the first
// forty-eight of however many there are. It looks like it works, which is the
// worst way for it to fail.
//
// The stated total — "Showing 49 - 64 of 64" — is read as a second stop, and
// pages are deduplicated, so a listing that ever does repeat itself ends the
// walk rather than looping.
//
// This is only offered on the hosts registered as KVS. The direct-link
// sniffer deliberately does not look at /members/ paths: that is far too
// ordinary a URL shape to spend a request on for every unrecognised site,
// where /videos/<id>/<slug>/ is specific enough to be worth the guess.

// kvsMemberSection is the listing this falls back to. A profile has several
// — favourites, albums, friends — and only a member's own videos are
// unambiguously theirs and reachable without an account.
const kvsMemberSection = "public_videos"

// kvsMemberSections are the names installs give that same listing. Which one
// exists varies, and guessing wrong is not a 404: this platform answers an
// unknown member section with a generic block of the site's newest videos,
// served under the member's own URL. That reads as a perfectly good listing
// and quietly downloads somebody else's work, so a section the caller
// actually named is kept rather than rewritten.
var kvsMemberSections = map[string]bool{
	"public_videos":   true,
	"videos":          true,
	"uploaded_videos": true,
}

// kvsShowingTotal reads the "Showing 49 - 64 of 64 videos" line, which is how
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
	section := kvsMemberSection
	if len(segs) >= 3 && kvsMemberSections[strings.ToLower(segs[2])] {
		section = strings.ToLower(segs[2])
	}
	return fmt.Sprintf("%s/members/%s/%s/", util.Origin(u), segs[1], section), true
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
		pages   []string
		title   string
		total   int
		seen    = make(map[string]bool)
		visited = make(map[string]bool)
		target  = listing
		block   string
	)

	for page := 1; page <= config.MaxAlbumPages && target != ""; page++ {
		if visited[target] {
			break // the listing pointed back at a page already walked
		}
		visited[target] = true

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
			title = kvsMemberTitle(root)
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
		if added == 0 {
			break
		}
		if total > 0 && len(pages) >= total {
			break
		}
		if len(pages) >= config.MaxListingFiles {
			break
		}
		// The listing itself says where the next page is, and says nothing
		// on the last one. That is the stop condition; everything above is
		// a guard against a listing that misreports itself.
		target = kvsNextPage(root, listing)
		if target == "" {
			if page == 1 {
				block = kvsListingBlock(root)
			}
			target = kvsAsyncPage(listing, block, page+1)
		}
	}
	return pages, title, nil
}

// kvsNextPage follows the listing's own "next" control.
func kvsNextPage(root *html.Node, base string) string {
	baseURL, err := url.Parse(base)
	if err != nil {
		return ""
	}
	next := findFirst(root, func(n *html.Node) bool {
		return hasClass(n, "pagination-next")
	})
	if next == nil {
		return ""
	}
	// The class sits on the list item, with the link inside it.
	if isElem(next, atom.A) {
		return resolveRef(baseURL, attr(next, "href"))
	}
	if a := findFirst(next, func(n *html.Node) bool { return isElem(n, atom.A) }); a != nil {
		return resolveRef(baseURL, attr(a, "href"))
	}
	return ""
}

// kvsListingBlock reads the id of the list block a page renders, which is
// what an async page request has to name.
func kvsListingBlock(root *html.Node) string {
	n := findFirst(root, func(n *html.Node) bool { return attr(n, "data-block-id") != "" })
	if n == nil {
		return ""
	}
	return attr(n, "data-block-id")
}

// kvsAsyncPage builds the listing's own async page request, for installs
// that page the member listing entirely in script and leave the "next"
// control pointing at "#" — there is no anchor to follow on those.
//
// Note what this is not. The same platform takes a ?from=<n> on a member
// listing and ignores it, handing back page one every time; a walk built on
// that collects the first page over and over and stops thinking it is done.
// The parameter that actually pages is named for the block it pages. The
// walk deduplicates and stops on a page that adds nothing either way, so an
// install that ignores this one as well ends the walk rather than looping.
func kvsAsyncPage(listing, block string, page int) string {
	if block == "" {
		return ""
	}
	return fmt.Sprintf("%s?mode=async&function=get_block&block_id=%s&from_videos=%d",
		listing, url.QueryEscape(block), page)
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
func kvsMemberTitle(root *html.Node) string {
	heading := strings.TrimSpace(firstText(root, atom.H2))
	// The heading reads "<name>'s Public Videos"; the possessive is the
	// name, and the rest is a section label nobody wants in a folder name.
	if name, _, ok := strings.Cut(heading, "'s "); ok && name != "" {
		return name
	}
	return util.FirstNonEmpty(heading, trimSiteSuffix(firstText(root, atomTitle)))
}
