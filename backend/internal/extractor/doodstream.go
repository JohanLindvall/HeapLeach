// SPDX-License-Identifier: MIT

package extractor

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
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
// with a random tail, the token and a timestamp. The handshake is the row's
// read, retries and all; a challenge can stand in for its player, and a
// refused address is retried through a proxy (proxyFallback). The media
// referer is the player page as finally served.

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

var errDoodReload = errors.New("player requested RELOAD")

// doodTailLength is how many random characters the player appends. The value
// is the player's, not ours: the host checks the total length.
const doodTailLength = 10

// A refused address gets this bare response on both landing and player
// pages. Match it exactly during identification: arbitrary text is not proof
// of this player, and a real removed video has its own HTML page.
const doodRefusalText = "The resource you are looking for has been removed or is temporarily unavailable."

var doodPlayer = playerSite{
	name:  "doodstream",
	shape: doodShape,
	// A player by the token endpoint read asks, or by the gate that stands
	// in for it until a challenge is passed; and a landing page by the
	// player it frames, under the site's name.
	recognise: func(p *Page) bool {
		if !doodShape(p.Final) {
			return false
		}
		if doodPassPath.MatchString(p.Body) || strings.TrimSpace(p.Body) == doodRefusalText || doodChallengePage(p.Body) {
			return true
		}
		// The landing page also identifies its site with the nonstandard
		// og:sitename key; that remains usable when its title has no brand.
		named := p.Named("DoodStream", "DoodStream.com") || strings.EqualFold(p.Meta("og:sitename"), "DoodStream.com")
		return doodLanding(p.Final) && named && p.frames(doodPlayerFrame)
	},
	recognises: "players (`/e/<code>`) naming the DoodStream `/pass_md5/` token endpoint, " +
		"landing pages (`/d/<code>`) naming DoodStream in their title or site metadata and framing its player, " +
		"or either form showing its player gate or exact bare address refusal",
	// The embed page is the one carrying the token endpoint; a /d/ link is
	// the same video behind a landing page.
	page:          func(u *url.URL) (string, error) { return strings.Replace(u.String(), "/d/", "/e/", 1), nil },
	read:          doodRead,
	signed:        true,
	proxyFallback: true,
}

// NewDoodStream builds the doodstream extractor: its row in the player
// table.
func NewDoodStream(client *httpx.Client) *Player { return NewPlayer(client, &doodPlayer) }

// doodRead runs the handshake, again from the player page whenever the
// token endpoint asks for a reload.
func doodRead(ctx context.Context, client *httpx.Client, page string) (*playerVideo, error) {
	attempt := 0
	video, err := withExtractRetries(ctx, func() (*playerVideo, error) {
		attempt++
		note := func(stage string) {
			ResolveNote(ctx, fmt.Sprintf("Doodstream: %s (%d/%d)", stage, attempt, config.ExtractRetries))
		}
		base, err := url.Parse(page)
		if err != nil {
			return nil, err
		}
		note("requesting player")
		doc, final, err := doodGetPage(ctx, client, page, util.Origin(base)+"/", "player", attempt)
		if err != nil {
			return nil, fmt.Errorf("fetch %s: %w", page, err)
		}
		page = final.String()

		m := doodPassPath.FindStringSubmatch(doc)
		if m == nil && doodRefused(doc) {
			return nil, &RefusedError{Err: fmt.Errorf("%s refused the player to this address: %q",
				final.Hostname(), util.Truncate(strings.TrimSpace(doc), 120))}
		}
		if m == nil && doodGone(doc) {
			return nil, fmt.Errorf("video not found on %s: the host has removed it", page)
		}
		if m == nil {
			return nil, fmt.Errorf("no media token on %s "+
				"(the video may have been removed, or the page layout changed)", page)
		}
		pass := util.Origin(final) + m[1]

		note("requesting media token")
		prefix, _, err := doodGetPage(ctx, client, pass, page, "token", attempt)
		if err != nil {
			return nil, fmt.Errorf("fetch media token: %w", err)
		}
		prefix = strings.TrimSpace(prefix)
		if prefix == "RELOAD" {
			// The player calls location.reload(): revisit the final page,
			// retaining this route's cookies and reading its new token path.
			return nil, errDoodReload
		}
		if !strings.HasPrefix(prefix, "http") {
			return nil, fmt.Errorf("media token endpoint returned %q", util.Truncate(prefix, 80))
		}

		token := m[1][strings.LastIndex(m[1], "/")+1:]
		link := prefix + randomToken(doodTailLength) +
			"?token=" + url.QueryEscape(token) +
			"&expiry=" + strconv.FormatInt(time.Now().UnixMilli(), 10)
		return &playerVideo{title: pageTitle(doc), candidates: []mediaCandidate{{URL: link}}, referer: page}, nil
	}, func(_ *playerVideo, err error) bool { return errors.Is(err, errDoodReload) })
	if errors.Is(err, errDoodReload) {
		err = &TransientError{Err: fmt.Errorf("%w after %d attempts", err, config.ExtractRetries)}
	}
	return video, err
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

// doodGetPage fetches one stage of the handshake, logging what came back.
func doodGetPage(ctx context.Context, client *httpx.Client, raw, referer, stage string, attempt int) (string, *url.URL, error) {
	page := referer
	if stage == "player" {
		page = raw
	}
	u, _ := url.Parse(page)
	log := extractionLogger(ctx).With("player", u.Redacted(), "stage", stage, "attempt", attempt,
		"route", cmp.Or(httpx.RouteID(ctx), httpx.DirectRoute))
	start := time.Now()
	log.DebugContext(ctx, "doodstream: request started")
	doc, final, err := client.GetPage(httpx.WithResponseLog(ctx, log), raw, httpx.Referer(referer), func(doc string) bool {
		challenged := doodChallengePage(doc)
		if challenged {
			doodLogPage(ctx, log, doc, true)
		}
		return challenged
	})
	if err != nil {
		_, challenged := errors.AsType[*httpx.ChallengeError](err)
		log.DebugContext(ctx, "doodstream: request failed", "challenge", challenged, "error", err, "elapsed", time.Since(start))
		return doc, final, err
	}
	log = log.With("elapsed", time.Since(start), "redirected", final.String() != raw)
	if stage == "player" {
		log = log.With("final_page", final.Redacted())
	}
	doodLogPage(ctx, log, doc, false)
	return doc, final, nil
}

// Keep enough of an unexpected response's shape to distinguish a new gate
// from a missing player without logging its HTML, cookies or media tokens.
func doodLogPage(ctx context.Context, log *slog.Logger, doc string, challenged bool) {
	if !log.Enabled(ctx, slog.LevelDebug) {
		return
	}
	log.DebugContext(ctx, "doodstream: page inspected",
		"body_bytes", len(doc), "title", util.Truncate(pageTitle(doc), 160),
		"media_token", doodPassPath.MatchString(doc), "challenge", challenged,
		"captcha_player", strings.Contains(doc, "captcha-player"),
		"turnstile", strings.Contains(doc, "turnstile"),
		"cf_challenge_options", strings.Contains(doc, "_cf_chl_opt"),
		"cf_challenge_page", strings.Contains(doc, "/orchestrate/chl_page/"),
		"packed_script", strings.Contains(doc, "eval(function(p,a,c,k,e,d)"),
		"reload", strings.TrimSpace(doc) == "RELOAD")
}
