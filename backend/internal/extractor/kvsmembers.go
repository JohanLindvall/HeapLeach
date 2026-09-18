package extractor

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/JohanLindvall/HeapLeach/internal/util"
	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// A member's public videos on a Kernel Video Sharing install.
//
// The platform lists them at /members/<id>/public_videos/, forty-eight at a
// time, and pages them at /public_videos/<n>/. The walk is the one every
// listing on the platform shares, in kvslisting.go; what is here is what
// makes a profile a profile — which URLs name one, which section of it is
// the member's own work, and what to call the job.
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

// kvsListingForMember describes a profile's videos to the shared walk.
func kvsListingForMember(listing string) kvsListing {
	return kvsListing{
		url:   listing,
		title: kvsMemberTitle,
		empty: fmt.Sprintf("no public videos on %s (the member may have none, or may keep them private)", listing),
	}
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
