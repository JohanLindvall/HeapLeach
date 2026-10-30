// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"errors"
	"log/slog"
	"net/url"
	"strings"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/httpx"
	"github.com/JohanLindvall/HeapLeach/internal/util"
)

// doodGetPage fetches one stage of the handshake, logging what came back.
func doodGetPage(ctx context.Context, client *httpx.Client, raw, referer, stage string, attempt int) (string, *url.URL, error) {
	page := referer
	if stage == "player" {
		page = raw
	}
	u, _ := url.Parse(page)
	log := extractionLogger(ctx).With("player", u.Redacted(), "stage", stage, "attempt", attempt,
		"route", util.FirstNonEmpty(httpx.RouteID(ctx), httpx.DirectRoute))
	start := time.Now()
	log.DebugContext(ctx, "doodstream: request started")
	doc, final, err := client.GetPage(httpx.WithLogger(ctx, log), raw, httpx.Referer(referer), func(doc string) bool {
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
