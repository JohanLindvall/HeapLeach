package extractor

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path"
	"slices"
	"strconv"
	"strings"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
	"github.com/JohanLindvall/HeapLeach/internal/util"
)

// BBCSounds resolves bbc.co.uk/sounds episodes, and the brands and series
// they belong to.
//
// The BBC publishes the radio metadata service its own player uses, and it
// answers anonymously: one GET names an episode, its titles, the container it
// belongs to and — the point of this extractor — what may be downloaded. That
// much is svtplay and nrk a third time. Three things here are this host's own.
//
// The first is what "drm" means in this API, and reading it the obvious way
// would refuse most of the catalogue for nothing. Every playable item carries
// a `download.type` of `non_drm` or `drm`, and the second is a *licence* flag
// rather than a statement about the bytes: an item marked `drm` streams a
// completely clear playlist — not one EXT-X-KEY line in it, plain MPEG-TS
// parts — and what the flag actually withholds is the offline copy, whose
// `file_url` comes back null beside it. So nothing below reads that field at
// all. A published URL is downloaded and a withheld one is not, which is the
// same fact without the chance of misreading it, and it also settles the
// other direction: no URL is ever minted for an item the BBC declined to
// publish one for.
//
// The second is which route that leads to. Speech and podcast items publish
// an ordinary MP3 behind an unsigned redirect, and that is much the better
// thing to hand the downloader — a rangeable file gets the segmented engine,
// resume across runs and the skip check, none of which a segment list can
// have. The redirect is stable and only what it lands on is signed, so it
// needs no Resolve: a retry re-follows it and is signed afresh. Only where no
// such URL was published does this fall back to the media selector's HLS,
// which is audio alone — so hlsVariant.muxed() has nothing to decide here —
// and which renders exactly one 48 kbps variant whatever bitrate the media
// record claims for itself.
//
// The third is the naming, and it is the reason bbcTitles has two methods
// instead of a field. The BBC names a programme in up to three levels, and
// which of them is the episode's own name depends on how many were filled in.
type BBCSounds struct {
	client *httpx.Client
}

const (
	bbcSoundsRoot = "https://www.bbc.co.uk/sounds"

	// bbcProgrammesAPI answers /<episodePid>/playable with one item, and
	// /playable?container=<pid> with a container's listing.
	bbcProgrammesAPI = "https://rms.api.bbc.co.uk/v2/programmes/"
	bbcListingAPI    = bbcProgrammesAPI + "playable"

	// bbcMediaSelector resolves a version pid to the streams on offer.
	// "iptv-all" is the mediaset the player itself asks for.
	bbcMediaSelector = "https://open.live.bbc.co.uk/mediaselector/6/select/version/2.0/mediaset/iptv-all/vpid/"
)

// NewBBCSounds builds the BBC Sounds extractor.
func NewBBCSounds(client *httpx.Client) *BBCSounds { return &BBCSounds{client: client} }

func (b *BBCSounds) Name() string { return "bbcsounds" }

// Match takes the Sounds section of bbc.co.uk and nothing else on it. The
// rest of the domain is news, iPlayer and a great deal besides that nothing
// here resolves, and the two API hosts this talks to are never pasted by
// anyone.
func (b *BBCSounds) Match(u *url.URL) bool {
	if !util.HostMatches(u.Host, "bbc.co.uk") {
		return false
	}
	segs := util.PathSegments(u)
	return len(segs) > 0 && strings.EqualFold(segs[0], "sounds")
}

// Extract handles one episode, or a brand or series expanded into its
// episodes.
func (b *BBCSounds) Extract(ctx context.Context, u *url.URL, _ Options) (*Result, error) {
	segs := util.PathSegments(u)
	if len(segs) < 3 {
		return nil, fmt.Errorf("bbcsounds: %s names nothing to download "+
			"(expected /sounds/play/<pid>, /sounds/brand/<pid> or /sounds/series/<pid>)", u.Redacted())
	}

	switch id := segs[2]; strings.ToLower(segs[1]) {
	case "play":
		if bbcIsLive(id) {
			return nil, fmt.Errorf("bbcsounds: %s is a live radio stream, which has no end to download "+
				"(a station's schedule page links the episodes it has already broadcast)", u.Redacted())
		}
		return b.episode(ctx, id)
	case "brand", "series":
		return b.container(ctx, id)
	}
	return nil, fmt.Errorf("bbcsounds: %s is a page of the site rather than a programme "+
		"(expected /sounds/play/<pid>, /sounds/brand/<pid> or /sounds/series/<pid>)", u.Redacted())
}

// bbcIsLive recognises a station's live stream, which the site addresses in
// the same position as an episode pid. Both spellings occur: the link carries
// a colon and the site redirects it to a path segment of its own.
func bbcIsLive(id string) bool {
	id = strings.ToLower(id)
	return id == "live" || strings.HasPrefix(id, "live:")
}

// episode resolves one episode pid into a job of its own.
func (b *BBCSounds) episode(ctx context.Context, pid string) (*Result, error) {
	var item bbcItem
	if err := b.client.GetJSON(ctx, bbcProgrammesAPI+url.PathEscape(pid)+"/playable",
		httpx.Referer(bbcSoundsRoot+"/"), &item); err != nil {
		return nil, fmt.Errorf("bbcsounds: %s: %w", pid, err)
	}

	file, err := b.file(ctx, item)
	if err != nil {
		return nil, err
	}
	// A lone file lands straight in the download directory with no folder to
	// say what it belongs to, so every level of the name is used: a Radio 4
	// episode title on its own is routinely just a date.
	title := util.FirstNonEmpty(bbcName(item.Titles.full()), pid)
	file.Name = title + file.Name
	return &Result{Title: title, Files: []File{*file}}, nil
}

// container expands a brand or a series into one file per episode.
func (b *BBCSounds) container(ctx context.Context, pid string) (*Result, error) {
	items, err := b.listing(ctx, pid)
	if err != nil {
		return nil, err
	}
	if len(items) == 0 {
		return nil, fmt.Errorf("bbcsounds: %s lists no available episodes", pid)
	}

	// Every row names its container, so the job's folder is named without a
	// request of its own.
	title := util.FirstNonEmpty(bbcName(items[0].Container.Title), bbcName(items[0].Titles.Primary), pid)

	// A listing of downloadable episodes is already complete — each row
	// carries its own file_url — and one of streamed episodes needs a media
	// selection and two playlists each. Fetched one after another that is
	// minutes before a byte moves, so they go out several at a time, in the
	// listing's order.
	resolved := FanOut(ctx, items, func(ctx context.Context, item bbcItem) ([]bbcResolved, error) {
		file, err := b.file(ctx, item)
		if err != nil {
			return []bbcResolved{{refusal: err}}, nil
		}
		// The folder is named for the container, which is the first level of
		// every title in it, so the file is named by what lies below it.
		file.Name = util.FirstNonEmpty(bbcName(item.Titles.within()), item.pid()) + file.Name
		return []bbcResolved{{file: file}}, nil
	})

	result := &Result{Title: title}
	var (
		refused *bbcRefused
		failed  error
	)
	for _, r := range resolved {
		if r.file != nil {
			result.Files = append(result.Files, *r.file)
			continue
		}
		// One refused episode must not sink the rest, but its reason is kept:
		// when every episode is refused it is refused for the same reason,
		// and that reason is the only thing the caller can act on. A refusal
		// outranks an ordinary failure, since it is the one that explains a
		// container that resolves to nothing at all.
		if refused == nil {
			refused, _ = errors.AsType[*bbcRefused](r.refusal)
		}
		if failed == nil {
			failed = r.refusal
		}
	}
	if len(result.Files) == 0 {
		reason := "the media selector named no stream for any of them"
		switch {
		case refused != nil:
			reason = refused.reason()
		case failed != nil:
			// The failure already opens with this extractor's name, and the
			// sentence it is being folded into does too.
			reason = strings.TrimPrefix(failed.Error(), "bbcsounds: ")
		}
		return nil, fmt.Errorf("bbcsounds: none of the %d episodes of %s are playable: %s",
			len(items), title, reason)
	}
	return result, nil
}

// bbcResolved is one listing row after resolving: a file, or the reason there
// is not one. FanOut drops errors, and here the reason is worth keeping — a
// container refused for one reason should say that reason once rather than a
// hundred times — so it travels as a value instead.
type bbcResolved struct {
	file    *File
	refusal error
}

// listing pages a container's episodes.
//
// The endpoint caps a page at a hundred rows: a larger limit is echoed back
// and then ignored, which is exactly config.FolderPageSize, so paging is by
// the count actually returned rather than by the limit asked for.
func (b *BBCSounds) listing(ctx context.Context, container string) ([]bbcItem, error) {
	var items []bbcItem
	for offset := 0; ; {
		query := url.Values{
			"container": {container},
			// The order the site itself lists a container in.
			"sort":   {"sequential"},
			"type":   {"episode"},
			"limit":  {strconv.Itoa(config.FolderPageSize)},
			"offset": {strconv.Itoa(offset)},
		}
		var page bbcListing
		if err := b.client.GetJSON(ctx, bbcListingAPI+"?"+query.Encode(),
			httpx.Referer(bbcSoundsRoot+"/"), &page); err != nil {
			return nil, fmt.Errorf("bbcsounds: episodes of %s: %w", container, err)
		}
		if len(page.Data) == 0 {
			return items, nil
		}
		items = append(items, page.Data...)
		offset += len(page.Data)
		if offset >= page.Total || len(items) >= config.MaxListingVideos {
			// A strand like Newshour runs to several thousand editions, which
			// is a mistake being carried out efficiently rather than a
			// download. An hour of audio each is the same shape as a channel
			// of videos, so it is bounded the same way.
			return items[:min(len(items), config.MaxListingVideos)], nil
		}
	}
}

// file turns one playable item into something to download. The returned File
// carries only the extension in Name; the caller supplies the stem, which
// differs between a lone episode and one row of a container.
func (b *BBCSounds) file(ctx context.Context, item bbcItem) (*File, error) {
	headers := httpx.Referer(bbcSoundsRoot + "/")

	// file_size is rounded to two significant figures for the label printed
	// beside it — 54000000 declared against 53098695 delivered — so it may
	// total a job but must never conclude that a file already on disk is this
	// one.
	if variant, ok := item.Download.best(); ok {
		return &File{
			Name:       bbcDownloadExtension(variant.FileURL),
			URL:        variant.FileURL,
			Size:       variant.size(),
			SizeApprox: variant.size() > 0,
			Headers:    headers,
		}, nil
	}

	// Without a download the stream is all there is, and it is keyed on the
	// version rather than on the episode.
	if item.ID == "" {
		return nil, fmt.Errorf("bbcsounds: %s names no version to play", item.pid())
	}
	stream, err := b.stream(ctx, item.ID)
	if err != nil {
		return nil, err
	}
	segments, variant, err := resolvePlaylist(ctx, b.client, stream, headers)
	if err != nil {
		return nil, fmt.Errorf("bbcsounds: %s: %w", item.pid(), err)
	}
	return &File{
		Name:     playlistExtension(variant),
		Size:     -1,
		Headers:  headers,
		Segments: segments,
	}, nil
}

// stream asks the media selector for a version's playlist.
//
// This is also where a refusal surfaces, and it is the only place it can:
// the metadata API answers everywhere and says nothing about territory, so an
// item reads as perfectly playable right up to the point where this endpoint
// declines it.
func (b *BBCSounds) stream(ctx context.Context, vpid string) (string, error) {
	var doc bbcSelection
	if err := b.client.GetJSON(ctx, bbcMediaSelector+url.PathEscape(vpid)+"/format/json",
		httpx.Referer(bbcSoundsRoot+"/"), &doc); err != nil {
		if refusal := bbcRefusal(vpid, err); refusal != nil {
			return "", refusal
		}
		return "", fmt.Errorf("bbcsounds: media selector for %s: %w", vpid, err)
	}
	if doc.Result != "" {
		return "", &bbcRefused{pid: vpid, result: doc.Result}
	}
	if href := doc.stream(); href != "" {
		return href, nil
	}
	return "", fmt.Errorf("bbcsounds: %s: the media selector named no HTTPS playlist", vpid)
}

// bbcRefusal reads the media selector's own word for a refusal out of a
// failed request.
//
// The endpoint answers 403 or 404 with one machine-readable field and a
// paragraph of legal boilerplate, so the status alone throws away the only
// fact worth having. Neither status is retried by the client, which is what
// makes the body available here at all.
func bbcRefusal(pid string, err error) error {
	status, ok := errors.AsType[*httpx.StatusError](err)
	if !ok || status.Body == "" {
		return nil
	}
	var doc struct {
		Result string `json:"result"`
	}
	if json.Unmarshal([]byte(status.Body), &doc) != nil || doc.Result == "" {
		return nil
	}
	return &bbcRefused{pid: pid, result: doc.Result}
}

// bbcRefused is the media selector declining to name a stream.
//
// It is a type rather than a message so that expanding a container can report
// the reason once instead of the same refusal several hundred times. Unlike
// NRK, which refuses in a sentence written for a viewer and worth passing
// through untouched, the BBC refuses in a token: "selectionunavailable" is
// not an answer to give anybody, so it is turned into one here.
type bbcRefused struct {
	pid    string
	result string
}

func (e *bbcRefused) Error() string { return "bbcsounds: " + e.pid + ": " + e.reason() }

func (e *bbcRefused) reason() string {
	switch strings.ToLower(e.result) {
	case "geolocation":
		return "BBC Sounds offers this inside the United Kingdom only"
	case "selectionunavailable":
		return "BBC Sounds holds no stream for this any more — its availability window has closed"
	}
	return "the media selector refused it: " + e.result
}

// bbcListing is one page of a container's episodes.
type bbcListing struct {
	Total int       `json:"total"`
	Data  []bbcItem `json:"data"`
}

// bbcItem is one playable item, whether fetched on its own or read out of a
// container listing: the two are the same document, which is what makes a
// container of downloadable episodes cost a single request.
type bbcItem struct {
	// ID is the VERSION pid and Urn carries the EPISODE pid. They differ, and
	// the difference is load-bearing: the page URL and the metadata endpoint
	// are keyed on the episode, the media selector and the download URL on
	// the version.
	ID       string      `json:"id"`
	Urn      string      `json:"urn"`
	Titles   bbcTitles   `json:"titles"`
	Download bbcDownload `json:"download"`
	// Container is the brand or series this belongs to, and is present on
	// every row of a listing as well as on an item of its own.
	Container struct {
		ID    string `json:"id"`
		Title string `json:"title"`
	} `json:"container"`
}

// pid is the episode pid, for error messages. The urn is a colon-separated
// path whose last element is the id.
func (i bbcItem) pid() string {
	if idx := strings.LastIndex(i.Urn, ":"); idx >= 0 {
		return i.Urn[idx+1:]
	}
	return util.FirstNonEmpty(i.Urn, i.ID)
}

// bbcDownload is the offline copy, where one is offered.
type bbcDownload struct {
	// Type reads `non_drm` or `drm`, and nothing here reads it — see the note
	// on the extractor. It is kept because leaving it out would invite the
	// next reader to add it back and branch on it.
	Type            string                `json:"type"`
	QualityVariants map[string]bbcVariant `json:"quality_variants"`
}

// bbcVariant is one offered quality of the offline copy.
type bbcVariant struct {
	Bitrate  int    `json:"bitrate"`
	FileURL  string `json:"file_url"`
	FileSize int64  `json:"file_size"`
}

// size is the declared length, or -1 where none was declared.
func (v bbcVariant) size() int64 {
	if v.FileSize <= 0 {
		return -1
	}
	return v.FileSize
}

// bbcQualityOrder ranks the tiers the BBC keys its variants by. It is only
// the tie-break: the bitrate decides, and these are named rather than listed,
// so a map iteration would otherwise pick a different one each run.
var bbcQualityOrder = []string{"high", "medium", "low"}

// best is the largest copy a URL was published for, and reports whether there
// was one at all.
//
// An item that may not be downloaded still lists every tier, with null where
// the URL would be, so the presence of a URL is the whole question. Three
// tiers pointing at one 128 kbps file is the ordinary case for speech.
func (d bbcDownload) best() (bbcVariant, bool) {
	names := make([]string, 0, len(d.QualityVariants))
	for name, variant := range d.QualityVariants {
		if strings.TrimSpace(variant.FileURL) != "" {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return bbcVariant{}, false
	}
	slices.SortFunc(names, func(x, y string) int {
		a, b := d.QualityVariants[x], d.QualityVariants[y]
		return cmp.Or(
			cmp.Compare(b.Bitrate, a.Bitrate), // highest first
			cmp.Compare(bbcQualityRank(x), bbcQualityRank(y)),
			strings.Compare(x, y),
		)
	})
	return d.QualityVariants[names[0]], true
}

// bbcQualityRank places a tier name. One nobody here has seen ranks last but
// is still taken, since a URL that was published is a URL that works.
func bbcQualityRank(name string) int {
	for i, known := range bbcQualityOrder {
		if strings.EqualFold(name, known) {
			return i
		}
	}
	return len(bbcQualityOrder)
}

// bbcSelection is the media selector's answer.
type bbcSelection struct {
	Media []bbcMedia `json:"media"`
	// Result is the machine-readable refusal, which arrives instead of media.
	Result string `json:"result"`
}

// bbcMedia is one thing the selector offers, in every delivery it offers it.
type bbcMedia struct {
	Kind       string          `json:"kind"`
	Type       string          `json:"type"`
	Connection []bbcConnection `json:"connection"`
}

// bbcConnection is one supplier's copy of a media item.
type bbcConnection struct {
	Protocol       string `json:"protocol"`
	TransferFormat string `json:"transferFormat"`
	Href           string `json:"href"`
}

// stream picks the playlist to fetch.
//
// Images are skipped rather than ranked away: the selector lists a
// programme's thumbnail track beside its audio, and taking it would download
// a picture that finishes cleanly and reports success. Of what is left, only
// HLS is any use — a DASH manifest describes its segments by template rather
// than listing them — and the connections arrive in the supplier order the
// BBC wants used, so the first that fits wins.
func (s bbcSelection) stream() string {
	for _, media := range s.Media {
		if strings.HasPrefix(strings.ToLower(media.Type), "image/") ||
			strings.EqualFold(media.Kind, "image") {
			continue
		}
		for _, conn := range media.Connection {
			if strings.EqualFold(conn.Protocol, "https") &&
				strings.EqualFold(conn.TransferFormat, "hls") {
				return conn.Href
			}
		}
	}
	return ""
}

// bbcTitles is the BBC's name for something, in up to three levels.
//
// Which level is the episode's own name depends on how many were filled in,
// and that is the whole difficulty. A daily programme uses two — "Any
// Answers?" and the date — so the second is the episode. A serial uses all
// three — the strand, then the book, then "10. A Fateful Night" — so there
// the second is a *series* and the third is the episode. A one-off fills in
// only the first. Taking the second level as the episode name, which two
// levels is enough to suggest, would file every instalment of a serial under
// the name of the book.
type bbcTitles struct {
	Primary   string `json:"primary"`
	Secondary string `json:"secondary"`
	Tertiary  string `json:"tertiary"`
}

// full names an item that stands alone, so every level it has is used.
func (t bbcTitles) full() string { return bbcJoin(t.Primary, t.Secondary, t.Tertiary) }

// within names an item inside a folder already named for its container. The
// container is the first level, so that level is dropped — unless it is the
// only level there is.
func (t bbcTitles) within() string {
	return util.FirstNonEmpty(bbcJoin(t.Secondary, t.Tertiary), bbcJoin(t.Primary))
}

// bbcJoin names something from the levels that were filled in, outermost
// first. A level that only repeats the one above it is dropped rather than
// said twice.
func bbcJoin(levels ...string) string {
	var out []string
	for _, level := range levels {
		level = strings.TrimSpace(level)
		if level == "" || (len(out) > 0 && strings.EqualFold(out[len(out)-1], level)) {
			continue
		}
		out = append(out, level)
	}
	return strings.Join(out, " - ")
}

// bbcDownloadExtension names the container the offline copy arrives in.
//
// The URL says so and nothing else in the document does — there is no media
// type beside it — which is why this reads the URL rather than writing the
// answer down. MP3 is the only one the BBC has ever served here, and is what
// a URL carrying no extension is assumed to be.
func bbcDownloadExtension(fileURL string) string {
	if ext := path.Ext(util.NameFromURL(fileURL)); ext != "" {
		return strings.ToLower(ext)
	}
	return ".mp3"
}

// bbcName reduces a title to something that survives as a filename.
//
// Radio 4 names a daily edition by its date — "22/08/2026" — and SafeName has
// to read a slash as a path separator and keep only what follows the last
// one, so that edition would land on disk as "2026" and the week before it as
// "2026" again. Here the difference is still known: this is prose, not a
// path. feedSlashes is the feed reader's replacer, and the decision behind it
// is the same one.
func bbcName(title string) string {
	return strings.TrimSpace(feedSlashes.Replace(strings.TrimSpace(title)))
}
