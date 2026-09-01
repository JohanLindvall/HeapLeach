package extractor

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"slices"
	"strconv"
	"strings"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
	"github.com/JohanLindvall/HeapLeach/internal/util"
)

// PeerTube instances.
//
// PeerTube is federated video hosting: some seventeen hundred independent
// sites, each running the same versioned REST API under /api/v1. That makes
// this the same bargain as the Kernel Video Sharing sites on far better
// terms. There is no markup to guess at, because every instance answers the
// same documented endpoints with the same JSON; and whether an unknown host
// is one at all is settled by asking /api/v1/config for its serverVersion
// rather than by recognising a page, which is a question with an answer
// instead of a heuristic.
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
	hostSet
	client *httpx.Client
	host   string
}

// peerTubeKnownHosts are the instances this build ships knowing about, so
// each is matched on its host alone and named for itself in the UI.
//
// The list is a convenience and not the boundary of what works, which is
// just as well: no list of seven can stand in for seventeen hundred. Hosts
// nobody named are reached two other ways — the "peertube" family of
// HEAPLEACH_EXTRA_HOSTS adds them at runtime, and peerTubeSniff recognises
// the rest from their own API.
var peerTubeKnownHosts = []string{
	"framatube.org",
	"tilvids.com",
	"video.blender.org",
	"makertube.net",
	"tube.tchncs.de",
	"kolektiva.media",
	"spectra.video",
}

const (
	// peerTubeAPIPath is the API root every instance serves.
	peerTubeAPIPath = "/api/v1"

	// peerTubeHeaderPassword unlocks a password-protected video. The same
	// header is honoured by the API and by the route serving a protected
	// video's own files, which is what makes such a video downloadable with
	// no account and no separately minted file token.
	peerTubeHeaderPassword = "x-peertube-video-password"
)

// Server error codes PeerTube returns in the body of a refusal. They say what
// the status alone cannot: 401 and 403 are also how a private or blocked
// video answers.
const (
	peerTubeErrNeedsPassword = "video_requires_password"
	peerTubeErrWrongPassword = "incorrect_video_password"
)

// peerTubeGenericLabels are the words an instance's hostname uses to say it
// is a video site rather than to name itself, plus the second-level labels
// that are really part of a public suffix.
var peerTubeGenericLabels = map[string]bool{
	"video": true, "videos": true, "tube": true, "peertube": true,
	"media": true, "watch": true, "stream": true, "tv": true,
	"co": true, "com": true, "org": true, "net": true,
}

// NewPeerTubeSites builds one extractor per known instance, exactly as the
// KVS sites do: matched on its host, named for itself, and indistinguishable
// in the UI from a hand-written host.
//
// The runtime list is read here rather than carried on config.Config, which
// is the surface the command line and Prepare share. This one is consulted by
// a single extractor, has no flag to be overridden by and nothing to
// validate, so the trip through Config would carry it and change nothing;
// httpx reads a setting of its own the same way.
func NewPeerTubeSites(cfg *config.Config, client *httpx.Client) []Extractor {
	hosts := append([]string(nil), peerTubeKnownHosts...)
	hosts = append(hosts, cfg.ExtraHostsFor(config.FamilyPeerTube)...)

	out := make([]Extractor, 0, len(hosts))
	for _, host := range util.Dedupe(hosts) {
		host = strings.ToLower(host)
		out = append(out, &PeerTube{hostSet: hostSet{host}, client: client, host: host})
	}
	return out
}

func (p *PeerTube) Name() string { return peerTubeLabel(p.host) }

// Extract resolves a video, channel, account or playlist page.
func (p *PeerTube) Extract(ctx context.Context, u *url.URL, opts Options) (*Result, error) {
	return peerTubeExtract(ctx, p.client, u, p.Name(), opts)
}

// peerTubeLabel names an instance after the part of its hostname that
// identifies the site.
//
// Operators are strikingly consistent about saying "video" somewhere:
// video.blender.org, tube.tchncs.de, spectra.video, makertube.net. Taking the
// first label the way the KVS sites do would call the first of those "video"
// and the second "tube", and would give the same name to every other instance
// that starts the same way — which the registry cannot allow, since a label
// is what the UI lists and what every error message opens with. So the
// rightmost label that is neither the public suffix nor one of those generic
// words is what names the site. A hostname made of nothing but generics —
// peertube.tv — keeps its first label rather than ending up with none.
func peerTubeLabel(host string) string {
	host = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "."))
	if h, _, ok := strings.Cut(host, ":"); ok {
		host = h
	}
	host = strings.TrimPrefix(host, "www.")
	if host == "" {
		return "peertube"
	}

	labels := strings.Split(host, ".")
	if len(labels) > 1 {
		labels = labels[:len(labels)-1]
	}
	for _, label := range slices.Backward(labels) {
		if !peerTubeGenericLabels[label] {
			return label
		}
	}
	return labels[0]
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
func peerTubeExtract(ctx context.Context, client *httpx.Client, u *url.URL, label string, opts Options) (*Result, error) {
	route := peerTubeParseRoute(u)
	switch route.Kind {
	case peerTubeVideoPage:
		files, title, err := peerTubeVideoFiles(ctx, client, u, route.ID, label, opts)
		if err != nil {
			return nil, err
		}
		return &Result{Title: title, Files: files}, nil
	case peerTubeChannelPage:
		return peerTubeListingResult(ctx, client, u, "/video-channels/"+url.PathEscape(route.ID), "channel", label, opts)
	case peerTubeAccountPage:
		return peerTubeListingResult(ctx, client, u, "/accounts/"+url.PathEscape(route.ID), "account", label, opts)
	case peerTubePlaylistPage:
		return peerTubeListingResult(ctx, client, u, "/video-playlists/"+url.PathEscape(route.ID), "playlist", label, opts)
	}
	return nil, fmt.Errorf("%s: %s is none of a video (/w/<id>), a channel (/c/<handle>), "+
		"an account (/a/<handle>) or a playlist (/w/p/<id>)", label, u.Redacted())
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
	id, label string, opts Options) ([]File, string, error) {

	video, err := peerTubeFetchVideo(ctx, client, u, id, label, opts)
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
		segments, variant, err := resolvePlaylist(ctx, client, playlist, headers)
		if err != nil {
			return nil, "", fmt.Errorf("%s: %s: %w", label, title, err)
		}
		return []File{{
			// Current instances serve fragmented MP4 and older ones MPEG-TS;
			// the segments say which. See segmentsExtension.
			Name:     title + segmentsExtension(segments, variant),
			Size:     -1,
			Headers:  headers,
			Segments: segments,
		}}, title, nil
	}

	return nil, "", peerTubeNoMedia(video, label)
}

// peerTubeFileFor turns the chosen rendition into the file to queue.
//
// Size is carried through as the API gave it, with no SizeApprox: it is the
// stored object's own length rather than a listing's rounding, and the
// downloader may act on it — skipping a file already on disk before it
// connects at all.
func peerTubeFileFor(u *url.URL, title string, best peerTubeCandidate, password string) File {
	size := best.Size
	if size <= 0 {
		// A record with no length is not a zero-length file; the contract
		// spells that absence -1, and 0 would be read as a finished download.
		size = -1
	}
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
	id, label string, opts Options) (*peerTubeVideo, error) {

	endpoint := peerTubeAPI(u) + "/videos/" + url.PathEscape(id)
	var video peerTubeVideo
	if err := client.GetJSON(ctx, endpoint, peerTubeAPIHeaders(u, opts.Password), &video); err != nil {
		return nil, peerTubeExplain(err, label, id)
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
func peerTubeExplain(err error, label, id string) error {
	var status *httpx.StatusError
	if !errors.As(err, &status) {
		return fmt.Errorf("%s: %s: %w", label, id, err)
	}
	switch {
	case strings.Contains(status.Body, peerTubeErrNeedsPassword):
		return fmt.Errorf("%s: %w", label, ErrPasswordRequired)
	case strings.Contains(status.Body, peerTubeErrWrongPassword):
		return fmt.Errorf("%s: the password was not accepted for %s", label, id)
	case status.Code == http.StatusNotFound:
		return fmt.Errorf("%s: %s does not exist on this instance (it may have been deleted, "+
			"or it may live on another one)", label, id)
	case status.Code == http.StatusUnauthorized, status.Code == http.StatusForbidden:
		return fmt.Errorf("%s: %s is not available to anonymous callers "+
			"(it may be private, password protected, or blocked here)", label, id)
	}
	return fmt.Errorf("%s: %s: %w", label, id, err)
}

// peerTubeNoMedia explains a video record that carries nothing to fetch.
func peerTubeNoMedia(video *peerTubeVideo, label string) error {
	name := util.FirstNonEmpty(strings.TrimSpace(video.Name), "this video")
	switch {
	case video.IsLive:
		return fmt.Errorf("%s: %q is a live stream, which has no finished file to fetch", label, name)
	case video.State.Label != "" && !strings.EqualFold(video.State.Label, "Published"):
		return fmt.Errorf("%s: %q is not ready yet (%s)", label, name, video.State.Label)
	}
	return fmt.Errorf("%s: %q lists no media file (the instance may still be transcoding it)", label, name)
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
	ext := path.Ext(util.NameFromURL(candidate.URL))
	if ext == "" {
		ext = ".mp4"
	}
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
	ownerPath, kind, label string, opts Options) (*Result, error) {

	headers := peerTubeAPIHeaders(u, opts.Password)
	base := peerTubeAPI(u) + ownerPath

	ids, err := peerTubeListVideos(ctx, client, base+"/videos", headers, label, kind)
	if err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, fmt.Errorf("%s: this %s lists no videos", label, kind)
	}

	title := peerTubeOwnerName(ctx, client, base, headers)
	if title == "" {
		title = strings.Trim(u.Path, "/")
	}

	files := peerTubeExpand(ctx, client, u, ids, label, opts)
	// Expansion skips a video it cannot read, which is right for one bad
	// entry and wrong for a cancelled run: every remaining video would be
	// "skipped" and the job would look like a complete, much shorter
	// channel. A cancellation is reported as one.
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return &Result{Title: title, Files: files}, nil
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

// peerTubeListVideos walks a paginated /videos listing for the ids it names.
//
// nsfw=both is asked for because the default is the instance's own, which on
// most of them hides such videos — a channel would then be downloaded with
// part of it silently missing rather than visibly refused. Paging stops at a
// short page, at the cap, and at a page that adds nothing new, the last
// because a listing run past its end tends to answer with its last page
// again rather than with nothing.
func peerTubeListVideos(ctx context.Context, client *httpx.Client, endpoint string,
	headers httpx.Header, label, kind string) ([]string, error) {

	var ids []string
	seen := make(map[string]bool)

	for start := 0; start < config.MaxListingVideos; start += config.FolderPageSize {
		count := min(config.FolderPageSize, config.MaxListingVideos-start)
		query := url.Values{
			"count": {strconv.Itoa(count)},
			"start": {strconv.Itoa(start)},
			"nsfw":  {"both"},
		}

		var page peerTubeListing
		if err := client.GetJSON(ctx, endpoint+"?"+query.Encode(), headers, &page); err != nil {
			if start == 0 {
				return nil, fmt.Errorf("%s: read this %s: %w", label, kind, err)
			}
			break // a later page failing still leaves the earlier ones
		}

		added := 0
		for _, entry := range page.Data {
			id := entry.videoID()
			if id == "" || seen[id] {
				continue
			}
			seen[id] = true
			ids = append(ids, id)
			added++
		}
		if added == 0 || len(page.Data) < count {
			break
		}
	}
	return ids, nil
}

// peerTubeExpand reads every listed video's own record, several at a time.
//
// One video that will not load is skipped rather than failing the job: on a
// channel of hundreds — where a single entry may be a live stream, or still
// transcoding — the rest are still worth having.
func peerTubeExpand(ctx context.Context, client *httpx.Client, u *url.URL,
	ids []string, label string, opts Options) []File {

	return FanOut(ctx, ids, func(ctx context.Context, id string) ([]File, error) {
		files, _, err := peerTubeVideoFiles(ctx, client, u, id, label, opts)
		return files, err
	})
}

// ------------------------------------------------------------------- sniff

// peerTubeSniff resolves an instance URL whose host nobody registered. The
// second result reports whether this took ownership of the URL, in which case
// the first two are the answer and the fallback must not go on to treat the
// page as a file.
//
// This is where the platform's economics beat the KVS sniff outright. That
// one has to fetch a page and decide from its markup whether it is looking at
// the right player; this one asks /api/v1/config, which either answers with a
// server version or does not exist. Two things follow from having a real
// answer rather than a guess:
//
//   - A host that is not an instance costs one small request and falls
//     straight through to the direct-link handling that would have run
//     anyway. The path shape is checked first, so a URL that looks nothing
//     like an instance costs nothing at all.
//   - A host that is one keeps the URL even when extraction then fails.
//     Falling through there would save the watch page's HTML under the
//     video's name and call it a finished download, when what happened is
//     that the video is live, or still transcoding, or gone — all of which
//     the error says.
func peerTubeSniff(ctx context.Context, client *httpx.Client, u *url.URL, opts Options) (*Result, bool, error) {
	if peerTubeParseRoute(u).Kind == peerTubeNothing {
		return nil, false, nil
	}
	if !peerTubeIsInstance(ctx, client, u) {
		return nil, false, nil
	}
	res, err := peerTubeExtract(ctx, client, u, peerTubeLabel(u.Hostname()), opts)
	return res, true, err
}

// peerTubeIsInstance asks a host whether it runs PeerTube. serverVersion is
// what settles it: the endpoint exists on every instance and on nothing else,
// and a version string is proof the answer came from the software rather than
// from a catch-all that returns JSON for any path.
func peerTubeIsInstance(ctx context.Context, client *httpx.Client, u *url.URL) bool {
	var cfg struct {
		ServerVersion string `json:"serverVersion"`
	}
	if err := client.GetJSON(ctx, peerTubeAPI(u)+"/config", httpx.Referer(util.Origin(u)+"/"), &cfg); err != nil {
		return false
	}
	return strings.TrimSpace(cfg.ServerVersion) != ""
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
	Data []peerTubeListingEntry `json:"data"`
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
