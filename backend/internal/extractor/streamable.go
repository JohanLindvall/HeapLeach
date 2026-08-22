package extractor

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"slices"
	"strings"

	"github.com/JohanLindvall/HeapLeach/internal/httpx"
	"github.com/JohanLindvall/HeapLeach/internal/util"
)

// Streamable resolves streamable.com video pages.
//
// The site's own player reads one JSON document per video, and that document
// is served to anyone: no key, no cookie, not even a browser-shaped user
// agent. It states whether the transcode is finished, what the video is
// called, how many bytes the rendition is and where to fetch it — everything
// an item needs — so none of the page's HTML is read at all.
//
// What shapes the rest is that the link in that document is a CloudFront one
// carrying Expires and Signature, while the document itself is cheap. So the
// whole request lives inside Resolve and runs again before every attempt, and
// File.URL is left empty: a link signed while the item sat in a long queue
// would have lapsed by the time its turn came.
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
type Streamable struct {
	client *httpx.Client
	// api is where the document is read from, kept as a field so the whole
	// path — including a resolver that must come back with a *different*
	// link each time it is called — can be tested against a stub. Nothing
	// but a test ever sets it to anything else.
	api string
}

const (
	// streamableSite is the only host that serves video pages, and matching
	// is an allowlist against it rather than the usual subdomain test: the
	// media comes from cdn-*.streamable.com and this metadata from ajax.,
	// and neither belongs here. A CDN link is already a direct link, and
	// putting one through this extractor would turn a working URL into a
	// lookup of a video id that does not exist.
	streamableSite = "streamable.com"

	// streamableAPI is the document the site's own player reads.
	streamableAPI = "https://ajax.streamable.com/videos/"

	// streamableExt is what the site transcodes everything to, whatever
	// went in.
	streamableExt = ".mp4"
)

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

// NewStreamable builds the streamable extractor.
func NewStreamable(client *httpx.Client) *Streamable {
	return &Streamable{client: client, api: streamableAPI}
}

func (s *Streamable) Name() string { return "streamable" }

func (s *Streamable) Match(u *url.URL) bool {
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	return host == streamableSite || host == "www."+streamableSite
}

// Extract resolves a video page to its best rendition.
func (s *Streamable) Extract(ctx context.Context, u *url.URL, _ Options) (*Result, error) {
	id := streamableID(u)
	if id == "" {
		return nil, fmt.Errorf("streamable: no video id in %s", u.Redacted())
	}

	// Read the document once now, so a video that does not exist or is not
	// finished fails the job immediately rather than at the head of the
	// queue, and so the item shows its real name and length while it waits.
	video, err := s.fetch(ctx, id)
	if err != nil {
		return nil, err
	}
	target, err := streamableTarget(video, id)
	if err != nil {
		return nil, err
	}

	return &Result{
		Title: streamableTitle(video, id),
		Files: []File{{
			Name: target.Name,
			// The link this document carries is deliberately dropped rather
			// than published here: it is signed, and would go stale while
			// the item waited its turn. Resolve fetches a fresh one before
			// each attempt. The size is published, and as an exact figure —
			// see the type's comment for why that pairing is the point.
			Size: target.Size,
			Resolve: func(ctx context.Context) (*Target, error) {
				fresh, err := s.fetch(ctx, id)
				if err != nil {
					return nil, err
				}
				return streamableTarget(fresh, id)
			},
		}},
	}, nil
}

// fetch reads one video's document.
//
// The endpoint needs no headers whatsoever, which is why none are sent: a
// Referer or an Origin here would be decoration, and decoration on a request
// is indistinguishable from a requirement the next reader must preserve.
func (s *Streamable) fetch(ctx context.Context, id string) (*streamableVideo, error) {
	var video streamableVideo
	if err := s.client.GetJSON(ctx, s.api+url.PathEscape(id), nil, &video); err != nil {
		if httpx.HasStatus(err, http.StatusNotFound) {
			// A missing video answers 404 with a line of plain text, which
			// surfaces as a JSON decode failure naming the API endpoint —
			// true, and no use to whoever pasted the link.
			return nil, fmt.Errorf("streamable: there is no video %s "+
				"(it was removed, or the link is mistyped)", id)
		}
		return nil, fmt.Errorf("streamable: video %s: %w", id, err)
	}
	return &video, nil
}

// streamableTarget turns one document into the link to fetch. Extraction and
// every retry go through it, so the two can never disagree about which
// rendition this file is.
func streamableTarget(video *streamableVideo, id string) (*Target, error) {
	if video.Status != streamableReady {
		return nil, streamableNotReady(video, id)
	}
	best, ok := streamableBest(video.Files)
	if !ok {
		return nil, fmt.Errorf("streamable: %s is transcoded but offers no rendition to download", id)
	}

	// Zero here means absent, not empty: videos from the site's early years
	// list a rendition with no length at all. original_size, one level up,
	// is no substitute — it measures a different file.
	size := best.Size
	if size <= 0 {
		size = -1
	}
	return &Target{
		URL:  streamableLink(best.URL),
		Name: streamableTitle(video, id) + streamableExt,
		Size: size,
	}, nil
}

// streamableBest picks the rendition to fetch.
//
// The site publishes at most two, "mp4" and — for a video large enough to be
// worth one — "mp4-mobile", and the names say nothing about which is larger,
// so the stated height decides. The map is walked in sorted key order because
// Go randomises map iteration: one document must always name the same
// rendition, since Resolve re-reads it before every attempt and a transfer
// that resumed into the other encode would be splicing two files together.
func streamableBest(files map[string]streamableFile) (streamableFile, bool) {
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	slices.Sort(names)

	var best streamableFile
	found := false
	for _, name := range names {
		f := files[name]
		// A rendition still being made is listed with its dimensions and
		// everything else but a link, so the link is the test. Its own
		// status field is not: the videos that omit a size omit that too.
		if f.URL == "" {
			continue
		}
		if !found || f.Height > best.Height || (f.Height == best.Height && f.Size > best.Size) {
			best, found = f, true
		}
	}
	return best, found
}

// streamableNotReady explains a video that is not fetchable yet, or ever.
func streamableNotReady(video *streamableVideo, id string) error {
	switch video.Status {
	case streamableUploading:
		return fmt.Errorf("streamable: %s is still uploading, so there is nothing to fetch yet", id)
	case streamableProcessing:
		return fmt.Errorf("streamable: %s is still being transcoded (%d%% done); try again shortly",
			id, video.Percent)
	case streamableFailed:
		return fmt.Errorf("streamable: %s failed to transcode, so the site has no video to serve", id)
	}
	return fmt.Errorf("streamable: %s is not ready (the site reports status %d)", id, video.Status)
}

// streamableTitle names the video, without an extension.
//
// The upload's own name is the fallback rather than the first choice, and its
// extension is dropped when it is used: original_name describes what was sent
// in, so a clip uploaded as .mov would otherwise land as a .mov file holding
// the MP4 the site transcoded it into.
func streamableTitle(video *streamableVideo, id string) string {
	name := strings.TrimSpace(video.Title)
	if name == "" {
		upload := strings.TrimSpace(video.OriginalName)
		name = strings.TrimSuffix(upload, path.Ext(upload))
	}
	return util.FirstNonEmpty(name, id)
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
	Title        string                    `json:"title"`
	OriginalName string                    `json:"original_name"`
	Files        map[string]streamableFile `json:"files"`
}

// streamableFile is one rendition. The document states a width beside the
// height; only the height is read, since the two order the renditions
// identically and reading both would only invite them to disagree.
type streamableFile struct {
	URL    string `json:"url"`
	Size   int64  `json:"size"`
	Height int    `json:"height"`
}
