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

// DoodStream hands out its link in two steps: the page names a token
// endpoint, and that endpoint returns a prefix which the player completes
// with a random tail, the token and a timestamp.
//
// It stays apart from the player table (players.go): besides that handshake,
// a challenge can stand in for its player and a refused address is retried
// through a proxy, none of which the table's other sites need.
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
