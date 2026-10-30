// SPDX-License-Identifier: MIT

package extractor

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
	"github.com/JohanLindvall/HeapLeach/internal/util"
	"golang.org/x/net/html"
)

// PeerTube instances.
//
// PeerTube is federated video hosting: some seventeen hundred independent
// sites, each running the same versioned REST API under /api/v1. That makes
// this the same bargain as the Kernel Video Sharing sites on far better
// terms. There is no markup to guess at, because every instance answers the
// same documented endpoints with the same JSON. An instance is recognised by
// its web client, which is served its server configuration inline; where a
// challenge stands in front of the page, /api/v1/config answering with a
// serverVersion settles it instead. No instance is known by name.
//
// What that JSON hands back is the best case the downloader has: an unsigned
// link to a plain MP4 with an exact byte length. Nothing expires, so there is
// no resolver; the length is the file's own rather than a listing's rounding,
// so the pre-connect skip, cross-run resume and multi-connection splitting
// all work with no host-specific plumbing whatsoever.
//
// Three things in that JSON are not what they look like:
//
//   - The obvious link is the wrong one. Every rendition carries both a
//     fileUrl and a fileDownloadUrl, and the second is the tempting one — it
//     goes through the instance and even sets a Content-Disposition naming
//     the video. It also answers a Range request with 200, no Content-Length
//     and the whole file, so taking it would cost the exact size, the resume
//     and every extra connection at once. fileUrl points at the stored object
//     instead, which honours ranges; on an instance sitting behind an
//     anti-bot challenge it is also the only one of the two that answers with
//     bytes rather than with a challenge page.
//
//   - files[] being non-empty does not mean it holds the video. An instance
//     that transcodes to HLS only still publishes an audio-only web version,
//     so files[] arrives holding exactly one entry — resolution 0, a
//     fraction of the size — while the real 1080p rendition sits in
//     streamingPlaylists[].files[]. Reading files[] first and falling back
//     only when it is empty would quietly download the soundtrack of every
//     video on such a host. Both lists are merged and ranked together
//     instead.
//
//   - Size does not order renditions. The renditions are encoded
//     independently and a 720p file is routinely the larger of the two, so
//     resolution.id is what ranks them and size only breaks ties.
//
// One more consequence of federation: a video's fileUrl frequently names a
// different host from the one that was asked, because an instance is a
// directory as much as a store. The link is followed exactly as given and
// never rebuilt against the instance.
type PeerTube struct {
	client *httpx.Client
}

const (
	// peerTubeAPIPath is the API root every instance serves.
	peerTubeAPIPath = "/api/v1"

	// peerTubeHeaderPassword unlocks a password-protected video. The same
	// header is honoured by the API and by the route serving a protected
	// video's own files, which is what makes such a video downloadable with
	// no account and no separately minted file token.
	peerTubeHeaderPassword = "x-peertube-video-password"

	// peerTubeLabel names every instance: the software.
	peerTubeLabel = "peertube"

	// peerTubeConfigGlobal is the server configuration the web client is
	// served inline, which no other software prints.
	peerTubeConfigGlobal = "window.PeerTubeServerConfig"
)

// Server error codes PeerTube returns in the body of a refusal. They say what
// the status alone cannot: 401 and 403 are also how a private or blocked
// video answers.
const (
	peerTubeErrNeedsPassword = "video_requires_password"
	peerTubeErrWrongPassword = "incorrect_video_password"
)

// NewPeerTube builds the extractor covering every PeerTube instance.
func NewPeerTube(client *httpx.Client) *PeerTube { return &PeerTube{client: client} }

func (p *PeerTube) Name() string { return peerTubeLabel }

// Recognises is the inventory's description.
func (p *PeerTube) Recognises() string {
	return "PeerTube videos, playlists, channels and accounts (`/w/<id>`, `/videos/watch/<id>`, " +
		"`/w/p/<id>`, `/c/<handle>`, `/a/<handle>`) whose page is the PeerTube web client with its " +
		"inline server configuration, or whose origin's `/api/v1/config` reports a server version"
}

// Shape reports an instance route.
func (p *PeerTube) Shape(u *url.URL) bool { return peerTubeParseRoute(u).Kind != peerTubeNothing }

// Recognise takes an instance route served by the PeerTube web client: its
// application element and the server configuration it is handed inline.
// A page that cannot say — a challenge or a bot check in front of it — is
// settled by asking the origin's /api/v1/config for its server version,
// which every instance answers and nothing else does.
func (p *PeerTube) Recognise(ctx context.Context, page *Page) bool {
	if !p.Shape(page.Final) {
		return false
	}
	if page.Has(peerTubeConfigGlobal) &&
		page.hasElement(func(n *html.Node) bool { return n.Type == html.ElementNode && n.Data == "my-app" }) {
		return true
	}
	return page.Silent() && peerTubeIsInstance(ctx, page, p.client)
}

// Extract resolves a video, channel, account or playlist page.
func (p *PeerTube) Extract(ctx context.Context, u *url.URL, opts Options) (*Result, error) {
	return peerTubeExtract(ctx, p.client, u, opts)
}

// ------------------------------------------------------------------ routes

// peerTubeKind is what an instance URL points at.
type peerTubeKind int

const (
	peerTubeNothing peerTubeKind = iota
	peerTubeVideoPage
	peerTubePlaylistPage
	peerTubeChannelPage
	peerTubeAccountPage
)

// peerTubeRoute is a parsed instance URL: what it names, and the id or handle
// naming it.
type peerTubeRoute struct {
	Kind peerTubeKind
	ID   string
}

// peerTubeParseRoute reads what an instance URL points at.
//
// Every instance serves the same routes, each under both its modern short
// form and the long one it replaced, so the two are simply listed together.
// Two details decide the shape of this:
//
//   - /w/p/<id> is a playlist and /w/<id> is a video, so the longer prefix
//     has to be tested first or every playlist link resolves to a video
//     called "p".
//   - A link to a comment is /w/<id>;threadId=<n>. The matrix parameter is
//     part of the path segment as far as any URL parser is concerned, and
//     the API answers 404 to an id carrying one, so it is cut off here —
//     which is also what the instance's own server does with it.
//
// Trailing segments are ignored throughout: a channel's page is /c/<handle>
// and its video tab is /c/<handle>/videos, and both mean the same thing
// here.
func peerTubeParseRoute(u *url.URL) peerTubeRoute {
	segs := util.PathSegments(u)
	at := func(i int) string {
		if i < len(segs) {
			return peerTubeTrimMatrix(segs[i])
		}
		return ""
	}
	route := func(kind peerTubeKind, id string) peerTubeRoute {
		if id == "" {
			return peerTubeRoute{}
		}
		return peerTubeRoute{Kind: kind, ID: id}
	}

	switch at(0) {
	case "w":
		if at(1) == "p" {
			return route(peerTubePlaylistPage, at(2))
		}
		return route(peerTubeVideoPage, at(1))
	case "videos":
		switch at(1) {
		case "watch":
			if at(2) == "playlist" {
				return route(peerTubePlaylistPage, at(3))
			}
			return route(peerTubeVideoPage, at(2))
		case "embed":
			return route(peerTubeVideoPage, at(2))
		}
	case "video-playlists":
		if at(1) == "embed" {
			return route(peerTubePlaylistPage, at(2))
		}
		return route(peerTubePlaylistPage, at(1))
	case "c", "video-channels":
		return route(peerTubeChannelPage, at(1))
	case "a", "accounts":
		return route(peerTubeAccountPage, at(1))
	}
	return peerTubeRoute{}
}

// peerTubeTrimMatrix drops a ;name=value matrix parameter from a segment.
func peerTubeTrimMatrix(seg string) string {
	id, _, _ := strings.Cut(seg, ";")
	return id
}

// ----------------------------------------------------------------- extract

// peerTubeExtract resolves any instance URL against the API at its own
// origin.
func peerTubeExtract(ctx context.Context, client *httpx.Client, u *url.URL, opts Options) (*Result, error) {
	route := peerTubeParseRoute(u)
	switch route.Kind {
	case peerTubeVideoPage:
		files, title, err := peerTubeVideoFiles(ctx, client, u, route.ID, opts)
		if err != nil {
			return nil, err
		}
		return &Result{Title: title, Files: files}, nil
	case peerTubeChannelPage:
		return peerTubeListingResult(ctx, client, u, "/video-channels/"+url.PathEscape(route.ID), "channel", opts)
	case peerTubeAccountPage:
		return peerTubeListingResult(ctx, client, u, "/accounts/"+url.PathEscape(route.ID), "account", opts)
	case peerTubePlaylistPage:
		return peerTubeListingResult(ctx, client, u, "/video-playlists/"+url.PathEscape(route.ID), "playlist", opts)
	}
	return nil, fmt.Errorf("%s: %s is none of a video (/w/<id>), a channel (/c/<handle>), "+
		"an account (/a/<handle>) or a playlist (/w/p/<id>)", peerTubeLabel, u.Redacted())
}

// peerTubeAPI is the API root for the instance a URL belongs to.
func peerTubeAPI(u *url.URL) string { return util.Origin(u) + peerTubeAPIPath }

// peerTubeAPIHeaders are the headers every API request carries.
func peerTubeAPIHeaders(u *url.URL, password string) httpx.Header {
	headers := httpx.Referer(util.Origin(u) + "/")
	if password != "" {
		headers[peerTubeHeaderPassword] = password
	}
	return headers
}

// peerTubeFileHeaders are the headers a media request carries.
//
// The route serving a protected video's files accepts the same password
// header the API does, which is the whole of what a protected video needs.
// It is sent only to the instance that was asked, though: a federated video's
// media routinely lives on a host with no connection to this password, and
// handing a user's secret to a third party buys nothing.
func peerTubeFileHeaders(u *url.URL, mediaURL, password string) httpx.Header {
	headers := httpx.Referer(util.Origin(u) + "/")
	if password == "" {
		return headers
	}
	if media, err := url.Parse(mediaURL); err == nil && strings.EqualFold(media.Host, u.Host) {
		headers[peerTubeHeaderPassword] = password
	}
	return headers
}

// ------------------------------------------------------------------ videos

// peerTubeVideoFiles resolves one video into the file to fetch.
//
// A video is one file: the best rendition, named after the video and the
// rendition's own label so it is obvious afterwards what was taken.
func peerTubeVideoFiles(ctx context.Context, client *httpx.Client, u *url.URL,
	id string, opts Options) ([]File, string, error) {

	video, err := peerTubeFetchVideo(ctx, client, u, id, opts)
	if err != nil {
		return nil, "", err
	}
	title := util.FirstNonEmpty(strings.TrimSpace(video.Name), id)

	if best, ok := peerTubeBest(peerTubeCandidates(video)); ok {
		return []File{peerTubeFileFor(u, title, best, opts.Password)}, title, nil
	}

	// Nothing self-contained anywhere. An instance old enough to publish a
	// playlist without also listing its renditions still leaves the master
	// playlist, which is a slower path to the same video: fetched in parts
	// and joined, with no ranges and no resume.
	if playlist := peerTubePlaylistURL(video); playlist != "" {
		headers := peerTubeFileHeaders(u, playlist, opts.Password)
		media, err := resolveMediaPlaylist(ctx, client, playlist, headers)
		if err != nil {
			return nil, "", fmt.Errorf("%s: %s: %w", peerTubeLabel, title, err)
		}
		// Current instances serve fragmented MP4 and older ones MPEG-TS; the
		// segments say which. See segmentsExtension.
		return []File{media.file(title+media.extension(), headers)}, title, nil
	}

	return nil, "", peerTubeNoMedia(video)
}

// peerTubeFileFor turns the chosen rendition into the file to queue.
//
// Size is carried through as the API gave it, with no SizeApprox: it is the
// stored object's own length rather than a listing's rounding, and the
// downloader may act on it — skipping a file already on disk before it
// connects at all.
func peerTubeFileFor(u *url.URL, title string, best peerTubeCandidate, password string) File {
	size := knownSize(best.Size)
	return File{
		Name:    peerTubeFileName(title, best),
		URL:     best.URL,
		Size:    size,
		Headers: peerTubeFileHeaders(u, best.URL, password),
	}
}

// peerTubeFetchVideo reads one video's record. Both id forms the site uses —
// the full UUID and the short one — are accepted by this one path, so the
// route parser never has to tell them apart.
func peerTubeFetchVideo(ctx context.Context, client *httpx.Client, u *url.URL,
	id string, opts Options) (*peerTubeVideo, error) {

	endpoint := peerTubeAPI(u) + "/videos/" + url.PathEscape(id)
	var video peerTubeVideo
	if err := client.GetJSON(ctx, endpoint, peerTubeAPIHeaders(u, opts.Password), &video); err != nil {
		return nil, peerTubeExplain(err, id)
	}
	return &video, nil
}

// peerTubeExplain turns a refusal into something the user can act on.
//
// A status alone does not say enough here: 401 and 403 are how a
// password-protected video answers, and equally how a private or blocked one
// does. The instance names the real reason in the body, using its own
// documented error codes, so that is what is read — and only when it is
// absent does the wording fall back to listing the possibilities.
func peerTubeExplain(err error, id string) error {
	status, ok := errors.AsType[*httpx.StatusError](err)
	if !ok {
		return fmt.Errorf("%s: %s: %w", peerTubeLabel, id, err)
	}
	switch {
	case strings.Contains(status.Body, peerTubeErrNeedsPassword):
		return fmt.Errorf("%s: %w", peerTubeLabel, ErrPasswordRequired)
	case strings.Contains(status.Body, peerTubeErrWrongPassword):
		return fmt.Errorf("%s: the password was not accepted for %s", peerTubeLabel, id)
	case status.Code == http.StatusNotFound:
		return fmt.Errorf("%s: %s does not exist on this instance (it may have been deleted, "+
			"or it may live on another one)", peerTubeLabel, id)
	case status.Code == http.StatusUnauthorized, status.Code == http.StatusForbidden:
		return fmt.Errorf("%s: %s is not available to anonymous callers "+
			"(it may be private, password protected, or blocked here)", peerTubeLabel, id)
	}
	return fmt.Errorf("%s: %s: %w", peerTubeLabel, id, err)
}

// peerTubeNoMedia explains a video record that carries nothing to fetch.
// It is the instance's reason rather than a failure, so a listing none of
// whose videos could be read says it (peerTubeListingResult).
func peerTubeNoMedia(video *peerTubeVideo) error {
	name := util.FirstNonEmpty(strings.TrimSpace(video.Name), "this video")
	reason := fmt.Sprintf("%q lists no media file (the instance may still be transcoding it)", name)
	switch {
	case video.IsLive:
		reason = fmt.Sprintf("%q is a live stream, which has no finished file to fetch", name)
	case video.State.Label != "" && !strings.EqualFold(video.State.Label, "Published"):
		reason = fmt.Sprintf("%q is not ready yet (%s)", name, video.State.Label)
	}
	return &broadcastRefusal{site: peerTubeLabel, reason: reason}
}

// peerTubeCandidate is one rendition, from either of the two lists a video
// record keeps them in.
type peerTubeCandidate struct {
	URL   string
	Size  int64
	Label string
	// Resolution is the vertical pixel count the instance recorded, and 0 is
	// its own marker for an audio-only rendition.
	Resolution int
	HasVideo   bool
	// Progressive marks a plain web video rather than a rendition belonging
	// to a streaming playlist.
	Progressive bool
}

// peerTubeCandidates collects every rendition the record offers, from both
// lists at once. Which list a rendition came from decides nothing but a tie;
// see the note at the top of the file on why reading one before the other
// goes wrong.
func peerTubeCandidates(video *peerTubeVideo) []peerTubeCandidate {
	var out []peerTubeCandidate
	add := func(files []peerTubeFile, progressive bool) {
		for _, f := range files {
			if f.FileURL == "" {
				continue
			}
			out = append(out, peerTubeCandidate{
				URL:         f.FileURL,
				Size:        f.Size,
				Label:       strings.TrimSpace(f.Resolution.Label),
				Resolution:  f.Resolution.ID,
				HasVideo:    f.HasVideo,
				Progressive: progressive,
			})
		}
	}
	add(video.Files, true)
	for _, playlist := range video.StreamingPlaylists {
		add(playlist.Files, false)
	}
	return out
}

// peerTubeBest picks the rendition to fetch.
//
// hasVideo leads because an audio-only rendition is the one wrong answer
// that looks entirely plausible — it is an ordinary MP4 of ordinary size —
// and an instance that does not publish the flag simply reports it false
// throughout, which makes the key uniform and hands the decision to the next
// one. Resolution then ranks the rest, since size cannot: renditions are
// encoded independently and a 720p file is often larger than the 1080p
// beside it. A plain web video wins a tie against the same resolution inside
// a streaming playlist, being the more ordinary MP4 of the two.
func peerTubeBest(candidates []peerTubeCandidate) (peerTubeCandidate, bool) {
	if len(candidates) == 0 {
		return peerTubeCandidate{}, false
	}
	ranked := append([]peerTubeCandidate(nil), candidates...)
	slices.SortStableFunc(ranked, func(a, b peerTubeCandidate) int {
		return cmp.Or(
			cmpBool(b.HasVideo, a.HasVideo), // video first
			cmp.Compare(b.Resolution, a.Resolution),
			cmpBool(b.Progressive, a.Progressive), // a plain file over a playlist
			cmp.Compare(b.Size, a.Size),
		)
	})
	return ranked[0], true
}

// peerTubePlaylistURL is the master playlist of the first streaming playlist
// that has one.
func peerTubePlaylistURL(video *peerTubeVideo) string {
	for _, playlist := range video.StreamingPlaylists {
		if playlist.PlaylistURL != "" {
			return playlist.PlaylistURL
		}
	}
	return ""
}

// peerTubeFileName names the download after the video and the rendition,
// which is the site's own convention and the only thing distinguishing two
// downloads of one video at different qualities.
func peerTubeFileName(title string, candidate peerTubeCandidate) string {
	ext := linkExtAsIs(candidate.URL, ".mp4")
	if candidate.Label == "" {
		return title + ext
	}
	return title + "-" + candidate.Label + ext
}

// ---------------------------------------------------------------- listings

// peerTubeListingResult expands a channel, account or playlist.
//
// The listing itself is fetched for its ids and the thing's own name, and
// then every video is read separately, because a listing entry carries no
// media at all — no file list, no size, not even a link. Doing that one
// after another would leave a channel of several hundred resolving for
// minutes before a byte moved, so they go out several at a time and are
// collected by index, which keeps the job in the order the site listed it
// however the requests interleave.
func peerTubeListingResult(ctx context.Context, client *httpx.Client, u *url.URL,
	ownerPath, kind string, opts Options) (*Result, error) {

	headers := peerTubeAPIHeaders(u, opts.Password)
	base := peerTubeAPI(u) + ownerPath

	ids, note, err := peerTubeListVideos(ctx, client, base+"/videos", headers, kind, opts)
	if err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, fmt.Errorf("%s: this %s lists no videos", peerTubeLabel, kind)
	}

	title := peerTubeOwnerName(ctx, client, base, headers)
	if title == "" {
		title = strings.Trim(u.Path, "/")
	}

	files, refusals, failed := peerTubeExpand(ctx, client, u, ids, opts)
	// Expansion skips a video it cannot read, which is right for one bad
	// entry and wrong for a cancelled run: every remaining video would be
	// "skipped" and the job would look like a complete, much shorter
	// channel. A cancellation is reported as one.
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, noneRead(peerTubeLabel, len(ids), "videos", u.Redacted(), refusals, failed)
	}
	return &Result{Title: title, Files: files, Note: listingNote(note, refusedNote(refusals))}, nil
}

// peerTubeOwnerName reads the display name of the channel, account or
// playlist, which becomes the destination folder. A handle is a poor folder
// name — "thelinuxexperiment_channel" against "The Linux Experiment" — and
// this is one request, so it is worth asking; a failure is not, so it falls
// back rather than failing the job.
func peerTubeOwnerName(ctx context.Context, client *httpx.Client, endpoint string, headers httpx.Header) string {
	var owner struct {
		DisplayName string `json:"displayName"`
		Name        string `json:"name"`
	}
	if err := client.GetJSON(ctx, endpoint, headers, &owner); err != nil {
		return ""
	}
	return strings.TrimSpace(util.FirstNonEmpty(owner.DisplayName, owner.Name))
}

// peerTubeListVideos walks a paginated /videos listing for the ids it names,
// to the total the listing states, or a page that comes back empty. Every
// id listed costs a request of its own, so the listing cap bounds it.
//
// nsfw=both is asked for because the default is the instance's own, which on
// most of them hides such videos — a channel would then be downloaded with
// part of it silently missing rather than visibly refused. A listing run
// past its end tends to answer with its last page again rather than with
// nothing, which adds nothing and so ends the walk too.
func peerTubeListVideos(ctx context.Context, client *httpx.Client, endpoint string,
	headers httpx.Header, kind string, opts Options) ([]string, string, error) {

	limit, limitNote := listingLimit(opts)
	start := 0
	return walkPages(ctx, pageWalk[string]{
		key:   func(id string) string { return id },
		limit: limit, limitNote: limitNote,
		fetch: func(ctx context.Context, _ int) (pageOf[string], error) {
			query := url.Values{
				"count": {strconv.Itoa(config.FolderPageSize)},
				"start": {strconv.Itoa(start)},
				"nsfw":  {"both"},
			}
			var page peerTubeListing
			if err := client.GetJSON(ctx, endpoint+"?"+query.Encode(), headers, &page); err != nil {
				return pageOf[string]{}, fmt.Errorf("%s: read this %s: %w", peerTubeLabel, kind, err)
			}
			start += len(page.Data)
			ids := make([]string, 0, len(page.Data))
			for _, entry := range page.Data {
				if id := entry.videoID(); id != "" {
					ids = append(ids, id)
				}
			}
			return pageOf[string]{items: ids, next: true, end: len(page.Data) == 0 || (page.Total > 0 && start >= page.Total)}, nil
		},
	})
}

// peerTubeExpand reads every listed video's own record, several at a time.
//
// One video that will not load is skipped rather than failing the job: on a
// channel of hundreds — where a single entry may be a live stream, or still
// transcoding — the rest are still worth having. Why each was skipped comes
// back beside the files, for when none could be read.
func peerTubeExpand(ctx context.Context, client *httpx.Client, u *url.URL,
	ids []string, opts Options) (files []File, refusals []string, failed error) {

	return resolveFiles(ctx, ids, func(ctx context.Context, id string) ([]File, error) {
		files, _, err := peerTubeVideoFiles(ctx, client, u, id, opts)
		return files, err
	})
}

// ------------------------------------------------------------------- probe

// peerTubeIsInstance asks an origin whether it runs PeerTube. serverVersion
// is what settles it: the endpoint exists on every instance and on nothing
// else, and a version string is proof the answer came from the software
// rather than from a catch-all that returns JSON for any path. The answer is
// kept on the probed page, so it is asked once however often it is wanted.
func peerTubeIsInstance(ctx context.Context, page *Page, client *httpx.Client) bool {
	var cfg struct {
		ServerVersion string `json:"serverVersion"`
	}
	return pageJSON(ctx, page, client, peerTubeAPI(page.Final)+"/config", httpx.Referer(util.Origin(page.Final)+"/"), &cfg) &&
		strings.TrimSpace(cfg.ServerVersion) != ""
}

// -------------------------------------------------------------------- JSON

// peerTubeVideo is the part of a video record this needs.
type peerTubeVideo struct {
	Name   string `json:"name"`
	IsLive bool   `json:"isLive"`
	// State says why a video that is otherwise fine carries no file — still
	// transcoding, waiting for a live stream to start. Only the label is
	// read: it is already the wording to show, and the numbering behind it
	// has gained members over the years.
	State struct {
		Label string `json:"label"`
	} `json:"state"`
	Files              []peerTubeFile      `json:"files"`
	StreamingPlaylists []peerTubeStreaming `json:"streamingPlaylists"`
}

// peerTubeStreaming is one adaptive presentation of a video.
type peerTubeStreaming struct {
	PlaylistURL string         `json:"playlistUrl"`
	Files       []peerTubeFile `json:"files"`
}

// peerTubeFile is one rendition. fileDownloadUrl is deliberately absent: see
// the note at the top of the file on why the link that names itself the
// download is the one to avoid.
type peerTubeFile struct {
	Resolution struct {
		ID    int    `json:"id"`
		Label string `json:"label"`
	} `json:"resolution"`
	Size    int64  `json:"size"`
	FileURL string `json:"fileUrl"`
	// HasVideo is absent before PeerTube 5.1, which decodes as false
	// throughout and so leaves the ranking to resolution alone.
	HasVideo bool `json:"hasVideo"`
}

// peerTubeListing is a page of any of the three video listings.
type peerTubeListing struct {
	Total int                    `json:"total"`
	Data  []peerTubeListingEntry `json:"data"`
}

// peerTubeListingEntry is one row of a listing.
//
// A channel's and an account's rows are videos; a playlist's are positions
// that wrap one, which is why the type refers to itself. Video is a pointer
// because a playlist keeps the position of an element whose video has since
// been deleted or made private, and sends it as null.
type peerTubeListingEntry struct {
	UUID      string                `json:"uuid"`
	ShortUUID string                `json:"shortUUID"`
	Video     *peerTubeListingEntry `json:"video"`
}

// videoID is the id to ask the API for, or empty for a row with no video
// behind it.
func (e peerTubeListingEntry) videoID() string {
	if e.Video != nil {
		return e.Video.videoID()
	}
	return util.FirstNonEmpty(e.UUID, e.ShortUUID)
}
