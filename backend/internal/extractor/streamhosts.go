// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
	"github.com/JohanLindvall/HeapLeach/internal/util"
	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// Video hosts that embed rather than list: the page is a player, and the
// media link is assembled by the script running in it.
//
// All three here mint a link that expires within minutes and is tied to the
// visit that produced it, so all three resolve at download time rather than
// at extraction time — a link signed while an item waited its turn in a
// queue would be spent before the item started.
//
// They are also the most fragile extractors in the program. Each depends on
// the exact shape of a script the host can change at any time, which is why
// each reads the page for the numbers it needs — the substring offset, the
// packer's parameters — instead of hard-coding what those numbers were on
// the day it was written.

// ------------------------------------------------------------- streamtape

// Streamtape assembles its link from two halves the page never joins in the
// markup: an incomplete one in a hidden element, and the remainder in a
// script that appends it minus a few leading characters.
type Streamtape struct {
	client *httpx.Client
}

// streamtapeShape is a watch page or a player, /v/<id> or /e/<id>.
var streamtapeShape = pathShape(`^/[ve]/[0-9A-Za-z]{6,}(?:/[^/]*)?/?$`)

// streamtapeLink matches the assignment that joins the two halves, taking
// the substring offset from the page rather than assuming it.
var streamtapeLink = regexp.MustCompile(
	`(?s)ById\(\s*['"](?:no)?robotlink['"]\s*\)\.innerHTML\s*=\s*['"]([^'"]*)['"]\s*\+\s*` +
		`\(?\s*['"]([^'"]*)['"]\s*\)?\s*\.\s*substring\(\s*(\d+)\s*\)`)

// NewStreamtape builds the streamtape extractor.
func NewStreamtape(client *httpx.Client) *Streamtape {
	return &Streamtape{client: client}
}

func (s *Streamtape) Name() string { return "streamtape" }

// Recognises is the inventory's description.
func (s *Streamtape) Recognises() string {
	return "videos (`/v/<id>`, `/e/<id>`) whose page assembles the Streamtape `robotlink` media link"
}

// Shape reports a watch page or a player.
func (s *Streamtape) Shape(u *url.URL) bool { return streamtapeShape(u) }

// Recognise takes a page that assembles the link Extract assembles.
func (s *Streamtape) Recognise(_ context.Context, p *Page) bool {
	return streamtapeShape(p.Final) && streamtapeLink.MatchString(p.Body)
}

// Extract resolves an embed or watch page.
func (s *Streamtape) Extract(ctx context.Context, u *url.URL, _ Options) (*Result, error) {
	page := u.String()
	return refetchedVideo(ctx, ".mp4", httpx.Referer(page), func(ctx context.Context) (string, string, error) {
		link, title, err := s.media(ctx, page)
		return title, link, err
	})
}

func (s *Streamtape) media(ctx context.Context, page string) (link, title string, err error) {
	doc, err := fetchPage(ctx, s.client, page, httpx.Referer(streamtapeReferer(page)))
	if err != nil {
		return "", "", fmt.Errorf("streamtape: fetch %s: %w", page, err)
	}
	if link, err = streamtapeAssemble(doc, page); err != nil {
		return "", "", err
	}
	return link, util.FirstNonEmpty(pageTitle(doc), "streamtape"), nil
}

// streamtapeAssemble joins the two halves of the link the page carries.
//
// Kept apart from the fetch because this is the part that breaks: the host
// changes how much of the second half is dead weight, so the offset is read
// from the page rather than assumed, and this is what a fixture can check.
func streamtapeAssemble(doc, page string) (string, error) {
	m := streamtapeLink.FindStringSubmatch(doc)
	if m == nil {
		return "", fmt.Errorf("streamtape: no media link on %s "+
			"(the video may have been removed, or the page layout changed)", page)
	}
	head, tail := m[1], m[2]
	offset, err := strconv.Atoi(m[3])
	if err != nil || offset > len(tail) {
		return "", fmt.Errorf("streamtape: media link on %s is assembled in a way this does not follow", page)
	}

	link := head + tail[offset:]
	switch {
	case strings.HasPrefix(link, "//"):
		link = "https:" + link
	case strings.HasPrefix(link, "/"):
		link = streamtapeReferer(page) + link
	}
	// The player asks for the stream itself; without this the host answers
	// with a redirect back to the page.
	if !strings.Contains(link, "stream=") {
		sep := "&"
		if !strings.Contains(link, "?") {
			sep = "?"
		}
		link += sep + "stream=1"
	}
	return link, nil
}

// streamtapeReferer is the site root of whichever domain the link used, as
// these hosts reject a referer from a sibling domain.
func streamtapeReferer(page string) string {
	if u, err := url.Parse(page); err == nil && u.Host != "" {
		return util.Origin(u)
	}
	return "https://streamtape.com"
}

// --------------------------------------------------------------- doodstream

// DoodStream hands out its link in two steps: the page names a token
// endpoint, and that endpoint returns a prefix which the player completes
// with a random tail, the token and a timestamp.
type DoodStream struct {
	client *httpx.Client
}

var (
	// doodShape is a player or its landing page, /e/<code> or /d/<code>.
	doodShape = pathShape(`^/[ed]/[0-9a-z]{6,}/?$`)
	// doodLanding is the landing page, which frames the player.
	doodLanding = pathShape(`^/d/[0-9a-z]{6,}/?$`)
	// doodPlayerFrame is the player a landing page frames.
	doodPlayerFrame = pathShape(`^/e/[0-9a-z]{6,}`)
)

// doodPassPath finds the token endpoint the page points at.
var doodPassPath = regexp.MustCompile(`(/pass_md5/[^'"\s]+)`)

var errDoodReload = errors.New("doodstream: player requested RELOAD")

// doodTailLength is how many random characters the player appends. The value
// is the player's, not ours: the host checks the total length.
const doodTailLength = 10

// NewDoodStream builds the doodstream extractor.
func NewDoodStream(client *httpx.Client) *DoodStream {
	return &DoodStream{client: client}
}

func (d *DoodStream) Name() string { return "doodstream" }

// Recognises is the inventory's description.
func (d *DoodStream) Recognises() string {
	return "players (`/e/<code>`) naming the DoodStream `/pass_md5/` token endpoint or showing its " +
		"player gate, and landing pages (`/d/<code>`) named DoodStream that frame such a player"
}

// Shape reports a player or a landing page.
func (d *DoodStream) Shape(u *url.URL) bool { return doodShape(u) }

// Recognise takes a player by the token endpoint Extract asks, or by the
// gate that stands in for it until a challenge is passed; and a landing
// page by the player it frames, under the site's name.
func (d *DoodStream) Recognise(_ context.Context, p *Page) bool {
	if !doodShape(p.Final) {
		return false
	}
	if doodPassPath.MatchString(p.Body) || doodChallengePage(p.Body) {
		return true
	}
	return doodLanding(p.Final) && p.Named("DoodStream") && p.frames(doodPlayerFrame)
}

func (d *DoodStream) ProxyFallback() bool { return true }

// Extract resolves a watch or embed page.
func (d *DoodStream) Extract(ctx context.Context, u *url.URL, _ Options) (*Result, error) {
	// The embed page is the one carrying the token endpoint; a /d/ link is
	// the same video behind a landing page.
	page := strings.Replace(u.String(), "/d/", "/e/", 1)
	target, title, err := d.media(ctx, page)
	if err != nil {
		return nil, err
	}
	return &Result{Title: title, Files: []File{{
		Name: target.Name, URL: target.URL, Size: -1, Headers: target.Headers,
		ProxyFallback: true,
		Resolve: func(ctx context.Context) (*Target, error) {
			fresh, _, err := d.media(ctx, page)
			return fresh, err
		},
	}}}, nil
}

func (d *DoodStream) media(ctx context.Context, page string) (*Target, string, error) {
	var title string
	attempt := 0
	target, err := withExtractRetries(ctx, func() (*Target, error) {
		attempt++
		note := func(stage string) {
			ResolveNote(ctx, fmt.Sprintf("Doodstream: %s (%d/%d)", stage, attempt, config.ExtractRetries))
		}
		base, err := url.Parse(page)
		if err != nil {
			return nil, fmt.Errorf("doodstream: %w", err)
		}
		note("requesting player")
		doc, final, err := d.getPage(ctx, page, util.Origin(base)+"/", "player", attempt)
		if err != nil {
			return nil, fmt.Errorf("doodstream: fetch %s: %w", page, err)
		}
		page = final.String()

		m := doodPassPath.FindStringSubmatch(doc)
		if m == nil && doodRefused(doc) {
			return nil, &RefusedError{Err: fmt.Errorf("doodstream: %s refused the player to this address: %q",
				final.Hostname(), util.Truncate(strings.TrimSpace(doc), 120))}
		}
		if m == nil && doodGone(doc) {
			return nil, fmt.Errorf("doodstream: video not found on %s: the host has removed it", page)
		}
		if m == nil {
			return nil, fmt.Errorf("doodstream: no media token on %s "+
				"(the video may have been removed, or the page layout changed)", page)
		}
		pass := util.Origin(final) + m[1]

		note("requesting media token")
		prefix, _, err := d.getPage(ctx, pass, page, "token", attempt)
		if err != nil {
			return nil, fmt.Errorf("doodstream: fetch media token: %w", err)
		}
		prefix = strings.TrimSpace(prefix)
		if prefix == "RELOAD" {
			// The player calls location.reload(): revisit the final page,
			// retaining this route's cookies and reading its new token path.
			return nil, errDoodReload
		}
		if !strings.HasPrefix(prefix, "http") {
			return nil, fmt.Errorf("doodstream: media token endpoint returned %q", util.Truncate(prefix, 80))
		}

		token := m[1][strings.LastIndex(m[1], "/")+1:]
		link := prefix + randomToken(doodTailLength) +
			"?token=" + url.QueryEscape(token) +
			"&expiry=" + strconv.FormatInt(time.Now().UnixMilli(), 10)
		title = util.FirstNonEmpty(pageTitle(doc), "doodstream")
		return &Target{URL: link, Name: title + ".mp4", Size: -1, Headers: httpx.Referer(page)}, nil
	}, func(_ *Target, err error) bool { return errors.Is(err, errDoodReload) })
	if errors.Is(err, errDoodReload) {
		err = &TransientError{Err: fmt.Errorf("%w after %d attempts", err, config.ExtractRetries)}
	}
	return target, title, err
}

// doodRefused recognises the host turning an address away, which is not the
// video having gone. Some addresses are answered, with a 200, by one bare
// sentence where the player should be ("…has been removed or is temporarily
// unavailable"), for a video every other address is shown and for one that
// does not exist alike; a video that really has gone is shown the host's own
// HTML page saying so. So what is read is the missing markup, not the
// sentence: the player and its not-found page are both documents, and this
// is not one. Only the player is asked: the token endpoint answers in plain
// text by design.
func doodRefused(doc string) bool {
	return !strings.Contains(doc, "<")
}

// doodGone recognises the host's own page for a video that has gone, which
// is a document titled "Video not found", where a refused address gets one
// bare sentence (doodRefused). Read as a page without a token, the video's
// absence was reported as possibly a changed layout, and an hour of one
// site's posts linking forty-four such players read as forty-four parser
// faults.
func doodGone(doc string) bool {
	return strings.Contains(strings.ToLower(pageTitle(doc)), "video not found")
}

// Recognise both the host's Turnstile player gate and an interstitial whose
// cf-mitigated header is absent. A background script or unrelated widget is
// not a gate. Check the interstitial first: it can echo the requested token
// endpoint in its challenge options without supplying a playable video.
func doodChallengePage(doc string) bool {
	root, err := parseHTML(doc)
	if err != nil {
		return false
	}
	if findFirst(root, func(n *html.Node) bool {
		if !isElem(n, atom.Script) {
			return false
		}
		script := textOf(n)
		return strings.Contains(script, "_cf_chl_opt") &&
			strings.Contains(script, "/cdn-cgi/challenge-platform/") &&
			strings.Contains(script, "/orchestrate/chl_page/")
	}) != nil {
		return true
	}
	if doodPassPath.MatchString(doc) {
		return false
	}
	gate := findFirst(root, func(n *html.Node) bool { return hasClass(n, "captcha-player") })
	return gate != nil && findFirst(gate, func(n *html.Node) bool {
		return attr(n, "id") == "turnstile-container"
	}) != nil
}

// ----------------------------------------------------------------- mixdrop

// MixDrop keeps its link inside a packed script, which unpacks to an
// assignment naming the delivery host.
type MixDrop struct {
	client *httpx.Client
}

var (
	// mixdropShape is a player or its landing page, /e/<code> or /f/<code>.
	mixdropShape = pathShape(`^/[ef]/[0-9a-z]{6,}(?:/[^/]*)?/?$`)
	// mixdropLanding is the landing page.
	mixdropLanding = pathShape(`^/f/[0-9a-z]{6,}(?:/[^/]*)?/?$`)
)

// mixdropURL matches the assignment the unpacked script leaves behind.
var mixdropURL = regexp.MustCompile(`MDCore\.(?:wurl|vsrc)\s*=\s*["']([^"']+)["']`)

// NewMixDrop builds the mixdrop extractor.
func NewMixDrop(client *httpx.Client) *MixDrop {
	return &MixDrop{client: client}
}

func (m *MixDrop) Name() string { return "mixdrop" }

// mixdropCore is the player object whose settings the page assigns.
const mixdropCore = "MDCore."

// Recognises is the inventory's description.
func (m *MixDrop) Recognises() string {
	return "players (`/e/<code>`) configuring the MixDrop player object (`MDCore`), and landing pages " +
		"(`/f/<code>`) named MixDrop that frame or configure one"
}

// Shape reports a player or a landing page.
func (m *MixDrop) Shape(u *url.URL) bool { return mixdropShape(u) }

// Recognise takes a page that configures the player object whose media
// link Extract reads. A landing page frames the player rather than being
// it, so it is taken under the site's name when it frames a player of its
// own form.
func (m *MixDrop) Recognise(_ context.Context, p *Page) bool {
	if !mixdropShape(p.Final) {
		return false
	}
	if p.Has(mixdropCore) {
		return true
	}
	return mixdropLanding(p.Final) && p.Named("MixDrop") && p.frames(func(src *url.URL) bool {
		return strings.HasPrefix(src.EscapedPath(), "/e/")
	})
}

// Extract resolves a file or embed page.
func (m *MixDrop) Extract(ctx context.Context, u *url.URL, _ Options) (*Result, error) {
	// The embed page carries the player; /f/ is a landing page around it.
	page := strings.Replace(u.String(), "/f/", "/e/", 1)
	return refetchedVideo(ctx, ".mp4", httpx.Referer(page), func(ctx context.Context) (string, string, error) {
		link, title, err := m.media(ctx, page)
		return title, link, err
	})
}

func (m *MixDrop) media(ctx context.Context, page string) (link, title string, err error) {
	base, err := url.Parse(page)
	if err != nil {
		return "", "", fmt.Errorf("mixdrop: %w", err)
	}
	doc, err := fetchPage(ctx, m.client, page, httpx.Referer(util.Origin(base)+"/"))
	if err != nil {
		return "", "", fmt.Errorf("mixdrop: fetch %s: %w", page, err)
	}

	// The assignment is usually packed; an unpacked page is read directly.
	source := doc
	if unpacked, ok := unpackJS(doc); ok {
		source = unpacked + doc
	}
	found := mixdropURL.FindStringSubmatch(source)
	if found == nil {
		return "", "", fmt.Errorf("mixdrop: no media link on %s "+
			"(the video may have been removed)", page)
	}

	link = found[1]
	if strings.HasPrefix(link, "//") {
		link = "https:" + link
	}
	return link, util.FirstNonEmpty(pageTitle(doc), util.NameFromURL(page), "mixdrop"), nil
}

// ------------------------------------------------------------------ shared

// randomToken returns n alphanumeric characters, for hosts whose player
// completes a link with a nonce the server only checks the length of.
func randomToken(n int) string {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, n)
	for i := range b {
		b[i] = alphabet[rand.IntN(len(alphabet))]
	}
	return string(b)
}

// packedScript matches a script compressed by the p,a,c,k,e,d packer, which
// is what these players ship instead of readable source.
var packedScript = regexp.MustCompile(
	`(?s)\}\s*\(\s*'(.*?)'\s*,\s*(\d+)\s*,\s*(\d+)\s*,\s*'(.*?)'\s*\.\s*split\(\s*'\|'\s*\)`)

// unpackJS reverses that packer.
//
// The scheme is a dictionary substitution: every identifier in the payload
// was replaced by its index in a word list, written in base a. Undoing it is
// walking the payload word by word and putting the words back — no
// evaluation, and nothing host-specific, which is why it sits here rather
// than in one host's file.
func unpackJS(script string) (string, bool) {
	m := packedScript.FindStringSubmatch(script)
	if m == nil {
		return "", false
	}
	payload := unescapeJSString(m[1])
	base, err := strconv.Atoi(m[2])
	if err != nil || base < 2 || base > 62 {
		return "", false
	}
	count, err := strconv.Atoi(m[3])
	if err != nil || count < 0 {
		return "", false
	}
	words := strings.Split(unescapeJSString(m[4]), "|")
	if len(words) < count {
		count = len(words)
	}

	var out strings.Builder
	out.Grow(len(payload))
	for i := 0; i < len(payload); {
		if !isWordByte(payload[i]) {
			out.WriteByte(payload[i])
			i++
			continue
		}
		start := i
		for i < len(payload) && isWordByte(payload[i]) {
			i++
		}
		word := payload[start:i]
		if index, ok := packerIndex(word, base); ok && index < count && words[index] != "" {
			out.WriteString(words[index])
			continue
		}
		out.WriteString(word)
	}
	return out.String(), true
}

// isWordByte reports whether a byte can be part of a packed token.
func isWordByte(c byte) bool {
	return c == '_' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

// packerIndexMax bounds the value a token may decode to. A packed index is
// small — it names a position in the word list — while the payload also
// carries ordinary words whose characters all happen to be digits of the
// base: a long lowercase run, a hex token inside a URL. Read without a bound
// those overflow the accumulator, and a wrapped-negative "index" would pass
// the caller's range check and panic the word lookup.
const packerIndexMax = 1 << 30

// packerIndex reads a token as a number in the packer's alphabet: digits,
// then lowercase, then uppercase — the last of which only appears once the
// word list outgrows base 36.
func packerIndex(word string, base int) (int, bool) {
	value := 0
	for i := 0; i < len(word); i++ {
		var digit int
		switch c := word[i]; {
		case c >= '0' && c <= '9':
			digit = int(c - '0')
		case c >= 'a' && c <= 'z':
			digit = int(c-'a') + 10
		case c >= 'A' && c <= 'Z':
			digit = int(c-'A') + 36
		default:
			return 0, false
		}
		if digit >= base || value > (packerIndexMax-digit)/base {
			return 0, false
		}
		value = value*base + digit
	}
	return value, len(word) > 0
}
