// SPDX-License-Identifier: MIT

package extractor

import (
	"cmp"
	"context"
	"fmt"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
	"github.com/JohanLindvall/HeapLeach/internal/util"
	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// Kernel Video Sharing (KVS) is an off-the-shelf tube-site platform. Its
// player is configured by a `flashvars` object, and on most installs the
// media URL is scrambled: the first path segment after /get_file/<n>/ has
// its leading 32 characters permuted, with the permutation derived from the
// page's own `license_code`. Undoing it is pure arithmetic — no request to
// any extra endpoint.

// kvsHashLength is how many leading characters of the path segment are
// permuted.
const kvsHashLength = 32

// kvsScrambledPrefix marks a URL that needs unscrambling.
const kvsScrambledPrefix = "function/0/"

// flashvarsStart finds the player configuration's opening brace.
var flashvarsStart = regexp.MustCompile(`flashvars\s*=\s*\{`)

// flashvarsBlock returns the body of the player configuration object.
//
// The object is scanned rather than matched against a closing pattern,
// because installs disagree about where the brace goes: some put it on a
// line of its own, others pack the whole object onto one line and close it
// with "};" immediately behind the last value. Scanning also means a brace
// inside a quoted value cannot end the object early, which is the way any
// terminator pattern eventually gets it wrong.
func flashvarsBlock(doc string) (string, bool) {
	loc := flashvarsStart.FindStringIndex(doc)
	if loc == nil {
		return "", false
	}
	// The match ends on the opening brace; the scan starts there and the
	// body handed back is what sits between the braces.
	end, ok := scanObject(doc, loc[1]-1, '\'')
	if !ok {
		return "", false
	}
	return doc[loc[1] : end-1], true
}

// flashvarEntry matches one `key: 'value'` pair, tolerating escaped quotes.
var flashvarEntry = regexp.MustCompile(`(\w+)\s*:\s*'((?:[^'\\]|\\.)*)'`)

// parseFlashvars pulls the player configuration out of a KVS page.
func parseFlashvars(doc string) (map[string]string, error) {
	block, ok := flashvarsBlock(doc)
	if !ok {
		return nil, fmt.Errorf("no player configuration found")
	}
	out := make(map[string]string)
	for _, m := range flashvarEntry.FindAllStringSubmatch(block, -1) {
		out[m[1]] = unescapeJSString(m[2])
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("player configuration is empty")
	}
	return out, nil
}

// unescapeJSString undoes the backslash escapes of a single-quoted JS string.
func unescapeJSString(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' || i+1 >= len(s) {
			b.WriteByte(s[i])
			continue
		}
		i++
		switch s[i] {
		case 'n':
			b.WriteByte('\n')
		case 't':
			b.WriteByte('\t')
		case 'r':
			b.WriteByte('\r')
		default:
			// \' \" \\ \/ and anything else: keep the literal character.
			b.WriteByte(s[i])
		}
	}
	return b.String()
}

// kvsRealURL undoes the permutation applied to a scrambled media URL. A URL
// without the marker prefix is already direct and is returned unchanged.
func kvsRealURL(videoURL, licenseCode string) (string, error) {
	if !strings.HasPrefix(videoURL, kvsScrambledPrefix) {
		return videoURL, nil
	}
	raw := strings.TrimPrefix(videoURL, kvsScrambledPrefix)

	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("bad media url: %w", err)
	}

	token, err := kvsLicenseToken(licenseCode)
	if err != nil {
		return "", err
	}
	if len(token) < kvsHashLength {
		return "", fmt.Errorf("license token is %d values, need %d", len(token), kvsHashLength)
	}

	parts := strings.Split(u.Path, "/")
	if len(parts) < 4 {
		return "", fmt.Errorf("unexpected media path %q", u.Path)
	}
	segment := parts[3]
	if len(segment) < kvsHashLength {
		return "", fmt.Errorf("media path segment is too short to unscramble")
	}

	// Replay the player's swap sequence: walking backwards, each position is
	// exchanged with one derived from the running sum of the token.
	hash := segment[:kvsHashLength]
	order := make([]int, kvsHashLength)
	for i := range order {
		order[i] = i
	}
	accum := 0
	for src := kvsHashLength - 1; src >= 0; src-- {
		accum += token[src]
		dst := (src + accum) % kvsHashLength
		order[src], order[dst] = order[dst], order[src]
	}

	var unscrambled strings.Builder
	unscrambled.Grow(len(segment))
	for _, index := range order {
		unscrambled.WriteByte(hash[index])
	}
	unscrambled.WriteString(segment[kvsHashLength:])

	parts[3] = unscrambled.String()
	u.Path = strings.Join(parts, "/")
	return u.String(), nil
}

// kvsLicenseToken expands the page's license code into the digit sequence
// that drives the permutation.
func kvsLicenseToken(code string) ([]int, error) {
	code = strings.ReplaceAll(code, "$", "")
	if code == "" {
		return nil, fmt.Errorf("empty license code")
	}
	digits := make([]int, 0, len(code))
	for _, r := range code {
		if r < '0' || r > '9' {
			return nil, fmt.Errorf("license code contains %q", r)
		}
		digits = append(digits, int(r-'0'))
	}

	// Zeros are mapped to ones so both halves stay parseable numbers.
	modified := strings.ReplaceAll(code, "0", "1")
	center := len(modified) / 2
	front, err := strconv.ParseInt(modified[:center+1], 10, 64)
	if err != nil {
		return nil, fmt.Errorf("license code front half: %w", err)
	}
	back, err := strconv.ParseInt(modified[center:], 10, 64)
	if err != nil {
		return nil, fmt.Errorf("license code back half: %w", err)
	}

	seed := strconv.FormatInt(4*abs64(front-back), 10)
	if len(seed) > center+1 {
		seed = seed[:center+1]
	}

	token := make([]int, 0, len(seed)*4)
	for i := 0; i < len(seed); i++ {
		current := int(seed[i] - '0')
		for offset := range 4 {
			if i+offset >= len(digits) {
				return nil, fmt.Errorf("license code is too short for its seed")
			}
			token = append(token, (digits[i+offset]+current)%10)
		}
	}
	return token, nil
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

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
	about
	client *httpx.Client
}

// kvsLabel names every install: the software, since the extractor is not
// any one site's.
const kvsLabel = "kvs"

// NewKVS builds the Kernel Video Sharing extractor.
func NewKVS(client *httpx.Client) *KVS {
	return &KVS{client: client, about: about{name: kvsLabel, shape: kvsShape,
		recognises: "Kernel Video Sharing pages — videos, member profiles, searches and listings — carrying " +
			"the licensed player's configuration or script, or its `list_videos` blocks (asynchronous " +
			"controls, or a container and its `_items` list), and video pages whose player is gone by the " +
			"platform's generator, a block's `_items` list or its screenshot store"}}
}

// kvsShape is the URL forms worth a probe: a video page under a /videos/
// or /video/ segment, and a member profile. Listings and searches are
// recognised once fetched, but their paths are every site's.
func kvsShape(u *url.URL) bool {
	if kvsVideoPath(u) {
		return true
	}
	_, member := kvsMemberPath(u)
	return member
}

// Recognise takes any page of an install: the structure the extraction
// reads is the evidence, whatever the path, since installs rename their
// sections. The front page is taken too, so that Extract can refuse it with
// the install's own words rather than the fallback walking whatever the
// site promotes there.
//
// A video page whose player is gone — private, removed, or answered with
// 410 — keeps the rest of the platform's page, but not the parts the
// fingerprint reads. It is still the install's, and its error is the one
// worth reporting: left to the fallback, the related-videos block beside
// the missing player is what would be downloaded in its place. So a video
// URL also counts the platform's weaker signs: its own generator, the
// related block's items list named the way the platform names it, or its
// screenshot store.
func (k *KVS) Recognise(_ context.Context, p *Page) bool {
	if p.HasAny(kvsMarkers...) {
		if root := p.HTML(); root != nil && kvsFingerprint(root, p.Body) {
			return true
		}
	}
	return kvsVideoPath(p.Final) && kvsVideoPageSigns(p)
}

// kvsMarkers are the literals every part of the fingerprint contains, one
// of which the body must carry before the document is walked for it.
var kvsMarkers = []string{"license_code", "kt_player", "list_videos"}

// kvsItemsList is the id the platform gives a block's items list,
// list_videos_<block>_items.
var kvsItemsList = regexp.MustCompile(`^list_videos_[a-z0-9_]+_items$`)

// kvsVideoPageSigns reports what is left of the platform on a video page
// without its player: the software's own generator, a block's items list,
// or a link into its screenshot store. None of these alone is the
// fingerprint, which must also hold off a video path.
func kvsVideoPageSigns(p *Page) bool {
	if g := p.Generator(); g == "kvs" || strings.HasPrefix(g, "kernel video sharing") {
		return true
	}
	if strings.Contains(p.Body, "/contents/videos_screenshots/") {
		return true
	}
	return strings.Contains(p.Body, "list_videos_") &&
		p.hasElement(func(n *html.Node) bool { return kvsItemsList.MatchString(attr(n, "id")) })
}

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
		if p := probed(ctx, u); p != nil {
			if doc, ok := p.Document(); ok {
				l.first = &document{body: doc, final: cloneURL(p.Final), probe: p}
			}
		}
	}
	return kvsListingResult(ctx, k.client, l)
}

// kvsName names an install in its errors: its host's first label, as its
// jobs are labelled (KVS.installName), or the software where there is no
// host to name.
func kvsName(u *url.URL) string {
	if u != nil {
		if label := hostLabel(u.Hostname()); label != "" {
			return label
		}
	}
	return kvsLabel
}

// kvsNameOf is kvsName of a URL kept as a string.
func kvsNameOf(raw string) string {
	u, _ := url.Parse(raw)
	return kvsName(u)
}

// kvsExtract fetches a video page and resolves it. Everything needed is
// inline in the page, so one request resolves a video.
func kvsExtract(ctx context.Context, client *httpx.Client, u *url.URL) (*Result, error) {
	doc, _, err := fetchDocument(ctx, client, u, httpx.Referer(util.Origin(u)+"/"))
	if err != nil {
		return nil, fmt.Errorf("%s: fetch %s: %w", kvsName(u), u.Redacted(), err)
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
			"or this host may not run the player this handles)", kvsName(u), err)
	}

	renditions, err := kvsRenditions(vars)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", kvsName(u), err)
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
				"which this host is not expected to and so is not handled", kvsName(u))
		}
		return nil, fmt.Errorf("%s: no media URL in the player configuration", kvsName(u))
	}

	title := cmp.Or(
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
	for i := 0; i <= config.MaxKVSAlternates; i++ {
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
	return linkExtAsIs(direct, ".mp4")
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
