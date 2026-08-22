package extractor

import (
	"context"
	"fmt"
	"math/rand/v2"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/httpx"
	"github.com/JohanLindvall/HeapLeach/internal/util"
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
type Streamtape struct{ client *httpx.Client }

var streamtapeHosts = []string{
	"streamtape.com", "streamtape.net", "streamtape.to", "streamtape.xyz",
	"streamtape.cc", "streamtape.site", "streamta.pe", "strcloud.link",
}

// streamtapeLink matches the assignment that joins the two halves, taking
// the substring offset from the page rather than assuming it.
var streamtapeLink = regexp.MustCompile(
	`(?s)ById\(\s*['"](?:no)?robotlink['"]\s*\)\.innerHTML\s*=\s*['"]([^'"]*)['"]\s*\+\s*` +
		`\(?\s*['"]([^'"]*)['"]\s*\)?\s*\.\s*substring\(\s*(\d+)\s*\)`)

// NewStreamtape builds the streamtape extractor.
func NewStreamtape(client *httpx.Client) *Streamtape { return &Streamtape{client: client} }

func (s *Streamtape) Name() string { return "streamtape" }

func (s *Streamtape) Match(u *url.URL) bool { return matchesHosts(u, streamtapeHosts) }

// Extract resolves an embed or watch page.
func (s *Streamtape) Extract(ctx context.Context, u *url.URL, _ Options) (*Result, error) {
	page := u.String()
	return refetchedVideo(ctx, ".mp4", httpx.Referer(page), func(ctx context.Context) (string, string, error) {
		link, title, err := s.media(ctx, page)
		return title, link, err
	})
}

func (s *Streamtape) media(ctx context.Context, page string) (link, title string, err error) {
	doc, err := s.client.GetString(ctx, page, httpx.Referer(streamtapeReferer(page)))
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
		link += "&stream=1"
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
type DoodStream struct{ client *httpx.Client }

var doodHosts = []string{
	"doodstream.com", "dood.to", "dood.so", "dood.watch", "dood.la", "dood.ws",
	"dood.sh", "dood.yt", "dood.re", "dood.li", "ds2play.com", "d0o0d.com",
	"do0od.com", "dooood.com", "vidply.com",
}

// doodPassPath finds the token endpoint the page points at.
var doodPassPath = regexp.MustCompile(`(/pass_md5/[^'"\s]+)`)

// doodTailLength is how many random characters the player appends. The value
// is the player's, not ours: the host checks the total length.
const doodTailLength = 10

// NewDoodStream builds the doodstream extractor.
func NewDoodStream(client *httpx.Client) *DoodStream { return &DoodStream{client: client} }

func (d *DoodStream) Name() string { return "doodstream" }

func (d *DoodStream) Match(u *url.URL) bool { return matchesHosts(u, doodHosts) }

// Extract resolves a watch or embed page.
func (d *DoodStream) Extract(ctx context.Context, u *url.URL, _ Options) (*Result, error) {
	// The embed page is the one carrying the token endpoint; a /d/ link is
	// the same video behind a landing page.
	page := strings.Replace(u.String(), "/d/", "/e/", 1)
	return refetchedVideo(ctx, ".mp4", httpx.Referer(page), func(ctx context.Context) (string, string, error) {
		link, title, err := d.media(ctx, page)
		return title, link, err
	})
}

func (d *DoodStream) media(ctx context.Context, page string) (link, title string, err error) {
	base, err := url.Parse(page)
	if err != nil {
		return "", "", fmt.Errorf("doodstream: %w", err)
	}
	doc, err := d.client.GetString(ctx, page, httpx.Referer(util.Origin(base)+"/"))
	if err != nil {
		return "", "", fmt.Errorf("doodstream: fetch %s: %w", page, err)
	}

	m := doodPassPath.FindStringSubmatch(doc)
	if m == nil {
		return "", "", fmt.Errorf("doodstream: no media token on %s "+
			"(the video may have been removed)", page)
	}
	pass := util.Origin(base) + m[1]

	prefix, err := d.client.GetString(ctx, pass, httpx.Referer(page))
	if err != nil {
		return "", "", fmt.Errorf("doodstream: fetch media token: %w", err)
	}
	prefix = strings.TrimSpace(prefix)
	if !strings.HasPrefix(prefix, "http") {
		return "", "", fmt.Errorf("doodstream: media token endpoint returned %q", util.Truncate(prefix, 80))
	}

	token := m[1][strings.LastIndex(m[1], "/")+1:]
	link = prefix + randomToken(doodTailLength) +
		"?token=" + url.QueryEscape(token) +
		"&expiry=" + strconv.FormatInt(time.Now().UnixMilli(), 10)
	return link, util.FirstNonEmpty(pageTitle(doc), "doodstream"), nil
}

// ----------------------------------------------------------------- mixdrop

// MixDrop keeps its link inside a packed script, which unpacks to an
// assignment naming the delivery host.
type MixDrop struct{ client *httpx.Client }

var mixdropHosts = []string{
	"mixdrop.co", "mixdrop.to", "mixdrop.ag", "mixdrop.bz", "mixdrop.ch",
	"mixdrop.club", "mixdrop.gl", "mixdrop.is", "mixdrop.my", "mixdrop.sx",
	"mixdrop.ps", "mixdrp.co", "mixdrp.to", "mdbekjwqa.pw",
}

// mixdropURL matches the assignment the unpacked script leaves behind.
var mixdropURL = regexp.MustCompile(`MDCore\.(?:wurl|vsrc)\s*=\s*["']([^"']+)["']`)

// NewMixDrop builds the mixdrop extractor.
func NewMixDrop(client *httpx.Client) *MixDrop { return &MixDrop{client: client} }

func (m *MixDrop) Name() string { return "mixdrop" }

func (m *MixDrop) Match(u *url.URL) bool { return matchesHosts(u, mixdropHosts) }

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
	doc, err := m.client.GetString(ctx, page, httpx.Referer(util.Origin(base)+"/"))
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

// matchesHosts reports whether a URL belongs to any of a host list.
func matchesHosts(u *url.URL, hosts []string) bool {
	for _, host := range hosts {
		if util.HostMatches(u.Host, host) {
			return true
		}
	}
	return false
}

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
		if digit >= base {
			return 0, false
		}
		value = value*base + digit
	}
	return value, len(word) > 0
}
