package extractor

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"testing"

	"github.com/JohanLindvall/HeapLeach/internal/config"
)

// The fixtures below are hand-written in the shape the metadata API answers
// in, cut down to the fields the extractor reads and keeping every trap the
// real documents carry. Each one says which trap it is for.

// A movies item. The original is not the largest file — the h.264 derivative
// is more than twice its size — and there is a still image filed as an
// original beside the film, the same film derived three more ways, a
// thumbnail directory whose names hold both a slash and a space, an entry
// with no size at all, and the item's own bookkeeping filed as "original"
// rather than as "metadata".
const archiveMoviesItem = `{
  "metadata": {"identifier": "a-film", "title": "A Film", "mediatype": "movies"},
  "files": [
    {"name": "A Film.mp4", "source": "original", "format": "MPEG4", "size": "203137386"},
    {"name": "A Film.mp4.srt", "source": "derivative", "format": "SubRip", "size": "25252"},
    {"name": "A Film.ogv", "source": "derivative", "format": "Ogg Video", "size": "2698690"},
    {"name": "A Film_512kb.mp4", "source": "derivative", "format": "512Kb MPEG4", "size": "2736231"},
    {"name": "A Film_hd.mp4", "source": "derivative", "format": "h.264", "size": "492189849"},
    {"name": "A Film.gif", "source": "derivative", "format": "Animated GIF", "size": "83125"},
    {"name": "Poster.png", "source": "original", "format": "PNG", "size": "130922"},
    {"name": "a-film.thumbs/A Film_000001.jpg", "source": "derivative", "format": "Thumbnail", "size": "35744"},
    {"name": "__ia_thumb.jpg", "source": "original", "format": "Item Tile", "size": "8614"},
    {"name": "a-film_files.xml", "source": "original", "format": "Metadata"},
    {"name": "a-film_archive.torrent", "source": "metadata", "format": "Archive BitTorrent", "size": "9551"}
  ]
}`

// The same item with no original video at all, which is common: an item's
// originals are routinely a poster, a script or a still.
const archiveDerivedMoviesItem = `{
  "metadata": {"identifier": "a-film", "title": "A Film", "mediatype": "movies"},
  "files": [
    {"name": "Poster.png", "source": "original", "format": "PNG", "size": "130922"},
    {"name": "A Film.ogv", "source": "derivative", "format": "Ogg Video", "size": "2698690"},
    {"name": "A Film_512kb.mp4", "source": "derivative", "format": "512Kb MPEG4", "size": "2736231"},
    {"name": "A Film_hd.mp4", "source": "derivative", "format": "h.264", "size": "492189849"}
  ]
}`

// A concert, which the archive files under the "etree" mediatype rather than
// "audio" and lays out as one item holding every track several times over.
// The traps are the waveform PNG sharing a track's stem, the spectrogram
// having a stem of its own, and a third track that was uploaded as an MP3 and
// so has no lossless copy to prefer.
const archiveConcertItem = `{
  "metadata": {"identifier": "a-concert", "title": "A Concert", "mediatype": "etree"},
  "files": [
    {"name": "__ia_thumb.jpg", "source": "original", "format": "Item Tile", "size": "28736"},
    {"name": "a-concert-d1t01.flac", "source": "original", "format": "Flac", "size": "936617"},
    {"name": "a-concert-d1t01.mp3", "source": "derivative", "format": "VBR MP3", "size": "485888"},
    {"name": "a-concert-d1t01.ogg", "source": "derivative", "format": "Ogg Vorbis", "size": "181926"},
    {"name": "a-concert-d1t01.png", "source": "derivative", "format": "PNG", "size": "6885"},
    {"name": "a-concert-d1t02.flac", "source": "original", "format": "Flac", "size": "2867726"},
    {"name": "a-concert-d1t02.mp3", "source": "derivative", "format": "VBR MP3", "size": "814080"},
    {"name": "a-concert-d1t02.ogg", "source": "derivative", "format": "Ogg Vorbis", "size": "328349"},
    {"name": "a-concert-d1t02_spectrogram.png", "source": "derivative", "format": "Spectrogram", "size": "200248"},
    {"name": "a-concert-d1t03.afpk", "source": "derivative", "format": "Columbia Peaks", "size": "515584"},
    {"name": "a-concert-d1t03.mp3", "source": "original", "format": "VBR MP3", "size": "5200384"},
    {"name": "a-concert-d1t03.ogg", "source": "derivative", "format": "Ogg Vorbis", "size": "2361144"},
    {"name": "a-concert_files.xml", "source": "metadata", "format": "XML"}
  ]
}`

// A scanned book, with the OCR family in full and an EPUB five times the size
// of the PDF that should still lose to it.
const archiveBookItem = `{
  "metadata": {"identifier": "a-book", "title": "A Book", "mediatype": "texts"},
  "files": [
    {"name": "OTIFF.zip", "source": "original", "format": "Million Books Original TIFF ZIP", "size": "48831369"},
    {"name": "a-book.djvu", "source": "derivative", "format": "DjVu", "size": "21335670"},
    {"name": "a-book.epub", "source": "derivative", "format": "EPUB", "size": "130383848"},
    {"name": "a-book.pdf", "source": "derivative", "format": "Text PDF", "size": "23599504"},
    {"name": "a-book_abbyy.gz", "source": "derivative", "format": "Abbyy GZ", "size": "3793965"},
    {"name": "a-book_djvu.txt", "source": "derivative", "format": "DjVuTXT", "size": "246179"},
    {"name": "a-book_djvu.xml", "source": "derivative", "format": "Djvu XML", "size": "2358455"},
    {"name": "a-book_hocr.html", "source": "derivative", "format": "hOCR", "size": "5538889"},
    {"name": "a-book_jp2.zip", "source": "derivative", "format": "Single Page Processed JP2 ZIP", "size": "19613037"},
    {"name": "a-book_meta.xml", "source": "metadata", "format": "Metadata", "size": "1370"}
  ]
}`

// A lending item. Everything readable is flagged private and would answer 401
// once the download redirect had been followed; the DRM-wrapped copies beside
// them are not flagged at all and would transfer perfectly into a file no
// reader will open.
const archiveLendingItem = `{
  "metadata": {"identifier": "a-loan", "title": "A Loan", "mediatype": "texts"},
  "files": [
    {"name": "a-loan.epub", "source": "derivative", "format": "EPUB", "size": "1374222", "private": "true"},
    {"name": "a-loan.pdf", "source": "derivative", "format": "Text PDF", "size": "6450146", "private": "true"},
    {"name": "a-loan.lcpdf", "source": "derivative", "format": "LCP Encrypted PDF", "size": "6808144"},
    {"name": "a-loan_encrypted.epub", "source": "derivative", "format": "ACS Encrypted EPUB", "size": "740357"},
    {"name": "a-loan_encrypted.pdf", "source": "derivative", "format": "ACS Encrypted PDF", "size": "6391485"},
    {"name": "a-loan_lcp.epub", "source": "derivative", "format": "LCP Encrypted EPUB", "size": "1950513"},
    {"name": "logs/a-loan_scanning.log", "source": "original", "format": "Log", "size": "523616", "private": "true"},
    {"name": "a-loan_meta.xml", "source": "metadata", "format": "Metadata", "size": "2732"}
  ]
}`

// An ebook collection's item, which carries no PDF and no EPUB at all. Those
// number in the tens of thousands, so the plain-text last resort is not a
// defensive extra.
const archiveEbookItem = `{
  "metadata": {"identifier": "an-ebook", "title": "An Ebook", "mediatype": "texts"},
  "files": [
    {"name": "book10.txt", "source": "original", "format": "Text", "size": "153986"},
    {"name": "book10.zip", "source": "original", "format": "ZIP", "size": "62417"},
    {"name": "an-ebook_files.xml", "source": "metadata", "format": "XML"},
    {"name": "an-ebook_meta.xml", "source": "metadata", "format": "XML", "size": "807"}
  ]
}`

// An item of a kind with no rendition policy of its own, where the rule is
// originals only. Its traps are a name holding both a slash and a space, an
// entry with no size, and three pieces of the archive's own bookkeeping filed
// as originals.
const archiveDataItem = `{
  "metadata": {"identifier": "a-dataset", "title": "A Dataset", "mediatype": "data"},
  "files": [
    {"name": "readings/2019 readings.csv", "source": "original", "format": "Comma-Separated Values", "size": "4096"},
    {"name": "notes.txt", "source": "original", "format": "Text"},
    {"name": "notes.txt.gz", "source": "derivative", "format": "GZIP", "size": "512"},
    {"name": "__ia_thumb.jpg", "source": "original", "format": "Item Tile", "size": "5020"},
    {"name": "a-dataset_files.xml", "source": "original", "format": "Metadata"},
    {"name": "a-dataset_meta.sqlite", "source": "original", "format": "Metadata", "size": "20480"},
    {"name": "a-dataset_archive.torrent", "source": "metadata", "format": "Archive BitTorrent", "size": "2865"}
  ]
}`

// A collection identifier, which is a perfectly valid item holding the
// artwork its own landing page is built from.
const archiveCollectionItem = `{
  "is_collection": true,
  "metadata": {"identifier": "a-collection", "title": "A Collection", "mediatype": "collection"},
  "files": [
    {"name": "header.jpg", "source": "original", "format": "JPEG", "size": "44100"},
    {"name": "icon.gif", "source": "original", "format": "GIF", "size": "1200"},
    {"name": "oldicon.jpg", "source": "original", "format": "JPEG", "size": "3400"},
    {"name": "__ia_thumb.jpg", "source": "original", "format": "Item Tile", "size": "5399"},
    {"name": "a-collection_meta.xml", "source": "metadata", "format": "Metadata", "size": "7105"}
  ]
}`

// An item whose fields arrived in the other spellings the archive's
// schemaless store permits: a repeated title as an array, a size as a number,
// a flag as a boolean.
const archiveOddlyTypedItem = `{
  "metadata": {
    "identifier": "an-odd-item",
    "title": ["A Title Edited Twice", "An Older Title"],
    "mediatype": ["movies"]
  },
  "files": [
    {"name": "clip.mp4", "source": "original", "format": "MPEG4", "size": 203137386, "private": false},
    {"name": "clip_hd.mp4", "source": "derivative", "format": "h.264", "size": "492189849"}
  ]
}`

func archiveDocOf(t *testing.T, body string) *archiveDoc {
	t.Helper()
	var doc archiveDoc
	if err := json.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatalf("fixture does not decode: %v", err)
	}
	return &doc
}

// archiveResolve runs the whole policy over a fixture, which is what every
// behavioural test here is really asserting about.
func archiveResolve(t *testing.T, body, id string) *Result {
	t.Helper()
	res, err := archiveResult(archiveDocOf(t, body), id, "", nil)
	if err != nil {
		t.Fatalf("archiveResult(%s): %v", id, err)
	}
	return res
}

func archiveNamesOf(files []File) []string {
	names := make([]string, 0, len(files))
	for _, f := range files {
		if f.Dir != "" {
			names = append(names, f.Dir+"/"+f.Name)
			continue
		}
		names = append(names, f.Name)
	}
	return names
}

func archiveWantNames(t *testing.T, files []File, want ...string) {
	t.Helper()
	got := archiveNamesOf(files)
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("files = %v, want %v", got, want)
	}
}

// TestArchivePaceIsAlwaysOneOfEverything is the test this whole extractor
// exists around. archive.org answers concurrency with a three-hundredfold
// slowdown applied to the client and outliving the request, so a file that
// leaves here without a pace is a file that will earn that penalty for every
// other job in the queue.
func TestArchivePaceIsAlwaysOneOfEverything(t *testing.T) {
	fixtures := map[string]string{
		"a-film":     archiveMoviesItem,
		"a-concert":  archiveConcertItem,
		"a-book":     archiveBookItem,
		"an-ebook":   archiveEbookItem,
		"a-dataset":  archiveDataItem,
		"an-odd-one": archiveOddlyTypedItem,
	}
	for id, body := range fixtures {
		for _, f := range archiveResolve(t, body, id).Files {
			if f.Pace.Streams != 1 || f.Pace.Files != 1 {
				t.Errorf("%s: %s carries pace %+v, want one connection and one file", id, f.Name, f.Pace)
			}
		}
	}

	// The selector path builds its file separately and has to agree.
	res, err := archiveResult(archiveDocOf(t, archiveMoviesItem), "a-film", "A Film.ogv", nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Files[0].Pace == nil || *res.Files[0].Pace != archivePace {
		t.Errorf("a selected file carries pace %+v, want %+v", res.Files[0].Pace, archivePace)
	}
}

func TestArchiveMoviesKeepsTheOriginalAndItsSubtitles(t *testing.T) {
	res := archiveResolve(t, archiveMoviesItem, "a-film")
	if res.Title != "A Film" {
		t.Errorf("title = %q, want the item's own title", res.Title)
	}
	// Not the h.264, which is 2.4 times the size: the original is preferred
	// because every derivative was made from it.
	archiveWantNames(t, res.Files, "A Film.mp4", "A Film.mp4.srt")

	film := res.Files[0]
	if film.Size != 203137386 || film.SizeApprox {
		t.Errorf("size = %d approx = %v, want the exact byte count the archive states",
			film.Size, film.SizeApprox)
	}
	if want := archiveDownloadRoot + "a-film/A%20Film.mp4"; film.URL != want {
		t.Errorf("url = %q, want %q", film.URL, want)
	}
}

// TestArchiveMoviesFallsBackToTheLargestMPEG4 covers the common case of an
// item whose original is a still rather than the film.
func TestArchiveMoviesFallsBackToTheLargestMPEG4(t *testing.T) {
	res := archiveResolve(t, archiveDerivedMoviesItem, "a-film")
	archiveWantNames(t, res.Files, "A Film_hd.mp4")
}

// TestArchiveTracksKeepOnePerStem pins the grouping that stops a fifteen-track
// concert resolving to sixty files.
func TestArchiveTracksKeepOnePerStem(t *testing.T) {
	res := archiveResolve(t, archiveConcertItem, "a-concert")
	archiveWantNames(t, res.Files,
		"a-concert-d1t01.flac", // lossless beats the MP3 and the Ogg beside it
		"a-concert-d1t02.flac",
		"a-concert-d1t03.mp3", // uploaded as an MP3; there is no lossless copy
	)
	if res.Files[0].Size != 936617 {
		t.Errorf("size = %d, want the flac's own length", res.Files[0].Size)
	}
}

func TestArchiveBookPrefersThePDFOverAMuchLargerEPUB(t *testing.T) {
	res := archiveResolve(t, archiveBookItem, "a-book")
	archiveWantNames(t, res.Files, "a-book.pdf")
}

// TestArchiveBookFallsBackToPlainText covers the ebook collections, whose
// items carry no PDF and no EPUB and would otherwise resolve to nothing.
func TestArchiveBookFallsBackToPlainText(t *testing.T) {
	res := archiveResolve(t, archiveEbookItem, "an-ebook")
	archiveWantNames(t, res.Files, "book10.txt")
}

// TestArchiveOriginalsOnlyDropsBookkeepingFiledAsOriginal is the reason the
// housekeeping filter reads both the format label and the name: an item's
// generated metadata is filed as an original about as often as not.
func TestArchiveOriginalsOnlyDropsBookkeepingFiledAsOriginal(t *testing.T) {
	res := archiveResolve(t, archiveDataItem, "a-dataset")
	archiveWantNames(t, res.Files, "readings/2019 readings.csv", "notes.txt")

	csv := res.Files[0]
	if csv.Dir != "readings" || csv.Name != "2019 readings.csv" {
		t.Errorf("dir/name = %q/%q, want the slash to become a directory", csv.Dir, csv.Name)
	}
	if want := archiveDownloadRoot + "a-dataset/readings/2019%20readings.csv"; csv.URL != want {
		t.Errorf("url = %q, want %q", csv.URL, want)
	}
	// Some entries carry no size at all, and an unknown length is -1 rather
	// than a zero that would read as an empty file.
	if res.Files[1].Size != -1 {
		t.Errorf("size = %d for an entry with no size, want -1", res.Files[1].Size)
	}
}

// TestArchiveRejectsACollection is the guard against the failure that looks
// like success: a collection identifier is a valid item, so without this
// /details/<collection> resolves happily and downloads five logos.
func TestArchiveRejectsACollection(t *testing.T) {
	if _, err := archiveResult(archiveDocOf(t, archiveCollectionItem), "a-collection", "", nil); err == nil {
		t.Fatal("a collection resolved to its own landing-page artwork")
	}

	// The two ways an item says so are checked separately, because an item is
	// allowed to carry one without the other.
	byMediatype := `{"metadata": {"identifier": "c", "mediatype": "collection"},
	                 "files": [{"name": "icon.gif", "source": "original", "format": "GIF", "size": "1"}]}`
	if _, err := archiveResult(archiveDocOf(t, byMediatype), "c", "", nil); err == nil {
		t.Error("mediatype alone did not identify a collection")
	}
	byFlag := `{"is_collection": true, "metadata": {"identifier": "c"},
	            "files": [{"name": "icon.gif", "source": "original", "format": "GIF", "size": "1"}]}`
	if _, err := archiveResult(archiveDocOf(t, byFlag), "c", "", nil); err == nil {
		t.Error("the top-level flag alone did not identify a collection")
	}
}

// TestArchiveRejectsAnIdentifierThatDoesNotExist covers the fact that nothing
// on this host 404s: an unknown identifier answers 200 with "{}".
func TestArchiveRejectsAnIdentifierThatDoesNotExist(t *testing.T) {
	_, err := archiveResult(archiveDocOf(t, `{}`), "not-an-item", "", nil)
	if err == nil {
		t.Fatal("an empty document resolved to something")
	}
	if !strings.Contains(err.Error(), "not-an-item") {
		t.Errorf("error %q does not name the identifier that was asked for", err)
	}
}

// TestArchiveRejectsADarkenedItem keeps a withdrawn item from being reported
// as one that happens to hold nothing. It keeps its metadata and loses its
// files, so it is otherwise indistinguishable.
func TestArchiveRejectsADarkenedItem(t *testing.T) {
	dark := `{"is_dark": true, "server": "a-node.example.test", "dir": "/24/items/a-dark-item"}`
	_, err := archiveResult(archiveDocOf(t, dark), "a-dark-item", "", nil)
	if err == nil || !strings.Contains(err.Error(), "darkened") {
		t.Fatalf("err = %v, want a darkened item to say so", err)
	}
}

// TestArchiveDropsRestrictedAndEncrypted covers the pair of traps on a
// lending item: the readable copies are flagged and would 401, and the copies
// that are not flagged would transfer perfectly into something unopenable.
func TestArchiveDropsRestrictedAndEncrypted(t *testing.T) {
	_, err := archiveResult(archiveDocOf(t, archiveLendingItem), "a-loan", "", nil)
	if err == nil {
		t.Fatal("a lending item resolved to something, which can only be DRM or a 401")
	}
	if !strings.Contains(err.Error(), "restricted") {
		t.Errorf("error %q does not explain that the item is restricted", err)
	}

	// Naming the encrypted format explicitly must not get it either: it is
	// not a rendition of anything, so the escape hatch does not reach it.
	for _, format := range []string{"LCP Encrypted PDF", "ACS Encrypted EPUB", "Text PDF"} {
		if _, err := archiveResult(archiveDocOf(t, archiveLendingItem), "a-loan", "", []string{format}); err == nil {
			t.Errorf("HEAPLEACH_ARCHIVE_FORMATS=%q yielded a file that cannot be downloaded or opened", format)
		}
	}
}

// TestArchiveFormatsOverrideThePolicy covers the escape for a rendition the
// compiled-in policy has no rule for.
func TestArchiveFormatsOverrideThePolicy(t *testing.T) {
	doc := archiveDocOf(t, archiveMoviesItem)
	res, err := archiveResult(doc, "a-film", "", []string{"ogg video", "  512Kb MPEG4  "})
	if err != nil {
		t.Fatalf("archiveResult: %v", err)
	}
	// Matched case-insensitively and with the spacing the environment gave.
	archiveWantNames(t, res.Files, "A Film.ogv", "A Film_512kb.mp4")

	if _, err := archiveResult(doc, "a-film", "", []string{"No Such Format"}); err == nil {
		t.Error("a format nothing matched resolved to something")
	}
}

// TestArchiveSelectorTakesOneNamedFile covers /details/<id>/<file>, which
// answers with the item's page rather than redirecting, so the path is the
// only place the wanted file is named.
func TestArchiveSelectorTakesOneNamedFile(t *testing.T) {
	// The policy is skipped: somebody who named the Ogg copy has chosen.
	res, err := archiveResult(archiveDocOf(t, archiveMoviesItem), "a-film", "A Film.ogv", nil)
	if err != nil {
		t.Fatalf("archiveResult: %v", err)
	}
	archiveWantNames(t, res.Files, "A Film.ogv")

	if _, err := archiveResult(archiveDocOf(t, archiveMoviesItem), "a-film", "nope.mp4", nil); err == nil {
		t.Error("a file the item does not hold resolved to something")
	}

	// A restricted file is refused now rather than after a 401.
	_, err = archiveResult(archiveDocOf(t, archiveLendingItem), "a-loan", "a-loan.pdf", nil)
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Errorf("err = %v, want the restriction explained before the transfer", err)
	}
}

// TestArchiveTextAcceptsTheOtherSpellings guards against losing a whole item
// to a decoding failure. The metadata is a schemaless store: any field may
// repeat, and sizes are written as strings by one deriver and as numbers by
// another.
func TestArchiveTextAcceptsTheOtherSpellings(t *testing.T) {
	res := archiveResolve(t, archiveOddlyTypedItem, "an-odd-item")
	if res.Title != "A Title Edited Twice" {
		t.Errorf("title = %q, want the first of the repeated titles", res.Title)
	}
	archiveWantNames(t, res.Files, "clip.mp4")
	if res.Files[0].Size != 203137386 {
		t.Errorf("size = %d, want a numeric size read as exact", res.Files[0].Size)
	}
}

func TestArchiveFileSizeIsExactOrUnknown(t *testing.T) {
	cases := map[string]int64{
		`{"size": "203137386"}`: 203137386,
		`{"size": 203137386}`:   203137386,
		`{"size": " 42 "}`:      42,
		`{}`:                    -1,
		`{"size": ""}`:          -1,
		`{"size": "banana"}`:    -1,
		`{"size": "-7"}`:        -1,
	}
	for body, want := range cases {
		var f archiveFile
		if err := json.Unmarshal([]byte(body), &f); err != nil {
			t.Fatalf("%s: %v", body, err)
		}
		if got := f.size(); got != want {
			t.Errorf("%s: size = %d, want %d", body, got, want)
		}
	}
}

func TestArchiveParse(t *testing.T) {
	type parsed struct {
		id, selector string
		ok           bool
	}
	cases := map[string]parsed{
		"https://archive.org/details/a-film":                                {"a-film", "", true},
		"https://archive.org/details/a-film/":                               {"a-film", "", true},
		"https://archive.org/details/a-film?start=90":                       {"a-film", "", true},
		"https://archive.org/download/a-film":                               {"a-film", "", true},
		"https://archive.org/metadata/a-film":                               {"a-film", "", true},
		"https://archive.org/embed/a-film":                                  {"a-film", "", true},
		"https://archive.org/details/a-film/A%20Film.mp4":                   {"a-film", "A Film.mp4", true},
		"https://archive.org/download/a-film/a-film.thumbs/A%20Film_01.jpg": {"a-film", "a-film.thumbs/A Film_01.jpg", true},
		// The book reader and the player write their position where a file
		// selector goes; neither is a request for a file called "page".
		"https://archive.org/details/a-book/page/n5/mode/2up": {"a-book", "", true},
		"https://archive.org/details/a-book/mode/2up":         {"a-book", "", true},
		"https://archive.org/details/a-film/theater":          {"a-film", "", true},
		"https://archive.org/details":                         {"", "", false},
		"https://archive.org/about":                           {"", "", false},
		"https://archive.org/":                                {"", "", false},
	}
	for raw, want := range cases {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		id, selector, ok := archiveParse(u)
		if id != want.id || selector != want.selector || ok != want.ok {
			t.Errorf("archiveParse(%s) = %q, %q, %v; want %q, %q, %v",
				raw, id, selector, ok, want.id, want.selector, want.ok)
		}
	}
}

// TestArchiveDownloadURLEscapesPerSegment pins the escaping, where getting it
// wrong in either direction breaks a real item: escaping the whole name turns
// a directory separator into %2F, and escaping nothing leaves spaces and
// question marks in the path.
func TestArchiveDownloadURLEscapesPerSegment(t *testing.T) {
	cases := []struct{ id, name, want string }{
		{"a-film", "A Film.mp4", archiveDownloadRoot + "a-film/A%20Film.mp4"},
		{"a-film", "a-film.thumbs/A Film_01.jpg", archiveDownloadRoot + "a-film/a-film.thumbs/A%20Film_01.jpg"},
		{"a-film", "why? (1948).mkv", archiveDownloadRoot + "a-film/why%3F%20%281948%29.mkv"},
		{"an item", "clip.mp4", archiveDownloadRoot + "an%20item/clip.mp4"},
	}
	for _, c := range cases {
		if got := archiveDownloadURL(c.id, c.name); got != c.want {
			t.Errorf("archiveDownloadURL(%q, %q) = %q, want %q", c.id, c.name, got, c.want)
		}
	}
}

// TestArchiveCapsTheFileCount covers an item with a file per page or per
// minute of broadcast.
func TestArchiveCapsTheFileCount(t *testing.T) {
	doc := &archiveDoc{Metadata: archiveItem{Identifier: "a-big-item", MediaType: "data"}}
	for i := range config.MaxListingFiles + 10 {
		doc.Files = append(doc.Files, archiveFile{
			Name:   fmt.Sprintf("page-%05d.jp2", i),
			Source: "original",
			Format: "JPEG 2000",
			Size:   "4096",
		})
	}
	res, err := archiveResult(doc, "a-big-item", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Files) != config.MaxListingFiles {
		t.Errorf("resolved %d files, want the cap of %d", len(res.Files), config.MaxListingFiles)
	}
}

// TestArchiveMatch keeps the Wayback Machine and the storage nodes out. Both
// are archive.org subdomains and neither has an identifier to look up; the
// direct-link fallback already handles them correctly.
func TestArchiveMatch(t *testing.T) {
	a := NewArchiveOrg(nil, nil)
	yes := []string{
		"https://archive.org/details/a-film",
		"https://www.archive.org/details/a-film",
		"https://archive.org/download/a-film/A%20Film.mp4",
		"https://archive.org./metadata/a-film",
	}
	no := []string{
		"https://web.archive.org/web/2020/https://example.test/",
		"https://ia601607.us.archive.org/6/items/a-film/A%20Film.mp4",
		"https://dn790009.ca.archive.org/0/items/a-film/A%20Film.mp4",
		"https://archive.org/about",
		"https://archive.org/",
		"https://openlibrary.org/works/OL1W",
	}
	for _, raw := range yes {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		if !a.Match(u) {
			t.Errorf("Match(%q) = false", raw)
		}
	}
	for _, raw := range no {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		if a.Match(u) {
			t.Errorf("Match(%q) = true", raw)
		}
	}
}

func TestArchiveFormatsComeFromTheConfig(t *testing.T) {
	a := NewArchiveOrg(&config.Config{ArchiveFormats: []string{"Ogg Video"}}, nil)
	if len(a.formats) != 1 || a.formats[0] != "Ogg Video" {
		t.Errorf("formats = %v, want the configured list", a.formats)
	}
	if NewArchiveOrg(nil, nil).formats != nil {
		t.Error("a nil config produced a format list")
	}
}
