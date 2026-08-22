package extractor

import (
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

// WeTransfer resolves a share link into the archive of everything behind it.
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
type WeTransfer struct {
	client *httpx.Client
	// api is the transfers endpoint's base, a field only so a test can point
	// it somewhere that is not the live site.
	api string
}

const (
	wetransferRoot = "https://wetransfer.com"
	wetransferAPI  = wetransferRoot + "/api/v4/transfers"

	// wetransferShort is the site's own URL shortener, which is a plain
	// redirect to a download page and carries nothing of its own.
	wetransferShort = "we.tl"

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
func NewWeTransfer(client *httpx.Client) *WeTransfer {
	return &WeTransfer{client: client, api: wetransferAPI}
}

func (w *WeTransfer) Name() string { return "wetransfer" }

// Match takes the site's download pages and its short links.
//
// The path gate on the site itself is deliberate. The bytes are served from a
// signed link on a subdomain of the same domain, and one of those pasted
// straight in is already a plain file that the direct fallback downloads as
// it stands — whereas matching the whole domain here would leave this
// extractor hunting for a transfer id in a storage path and failing the job.
func (w *WeTransfer) Match(u *url.URL) bool {
	if util.HostMatches(u.Host, wetransferShort) {
		return true
	}
	if !util.HostMatches(u.Host, "wetransfer.com") {
		return false
	}
	segs := util.PathSegments(u)
	return len(segs) > 0 && segs[0] == wetransferDownloads
}

// Extract resolves a share link to the transfer's archive.
func (w *WeTransfer) Extract(ctx context.Context, u *url.URL, _ Options) (*Result, error) {
	page := u
	if util.HostMatches(u.Host, wetransferShort) {
		var err error
		if page, err = w.follow(ctx, u); err != nil {
			return nil, err
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
	if _, err := w.mint(ctx, link); err != nil {
		return nil, err
	}

	name := wetransferName(link.id)
	return &Result{Title: name, Files: []File{{
		Name: name,
		// No URL on purpose: every attempt mints its own.
		Size:    -1,
		Headers: httpx.Referer(wetransferRoot + "/"),
		Resolve: func(ctx context.Context) (*Target, error) { return w.mint(ctx, link) },
	}}}, nil
}

// mint asks the API to build the archive and hand back a link to it.
func (w *WeTransfer) mint(ctx context.Context, link wetransferLink) (*Target, error) {
	body := wetransferRequest{
		Intent:       wetransferIntent,
		SecurityHash: link.hash,
		RecipientID:  link.recipient,
	}
	var out struct {
		DirectLink string `json:"direct_link"`
		Message    string `json:"message"`
	}
	endpoint := w.api + "/" + url.PathEscape(link.id) + "/download"
	if err := w.client.PostJSON(ctx, endpoint,
		httpx.RefererOrigin(wetransferRoot+"/", wetransferRoot), body, &out); err != nil {
		return nil, wetransferFailure(link.id, err)
	}
	if out.DirectLink == "" {
		return nil, fmt.Errorf("wetransfer: %s: %s", link.id,
			util.FirstNonEmpty(wetransferReason(out.Message), "the API returned no download link"))
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
	var status *httpx.StatusError
	if errors.As(err, &status) {
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

// follow resolves a short link to the download page behind it.
//
// The redirect's destination is the whole point: the id and hash the API
// needs are in the URL it lands on, and the page it lands on is an
// application shell that says nothing, so the body is drained and discarded.
// A code standing for nothing is redirected to an error page instead, which
// parses as no transfer at all and is reported as such.
func (w *WeTransfer) follow(ctx context.Context, short *url.URL) (*url.URL, error) {
	req, err := w.client.NewRequest(ctx, http.MethodGet, short.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set(httpx.HeaderAccept, httpx.AcceptHTML)
	resp, err := w.client.Do(req)
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
