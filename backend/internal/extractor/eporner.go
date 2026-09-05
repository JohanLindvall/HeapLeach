package extractor

import (
	"context"
	"encoding/json"
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

// Eporner resolves single videos and whole profile tabs on eporner.com.
//
// The renditions are not in the page. The page carries a video id and a
// 32-character hash, and that hash has to be folded into a shorter form
// before the player's own endpoint will answer with the source list — the
// hash as it stands is refused. The links that come back are signed with
// both an expiry and the requesting address, so they are minted at download
// time rather than at extraction time: a profile of a hundred videos would
// otherwise start failing partway down its own queue.
type Eporner struct {
	hostSet
	client *httpx.Client
}

const (
	epornerRoot   = "https://www.eporner.com"
	epornerDomain = "www.eporner.com"
	epornerSite   = "eporner.com"
	epornerExt    = ".mp4"

	// The player hash arrives as four groups of eight hexadecimal digits.
	epornerHashGroups   = 4
	epornerHashGroupLen = 8
	epornerHashLen      = epornerHashGroups * epornerHashGroupLen

	// epornerMaxProfilePages and epornerMaxVideos bound a profile walk, the
	// way erome's are bounded.
	epornerMaxProfilePages = 60
	epornerMaxVideos       = 2000
)

// NewEporner builds the eporner extractor.
func NewEporner(client *httpx.Client) *Eporner {
	return &Eporner{hostSet: hostSet{epornerSite}, client: client}
}

func (e *Eporner) Name() string { return "eporner" }

// Extract handles one video or a profile tab that lists them.
func (e *Eporner) Extract(ctx context.Context, u *url.URL, _ Options) (*Result, error) {
	segs := util.PathSegments(u)
	if user, tab, ok := epornerProfileOf(segs); ok {
		return e.profile(ctx, u, user, tab)
	}
	if epornerVideoID(segs) != "" {
		return e.video(ctx, u.String())
	}
	return nil, fmt.Errorf("eporner: no video or profile in %s", u.Redacted())
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

// video defers the whole resolution to download time, because the link the
// player hands out is signed and short-lived.
func (e *Eporner) video(ctx context.Context, page string) (*Result, error) {
	return refetchedVideo(ctx, epornerExt, nil, func(ctx context.Context) (string, string, error) {
		return e.rendition(ctx, page)
	})
}

// rendition reads a video page, asks the player endpoint for its sources and
// returns the page's title with the best MP4 link.
func (e *Eporner) rendition(ctx context.Context, page string) (string, string, error) {
	base := epornerBaseOf(page)
	doc, err := e.client.GetString(ctx, page, httpx.Referer(base+"/"))
	if err != nil {
		return "", "", fmt.Errorf("eporner: fetch video page: %w", err)
	}
	vid, hash := epornerPlayerVars(doc)
	if vid == "" || hash == "" {
		return "", "", fmt.Errorf("eporner: no player configuration in the page (the video may be private or removed)")
	}
	link, err := e.sources(ctx, base, vid, hash)
	if err != nil {
		return "", "", err
	}
	return util.FirstNonEmpty(epornerTitle(doc), vid), link, nil
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

// sources asks the player endpoint for the video's renditions and returns
// the best MP4 link.
func (e *Eporner) sources(ctx context.Context, base, vid, hash string) (string, error) {
	short := epornerPlayerHash(hash)
	if short == "" {
		return "", fmt.Errorf("eporner: the player hash is not the expected %d characters", epornerHashLen)
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
	if err := e.client.GetJSON(ctx, endpoint, headers, &payload); err != nil {
		return "", fmt.Errorf("eporner: player sources: %w", err)
	}
	if !payload.Available {
		if msg := strings.TrimSpace(payload.Message); msg != "" {
			return "", fmt.Errorf("eporner: %s", msg)
		}
		return "", fmt.Errorf("eporner: the host reports this video as unavailable")
	}
	best, ok := bestCandidate(epornerCandidates(payload.Sources.MP4))
	if !ok {
		return "", fmt.Errorf("eporner: no MP4 rendition is offered")
	}
	return best.URL, nil
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

// epornerVideo is one entry in a profile listing.
type epornerVideo struct {
	Page  string
	Title string
}

// profile walks a profile tab and queues every video it lists.
func (e *Eporner) profile(ctx context.Context, u *url.URL, user, tab string) (*Result, error) {
	videos, declared, err := e.profileVideos(ctx, u, user, tab)
	if err != nil {
		return nil, err
	}
	if len(videos) == 0 {
		return nil, fmt.Errorf("eporner: %s lists no videos under %s", user, tab)
	}
	files := make([]File, 0, len(videos))
	for _, v := range videos {
		files = append(files, e.deferredFile(v))
	}
	return &Result{Title: epornerProfileTitle(user, tab, len(videos), declared), Files: files}, nil
}

// deferredFile queues a listed video without resolving it. The listing gives
// the page and a title to show while it waits; the signed link is minted
// when the item's turn actually comes.
func (e *Eporner) deferredFile(v epornerVideo) File {
	page := v.Page
	fallback := v.Title
	return File{
		Name: fallback + epornerExt,
		Size: -1,
		Resolve: func(ctx context.Context) (*Target, error) {
			title, link, err := e.rendition(ctx, page)
			if err != nil {
				return nil, err
			}
			return &Target{
				URL:  link,
				Name: util.FirstNonEmpty(title, fallback) + epornerExt,
				Size: -1,
			}, nil
		},
	}
}

// profileVideos pages through a profile tab, collecting what it lists and
// the total the tab claims to hold.
func (e *Eporner) profileVideos(ctx context.Context, u *url.URL, user, tab string) ([]epornerVideo, int, error) {
	var videos []epornerVideo
	declared := 0
	seen := make(map[string]bool)

	for page := 1; page <= epornerMaxProfilePages && len(videos) < epornerMaxVideos; page++ {
		link := epornerProfilePage(u, user, tab, page)
		doc, err := e.client.GetString(ctx, link, httpx.Referer(epornerRoot+"/"))
		if err != nil {
			if page == 1 {
				return nil, 0, fmt.Errorf("eporner: fetch profile: %w", err)
			}
			// Asking past the last page is how the walk finds its end: the
			// listing answers 404 rather than an empty page. A short walk
			// is caught by the declared count, not by trusting this.
			break
		}
		root, err := parseHTML(doc)
		if err != nil {
			return nil, 0, fmt.Errorf("eporner: %w", err)
		}
		if page == 1 {
			declared = epornerDeclaredCount(root, tab)
		}
		found := epornerListing(root, link, seen)
		videos = append(videos, found...)
		if len(found) == 0 {
			break
		}
	}
	return videos, declared, nil
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
func epornerListing(root *html.Node, pageURL string, seen map[string]bool) []epornerVideo {
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

	found := make([]epornerVideo, 0, len(order))
	for _, id := range order {
		seen[id] = true
		found = append(found, epornerVideo{
			Page:  pages[id],
			Title: util.FirstNonEmpty(titles[id], util.NameFromURL(pages[id]), id),
		})
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

// epornerProfileTitle names the job, and says so when the walk came back
// short of what the tab claims to hold. A listing that stops early
// otherwise looks exactly like a complete one.
func epornerProfileTitle(user, tab string, got, declared int) string {
	title := user + " (" + strings.ReplaceAll(tab, "-", " ") + ")"
	if declared > 0 && got < declared {
		title += fmt.Sprintf(" (partial — %d of %d)", got, declared)
	}
	return title
}
