package extractor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path"
	"strings"

	"github.com/JohanLindvall/HeapLeach/internal/httpx"
	"github.com/JohanLindvall/HeapLeach/internal/util"
)

// RaiPlay resolves raiplay.it videos and programmes.
//
// Italy's public broadcaster answers every page at its own address with
// ".json" appended, and the media behind that document needs no key, no
// session and no signature — so in outline this is svtplay's and nrk's
// sibling. Three things are its own, and each of the three would produce a
// wrong download rather than a clean failure if it were left out.
//
// The first is that hlsVariant.muxed() must not be the gate here. Rai's
// commonest packaging declares no CODECS attribute at all, and muxed() reads
// a missing CODECS as "cannot tell, so no" — which is the right answer for
// the host it was written for and exactly backwards here, where those
// variants carry video and audio in the same MPEG-TS segments. What Rai does
// publish, on the one packaging that genuinely splits the two, is an
// EXT-X-MEDIA audio group with a URI of its own. So audioElsewhere is the
// field to read: a variant naming such a group is dropped, and an asset with
// nothing left after that is refused outright. bestVariant only ranks and
// never refuses, so leaving this out would hand back the video-only rendition
// and write a silent file that looks like a finished download.
//
// The second is that Rai declines by substitution rather than by an error. An
// unavailable video answers 200 with "ct" empty and a real, fetchable MP4 in
// place of the stream — a clip that says "video non disponibile" on screen.
// Queued, it would agree with its own Content-Length and record as complete,
// which is the failure rejectWebPage exists to stop one layer up. So the
// relinker's answer is inspected before anything is queued. Why it refused is
// not machine-readable: geoprotection is set on clips that play perfectly
// well, and login_required correlates without deciding. The report is
// therefore Rai's own sentence plus an honest list of what it can mean, which
// is as far as the API allows anyone to go.
//
// The third is the container. Both packagings serve MPEG-TS, but about half
// of them route it through a path ending ".mp4.csmil", so playlistExtension —
// which reads the variant's URL — would name the joined file .mp4. The
// segments are named honestly, and that is what is read instead.
//
// One trap that costs nothing here but would if anyone reused the relinker's
// output: its body is Latin-1 under a JSON content type, so every accented
// character in "description" arrives decoded as U+FFFD. Names come from the
// page document, which is proper UTF-8; the refusal sentence is ASCII and
// survives intact.
type RaiPlay struct {
	client *httpx.Client
}

const (
	raiRoot = "https://www.raiplay.it"

	// raiOutput asks the relinker for JSON. Without it the servlet answers a
	// bare 302 to the media, which says nothing about whether Rai is willing
	// to serve it.
	raiOutput = "output=62"

	// raiUnavailable is the placeholder clip Rai substitutes for a video it
	// will not serve.
	raiUnavailable = "video_no_available"

	// raiMaxEpisodes bounds how many entries one programme URL expands to. A
	// nightly news strand lists its last few hundred bulletins and its clips
	// beside them, which is a mistake being carried out efficiently rather
	// than a download anybody meant to start.
	raiMaxEpisodes = 500
)

// errRaiDemuxed is the one packaging this cannot take. It is a value rather
// than a formatted string because expanding a programme has to recognise it
// among the reasons an entry produced nothing.
var errRaiDemuxed = errors.New("Rai serves this one with its audio in a rendition of " +
	"its own, and joining segments end to end cannot put the two back together")

// NewRaiPlay builds the RaiPlay extractor.
func NewRaiPlay(client *httpx.Client) *RaiPlay { return &RaiPlay{client: client} }

func (r *RaiPlay) Name() string { return "raiplay" }

func (r *RaiPlay) Match(u *url.URL) bool { return util.HostMatches(u.Host, "raiplay.it") }

// Extract handles a single video or a programme, which expands to every
// episode and clip the programme lists.
func (r *RaiPlay) Extract(ctx context.Context, u *url.URL, _ Options) (*Result, error) {
	segs := util.PathSegments(u)
	if len(segs) >= 2 {
		switch segs[0] {
		case "video":
			return r.video(ctx, u.EscapedPath())
		case "programmi":
			return r.programme(ctx, u)
		}
	}
	// Claiming the host and then explaining what does not resolve beats
	// letting a channel or a category page fall through to the direct
	// downloader, which would fetch the page shell and call it a file.
	return nil, fmt.Errorf("raiplay: %s is neither a video nor a programme: paste a "+
		"/video/... page or a /programmi/<name> one (live channels and category "+
		"listings hold nothing to fetch)", u.Redacted())
}

// video resolves one video page into a job of its own.
func (r *RaiPlay) video(ctx context.Context, page string) (*Result, error) {
	file, title, err := r.episode(ctx, page)
	if err != nil {
		return nil, err
	}
	// A lone file is written straight into the download directory, with no
	// folder to say what it belongs to, so the name carries the programme as
	// well where the video's own name does not already.
	file.Name = title + file.Name
	return &Result{Title: title, Files: []File{*file}}, nil
}

// programme expands a programme into one file per entry, filed by listing
// when the programme has more than one.
func (r *RaiPlay) programme(ctx context.Context, u *url.URL) (*Result, error) {
	var doc raiProgramme
	if err := r.client.GetJSON(ctx, raiDocument(u.EscapedPath()), raiHeaders(), &doc); err != nil {
		return nil, fmt.Errorf("raiplay: programme %s: %w", u.Redacted(), err)
	}

	entries := r.entries(ctx, doc)
	if len(entries) == 0 {
		return nil, fmt.Errorf("raiplay: %s lists nothing to download", u.Redacted())
	}
	if len(entries) > raiMaxEpisodes {
		entries = entries[:raiMaxEpisodes]
	}
	nest := raiNest(entries)

	result := &Result{Title: util.FirstNonEmpty(doc.Name, strings.Trim(u.Path, "/"))}
	var refused string
	// Every entry is four requests deep — the page document, the relinker, the
	// master playlist and the chosen variant — and a programme routinely lists
	// several hundred, so they are resolved a few at a time rather than one
	// after another. FanOut keeps the listing's own order regardless.
	for _, resolved := range FanOut(ctx, entries, r.resolve) {
		if resolved.file == nil {
			// One refused entry must not sink the rest, but its reason is
			// kept: when every entry is refused they are refused for the same
			// reason, and Rai's own words for it beat anything concluded here.
			if refused == "" {
				refused = resolved.refused
			}
			continue
		}
		file := *resolved.file
		if !nest {
			file.Dir = ""
		}
		result.Files = append(result.Files, file)
	}
	if len(result.Files) == 0 {
		return nil, fmt.Errorf("raiplay: none of the %d entries of %s are playable: %s",
			len(entries), u.Redacted(),
			util.FirstNonEmpty(refused, "Rai offers no self-contained stream for any of them"))
	}
	return result, nil
}

// raiEntry is one video a programme lists, with the folder its listing would
// file it under.
type raiEntry struct {
	page  string
	name  string
	label string
}

// raiSetRef is one of a programme's listings.
type raiSetRef struct {
	label string
	page  string
}

// entries reads a programme's listings and collects what they hold. The
// listings are few and are read one after another; it is the entries inside
// them that run to the hundreds.
func (r *RaiPlay) entries(ctx context.Context, doc raiProgramme) []raiEntry {
	headers := raiHeaders()
	seen := make(map[string]bool)
	var entries []raiEntry
	for _, ref := range raiListings(doc) {
		var listing raiSet
		if err := r.client.GetJSON(ctx, raiDocument(ref.page), headers, &listing); err != nil {
			continue // one listing that will not load is not the programme
		}
		entries = append(entries, raiItems(listing, ref.label, seen)...)
	}
	return entries
}

// raiListings flattens a programme into the listings behind it.
//
// Rai nests a programme two levels deep — blocks holding sets — and puts the
// season on whichever level it feels like: one programme's blocks are its
// seasons and its sets are "Puntate" and "Clip", the next has that exactly the
// other way round. Reading one level alone therefore loses the distinction
// that matters, and would drop this year's episodes and last year's into a
// single folder named "Puntate". Both names are used instead.
func raiListings(doc raiProgramme) []raiSetRef {
	var refs []raiSetRef
	for _, block := range doc.Blocks {
		for _, set := range block.Sets {
			if set.PathID == "" {
				continue
			}
			refs = append(refs, raiSetRef{label: raiLabel(block.Name, set.Name), page: set.PathID})
		}
	}
	return refs
}

// raiItems collects one listing's entries, skipping any the caller has already
// taken from another listing: a clip is routinely filed under its season and
// again under the programme's clip reel.
func raiItems(listing raiSet, label string, seen map[string]bool) []raiEntry {
	var out []raiEntry
	for _, item := range listing.Items {
		if item.PathID == "" || seen[item.PathID] {
			continue
		}
		seen[item.PathID] = true
		out = append(out, raiEntry{page: item.PathID, name: item.Name, label: label})
	}
	return out
}

// raiNest reports whether a programme's entries want folders. One listing
// needs none: a folder every file goes in says nothing, and two programmes
// fetched separately would then share it.
func raiNest(entries []raiEntry) bool {
	distinct := make(map[string]bool)
	for _, entry := range entries {
		if entry.label != "" {
			distinct[entry.label] = true
		}
	}
	return len(distinct) > 1
}

// raiResolved is what expanding one entry produced: a file, or Rai's own words
// for why there is none.
//
// FanOut drops an item whose fetch returned an error, which is the right fate
// for one bad entry among hundreds — but it would drop the refusal message
// with it, and from outside Italy that message is the whole answer. So a
// refusal is reported as a result rather than as an error.
type raiResolved struct {
	file    *File
	refused string
}

// resolve turns one listed entry into a file, or into Rai's reason for there
// being none.
func (r *RaiPlay) resolve(ctx context.Context, entry raiEntry) ([]raiResolved, error) {
	file, title, err := r.episode(ctx, entry.page)
	if err != nil {
		var refusal *raiRefused
		if errors.As(err, &refusal) {
			return []raiResolved{{refused: refusal.reason()}}, nil
		}
		return nil, err
	}
	file.Name = util.FirstNonEmpty(entry.name, title) + file.Name
	file.Dir = entry.label
	return []raiResolved{{file: file}}, nil
}

// episode resolves one video page to a playable segment list. The returned
// File carries only the extension in Name; the caller supplies the stem, which
// differs between a standalone video and one entry of a programme.
func (r *RaiPlay) episode(ctx context.Context, page string) (*File, string, error) {
	headers := raiHeaders()

	var doc raiVideo
	if err := r.client.GetJSON(ctx, raiDocument(page), headers, &doc); err != nil {
		return nil, "", fmt.Errorf("raiplay: video %s: %w", page, err)
	}
	title := util.FirstNonEmpty(raiTitle(doc.ProgramInfo.Name, doc.Name),
		strings.TrimSuffix(util.NameFromURL(page), ".json"))

	switch {
	case !raiEmpty(doc.RightsManagement.Rights.DRM):
		// Never yet seen filled in, but it is the slot Rai documents for it,
		// and a DRM stream downloads to the end and then plays as noise.
		return nil, "", fmt.Errorf("raiplay: %s is DRM protected", title)
	case doc.IsLive:
		return nil, "", fmt.Errorf("raiplay: %s is a live stream", title)
	case doc.Video.ContentURL == "":
		return nil, "", fmt.Errorf("raiplay: %s names no stream", title)
	}

	manifest, err := r.manifest(ctx, doc.Video.ContentURL, title)
	if err != nil {
		return nil, "", err
	}
	variant, err := r.variant(ctx, manifest, headers)
	if err != nil {
		return nil, "", fmt.Errorf("raiplay: %s: %w", title, err)
	}
	segments, _, err := resolvePlaylist(ctx, r.client, variant, headers)
	if err != nil {
		return nil, "", fmt.Errorf("raiplay: %s: %w", title, err)
	}

	// The signed link the relinker mints lives for 150 seconds, but the CDN
	// enforces it on the master playlist alone: the chunklist and the segments
	// below it answer without a token long after it has expired. So these
	// resolve eagerly, and no File.Resolve is needed to keep a long queue
	// alive.
	return &File{
		Name:     raiExtension(segments),
		Size:     -1,
		Headers:  headers,
		Segments: segments,
	}, title, nil
}

// manifest asks the relinker for the playlist behind a content URL, and is
// where a refusal is caught before anything reaches the queue.
func (r *RaiPlay) manifest(ctx context.Context, contentURL, title string) (string, error) {
	var asset raiAsset
	if err := r.client.GetJSON(ctx, raiRelinker(contentURL), raiHeaders(), &asset); err != nil {
		return "", fmt.Errorf("raiplay: %s: relinker: %w", title, err)
	}

	link := asset.link()
	switch {
	case !raiEmpty(asset.LicenceServerMap):
		return "", fmt.Errorf("raiplay: %s is DRM protected", title)
	case asset.refused():
		return "", &raiRefused{title: title, message: asset.Description}
	case link == "":
		return "", fmt.Errorf("raiplay: %s: the relinker named no stream", title)
	case !strings.EqualFold(asset.CT, "m3u8"):
		return "", fmt.Errorf("raiplay: %s: the relinker offered a %q stream, "+
			"which this does not read", title, asset.CT)
	}
	return link, nil
}

// variant picks the rendition to fetch out of Rai's master playlist.
//
// This is resolvePlaylist's selection done by hand, and only because the
// refusal has to be explicit: bestVariant ranks the renditions but never
// declines, so an asset whose every variant is video only would otherwise be
// downloaded to the end and play silently. Filtering before the ranking rather
// than checking after it also keeps a demuxed rendition from outranking a
// usable one, which it would whenever neither declares CODECS.
func (r *RaiPlay) variant(ctx context.Context, manifest string, headers httpx.Header) (string, error) {
	base, err := ParseURL(manifest)
	if err != nil {
		return "", err
	}
	doc, err := r.client.GetString(ctx, manifest, headers)
	if err != nil {
		return "", fmt.Errorf("fetch playlist: %w", err)
	}

	variants := parseMasterPlaylist(doc, base)
	if len(variants) == 0 {
		return manifest, nil // already a media playlist; let the caller read it
	}
	best, err := raiUsable(variants)
	if err != nil {
		return "", err
	}
	return best.URL, nil
}

// raiUsable picks the best rendition that carries its own audio, and refuses
// when there is none.
func raiUsable(variants []hlsVariant) (hlsVariant, error) {
	var selfContained []hlsVariant
	for _, v := range variants {
		if !v.audioElsewhere {
			selfContained = append(selfContained, v)
		}
	}
	best, ok := bestVariant(selfContained)
	if !ok {
		return hlsVariant{}, errRaiDemuxed
	}
	return best, nil
}

// raiRefused is Rai declining to serve something, carrying its own words for
// it. It is a type rather than a message so that expanding a whole programme
// can report the reason once instead of several hundred times.
type raiRefused struct {
	title   string
	message string
}

func (e *raiRefused) Error() string { return "raiplay: " + e.title + ": " + e.reason() }

// reason is Rai's sentence with what it can mean spelled out beside it.
//
// Nothing sharper is available: the geoprotection flags are set on material
// that plays, login_required correlates with a refusal without deciding it,
// and the relinker reports "geoprotection":"N" while refusing. Naming one of
// the three causes would be a guess dressed up as an answer, so all three are
// named as the possibilities they are.
func (e *raiRefused) reason() string {
	said := util.FirstNonEmpty(strings.TrimSpace(e.message), "Rai will not serve this video")
	return said + " (Rai does not say why; the usual causes are a licence limited to " +
		"Italy, an availability window that has closed, and material that needs a " +
		"Rai account)"
}

// raiHeaders are what every request here carries. Nothing on Rai's side
// requires a Referer, but sending the one a browser would costs nothing and
// keeps this the same shape as its siblings.
func raiHeaders() httpx.Header { return httpx.Referer(raiRoot + "/") }

// raiDocument is the JSON document behind a page path. Rai serves one at every
// page's own address with ".json" appended, and its own listings quote paths
// in that form already — so a path that has it is left alone, and the .html a
// browser shows is traded for it.
func raiDocument(page string) string {
	page = strings.TrimSuffix(strings.TrimSuffix(page, "/"), ".html")
	if !strings.HasSuffix(page, ".json") {
		page += ".json"
	}
	return raiRoot + page
}

// raiRelinker asks the servlet for JSON rather than the redirect it gives by
// default. The content URL already carries a query, but a host that changed
// its mind about that would otherwise produce a URL with two question marks
// and no diagnosis.
func raiRelinker(contentURL string) string {
	if strings.Contains(contentURL, "?") {
		return contentURL + "&" + raiOutput
	}
	return contentURL + "?" + raiOutput
}

// raiTitle names a video by its programme and its own name together.
//
// Rai is inconsistent about whether the two overlap: a drama's episode is
// called "Trauma S1E1 - Episodio 1" and already says which series it belongs
// to, while a news clip's name says nothing about the bulletin it came from.
// So the programme is prefixed only where the name does not already open with
// it.
func raiTitle(program, name string) string {
	switch {
	case name == "":
		return program
	case program == "" || strings.HasPrefix(strings.ToLower(name), strings.ToLower(program)):
		return name
	}
	return program + " - " + name
}

// raiLabel names the folder one listing's videos go in, from the block and the
// set that hold it. The duplicate is dropped, which is the common case: a
// programme with one listing names the block and the set alike.
func raiLabel(block, set string) string {
	switch {
	case set == "" || strings.EqualFold(block, set):
		return util.FirstNonEmpty(block, set)
	case block == "":
		return set
	}
	return block + " - " + set
}

// raiExtension names the container the joined segments will be in, read off a
// segment rather than off the variant URL.
//
// playlistExtension reads the URL, and that is wrong for Rai's .csmil
// packaging, whose every path below ".mp4.csmil/" contains ".mp4" while the
// segments in it are MPEG-TS. It is not an edge case: about half the
// catalogue is served that way.
func raiExtension(segments []string) string {
	if len(segments) == 0 {
		return ".ts"
	}
	switch ext := strings.ToLower(path.Ext(util.NameFromURL(segments[0]))); ext {
	case ".mp4", ".m4s":
		return ".mp4"
	default:
		return ".ts"
	}
}

// raiEmpty reports whether a JSON value carries nothing. Rai fills its DRM and
// licence-server slots with an empty object rather than omitting them, and the
// value is read as raw JSON rather than decoded so that a host which one day
// puts something of another shape there still fails the test loudly instead of
// derailing the whole document.
func raiEmpty(raw json.RawMessage) bool {
	switch strings.TrimSpace(string(raw)) {
	case "", "null", "{}", "[]", `""`:
		return true
	}
	return false
}

// raiVideo is the part of a video document this needs.
type raiVideo struct {
	Name  string `json:"name"`
	Video struct {
		ContentURL string `json:"content_url"`
	} `json:"video"`
	IsLive      bool `json:"is_live"`
	ProgramInfo struct {
		Name string `json:"name"`
	} `json:"program_info"`
	RightsManagement struct {
		Rights struct {
			DRM json.RawMessage `json:"drm"`
		} `json:"rights"`
	} `json:"rights_management"`
}

// raiAsset is the relinker's answer.
type raiAsset struct {
	// Video holds the stream, and on a refusal the placeholder clip instead.
	Video []string `json:"video"`
	// CT is the container. Empty is the tell that this is not the video.
	CT string `json:"ct"`
	// Description is Rai's sentence about the asset. It is only ever read on
	// a refusal, where it is ASCII: the body is Latin-1 under a JSON content
	// type, so an accented character anywhere else in it arrives as U+FFFD.
	Description string `json:"description"`
	// LicenceServerMap names a DRM licence server, and is empty throughout the
	// catalogue this has seen.
	LicenceServerMap json.RawMessage `json:"licence_server_map"`
}

// link is the stream the relinker named, if it named one.
func (a raiAsset) link() string {
	if len(a.Video) == 0 {
		return ""
	}
	return strings.TrimSpace(a.Video[0])
}

// refused reports the substitution: a 200 naming a placeholder clip in place
// of the video. Both halves are checked because either alone would be a thin
// thread to hang a download on — the empty container is the deliberate signal,
// and the placeholder's own name is what makes the answer unmistakable.
func (a raiAsset) refused() bool {
	return a.CT == "" || strings.Contains(a.link(), raiUnavailable)
}

// raiProgramme is a programme page: blocks of listings, two levels deep.
type raiProgramme struct {
	Name   string `json:"name"`
	Blocks []struct {
		Name string `json:"name"`
		Sets []struct {
			Name   string `json:"name"`
			PathID string `json:"path_id"`
		} `json:"sets"`
	} `json:"blocks"`
}

// raiSet is one listing within a programme. Its own name repeats the one the
// programme document already gave it, so only the items are read here.
type raiSet struct {
	Items []struct {
		Name   string `json:"name"`
		PathID string `json:"path_id"`
	} `json:"items"`
}
