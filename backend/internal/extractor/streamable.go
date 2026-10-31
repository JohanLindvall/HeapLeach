// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"path"
	"slices"
	"strings"

	"github.com/JohanLindvall/HeapLeach/internal/httpx"
	"github.com/JohanLindvall/HeapLeach/internal/util"
)

// Streamable video pages, a row of the Player engine whose page is the
// document the site's own player reads.
//
// That document is served to anyone: no key, no cookie, not even a
// browser-shaped user agent. It states whether the transcode is finished,
// what the video is called, how many bytes the rendition is and where to
// fetch it — everything an item needs — so none of the page's HTML is read
// at all.
//
// What shapes the rest is that the link in that document is a CloudFront one
// carrying Expires and Signature, while the document itself is cheap. So the
// row is signed and withheld: the document is read again before every
// attempt, and the queued file carries no link, since one signed while the
// item sat in a long queue would have lapsed by the time its turn came.
//
// The size, though, is published up front and as an exact Size. It does not
// expire with the link, and it is the rendition's own byte count — it agrees
// with Content-Length to the byte, rather than being a listing figure rounded
// for display — so stating it now is what gives a queued job a real total
// before anything starts, and what lets a file already in the destination be
// recognised without opening a connection. An empty URL beside a known exact
// size is an unusual combination, and that is what it is for.
//
// One field must be left alone: original_size describes the file that was
// uploaded, not the transcode that is served. For an unlucky video the two
// differ tenfold, and published as an exact size it would make the skip check
// answer about the wrong file.

// streamableAPI is the document the site's own player reads.
const streamableAPI = "https://ajax.streamable.com/videos/"

// Transcoding states. Only ready has anything to download, and the others are
// answers rather than failures — a video part-way through transcoding will be
// fetchable in a few minutes and one that failed never will be — so each is
// reported as what it is instead of being retried, since nothing this end can
// change either outcome.
const (
	streamableUploading  = 0
	streamableProcessing = 1
	streamableReady      = 2
	streamableFailed     = 3
)

var (
	// streamablePage is the watch page, /<id>, or a player under one of its
	// prefixes. Both are far too common a form to single out to the link
	// scanner — a bare slug, or the /e/<id> of every other video host — so
	// the row reports no form of its own and is found by its pages.
	streamablePage = pathShape(`^/(?:[eost]/)?[0-9a-z]{4,10}/?$`)
	// streamableEmbed is the player a watch page offers for embedding.
	streamableEmbed = pathShape(`^/[eot]/[0-9a-z]+`)
)

var streamablePlayer = playerSite{
	name:  "streamable",
	shape: func(*url.URL) bool { return false },
	// A watch page by the player it offers for embedding, which is the id
	// the document is asked for.
	recognise: func(p *Page) bool {
		return streamablePage(p.Final) && p.playerMeta(streamableEmbed) && p.Named("Streamable")
	},
	recognises: "videos (`/<id>`, `/e/<id>`) whose page, named Streamable, offers the site's player " +
		"(`/t/<id>`) for embedding",
	page:     streamableDocument(streamableAPI),
	read:     streamableRead,
	signed:   true,
	withheld: true,
}

// NewStreamable builds the streamable extractor.
func NewStreamable(client *httpx.Client) *Player { return NewPlayer(client, &streamablePlayer) }

// streamableDocument reads a link as its video's document under api.
func streamableDocument(api string) func(*url.URL) (string, error) {
	return func(u *url.URL) (string, error) {
		id := streamableID(u)
		if id == "" {
			return "", fmt.Errorf("no video id in %s", u.Redacted())
		}
		return api + url.PathEscape(id), nil
	}
}

// streamableRead reads one video's document.
//
// The endpoint needs no headers whatsoever, which is why none are sent: a
// Referer or an Origin here would be decoration, and decoration on a request
// is indistinguishable from a requirement the next reader must preserve.
func streamableRead(ctx context.Context, client *httpx.Client, doc string) (*playerVideo, error) {
	id, _ := url.PathUnescape(path.Base(doc))
	var video streamableVideo
	if err := client.GetJSON(ctx, doc, nil, &video); err != nil {
		if httpx.HasStatus(err, http.StatusNotFound) {
			// A missing video answers 404 with a line of plain text, which
			// surfaces as a JSON decode failure naming the API endpoint —
			// true, and no use to whoever pasted the link.
			return nil, fmt.Errorf("there is no video %s (it was removed, or the link is mistyped)", id)
		}
		return nil, fmt.Errorf("video %s: %w", id, err)
	}
	return streamableVideoOf(&video, id)
}

// streamableVideoOf is what one document offers.
//
// The site publishes at most two renditions, "mp4" and — for a video large
// enough to be worth one — "mp4-mobile", and the names say nothing about
// which is larger, so the stated height decides, then the size. They are
// listed in sorted order because Go randomises map iteration: one document
// must always name the same rendition, since it is read again before every
// attempt and a transfer that resumed into the other encode would be
// splicing two files together. A rendition still being made is listed with
// everything but a link, which bestCandidate passes over.
func streamableVideoOf(video *streamableVideo, id string) (*playerVideo, error) {
	if video.Status != streamableReady {
		return nil, streamableNotReady(video, id)
	}
	var candidates []mediaCandidate
	for _, name := range slices.Sorted(maps.Keys(video.Files)) {
		f := video.Files[name]
		// Zero means absent, not empty: videos from the site's early years
		// list a rendition with no length at all. original_size, one level
		// up, is no substitute — it measures a different file.
		candidates = append(candidates, mediaCandidate{URL: streamableLink(f.URL), Quality: f.Height, Size: f.Size})
	}
	if _, ok := bestCandidate(candidates); !ok {
		return nil, fmt.Errorf("%s is transcoded but offers no rendition to download", id)
	}
	return &playerVideo{title: streamableTitle(video, id), candidates: candidates}, nil
}

// streamableNotReady explains a video that is not fetchable yet, or ever.
func streamableNotReady(video *streamableVideo, id string) error {
	switch video.Status {
	case streamableUploading:
		return fmt.Errorf("%s is still uploading, so there is nothing to fetch yet", id)
	case streamableProcessing:
		return fmt.Errorf("%s is still being transcoded (%d%% done); try again shortly", id, video.Percent)
	case streamableFailed:
		return fmt.Errorf("%s failed to transcode, so the site has no video to serve", id)
	}
	return fmt.Errorf("%s is not ready (the site reports status %d)", id, video.Status)
}

// streamableTitle names the video, without an extension: the site
// transcodes everything to MP4, whatever went in.
//
// The upload's own name is the fallback rather than the first choice, and its
// extension is dropped when it is used: original_name describes what was sent
// in, so a clip uploaded as .mov would otherwise land as a .mov file holding
// the MP4 the site transcoded it into.
func streamableTitle(video *streamableVideo, id string) string {
	if name := strings.TrimSpace(video.Title); name != "" {
		return name
	}
	if name := util.Stem(strings.TrimSpace(video.OriginalName)); name != "" {
		return name
	}
	return id
}

// streamableLink absolutises a media link, which the API states without a
// scheme.
func streamableLink(raw string) string {
	if strings.HasPrefix(raw, "//") {
		return "https:" + raw
	}
	return raw
}

// streamableID reads the id out of the link shapes the site hands out.
//
// A one-letter first segment is one of the site's own views of the same
// video — /e/ is the embeddable player — so the id is the segment after it.
// Anything longer is the id itself.
func streamableID(u *url.URL) string {
	segs := util.PathSegments(u)
	if len(segs) == 0 {
		return ""
	}
	if len(segs) >= 2 && len(segs[0]) == 1 {
		return segs[1]
	}
	return segs[0]
}

// streamableVideo is the part of the API's document this reads.
type streamableVideo struct {
	Status  int `json:"status"`
	Percent int `json:"percent"`
	// Title is what the uploader called the video; OriginalName is the file
	// they uploaded.
	Title        string `json:"title"`
	OriginalName string `json:"original_name"`
	// Files are the renditions. Each states a width beside the height; only
	// the height is read, since the two order the renditions identically
	// and reading both would only invite them to disagree.
	Files map[string]struct {
		URL    string `json:"url"`
		Size   int64  `json:"size"`
		Height int    `json:"height"`
	} `json:"files"`
}
