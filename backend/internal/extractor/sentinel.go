// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/JohanLindvall/HeapLeach/internal/captcha"
	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

type sentinelChallenge struct {
	P [][]json.RawMessage `json:"p"`
	N string              `json:"n"`
	M string              `json:"m"`
	I string              `json:"i"`
	V string              `json:"v"`
	B int                 `json:"b"`
}

var sentinelConfiguration = regexp.MustCompile(`(?s)var C=(.*?),rounds=`)

type sentinelReader func(context.Context, []byte, int, func(string)) ([]string, error)

type sentinelSolveError struct{ error }

func (e *sentinelSolveError) Unwrap() error { return e.error }

// sentinelPage keeps the image, proof, verification and re-read on one route
// and cookie jar. Only an exhausted solve escapes to the enclosing extraction;
// a locally handled challenge must not poison its challenge capture.
func sentinelPage(ctx context.Context, client *httpx.Client, raw string, headers httpx.Header, read sentinelReader) (doc string, final *url.URL, err error) {
	requestCtx := httpx.WithPageChallenge(httpx.WithoutChallengeCapture(ctx), "Sentinel", balbumsChallengePage)
	doc, final, err = client.GetPage(requestCtx, raw, headers, nil)
	gate, ok := errors.AsType[*httpx.ChallengeError](err)
	if !ok || gate.Provider != "Sentinel" {
		return doc, final, err
	}
	if err := ctx.Err(); err != nil {
		return "", nil, err
	}
	match := sentinelConfiguration.FindSubmatch(gate.Body)
	var challenge sentinelChallenge
	// A changed/unrecognized gate retains normal proxy recovery.
	if match == nil || json.Unmarshal(match[1], &challenge) != nil || challenge.N == "" || challenge.M == "" {
		return "", nil, err
	}
	defer func() {
		if err != nil {
			err = &sentinelSolveError{err}
		}
	}()
	solveCtx, cancel := context.WithTimeout(requestCtx, config.SentinelSolveTimeout)
	defer cancel()
	// The gate's response URL is where its relative verification endpoint lives.
	page, parseErr := url.Parse(gate.Err.URL)
	if parseErr != nil || page.Host == "" {
		return "", nil, errors.New("Sentinel: invalid checkpoint address")
	}
	fp, values := sentinelIdentity(httpx.RequestAgent(ctx, client))
	for round := 0; round < config.SentinelRounds; round++ {
		if solveCtx.Err() != nil {
			break
		}
		if challenge.V != "/.well-known/checkpoint" || challenge.N == "" {
			return "", nil, errors.New("Sentinel: unsupported verification endpoint or missing challenge")
		}
		answers, err := sentinelAnswers(challenge.P, values)
		if err != nil {
			return "", nil, err
		}
		var typed any
		if challenge.I != "" {
			if base64.StdEncoding.DecodedLen(len(challenge.I)) > config.Keep2ShareCaptchaBytes {
				return "", nil, errors.New("Sentinel: CAPTCHA image is too large")
			}
			rawImage, err := base64.StdEncoding.DecodeString(challenge.I)
			if err != nil {
				return "", nil, errors.New("Sentinel: invalid CAPTCHA image")
			}
			note := func(stage string) { ResolveNote(ctx, "Sentinel: "+stage) }
			readings, err := read(solveCtx, rawImage, config.SentinelCaptchaLength, note)
			if err != nil {
				return "", nil, fmt.Errorf("Sentinel: read CAPTCHA: %w", err)
			}
			if len(readings) == 0 {
				return "", nil, fmt.Errorf("Sentinel: %w", captcha.ErrUnreadable)
			}
			// Sentinel replaces a rejected image. Never spend alternatives against
			// the old nonce or use an old answer after the image has passed.
			typed = strings.ToUpper(readings[0])
		}
		ResolveNote(ctx, fmt.Sprintf("Sentinel: solving browser proof (%d/%d)", round+1, config.SentinelRounds))
		counter, err := sentinelProof(solveCtx, challenge.M, challenge.B)
		if err != nil {
			return "", nil, err
		}
		body, err := json.Marshal(map[string]any{"n": challenge.N, "c": counter, "a": answers, "fp": fp, "t": typed})
		if err != nil {
			return "", nil, err
		}
		endpoint := *page
		endpoint.Path = challenge.V
		endpoint.RawPath = ""
		endpoint.RawQuery = ""
		endpoint.Fragment = ""
		req, err := client.NewRequest(solveCtx, http.MethodPost, endpoint.String(), body)
		if err != nil {
			return "", nil, err
		}
		httpx.Navigate.Merge(httpx.Referer(page.String())).Merge(httpx.Header{httpx.HeaderContentType: httpx.ContentTypeJSON}).Apply(req)
		ResolveNote(ctx, "Sentinel: verifying browser check")
		resp, err := client.DoOnce(req)
		if err != nil {
			return "", nil, err
		}
		if resp.StatusCode == http.StatusNoContent {
			resp.Body.Close()
			// Exactly one re-read: a missing cookie or another gate is not a fresh
			// unbounded solving cycle. Return an actionable error instead of spinning.
			doc, final, err = client.GetPage(solveCtx, raw, headers, nil)
			if _, again := errors.AsType[*httpx.ChallengeError](err); again {
				return "", nil, errors.New("Sentinel: browser check passed but the search is still challenged")
			}
			return doc, final, err
		}
		reply, readErr := httpx.ReadBody(resp.Body)
		resp.Body.Close()
		if readErr != nil {
			return "", nil, fmt.Errorf("Sentinel: read verification: %w", readErr)
		}
		var next struct {
			Retry *sentinelChallenge `json:"retry"`
		}
		if resp.StatusCode != http.StatusOK || json.Unmarshal(reply, &next) != nil || next.Retry == nil {
			return "", nil, fmt.Errorf("Sentinel: browser check was refused (HTTP %d)", resp.StatusCode)
		}
		challenge = *next.Retry
	}
	if err := ctx.Err(); err != nil {
		return "", nil, err
	}
	if err := solveCtx.Err(); err != nil {
		return "", nil, fmt.Errorf("Sentinel: browser check timed out: %w", err)
	}
	return "", nil, fmt.Errorf("Sentinel: browser check did not pass after %d rounds", config.SentinelRounds)
}

func sentinelProof(ctx context.Context, seed string, bits int) (int, error) {
	if bits < 1 || bits > config.SentinelMaxProofBits || seed == "" || len(seed) > config.SentinelMaxProofSeedBytes {
		return 0, errors.New("Sentinel: unsupported browser proof difficulty or seed")
	}
	prefix := make([]byte, len(seed)+1, len(seed)+1+20)
	copy(prefix, seed+":")
	for counter := 0; ; counter++ {
		if counter%config.SentinelProofCheckInterval == 0 {
			if err := ctx.Err(); err != nil {
				return 0, err
			}
		}
		candidate := strconv.AppendInt(prefix[:len(seed)+1], int64(counter), 10)
		sum := sha256.Sum256(candidate)
		if binary.BigEndian.Uint32(sum[:4])>>uint(32-bits) == 0 {
			return counter, nil
		}
	}
}

func sentinelAnswers(prompts [][]json.RawMessage, values map[string]string) (map[string]string, error) {
	answers := make(map[string]string, len(prompts))
	for _, p := range prompts {
		var id int
		var expression string
		if len(p) != 2 || json.Unmarshal(p[0], &id) != nil || json.Unmarshal(p[1], &expression) != nil {
			return nil, errors.New("Sentinel: invalid browser question")
		}
		value, ok := values[expression]
		if !ok {
			return nil, errors.New("Sentinel: unsupported browser question")
		}
		key := strconv.Itoa(id)
		if _, duplicate := answers[key]; duplicate {
			return nil, errors.New("Sentinel: duplicate browser question")
		}
		answers[key] = value
	}
	return answers, nil
}

// The HTTP browser identity stays coherent across every round. The ten
// supported questions are data lookups, never evaluated server JavaScript.
func sentinelIdentity(agent string) (map[string]any, map[string]string) {
	platform := "Linux x86_64"
	if strings.Contains(agent, "Windows") {
		platform = "Win32"
	} else if strings.Contains(agent, "Macintosh") {
		platform = "MacIntel"
	}
	fp := map[string]any{"ua": agent, "platform": platform, "langs": "en-US,en", "tz": "UTC", "tzo": 0, "sw": 1920, "sh": 1080, "aw": 1920, "ah": 1040, "depth": 24, "ratio": 1, "hwc": 8, "mem": 8, "touch": 0, "webdriver": false, "cookies": true, "ls": true, "ss": true, "idb": true, "dnt": "", "plugins": "PDF Viewer,Chrome PDF Viewer,Chromium PDF Viewer,Microsoft Edge PDF Viewer,WebKit built-in PDF", "sig": []string{}}
	values := map[string]string{
		`(navigator.languages||[navigator.language]).join(',')`: "en-US,en",
		"navigator.hardwareConcurrency||0":                      "8", "screen.width": "1920", "screen.colorDepth": "24",
		"navigator.platform": platform, "new Date().getTimezoneOffset()": "0", "navigator.maxTouchPoints||0": "0",
		"!!navigator.webdriver": "false", "navigator.userAgent": agent, "screen.height": "1080",
	}
	return fp, values
}
