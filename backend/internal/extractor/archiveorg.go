package extractor

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"path"
	"strconv"
	"strings"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
	"github.com/JohanLindvall/HeapLeach/internal/util"
)

// ArchiveOrg resolves Internet Archive items.
//
// One anonymous call to the metadata API returns the entire item: every file
// with its exact byte count, its checksum, its format label and where it came
// from, alongside the item's title and mediatype. The download links are
// unsigned, permanent and honour ranges, so nothing here needs a resolver and
// nothing goes stale while an item waits its turn in the queue. That makes
// this the easiest host in the tree to extract from, and the two hardest
// things about it are both consequences of that ease.
//
// The first is pace, and it is the reason File.Pace exists at all. Measured
// against one item: a single connection moved 7.6 MB/s, three connections
// moved 21 kB/s each, and six moved 8 kB/s each — a penalty of roughly three
// hundred times, applied to the client address rather than to the item. It
// followed the same client to a different item on a different storage node,
// and had not lifted after four minutes of sitting idle. Nothing fails while
// this is happening; the server accepts every connection and serves all of
// them, badly, so none of the downloader's own measurements can find it. The
// stall watchdog sees a slow transfer rather than a stalled one, and the
// stream supervisor's response to a slow transfer is to open another
// connection, which is precisely what makes it worse. So every file this
// emits carries Pace{Streams: 1, Files: 1} and the question is never asked.
// Note also that the metadata API is not throttled while transfers are being
// punished: a fast extract says nothing whatever about the state of the host.
//
// The second is that an item is not a folder of things somebody wants. It is
// the upload plus everything the archive generated from it, and the archive
// generates a great deal: a film is listed beside its Ogg copy, its 512kbps
// MPEG4 and its h.264, a concert beside an MP3, an Ogg, a waveform PNG and a
// fingerprint for each of its tracks, a scanned book beside its OCR text, its
// hOCR, its page index and a zip of every page as a JP2. Dropping the obvious
// sidecars is not enough — one film still resolved to the same 1.04 GB six
// times over — so what survives is decided per mediatype by archiveChoose,
// which is an allow-list per kind rather than a list of things to drop. A
// deny-list cannot work here: a 48-item sample carried 74 distinct format
// labels, and the archive coins new ones whenever it adds a derivation.
// HEAPLEACH_ARCHIVE_FORMATS is the escape for when it coins one that matters.
type ArchiveOrg struct {
	client *httpx.Client
	// formats, when set, replaces the per-mediatype policy entirely.
	formats []string
}

const (
	archiveRoot = "https://archive.org"
	// archiveMetadataAPI answers with the whole item, for anyone, unsigned.
	archiveMetadataAPI = archiveRoot + "/metadata/"
	// archiveDownloadRoot redirects to whichever storage node currently has
	// the item. See archiveDownloadURL for why that redirect is followed
	// rather than pre-empted.
	archiveDownloadRoot = archiveRoot + "/download/"
)

// archivePace is what every file this extractor emits carries. See the type
// comment for the measurements behind it; the short version is that this host
// answers parallelism with a three-hundredfold slowdown that outlives the
// request, the item and the connection that earned it.
var archivePace = Pace{Streams: 1, Files: 1}

// archiveItemRoutes are the path prefixes that name an item. Every one of
// them puts the identifier in the same place, whatever it then does with it.
var archiveItemRoutes = map[string]bool{
	"details":  true,
	"download": true,
	"metadata": true,
	"embed":    true,
	"stream":   true,
	"serve":    true,
	"compress": true,
}

// archiveReaderPositions are the trailing segments the book reader and the
// video player use to record where in an item the viewer had got to. They sit
// in exactly the place a file selector sits, so /details/<id>/page/n5/mode/2up
// would otherwise be read as a request for a file called "page".
var archiveReaderPositions = map[string]bool{
	"page":    true,
	"mode":    true,
	"search":  true,
	"theater": true,
}

// NewArchiveOrg builds the archive.org extractor.
func NewArchiveOrg(cfg *config.Config, client *httpx.Client) *ArchiveOrg {
	a := &ArchiveOrg{client: client}
	if cfg != nil {
		a.formats = cfg.ArchiveFormats
	}
	return a
}

func (a *ArchiveOrg) Name() string { return "archive.org" }

// Match claims item URLs on the site itself and nothing else.
//
// Deliberately not a subdomain match. web.archive.org is the Wayback Machine,
// where a URL is a snapshot of somebody else's page and has no identifier to
// look up, and the ia*.us / dn*.ca storage nodes serve one plain file each.
// Both belong to the direct-link fallback, which handles them correctly
// already.
func (a *ArchiveOrg) Match(u *url.URL) bool {
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	if host != "archive.org" && host != "www.archive.org" {
		return false
	}
	_, _, ok := archiveParse(u)
	return ok
}

// Extract resolves an item, or one named file within it.
func (a *ArchiveOrg) Extract(ctx context.Context, u *url.URL, _ Options) (*Result, error) {
	id, selector, ok := archiveParse(u)
	if !ok {
		return nil, fmt.Errorf("archive.org: %s names no item "+
			"(expected /details/<identifier>)", u.Redacted())
	}

	link := archiveMetadataAPI + url.PathEscape(id)
	var doc archiveDoc
	if err := a.client.GetJSON(ctx, link, nil, &doc); err != nil {
		return nil, fmt.Errorf("archive.org: fetch %s: %w", link, err)
	}
	return archiveResult(&doc, id, selector, a.formats)
}

// archiveParse reads the identifier, and any file selector, out of a URL.
//
// A selector is what /details/<id>/<file> means: that route answers with the
// item's page rather than redirecting to the file, so the only place the
// intended file is named is the path itself.
func archiveParse(u *url.URL) (id, selector string, ok bool) {
	segs := util.PathSegments(u)
	if len(segs) < 2 || !archiveItemRoutes[strings.ToLower(segs[0])] {
		return "", "", false
	}
	rest := segs[2:]
	if len(rest) > 0 && archiveReaderPositions[strings.ToLower(rest[0])] {
		rest = nil
	}
	return segs[1], strings.Join(rest, "/"), true
}

// archiveResult turns a fetched metadata document into a job. Kept apart from
// the fetch so the whole rendition policy can be tested against documents
// rather than against the site.
func archiveResult(doc *archiveDoc, id, selector string, formats []string) (*Result, error) {
	// A darkened item is checked first, because it answers in very nearly the
	// shape a missing one does: no files, and no metadata to speak of. The
	// flag is the only thing that separates "withdrawn from view" from "never
	// existed", and they need saying differently.
	if doc.IsDark {
		return nil, fmt.Errorf("archive.org: %s has been darkened and serves no files", id)
	}
	// Nothing here 404s. An identifier nobody registered answers 200 with
	// the body "{}", so an empty document is what a typo looks like and
	// saying so is the only useful thing to report.
	if len(doc.Files) == 0 && doc.Metadata.Identifier == "" {
		return nil, fmt.Errorf("archive.org: there is no item called %q "+
			"(the metadata API answers 200 with an empty document for an identifier "+
			"that does not exist, so a typo arrives looking exactly like this)", id)
	}

	mediatype := strings.ToLower(strings.TrimSpace(string(doc.Metadata.MediaType)))
	// A collection is a valid item in its own right, holding the logos its
	// landing page is built from — header.jpg, icon.gif, oldicon.jpg. Without
	// this it would resolve happily and download five pieces of site
	// furniture instead of the thousands of items the user was looking at.
	if doc.IsCollection || mediatype == "collection" {
		return nil, fmt.Errorf("archive.org: %s is a collection, not an item — "+
			"it holds only the artwork for its own landing page. "+
			"Submit the items inside it instead", id)
	}

	title := util.FirstNonEmpty(strings.TrimSpace(string(doc.Metadata.Title)), id)

	if selector != "" {
		return archiveSelected(doc, id, title, selector)
	}

	usable := make([]archiveFile, 0, len(doc.Files))
	restricted := 0
	for _, f := range doc.Files {
		switch {
		case f.Name == "":
		case f.restricted():
			restricted++
		case archiveEncrypted(f.Format):
			// Not restricted, and that is the trap: these serve a clean 206
			// of a file no reader will open without a licence server. They
			// have to go by format, because nothing about the transfer
			// itself will ever tell us.
		default:
			usable = append(usable, f)
		}
	}

	chosen := archiveChoose(mediatype, usable, formats)
	if len(chosen) == 0 {
		return nil, archiveNothingErr(id, mediatype, restricted, len(doc.Files))
	}
	// A television archive files a recording a minute and a scanned item a
	// page at a time. Past this the queue has stopped being something a
	// person is reading.
	if len(chosen) > config.MaxListingFiles {
		chosen = chosen[:config.MaxListingFiles]
	}

	files := make([]File, 0, len(chosen))
	for _, f := range chosen {
		files = append(files, archiveFileFor(id, f))
	}
	return &Result{Title: title, Files: files}, nil
}

// archiveSelected resolves /details/<id>/<file> to that one file.
//
// The rendition policy is skipped entirely: somebody who named a file has
// already chosen, and refusing them the Ogg copy because the policy prefers
// the original would make the escape hatch no escape at all.
func archiveSelected(doc *archiveDoc, id, title, selector string) (*Result, error) {
	for _, f := range doc.Files {
		if f.Name != selector {
			continue
		}
		if f.restricted() {
			return nil, fmt.Errorf("archive.org: %s in %s is restricted and would answer 401 "+
				"once the download redirect had been followed", selector, id)
		}
		return &Result{Title: title, Files: []File{archiveFileFor(id, f)}}, nil
	}
	return nil, fmt.Errorf("archive.org: %s holds no file called %q", id, selector)
}

// archiveNothingErr explains an item that resolved to nothing, which is
// usually restriction rather than emptiness and reads very differently.
func archiveNothingErr(id, mediatype string, restricted, total int) error {
	if restricted > 0 {
		return fmt.Errorf("archive.org: %d of the %d files in %s are restricted, and nothing "+
			"downloadable is left: the item is lending-only or otherwise access-restricted",
			restricted, total, id)
	}
	return fmt.Errorf("archive.org: nothing in %s matched the rendition policy for a %q item; "+
		"set HEAPLEACH_ARCHIVE_FORMATS to the format labels you want kept", id, mediatype)
}

// ------------------------------------------------------- rendition policy

// archiveChoose applies one mediatype's rule to the item's usable files.
//
// Each branch is an allow-list of what that kind of item's content looks
// like, rather than a list of derivations to drop, because the set of
// derivations is open-ended and the set of things worth keeping is not.
func archiveChoose(mediatype string, files []archiveFile, formats []string) []archiveFile {
	if len(formats) > 0 {
		return archiveByFormat(files, formats)
	}
	switch mediatype {
	case "movies":
		return archiveMovie(files)
	case "audio", "etree":
		// etree is the archive's mediatype for a live concert, and it is
		// laid out exactly like an album: one item, one file per track,
		// three renditions of each.
		return archiveTracks(files)
	case "texts":
		return archiveBook(files)
	default:
		return archiveOriginals(files)
	}
}

// archiveByFormat is the runtime escape: it keeps whatever the caller named
// and lets the built-in policy alone.
//
// Restricted and encrypted files are still dropped ahead of this, because
// neither is a rendition of anything — one answers 401 and the other opens in
// nothing — so no format list should be able to ask for them.
func archiveByFormat(files []archiveFile, formats []string) []archiveFile {
	want := make(map[string]bool, len(formats))
	for _, f := range formats {
		if f = strings.ToLower(strings.TrimSpace(f)); f != "" {
			want[f] = true
		}
	}
	var out []archiveFile
	for _, f := range files {
		if want[archiveLabel(f.Format)] {
			out = append(out, f)
		}
	}
	return out
}

// archiveMovie picks the one video worth having, plus any subtitles.
//
// The original is preferred because every derivative was made from it and is
// therefore a smaller copy of the same thing. It is skipped only when it is
// not a video at all, which is common enough to need a real fallback rather
// than a defensive one: an item's originals are routinely a poster frame, a
// script, or a still somebody uploaded alongside the film. When there is no
// original video the largest MPEG4 variant is the best of the derivations,
// and when there is not even one of those anything playable beats nothing.
func archiveMovie(files []archiveFile) []archiveFile {
	pick := func(keep func(archiveFile) bool) *archiveFile {
		var best *archiveFile
		for i := range files {
			f := &files[i]
			if !keep(*f) {
				continue
			}
			if best == nil || f.size() > best.size() {
				best = f
			}
		}
		return best
	}

	best := pick(func(f archiveFile) bool { return f.isOriginal() && archiveIsVideo(f.Format) })
	if best == nil {
		best = pick(func(f archiveFile) bool { return archiveIsMPEG4(f.Format) })
	}
	if best == nil {
		best = pick(func(f archiveFile) bool { return archiveIsVideo(f.Format) })
	}
	if best == nil {
		return nil
	}

	out := []archiveFile{*best}
	for _, f := range files {
		if archiveIsSubtitle(f.Format) {
			out = append(out, f)
		}
	}
	return out
}

// archiveTracks keeps one file per track.
//
// A track is identified by its name with the extension taken off, because
// that is the one thing the archive holds constant across a track's
// renditions: the format labels vary by how and when the item was derived,
// and the sizes obviously do. Everything that is not an audio format — the
// waveform PNG, the fingerprint, the spectrogram the archive files beside
// each track — simply never enters the grouping, so no stem can be won by
// something that is not audio.
func archiveTracks(files []archiveFile) []archiveFile {
	best := make(map[string]archiveFile)
	rank := make(map[string]int)
	var order []string

	for _, f := range files {
		r := archiveAudioRank(f.Format)
		if r < 0 {
			continue
		}
		stem := archiveStem(f.Name)
		if _, seen := best[stem]; !seen {
			order = append(order, stem)
			best[stem], rank[stem] = f, r
			continue
		}
		if r < rank[stem] {
			best[stem], rank[stem] = f, r
		}
	}

	out := make([]archiveFile, 0, len(order))
	for _, stem := range order {
		out = append(out, best[stem])
	}
	return out
}

// archiveBook keeps the one readable copy of a scanned text.
//
// A scan is derived into roughly a dozen files and all but two of them are
// working material: the OCR in four spellings, an index into it, the page
// numbers as JSON, and a zip holding every page as a JP2. What a reader wants
// is the PDF, or failing that the EPUB, and an allow-list is what expresses
// that — the OCR family is not enumerated anywhere here because it never
// needed to be.
func archiveBook(files []archiveFile) []archiveFile {
	best, rank := archiveFile{}, -1
	for _, f := range files {
		r := archiveBookRank(f.Format)
		if r < 0 {
			continue
		}
		if rank < 0 || r < rank {
			best, rank = f, r
		}
	}
	if rank < 0 {
		return nil
	}
	return []archiveFile{best}
}

// archiveOriginals is the rule for everything else — software, images, data,
// web captures. Nothing here knows what a rendition of those looks like, so
// the only honest reading of "what somebody uploaded" is source == original,
// with the archive's own bookkeeping taken back out: an item's generated
// metadata is filed as an original about as often as it is filed as metadata.
func archiveOriginals(files []archiveFile) []archiveFile {
	var out []archiveFile
	for _, f := range files {
		if f.isOriginal() && !archiveHousekeeping(f) {
			out = append(out, f)
		}
	}
	return out
}

// --------------------------------------------------------- format labels

// archiveVideoFormats are the labels that name a video file. Generous on
// purpose: the cost of an unrecognised label is a film that resolves to
// nothing, while the cost of an extra label here is nil, since a movies item
// yields exactly one of whatever matched.
var archiveVideoFormats = map[string]bool{
	"mpeg4": true, "512kb mpeg4": true, "hires mpeg4": true, "hd mpeg4": true,
	"h.264": true, "h.264 ia": true, "h.264 hd": true, "mpeg-4 video": true,
	"mpeg1": true, "mpeg2": true, "quicktime": true, "matroska": true,
	"ogg video": true, "webm": true, "windows media": true, "wmv": true,
	"divx": true, "cinepack": true, "avi": true, "flash video": true,
	"asf": true, "vob": true, "dv": true, "mp4": true,
}

// archiveMPEG4Formats are the derivations to fall back to when an item's
// original is not a video. They are ranked by nothing but size, because the
// labels describe the encoder rather than the result.
var archiveMPEG4Formats = map[string]bool{
	"mpeg4": true, "512kb mpeg4": true, "hires mpeg4": true, "hd mpeg4": true,
	"h.264": true, "h.264 ia": true, "h.264 hd": true, "mpeg-4 video": true,
	"mp4": true,
}

// archiveSubtitleFormats travel with the video rather than instead of it.
var archiveSubtitleFormats = map[string]bool{
	"subrip": true, "closed caption text": true, "webvtt": true,
	"video text tracks": true, "scc": true, "srt": true,
}

// archiveAudioRanking orders audio renditions best first: lossless, then MP3
// at whatever rate, then Ogg. Anything absent from this is not audio and
// never becomes a track.
var archiveAudioRanking = []string{
	"24bit flac", "flac", "shorten", "wave", "aiff", "apple lossless audio",
	"vbr mp3", "320kbps mp3", "256kbps mp3", "192kbps mp3", "128kbps mp3", "64kbps mp3",
	"ogg vorbis", "mpeg-4 audio", "aac",
}

// archiveBookRanking orders a text's readable copies best first. Plain text
// is last and is there for the ebook collections, whose items carry no PDF at
// all and would otherwise resolve to nothing.
var archiveBookRanking = []string{
	"text pdf", "additional text pdf", "image container pdf", "pdf",
	"epub", "text",
}

// archiveSidecarFormats are labels that never name content, whatever the
// mediatype. Only the originals-only branch consults this; the other branches
// are allow-lists and never needed it.
var archiveSidecarFormats = map[string]bool{
	"metadata": true, "item tile": true, "item image": true, "jpeg thumb": true,
	"thumbnail": true, "archive bittorrent": true, "checksums": true, "log": true,
	"columbia peaks": true, "spectrogram": true, "scandata": true,
	"dublin core": true, "marc": true, "marc binary": true,
	"flac fingerprint": true, "essentia high gz": true, "essentia low gz": true,
	"cloth cover detection log": true, "title page detection log": true,
	"ocr page index": true, "ocr search text": true, "page numbers json": true,
	"animated gif": true,
}

// archiveGeneratedSuffixes catch the same bookkeeping by name.
//
// Both nets are needed. The archive files an item's files.xml as format
// "Metadata" in one item and as plain "XML" in the next, so the label alone
// misses it — while the names are built from the identifier by the archive
// itself and never vary.
var archiveGeneratedSuffixes = []string{
	"_files.xml", "_meta.xml", "_meta.sqlite", "_reviews.xml", "_archive.torrent",
	"_dc.xml", "_marc.xml", "_meta.mrc", "_events.json", "_scandata.xml",
	"_itemimage.jpg",
}

func archiveIsVideo(format archiveText) bool { return archiveVideoFormats[archiveLabel(format)] }

func archiveIsMPEG4(format archiveText) bool { return archiveMPEG4Formats[archiveLabel(format)] }

func archiveIsSubtitle(format archiveText) bool { return archiveSubtitleFormats[archiveLabel(format)] }

// archiveEncrypted recognises a DRM-wrapped rendition.
//
// ACS and LCP are the two schemes the archive uses for lending, and their
// files are not flagged private: the transfer succeeds, the bytes arrive, the
// length agrees, and what lands on disk opens in nothing without a licence
// server. Matching the word rather than the four current labels is deliberate
// — a scheme this cannot recognise is one that produces an unopenable file
// that looks like a finished download.
func archiveEncrypted(format archiveText) bool {
	return strings.Contains(archiveLabel(format), "encrypted")
}

// archiveHousekeeping reports whether a file is the archive's own bookkeeping
// rather than anything anybody uploaded.
func archiveHousekeeping(f archiveFile) bool {
	if archiveSidecarFormats[archiveLabel(f.Format)] {
		return true
	}
	name := strings.ToLower(path.Base(f.Name))
	if name == "__ia_thumb.jpg" {
		return true
	}
	for _, suffix := range archiveGeneratedSuffixes {
		if strings.HasSuffix(name, suffix) {
			return true
		}
	}
	// Scan and derivation logs are filed under a directory of their own.
	return strings.HasPrefix(strings.ToLower(f.Name), "logs/")
}

func archiveAudioRank(format archiveText) int { return archiveRankIn(archiveAudioRanking, format) }

func archiveBookRank(format archiveText) int { return archiveRankIn(archiveBookRanking, format) }

// archiveRankIn returns a format's position in a ranking, or -1 when it is
// absent — which, for an allow-list, means "not this kind of file".
func archiveRankIn(ranking []string, format archiveText) int {
	label := archiveLabel(format)
	for i, known := range ranking {
		if label == known {
			return i
		}
	}
	return -1
}

// archiveLabel normalises a format label for comparison. The archive writes
// them for people to read, so they carry capitals and spaces.
func archiveLabel(format archiveText) string {
	return strings.ToLower(strings.TrimSpace(string(format)))
}

// archiveStem is a name with its extension removed, which is what identifies
// a track across its renditions. The directory is kept, because a name may
// legitimately contain a slash and two directories may hold the same track
// name.
func archiveStem(name string) string {
	return strings.TrimSuffix(name, path.Ext(name))
}

// ------------------------------------------------------------ file layout

// archiveFileFor turns one metadata entry into a downloadable file.
func archiveFileFor(id string, f archiveFile) File {
	dir, name := path.Split(f.Name)
	return File{
		Name: name,
		Dir:  strings.Trim(dir, "/"),
		URL:  archiveDownloadURL(id, f.Name),
		// The archive states a real byte count rather than a rounded one, so
		// this is exact and a file already on disk at this length is this
		// file. SizeApprox would throw away the only host in the tree that
		// makes that check trustworthy.
		Size: f.size(),
		Pace: &archivePace,
	}
}

// archiveDownloadURL builds the link for one file within an item.
//
// Two things about it are load-bearing. The name is escaped a segment at a
// time, because a name legitimately contains "/" — a video's thumbnails live
// under a directory named after the item — and escaping the whole name at
// once turns that separator into %2F, asking for a file whose name really
// does contain a slash. And the /download/ route is used rather than the
// storage node the metadata names: the metadata for one item named two nodes
// in the .us pool while the redirect from here went to a .ca one, so pinning
// what the metadata said would be asking a machine that does not have it.
func archiveDownloadURL(id, name string) string {
	segs := strings.Split(name, "/")
	for i, seg := range segs {
		segs[i] = url.PathEscape(seg)
	}
	return archiveDownloadRoot + url.PathEscape(id) + "/" + strings.Join(segs, "/")
}

// ------------------------------------------------------------- the wire

// archiveDoc is the part of the metadata response this reads.
type archiveDoc struct {
	Files    []archiveFile `json:"files"`
	Metadata archiveItem   `json:"metadata"`
	// IsDark marks an item withdrawn from public view. It keeps its
	// metadata and loses its files, so it needs saying rather than being
	// reported as an item that happens to hold nothing.
	IsDark bool `json:"is_dark"`
	// IsCollection is the same fact as mediatype == "collection", stated
	// again at the top level. Both are checked because an item is allowed
	// to be inconsistent and the cost of the check is nothing.
	IsCollection bool `json:"is_collection"`
}

// archiveItem is the item's own metadata.
type archiveItem struct {
	Identifier string      `json:"identifier"`
	Title      archiveText `json:"title"`
	MediaType  archiveText `json:"mediatype"`
}

// archiveFile is one entry in the item's file list.
type archiveFile struct {
	Name   string      `json:"name"`
	Source archiveText `json:"source"`
	Format archiveText `json:"format"`
	// Size is a decimal string, and absent altogether on some entries.
	Size archiveText `json:"size"`
	// Private marks a file that is listed but not served: the download
	// redirect is issued as normal and the node behind it answers 401. It is
	// filtered on rather than discovered, because discovering it costs a
	// failed transfer per file on an item that can hold thirty of them.
	Private archiveText `json:"private"`
}

func (f archiveFile) isOriginal() bool {
	return strings.EqualFold(strings.TrimSpace(string(f.Source)), "original")
}

func (f archiveFile) restricted() bool {
	return strings.EqualFold(strings.TrimSpace(string(f.Private)), "true")
}

// size is the exact length, or -1 when the entry carried none.
func (f archiveFile) size() int64 {
	n, err := strconv.ParseInt(strings.TrimSpace(string(f.Size)), 10, 64)
	if err != nil || n < 0 {
		return -1
	}
	return n
}

// archiveText is a metadata value that may arrive as a string, a number, a
// boolean, or an array of any of those.
//
// The archive's metadata is a schemaless document store where every field may
// repeat: an item edited twice can carry two titles where its neighbour
// carries one, and sizes are written as strings by one deriver and as numbers
// by another. Decoding any of that into a plain Go string fails the whole
// document and loses the item, so every field read here goes through this and
// keeps the first value it finds.
type archiveText string

func (t *archiveText) UnmarshalJSON(b []byte) error {
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	*t = archiveText(archiveScalar(v))
	return nil
}

func archiveScalar(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case bool:
		return strconv.FormatBool(x)
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case []any:
		for _, e := range x {
			if s := archiveScalar(e); s != "" {
				return s
			}
		}
	}
	return ""
}
