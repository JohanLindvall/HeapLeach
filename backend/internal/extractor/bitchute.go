package extractor

import (
	"context"
	"fmt"
	"net/url"
	"path"
	"regexp"
	"strings"
	"unicode"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
	"github.com/JohanLindvall/HeapLeach/internal/util"
)

// BitChute resolves video pages and whole channels.
//
// The site's own pages are worth nothing but the id in their path. The watch
// page at www is an eight kilobyte JavaScript shell that names no media, and
// old.bitchute.com serves ninety-odd kilobytes of rendered page that names
// none either. Everything else comes from the beta API, which asks for no
// signature, token, session or key: one POST gives the media URL and a second
// gives the title. GET on those endpoints answers 405, which is worth saying
// only because it is the first thing anyone tries.
//
// What comes back is a permanent, unsigned .mp4 — nothing expires and no
// address is bound into it — so the link goes straight onto the File rather
// than behind a resolver, and an item cannot go stale waiting its turn in the
// queue.
//
// The media host cannot be derived from the id: eighty-odd videos sampled
// were spread over eighteen hosts, several of them opaque names rather than
// numbered ones. Asking is mandatory, and that is what the media endpoint —
// or, failing it, the embed page — is for.
//
// What those hosts then turn out to be is one mirror set. Every host serves
// every other host's paths byte for byte, while a path belonging to nobody
// 404s on all of them, which is what rules out a wildcard that merely answers
// anything put to it. So the file is offered from the whole set: a rotation
// that cannot pick a wrong source, costs nothing while the first host answers,
// and covers the one real flake seen under parallel load — a transient nginx
// 404 that succeeded on immediate retry.
type BitChute struct {
	hostSet
	client *httpx.Client
	// api and site are the two roots this reads, kept as fields rather than
	// used as constants so the paging, the fan-out and the embed fallback
	// can be exercised against a stand-in server. Testing them against the
	// live host would mean hundreds of requests to prove behaviour that is
	// entirely ours.
	api  string
	site string
}

const (
	bitchuteRoot = "https://www.bitchute.com"
	bitchuteAPI  = "https://api.bitchute.com/api/beta"

	// bitchutePageSize is the channel listing's stride.
	//
	// The endpoint takes a limit and caps it here without saying so: asked
	// for 100 or 200 it answers with a byte-identical fifty-record body. It
	// publishes no total either, so a short page is the only end-of-listing
	// signal there is.
	bitchutePageSize = 50
)

// bitchuteSeedHosts are the media hosts, each of which serves any path the
// others do. See the type comment for what that was measured against.
//
// Only the numbered names are listed. The opaque hosts seen beside them —
// machine-shaped strings rather than a name anyone chose — serve exactly the
// same content, but a list compiled into a binary wants entries likely to
// outlive the release, and an allocated-looking name is the wrong bet. A
// video served from one of those still leads with it, because MirrorHosts
// keeps the original link first.
var bitchuteSeedHosts = []string{
	"seed122.bitchute.com",
	"seed125b.bitchute.com",
	"seed126b.bitchute.com",
	"seed128.bitchute.com",
	"seed132.bitchute.com",
	"seed163.bitchute.com",
	"seed167b.bitchute.com",
	"seed171.bitchute.com",
	"seed177b.bitchute.com",
	"seed305.bitchute.com",
	"seed306b.bitchute.com",
	"seed307.bitchute.com",
}

// NewBitChute builds the bitchute extractor.
func NewBitChute(client *httpx.Client) *BitChute {
	return &BitChute{hostSet: hostSet{"bitchute.com"}, client: client, api: bitchuteAPI, site: bitchuteRoot}
}

func (b *BitChute) Name() string { return "bitchute" }

// Extract resolves a video page or a channel. The embed page is accepted as
// well: it names the same video, and it is the shape a link copied out of an
// iframe has.
func (b *BitChute) Extract(ctx context.Context, u *url.URL, _ Options) (*Result, error) {
	segs := util.PathSegments(u)
	if len(segs) >= 2 {
		switch segs[0] {
		case "video", "embed":
			return b.video(ctx, segs[1])
		case "channel":
			return b.channel(ctx, segs[1])
		}
	}
	return nil, fmt.Errorf("bitchute: %s is neither a video (/video/<id>/) nor a channel (/channel/<id>/)",
		u.Redacted())
}

// ------------------------------------------------------------------ video

// video resolves one video.
func (b *BitChute) video(ctx context.Context, id string) (*Result, error) {
	media, err := b.media(ctx, id)
	if err != nil {
		return nil, err
	}
	name := b.videoName(ctx, id)
	file, err := b.file(ctx, name, media)
	if err != nil {
		return nil, err
	}
	return &Result{Title: name, Files: []File{file}}, nil
}

// file turns one media answer into a downloadable entry.
//
// Size stays -1 deliberately. The API publishes no length anywhere, and the
// only way to fill one in would be to HEAD every video — doubling the request
// count of a channel job to learn what the transfer discovers by itself from
// the first response header.
func (b *BitChute) file(ctx context.Context, name string, media bitchuteMedia) (File, error) {
	if media.URL == "" {
		return File{}, fmt.Errorf("bitchute: %q has no media url", name)
	}
	headers := httpx.Referer(bitchuteRoot + "/")

	// Adaptive delivery is not a branch anything here is known to take: every
	// one of 136 videos sampled was a progressive MP4, and the API called
	// each of them "MPEG-4". These few lines exist so a video that is not
	// still downloads rather than landing a playlist on disk.
	if bitchuteIsPlaylist(media) {
		segments, _, err := resolvePlaylist(ctx, b.client, media.URL, headers)
		if err != nil {
			return File{}, fmt.Errorf("bitchute: %w", err)
		}
		return File{Name: name + ".ts", Size: -1, Headers: headers, Segments: segments}, nil
	}

	return Mirrored(File{
		Name:    name + bitchuteExt(media.URL),
		Size:    -1,
		Headers: headers,
	}, MirrorHosts(media.URL, bitchuteSeedHosts)), nil
}

// media asks the API where a video is, falling back to the embed page.
//
// The fallback is not for a video the API cannot find — that one is missing
// either way — but for the API itself moving out from under us, which a route
// with "beta" in its path invites. The embed page states the same URL inline
// and needs no session, so it is a whole second route to the media for ten
// lines. When both come up empty the API's own complaint is reported, since
// that is the one that says why.
func (b *BitChute) media(ctx context.Context, id string) (bitchuteMedia, error) {
	var out bitchuteMedia
	err := b.client.PostJSON(ctx, b.api+"/video/media", bitchuteAPIHeaders(),
		bitchuteVideoRequest{VideoID: id}, &out)
	if err == nil && out.URL != "" {
		return out, nil
	}
	if link := b.embedMedia(ctx, id); link != "" {
		return bitchuteMedia{URL: link}, nil
	}
	if err == nil {
		return bitchuteMedia{}, fmt.Errorf("bitchute: %s: the API named no media url", id)
	}
	return bitchuteMedia{}, fmt.Errorf("bitchute: %s: %w", id, err)
}

// bitchuteEmbedMedia finds the media URL the embed page assigns inline.
//
// It is anchored on the variable's own name rather than on the shape of an
// assignment, because the surrounding script assigns a thumbnail URL two
// lines above it and carries a commented-out advertising URL below — either
// of which a pattern matching "some url being assigned" would take instead.
var bitchuteEmbedMedia = regexp.MustCompile(`\bmedia_url\s*=\s*['"]([^'"]+)['"]`)

// embedMedia reads the embed page for the media URL, or "" when it has none.
//
// Only www serves this: old.bitchute.com answers the same path with a page
// that carries no media URL at all, so there is nothing to fall back to there.
func (b *BitChute) embedMedia(ctx context.Context, id string) string {
	page := b.site + "/embed/" + url.PathEscape(id) + "/"
	doc, err := b.client.GetString(ctx, page, httpx.Referer(bitchuteRoot+"/"))
	if err != nil {
		return ""
	}
	if m := bitchuteEmbedMedia.FindStringSubmatch(doc); m != nil {
		return strings.TrimSpace(m[1])
	}
	return ""
}

// videoName reads a video's own title.
//
// A failure here is not fatal and must not be: the media is already resolved
// by the time this is asked, and a file named after the id downloads exactly
// as well as one named after the video.
func (b *BitChute) videoName(ctx context.Context, id string) string {
	var out struct {
		Name string `json:"video_name"`
	}
	if err := b.client.PostJSON(ctx, b.api+"/video", bitchuteAPIHeaders(),
		bitchuteVideoRequest{VideoID: id}, &out); err == nil {
		if name := strings.TrimSpace(out.Name); name != "" {
			return name
		}
	}
	return "bitchute-" + id
}

// ---------------------------------------------------------------- channel

// channel expands a channel into every video it has published.
func (b *BitChute) channel(ctx context.Context, segment string) (*Result, error) {
	videos, err := b.channelVideos(ctx, segment)
	if err != nil {
		return nil, err
	}
	if len(videos) == 0 {
		return nil, fmt.Errorf("bitchute: channel %s lists no videos", segment)
	}

	files := b.expand(ctx, videos)
	if len(files) == 0 {
		return nil, fmt.Errorf("bitchute: none of the %d videos on channel %s could be resolved",
			len(videos), segment)
	}
	return &Result{Title: b.channelName(ctx, segment), Files: files}, nil
}

// channelVideos pages through a channel's listing.
//
// The path component is passed to the API untouched, because the endpoint
// takes a channel's legacy slug as readily as its id, and case-insensitively
// at that — so whatever spelling the link carried is already the right one.
//
// Paging stops at the first short page, which is the only end-of-listing
// signal on offer: the answer holds nothing but the records themselves, no
// total and no cursor. A page that fails after the first breaks out rather
// than sinking the job, since most of a large channel is still worth having.
//
// The page backstop is doing real work here rather than guarding a pathology.
// Channels on this site run to tens of thousands of videos — the largest in a
// sample of eighty-odd was 79,351 — so MaxAlbumPages is what actually decides
// how much of one a job takes: the newest MaxAlbumPages × bitchutePageSize of
// them, newest first, which is the order the listing itself gives.
func (b *BitChute) channelVideos(ctx context.Context, segment string) ([]bitchuteVideo, error) {
	var all []bitchuteVideo

	for page := range config.MaxAlbumPages {
		var out struct {
			Videos []bitchuteVideo `json:"videos"`
		}
		req := bitchuteChannelRequest{
			ChannelID: segment,
			Offset:    page * bitchutePageSize,
			Limit:     bitchutePageSize,
		}
		if err := b.client.PostJSON(ctx, b.api+"/channel/videos", bitchuteAPIHeaders(), req, &out); err != nil {
			if page == 0 {
				return nil, fmt.Errorf("bitchute: list channel %s: %w", segment, err)
			}
			break
		}
		all = append(all, out.Videos...)
		if len(out.Videos) < bitchutePageSize {
			break
		}
	}
	return all, nil
}

// channelName names the job, and so the folder every video lands in.
//
// This is one request more than the recipe needs, and it earns it: the
// listing carries the videos and nothing about the channel, so without it a
// modern channel's folder would be named by the opaque id in its URL. It is
// asked for only once per job, and a failure falls back to that id rather
// than failing anything.
func (b *BitChute) channelName(ctx context.Context, segment string) string {
	var out struct {
		Name string `json:"channel_name"`
	}
	if err := b.client.PostJSON(ctx, b.api+"/channel", bitchuteAPIHeaders(),
		bitchuteChannelRequest{ChannelID: segment}, &out); err == nil {
		if name := strings.TrimSpace(out.Name); name != "" {
			return name
		}
	}
	return segment
}

// expand resolves every listed video's media, several at a time.
//
// The listing does not carry media URLs, so this is one request per video and
// the cost is linear in the size of the channel — which, at the sizes above,
// is the slowest thing this extractor does. Running them several at a time is
// what keeps a job of a few hundred to seconds rather than minutes; the bound
// is deliberately low, since this is a burst of small requests at one host.
//
// Results are collected by index rather than appended as they arrive, so the
// queue follows the channel's own order however the requests interleave, and
// a video that will not resolve is dropped rather than failing the rest.
func (b *BitChute) expand(ctx context.Context, videos []bitchuteVideo) []File {
	return FanOut(ctx, videos, func(ctx context.Context, video bitchuteVideo) ([]File, error) {
		media, err := b.media(ctx, video.ID)
		if err != nil {
			return nil, err
		}
		file, err := b.file(ctx, bitchuteVideoName(video), media)
		if err != nil {
			return nil, err
		}
		return []File{file}, nil
	})
}

// ----------------------------------------------------------------- shared

// bitchuteAPIHeaders are what the site's own scripts send. The API lives on
// its own host, so a browser reaching it always states where it came from.
func bitchuteAPIHeaders() httpx.Header {
	return httpx.RefererOrigin(bitchuteRoot+"/", bitchuteRoot)
}

// bitchuteVideoName is a listed video's title, falling back to its id.
func bitchuteVideoName(video bitchuteVideo) string {
	return util.FirstNonEmpty(strings.TrimSpace(video.Name), "bitchute-"+video.ID)
}

// bitchuteExt keeps whatever extension the media URL carries.
//
// The checks are there because this is being read off a remote path rather
// than off a filename: something too long, or not starting with a letter, is
// a path that happens to contain a dot and not a file type. .mp4 is both the
// safer guess and what every sampled video actually was.
func bitchuteExt(link string) string {
	ext := path.Ext(util.NameFromURL(link))
	if len(ext) < 2 || len(ext) > 5 || !unicode.IsLetter(rune(ext[1])) {
		return ".mp4"
	}
	return ext
}

// bitchuteIsPlaylist recognises media that arrives as a playlist rather than
// as one file. Both the declared type and the link are consulted, since a
// host changing one without the other is likelier than changing both.
func bitchuteIsPlaylist(media bitchuteMedia) bool {
	kind := strings.ToLower(media.Type)
	if strings.Contains(kind, "hls") || strings.Contains(kind, "mpegurl") {
		return true
	}
	return strings.Contains(strings.ToLower(media.URL), ".m3u8")
}

// ------------------------------------------------------------ API payloads

// bitchuteVideoRequest is the body both video endpoints take.
type bitchuteVideoRequest struct {
	VideoID string `json:"video_id"`
}

// bitchuteChannelRequest is the body the channel endpoints take. Offset and
// limit are ignored by the metadata endpoint and required by the listing.
type bitchuteChannelRequest struct {
	ChannelID string `json:"channel_id"`
	Offset    int    `json:"offset"`
	Limit     int    `json:"limit"`
}

// bitchuteMedia is the media endpoint's answer.
type bitchuteMedia struct {
	Type string `json:"media_type"`
	URL  string `json:"media_url"`
}

// bitchuteVideo is one record of a channel listing.
type bitchuteVideo struct {
	ID   string `json:"video_id"`
	Name string `json:"video_name"`
}
