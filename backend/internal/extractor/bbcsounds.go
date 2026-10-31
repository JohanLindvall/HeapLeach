// SPDX-License-Identifier: MIT

package extractor

import (
	"cmp"
	"context"
	"fmt"
	"net/url"
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
	about
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
func NewBBCSounds(client *httpx.Client) *BBCSounds {
	return &BBCSounds{client: client, about: about{name: "bbcsounds", shape: bbcShape,
		recognises: "episodes, brands and series (`/sounds/play/<pid>`, `/sounds/brand/<pid>`, `/sounds/series/<pid>`) served by the BBC Sounds web app"}}
}

// bbcShape is an episode, brand or series: /sounds/<kind>/<pid>.
var bbcShape = pathShape(`^/sounds/(?:play|brand|series)/[A-Za-z0-9:]+`)

// bbcSoundsBundle is where the Sounds web app is served from.
const bbcSoundsBundle = "/sounds/web/sounds-web-spa/"

// Recognise takes an episode, brand or series page served by the Sounds web
// app (its bundle path): the page is the app's shell, and what it plays is
// the programmes API's to say, by the pid in the path.
func (b *BBCSounds) Recognise(_ context.Context, p *Page) bool {
	return bbcShape(p.Final) && p.Script(bbcSoundsBundle) && p.Named("BBC Sounds")
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
	return soleFile(cmp.Or(bbcName(item.Titles.full()), pid), file), nil
}

// container expands a brand or a series into one file per episode.
func (b *BBCSounds) container(ctx context.Context, pid string) (*Result, error) {
	items, total, walkNote, err := b.listing(ctx, pid, config.MaxResolvedEpisodes)
	if err != nil {
		return nil, err
	}
	if len(items) == 0 {
		return nil, fmt.Errorf("bbcsounds: %s lists no available episodes", pid)
	}

	// Every row names its container, so the job's folder is named without a
	// request of its own.
	title := cmp.Or(bbcName(items[0].Container.Title), bbcName(items[0].Titles.Primary), pid)

	// A listing of downloadable episodes is already complete — each row
	// carries its own file_url — and one of streamed episodes needs a media
	// selection and two playlists each. Fetched one after another that is
	// minutes before a byte moves, so they go out several at a time, in the
	// listing's order.
	return listSeries(ctx, title, series[bbcItem]{
		site: "bbcsounds", noun: "episodes", link: title,
		items: items, listed: total,
		resolve: func(ctx context.Context, item bbcItem) (*File, error) {
			file, err := b.file(ctx, item)
			if err != nil {
				return nil, err
			}
			// The folder is named for the container, which is the first
			// level of every title in it, so the file is named by what lies
			// below it.
			file.Name = cmp.Or(bbcName(item.Titles.within()), item.pid()) + file.Name
			return file, nil
		},
		unplayable: "the media selector named no stream for any of them",
	}, walkNote)
}

// listing pages a container's episodes, up to limit of them, with the
// count the listing states when it stopped there, and the note for a
// listing cut short some other way.
//
// The endpoint caps a page at a hundred rows: a larger limit is echoed back
// and then ignored, which is exactly config.FolderPageSize, so paging is by
// the count actually returned rather than by the limit asked for. A strand
// like a daily news programme runs to several thousand editions.
func (b *BBCSounds) listing(ctx context.Context, container string, limit int) (items []bbcItem, listed int, note string, err error) {
	offset, total := 0, 0
	items, more, note, err := walkEpisodes(ctx, limit, pageWalk[bbcItem]{
		fetch: func(ctx context.Context, _ int) (pageOf[bbcItem], error) {
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
				return pageOf[bbcItem]{}, err
			}
			offset += len(page.Data)
			total = page.Total
			// A listing that states no total is its first page alone.
			return pageOf[bbcItem]{items: page.Data, next: page.Total > 0, end: len(page.Data) == 0, total: page.Total}, nil
		},
		key: func(item bbcItem) string { return item.pid() },
	})
	if err != nil {
		return nil, 0, "", fmt.Errorf("bbcsounds: episodes of %s: %w", container, err)
	}
	if more {
		return items, total, "", nil
	}
	return items, 0, note, nil
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
			Name:       linkExt(variant.FileURL, ".mp3"),
			URL:        variant.FileURL,
			Size:       knownSize(variant.FileSize),
			SizeApprox: variant.FileSize > 0,
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
	return broadcastFile(ctx, b.client, "bbcsounds", item.pid(), stream, "", headers, variantExt)
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
		return "", &itemRefusal{"bbcsounds", vpid, bbcReason(doc.Result)}
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
	doc, ok := refusalBody[struct {
		Result string `json:"result"`
	}](err)
	if !ok || doc.Result == "" {
		return nil
	}
	return &itemRefusal{"bbcsounds", pid, bbcReason(doc.Result)}
}

// bbcReason is the media selector's refusal to name a stream, in words.
// Unlike NRK, which refuses in a sentence written for a viewer and worth
// passing through untouched, the BBC refuses in a token:
// "selectionunavailable" is not an answer to give anybody, so it is turned
// into one here.
func bbcReason(result string) string {
	switch strings.ToLower(result) {
	case "geolocation":
		return "BBC Sounds offers this inside the United Kingdom only"
	case "selectionunavailable":
		return "BBC Sounds holds no stream for this any more — its availability window has closed"
	}
	return "the media selector refused it: " + result
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
	return cmp.Or(i.Urn, i.ID)
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
func bbcQualityRank(name string) int { return rankIn(bbcQualityOrder, name, len(bbcQualityOrder)) }

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
	return cmp.Or(bbcJoin(t.Secondary, t.Tertiary), bbcJoin(t.Primary))
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
