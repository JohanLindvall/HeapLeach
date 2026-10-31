// SPDX-License-Identifier: MIT

package extractor

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
	"github.com/JohanLindvall/HeapLeach/internal/util"
)

// wetransfer resolves a share link into the archive of everything behind it.
//
// A transfer holds several files, and none of them is individually reachable
// by whoever holds the link: the endpoint that lists a transfer's contents
// answers 401 to a caller without an account, so there is nothing to
// enumerate and nothing to choose between. What is left is the download
// endpoint, which builds one archive of the whole transfer on request. That
// is the same bargain dropbox strikes for a folder link, down to the naming —
// the archive arrives with a Content-Disposition, so the file is queued under
// a name with no extension and the server's own wins on the way to disk.
//
// The link that endpoint mints is signed, and the transfer behind it is not
// permanent: a free one is deleted about a week after it was sent. So the
// link is minted per attempt through Resolve, never carried on the File,
// because one signed while the item waited its turn would already be spent.
//
// The two ways a transfer becomes permanently unreachable are worth telling
// apart, because the site words both as an ordinary 404: it is either gone,
// or it was addressed to named recipients and never shared by link at all.
// Neither improves by being retried, so both are mapped to something the user
// can act on rather than left to look like a transient failure.

const (
	wetransferRoot = "https://wetransfer.com"
	// wetransferAPI is the transfers endpoint's base, which a test points
	// somewhere that is not the live site (Share.api).
	wetransferAPI = wetransferRoot + "/api/v4/transfers"

	// wetransferDownloads is the one path on the site this handles.
	wetransferDownloads = "downloads"

	// wetransferIntent asks for a single archive of the whole transfer. The
	// endpoint validates it and answers an intent it does not know with a 500
	// whose message is suppressed, which is why this is a constant rather
	// than anything derived at runtime.
	wetransferIntent = "entire_transfer"
)

// The site's two permanent refusals, matched on the stable head of each. The
// rest of "Couldn't find Transfer" is the failed database lookup spelled out
// in full, which is an implementation detail and not ours to depend on.
const (
	wetransferGone     = "Couldn't find Transfer"
	wetransferNoAccess = "No download access"
)

// NewWeTransfer builds the wetransfer extractor.
func NewWeTransfer(client *httpx.Client) *Share { return newShare(client, &wetransferSite) }

// wetransferPageShape is a download page, /downloads/<id>[/<recipient>]/<hash>;
// wetransferShortShape the short link the site mails, /t-<code>, which
// redirects to one.
var (
	wetransferPageShape  = pathShape(`^/downloads/[A-Za-z0-9]+(/[A-Za-z0-9]+)?/[A-Za-z0-9]+/?$`)
	wetransferShortShape = pathShape(`^/t-[A-Za-z0-9]+/?$`)
)

// wetransferSite takes a download page — where a short link's redirect
// ends — served by the site's app, whose page data carries the security
// hash the download endpoint is then asked with. A short link is followed
// for that redirect. The bytes are served from signed links of another
// form, which stay the direct fallback's.
var wetransferSite = shareSite{
	name: "wetransfer",
	recognises: "download pages (`/downloads/<id>/<hash>`) and the short links (`/t-<code>`) that redirect to them, " +
		"served by the WeTransfer app with the transfer's `securityHash` in its page data",
	forms: []shareForm{
		{wetransferPageShape, func(p *Page) bool {
			return p.Has(`id="__NEXT_DATA__"`, `"securityHash"`) && p.Named("WeTransfer")
		}},
		{shape: wetransferShortShape},
	},
	extract: wetransferExtract,
	api:     wetransferAPI,
}

// wetransferExtract resolves a share link to the transfer's archive.
func wetransferExtract(ctx context.Context, s *Share, u *url.URL, _ Options) (*Result, error) {
	// A short link names no transfer itself: the page it redirected to,
	// which the probe already followed, or a fetch of it, does.
	page := u
	if _, ok := wetransferParse(u); !ok {
		if _, final, probed := probedDocument(ctx, u); probed {
			page = final
		} else {
			var err error
			if page, err = wetransferFollow(ctx, s, u); err != nil {
				return nil, err
			}
		}
	}

	link, ok := wetransferParse(page)
	if !ok {
		return nil, fmt.Errorf("wetransfer: %s is not a transfer download link "+
			"(they look like %s/%s/<id>/<hash>)", u.Redacted(), wetransferRoot, wetransferDownloads)
	}

	// Mint once here and discard the link. What this is for is not the link —
	// that one would be spent by the time the item ran — but the answer: a
	// transfer that has expired, or that was never shared by link at all, is
	// permanently unreachable, and belongs in the extraction error the user
	// reads at once rather than in a worker that would retry it first.
	if _, err := wetransferMint(ctx, s, link); err != nil {
		return nil, err
	}

	name := wetransferName(link.id)
	return &Result{Title: name, Files: []File{{
		Name: name,
		// No URL on purpose: every attempt mints its own.
		Size:    -1,
		Headers: httpx.Referer(wetransferRoot + "/"),
		Resolve: func(ctx context.Context) (*Target, error) { return wetransferMint(ctx, s, link) },
	}}}, nil
}

// wetransferMint asks the API to build the archive and hand back a link to
// it.
func wetransferMint(ctx context.Context, s *Share, link wetransferLink) (*Target, error) {
	body := wetransferRequest{
		Intent:       wetransferIntent,
		SecurityHash: link.hash,
		RecipientID:  link.recipient,
	}
	var out struct {
		DirectLink string `json:"direct_link"`
		Message    string `json:"message"`
	}
	endpoint := s.api + "/" + url.PathEscape(link.id) + "/download"
	if err := s.client.PostJSON(ctx, endpoint,
		httpx.SiteOrigin(wetransferRoot), body, &out); err != nil {
		return nil, wetransferFailure(link.id, err)
	}
	if out.DirectLink == "" {
		return nil, fmt.Errorf("wetransfer: %s: %s", link.id,
			cmp.Or(wetransferReason(out.Message), "the API returned no download link"))
	}
	return &Target{
		URL:     out.DirectLink,
		Name:    wetransferName(link.id),
		Size:    -1,
		Headers: httpx.Referer(wetransferRoot + "/"),
	}, nil
}

// wetransferRequest is the body the download endpoint takes. RecipientID is
// left out altogether for a link that carries none, which is the shape the
// site itself sends for a publicly shared transfer.
type wetransferRequest struct {
	Intent       string `json:"intent"`
	SecurityHash string `json:"security_hash"`
	RecipientID  string `json:"recipient_id,omitempty"`
}

// wetransferName is the placeholder the archive is queued under.
//
// It carries no extension on purpose, so the Content-Disposition the server
// sends with the archive is what names the file on disk; the API itself
// reports neither a name nor a length. The id is in it so two transfers
// queued at once do not collide on one placeholder.
func wetransferName(id string) string { return "wetransfer-" + id }

// wetransferFailure turns a rejected request into the reason behind it.
//
// The refusals arrive as a 404 carrying a JSON body, and a non-2xx answer is
// reported by the client as a status error without ever being decoded — so
// the message is read back out of the body the error captured, not out of the
// response that never happened.
func wetransferFailure(id string, err error) error {
	if status, ok := errors.AsType[*httpx.StatusError](err); ok {
		var out struct {
			Message string `json:"message"`
		}
		if json.Unmarshal([]byte(status.Body), &out) == nil {
			if reason := wetransferReason(out.Message); reason != "" {
				return fmt.Errorf("wetransfer: %s: %s", id, reason)
			}
		}
	}
	return fmt.Errorf("wetransfer: download %s: %w", id, err)
}

// wetransferReason explains the refusals that no amount of retrying will
// change, and says nothing about anything else.
func wetransferReason(message string) string {
	switch {
	case strings.Contains(message, wetransferGone):
		return "this transfer no longer exists " +
			"(a free transfer is deleted about a week after it is sent)"
	case strings.Contains(message, wetransferNoAccess):
		return "this transfer was sent to named recipients rather than shared by link, " +
			"so it can only be downloaded through the link one of them was mailed"
	}
	return ""
}

// wetransferFollow resolves a short link to the download page behind it.
//
// The redirect's destination is the whole point: the id and hash the API
// needs are in the URL it lands on, and the page it lands on is an
// application shell that says nothing, so the body is drained and discarded.
// A code standing for nothing is redirected to an error page instead, which
// parses as no transfer at all and is reported as such.
func wetransferFollow(ctx context.Context, s *Share, short *url.URL) (*url.URL, error) {
	req, err := s.client.NewRequest(ctx, http.MethodGet, short.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set(httpx.HeaderAccept, httpx.AcceptHTML)
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("wetransfer: follow %s: %w", short.Redacted(), err)
	}
	defer resp.Body.Close()
	// Drained rather than merely closed, so the connection returns to the pool.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, config.MaxResponseBytes))
	return resp.Request.URL, nil
}

// wetransferLink is what a download page's path carries.
type wetransferLink struct {
	id        string
	recipient string
	hash      string
}

// wetransferParse reads the identifiers out of a download page's path.
//
// The two shapes differ by a single segment and are told apart by length
// alone: a transfer shared by link states its id and then its security hash,
// while one mailed to named recipients carries that recipient's id between
// the two, and the endpoint wants it echoed back. Nothing here looks at the
// host, because a short link lands on whichever of the site's names the
// redirect chose.
func wetransferParse(u *url.URL) (wetransferLink, bool) {
	segs := util.PathSegments(u)
	if len(segs) == 0 || segs[0] != wetransferDownloads {
		return wetransferLink{}, false
	}
	switch len(segs) {
	case 3:
		return wetransferLink{id: segs[1], hash: segs[2]}, true
	case 4:
		return wetransferLink{id: segs[1], recipient: segs[2], hash: segs[3]}, true
	}
	return wetransferLink{}, false
}
