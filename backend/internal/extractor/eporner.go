// SPDX-License-Identifier: MIT

package extractor

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/JohanLindvall/HeapLeach/internal/httpx"
	"github.com/JohanLindvall/HeapLeach/internal/util"
	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// Single videos and whole profile tabs.
//
// The renditions are not in the page. The page carries a video id and a
// 32-character hash, and that hash has to be folded into a shorter form
// before the player's own endpoint will answer with the source list — the
// hash as it stands is refused. The links that come back are signed with
// both an expiry and the requesting address, so the row is signed and a
// profile's videos are queued unread: a profile of a hundred videos would
// otherwise start failing partway down its own queue. The media requests
// carry no Referer.
const (
	epornerRoot   = "https://www.eporner.com"
	epornerDomain = "www.eporner.com"

	// The player hash arrives as four groups of eight hexadecimal digits.
	epornerHashGroups   = 4
	epornerHashGroupLen = 8
	epornerHashLen      = epornerHashGroups * epornerHashGroupLen
)

var (
	// epornerVideoShape is a video by each of its routes.
	epornerVideoShape = pathShape(`^/video-[A-Za-z0-9]+(?:/[^/]*)?/?$`, `^/(?:hd-porn|embed)/[A-Za-z0-9]+(?:/[^/]*)?/?$`)
	// epornerProfileShape is a profile's video tab, or a later page of it.
	epornerProfileShape = pathShape(`^/profile/[^/]+/(?:uploaded-videos|videos)(?:/[0-9]+)?/?$`)
)

var epornerPlayer = playerSite{
	name:  "eporner",
	shape: func(u *url.URL) bool { return epornerVideoShape(u) || epornerProfileShape(u) },
	// A video page by the player's id and hash, which read folds into the
	// request for the sources, and a profile tab by the site's profile
	// script, under the site's own name.
	recognise: func(p *Page) bool {
		if !p.Named("EPORNER.COM", "EPORNER") {
			return false
		}
		if epornerVideoShape(p.Final) {
			return p.Has("EP.video.player") && epornerHashPattern.MatchString(p.Body)
		}
		return epornerProfileShape(p.Final) && p.Script("/profile.js")
	},
	recognises: "videos (`/video-<id>/<slug>/`, `/hd-porn/<id>/`) whose page sets the EPORNER player's " +
		"video id and hash, and profile video tabs rendered with its profile script",
	page: func(u *url.URL) (string, error) {
		if epornerVideoID(util.PathSegments(u)) == "" {
			return "", fmt.Errorf("no video or profile in %s", u.Redacted())
		}
		return u.String(), nil
	},
	read:   epornerRead,
	signed: true,
	listing: &playerListing{
		shape: func(u *url.URL) bool {
			_, _, ok := epornerProfileOf(util.PathSegments(u))
			return ok
		},
		links: epornerProfile,
	},
}

// ------------------------------------------------------------------ paths

// epornerVideoPrefix marks the usual video path, /video-<id>/<slug>/.
const epornerVideoPrefix = "video-"

// epornerVideoSections name the same video by a different route: the API
// hands out /hd-porn/ links and the player is framed from /embed/.
var epornerVideoSections = map[string]bool{"hd-porn": true, "embed": true}

// epornerVideoID reads the video id out of a path, or "" when there is none.
func epornerVideoID(segs []string) string {
	if len(segs) == 0 {
		return ""
	}
	if id := strings.TrimPrefix(segs[0], epornerVideoPrefix); id != segs[0] && id != "" {
		return id
	}
	if len(segs) >= 2 && epornerVideoSections[strings.ToLower(segs[0])] {
		return segs[1]
	}
	return ""
}

// epornerVideoTabs are the profile tabs that list videos. A profile has
// picture tabs too, and they list nothing this extractor can fetch.
var epornerVideoTabs = map[string]bool{"uploaded-videos": true, "videos": true}

// epornerProfileOf recognises /profile/<user>/<tab>/ and its later pages.
func epornerProfileOf(segs []string) (user, tab string, ok bool) {
	if len(segs) < 3 || !strings.EqualFold(segs[0], "profile") {
		return "", "", false
	}
	tab = strings.ToLower(segs[2])
	if !epornerVideoTabs[tab] {
		return "", "", false
	}
	return segs[1], tab, true
}

// epornerProfilePage builds one page of a profile tab. The tab is part of
// the path rather than a query, and page one is the bare path with later
// pages appending the number. The original host is kept so a language
// subdomain stays on the language the caller asked for.
func epornerProfilePage(u *url.URL, user, tab string, page int) string {
	base := epornerRoot
	if u != nil && u.Scheme != "" && u.Host != "" {
		base = u.Scheme + "://" + u.Host
	}
	link := base + "/profile/" + user + "/" + tab + "/"
	if page > 1 {
		link += strconv.Itoa(page) + "/"
	}
	return link
}

// epornerBaseOf keeps the player endpoint on the same host as the page it
// came from, so a language subdomain asks itself rather than being bounced
// to the main site.
func epornerBaseOf(page string) string {
	if u, err := url.Parse(page); err == nil && u.Scheme != "" && u.Host != "" {
		return u.Scheme + "://" + u.Host
	}
	return epornerRoot
}

// ------------------------------------------------------------ one video

// epornerRead reads a video page and asks the player endpoint for its
// sources.
func epornerRead(ctx context.Context, client *httpx.Client, page string) (*playerVideo, error) {
	base := epornerBaseOf(page)
	doc, err := fetchPage(ctx, client, page, httpx.Referer(base+"/"))
	if err != nil {
		return nil, fmt.Errorf("fetch video page: %w", err)
	}
	vid, hash := epornerPlayerVars(doc)
	if vid == "" || hash == "" {
		return nil, fmt.Errorf("no player configuration in the page (the video may be private or removed)")
	}
	candidates, err := epornerSources(ctx, client, base, vid, hash)
	if err != nil {
		return nil, err
	}
	return &playerVideo{title: cmp.Or(epornerTitle(doc), vid), candidates: candidates}, nil
}

// epornerTitleSuffix strips the site's own name, and any tagline behind it,
// from the end of a document title.
//
// It has to cut from the end rather than at the first " - ", which is what
// the shared trimSiteSuffix looks for. Titles on this host routinely contain
// that separator themselves — a series numbered "... - 2", "... - 3" — and
// cutting at the first one leaves five different videos sharing a single
// name, which on disk is one file that five downloads take turns overwriting.
var epornerTitleSuffix = regexp.MustCompile(`(?i)\s*[-|]\s*EPORNER\b.*$`)

// epornerTitle reads a video page's own title, preferring the exact name the
// page states in its linked data over the document title.
func epornerTitle(doc string) string {
	root, err := parseHTML(doc)
	if err != nil {
		return ""
	}
	if name := epornerLinkedName(root); name != "" {
		return name
	}
	return strings.TrimSpace(epornerTitleSuffix.ReplaceAllString(firstText(root, atomTitle), ""))
}

// epornerLinkedName reads the VideoObject name out of a page's JSON-LD. The
// page carries a breadcrumb list in the same form, so the type is checked
// rather than taking the first name found.
func epornerLinkedName(root *html.Node) string {
	for _, script := range findAll(root, func(n *html.Node) bool {
		return isElem(n, atom.Script) && strings.Contains(strings.ToLower(attr(n, "type")), "ld+json")
	}) {
		var obj struct {
			Type string `json:"@type"`
			Name string `json:"name"`
		}
		if json.Unmarshal([]byte(textOf(script)), &obj) != nil {
			continue
		}
		if strings.EqualFold(obj.Type, "VideoObject") {
			if name := strings.TrimSpace(obj.Name); name != "" {
				return name
			}
		}
	}
	return ""
}

var (
	// The player's two variables sit in the page as plain assignments. The
	// single quotes matter: the <video> element carries a data-vid of its
	// own, in double quotes, holding a path rather than the bare id.
	epornerVidPattern  = regexp.MustCompile(`\bvid\s*=\s*'([A-Za-z0-9]+)'`)
	epornerHashPattern = regexp.MustCompile(`\bhash\s*=\s*'([0-9a-fA-F]{32})'`)
)

// epornerPlayerVars reads the video id and the player hash out of a page.
func epornerPlayerVars(doc string) (vid, hash string) {
	if m := epornerVidPattern.FindStringSubmatch(doc); m != nil {
		vid = m[1]
	}
	if m := epornerHashPattern.FindStringSubmatch(doc); m != nil {
		hash = m[1]
	}
	return vid, hash
}

// epornerPlayerHash folds the page's hash into the form the endpoint wants:
// each group of eight hexadecimal digits re-expressed in base 36, joined
// back together. Sending the hash as it stands is refused.
func epornerPlayerHash(raw string) string {
	if len(raw) != epornerHashLen {
		return ""
	}
	var out strings.Builder
	for i := range epornerHashGroups {
		n, err := strconv.ParseUint(raw[i*epornerHashGroupLen:(i+1)*epornerHashGroupLen], 16, 64)
		if err != nil {
			return ""
		}
		out.WriteString(strconv.FormatUint(n, 36))
	}
	return out.String()
}

// epornerSource is one rendition in the player's source list.
type epornerSource struct {
	LabelShort string `json:"labelShort"`
	Src        string `json:"src"`
}

// epornerSources asks the player endpoint for the video's MP4 renditions.
func epornerSources(ctx context.Context, client *httpx.Client, base, vid, hash string) ([]mediaCandidate, error) {
	short := epornerPlayerHash(hash)
	if short == "" {
		return nil, fmt.Errorf("the player hash is not the expected %d characters", epornerHashLen)
	}
	query := url.Values{
		"hash":             {short},
		"domain":           {epornerDomain},
		"fallback":         {"false"},
		"embed":            {"false"},
		"supportedFormats": {"dash,mp4"},
	}
	endpoint := base + "/xhr/video/" + vid + "?" + query.Encode()

	var payload struct {
		Available bool   `json:"available"`
		Message   string `json:"message"`
		Sources   struct {
			MP4 json.RawMessage `json:"mp4"`
		} `json:"sources"`
	}
	headers := httpx.Referer(base + "/")
	headers[httpx.HeaderRequestedWith] = httpx.RequestedWithXHR
	if err := client.GetJSON(ctx, endpoint, headers, &payload); err != nil {
		return nil, fmt.Errorf("player sources: %w", err)
	}
	if !payload.Available {
		if msg := strings.TrimSpace(payload.Message); msg != "" {
			return nil, errors.New(msg)
		}
		return nil, fmt.Errorf("the host reports this video as unavailable")
	}
	candidates := epornerCandidates(payload.Sources.MP4)
	if _, ok := bestCandidate(candidates); !ok {
		return nil, fmt.Errorf("no MP4 rendition is offered")
	}
	return candidates, nil
}

// epornerCandidates reads the MP4 source map. A video offering none answers
// with an empty array rather than an empty object, so a decode that fails
// means "none offered" rather than a response worth reporting as broken.
func epornerCandidates(raw json.RawMessage) []mediaCandidate {
	var byLabel map[string]epornerSource
	if len(raw) == 0 || json.Unmarshal(raw, &byLabel) != nil {
		return nil
	}
	labels := make([]string, 0, len(byLabel))
	for label := range byLabel {
		labels = append(labels, label)
	}
	slices.Sort(labels) // map order is random; keep the choice reproducible
	candidates := make([]mediaCandidate, 0, len(labels))
	for _, label := range labels {
		src := byLabel[label]
		if src.Src == "" {
			continue
		}
		candidates = append(candidates, mediaCandidate{
			URL:     src.Src,
			Quality: epornerQuality(src.LabelShort, label, src.Src),
		})
	}
	return candidates
}

// epornerQuality reads the vertical resolution from whichever of the short
// label, the label or the filename states one. The label is not always a
// bare resolution — "720p HD" — and one labelled only "4K" still carries
// the number in its filename.
func epornerQuality(short, label, src string) int {
	for _, text := range []string{short, label, src} {
		if q := qualityOf(text); q > 0 {
			return q
		}
	}
	return 0
}

// ------------------------------------------------------------- a profile

// epornerProfile walks a profile tab for the videos it lists, up to the
// listing cap. A walk the cap cut short says so; one that came back short
// of the tab's own count otherwise says that ("4 of 9"), for a listing that
// stops early otherwise looks exactly like a complete one. It is a note
// rather than part of the title, which names the folder, so a later
// complete run files into the same one.
func epornerProfile(ctx context.Context, client *httpx.Client, u *url.URL, opts Options) ([]listingLink, string, string, error) {
	user, tab, _ := epornerProfileOf(util.PathSegments(u))
	links, declared, note, err := epornerProfileVideos(ctx, client, u, user, tab, opts)
	if err != nil {
		return nil, "", "", err
	}
	if len(links) == 0 {
		return nil, "", "", fmt.Errorf("%s lists no videos under %s", user, tab)
	}
	return links, epornerProfileTitle(user, tab), cmp.Or(note, cappedNote(len(links), declared, "")), nil
}

// epornerProfileVideos pages through a profile tab by number, collecting
// what it lists and the total the tab claims to hold. Asking past the last
// page is how the walk finds its end — the listing answers 404 rather than
// an empty page — so the walk is quiet: a short one is caught by the
// declared count, not by trusting how it stopped. Its videos are read only
// when their transfers start, so the listing cap is what bounds it, and
// the note is the cap's when it cut the walk short.
func epornerProfileVideos(ctx context.Context, client *httpx.Client, u *url.URL, user, tab string, opts Options) ([]listingLink, int, string, error) {
	headers := httpx.Referer(epornerRoot + "/")
	first, err := url.Parse(epornerProfilePage(u, user, tab, 1))
	if err != nil {
		return nil, 0, "", err
	}
	root, err := fetchPageRoot(ctx, client, first.String(), headers)
	if err != nil {
		return nil, 0, "", fmt.Errorf("fetch profile: %w", err)
	}
	declared := epornerDeclaredCount(root, tab)
	seen := make(map[string]bool)
	page := 1
	limit, limitNote := listingLimit(opts)
	links, _, note, err := walkListing(ctx, client, first, listingWalk{
		name: "eporner", quiet: true, anyRedirect: true, root: root,
		limit: limit, limitNote: limitNote,
		fetch: fetchAsAsked(client, headers),
		read: func(root *html.Node, at, _ *url.URL) listingPage {
			lp := listingPage{links: epornerListing(root, at.String(), seen)}
			if len(lp.links) == 0 {
				return listingPage{end: true}
			}
			page++
			lp.next, _ = url.Parse(epornerProfilePage(u, user, tab, page))
			return lp
		},
	})
	return links, declared, note, err
}

// epornerListingClass marks the profile's own list. The page also carries a
// sidebar holding some of the same tiles and a block of unrelated ones, so
// taking every /video- link on the page collects entries the tab does not
// own — which is how a listing of twelve reads as fourteen.
//
// The neighbouring "plister" block is a decoy: it is the tab's header, it
// sits immediately before the list, and it holds no tiles at all. Anything
// that picks a container by what precedes a tile rather than what contains
// it lands on that one and finds nothing.
const epornerListingClass = "streamevents"

// epornerVideoHref matches a listing link and captures the video id.
var epornerVideoHref = regexp.MustCompile(`^/` + epornerVideoPrefix + `([A-Za-z0-9]+)/`)

// epornerListing collects the videos one page owns, in the page's own order.
func epornerListing(root *html.Node, pageURL string, seen map[string]bool) []listingLink {
	scope := findFirst(root, func(n *html.Node) bool { return hasClass(n, epornerListingClass) })
	if scope == nil {
		return nil
	}
	base, err := url.Parse(pageURL)
	if err != nil {
		return nil
	}

	// Every video is linked twice per tile — once from the thumbnail and
	// once from the caption — so the id decides identity and the better of
	// the two titles wins.
	var order []string
	pages := make(map[string]string)
	titles := make(map[string]string)
	for _, a := range findAll(scope, func(n *html.Node) bool { return isElem(n, atom.A) }) {
		m := epornerVideoHref.FindStringSubmatch(attr(a, "href"))
		if m == nil || seen[m[1]] {
			continue
		}
		id := m[1]
		if _, ok := pages[id]; !ok {
			order = append(order, id)
			pages[id] = resolveRef(base, attr(a, "href"))
		}
		if titles[id] == "" {
			titles[id] = epornerLinkTitle(a)
		}
	}

	found := make([]listingLink, 0, len(order))
	for _, id := range order {
		seen[id] = true
		found = append(found, listingLink{url: pages[id], title: cmp.Or(titles[id], util.NameFromURL(pages[id]), id)})
	}
	return found
}

// epornerLinkTitle reads a tile's title: the caption link carries it as
// text, the thumbnail link on the image beside it.
func epornerLinkTitle(a *html.Node) string {
	if t := strings.TrimSpace(attr(a, "title")); t != "" {
		return t
	}
	if img := findFirst(a, func(n *html.Node) bool { return isElem(n, atom.Img) }); img != nil {
		if alt := strings.TrimSpace(attr(img, "alt")); alt != "" {
			return alt
		}
	}
	return strings.TrimSpace(textOf(a))
}

// epornerCountPattern reads a tab heading's own total, "Uploaded videos (15)".
var epornerCountPattern = regexp.MustCompile(`([A-Za-z][A-Za-z ]{0,24}?)\s*\(\s*([\d,]+)\s*\)`)

// epornerDeclaredCount reads the total the tab states for itself. The
// heading is matched in full rather than by suffix, because a profile also
// shows "Watched videos (959)" and that is somebody else's number.
func epornerDeclaredCount(root *html.Node, tab string) int {
	want := strings.ReplaceAll(tab, "-", " ")
	best := 0
	walk(root, func(n *html.Node) {
		if n.Type != html.TextNode {
			return
		}
		m := epornerCountPattern.FindStringSubmatch(n.Data)
		if m == nil || !strings.EqualFold(strings.TrimSpace(m[1]), want) {
			return
		}
		if got, err := strconv.Atoi(strings.ReplaceAll(m[2], ",", "")); err == nil && got > best {
			best = got
		}
	})
	return best
}

// epornerProfileTitle names the job after the profile and its tab.
func epornerProfileTitle(user, tab string) string {
	return user + " (" + strings.ReplaceAll(tab, "-", " ") + ")"
}
