package extractor

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

func testABCListen(t *testing.T) *ABCListen {
	t.Helper()
	// No retries: a test that means to see a failure should see it at once,
	// and none of these fixtures is worth asking twice for.
	return &ABCListen{client: httpx.New("test-agent", "en-AU", 0, 5*time.Second)}
}

// abcListenHTML wraps a JSON island in the least page that carries one, which
// is all the parser looks at.
func abcListenHTML(title, island string) string {
	return `<!doctype html><html><head><title>` + title + `</title>` +
		`<meta charset="utf-8"></head><body><div id="root"></div>` +
		`<script id="__NEXT_DATA__" type="application/json">` + island +
		`</script></body></html>`
}

// ------------------------------------------------------------------ traps

// abcListenAACIsland is one episode with every rendition trap the real
// catalogue has, and nothing else changed:
//
//   - a blank entry, every field null and nowhere to fetch it from;
//   - a smaller MP3 whose bitrate is the only one stated, so ranking by
//     bitrate would take it over the two-hour AAC beside it;
//   - that AAC listed twice, the second time under
//     application/vnd.apple.mpegurl — the HLS type — though the URL under
//     both is the same plain .aac file.
//
// The programme is also named exactly what the episode is, which is how a
// music show titles every week's instalment.
const abcListenAACIsland = `{
  "props": { "pageProps": {
    "webDistributionRestricted": false,
    "data": {
      "allowMediaDownload": "all",
      "documentProps": {
        "id": "100000001",
        "title": "00s Night",
        "docType": "audioepisode",
        "duration": 7200,
        "webDistributionRestricted": false,
        "analytics": { "document": { "program": { "name": "00s Night", "id": "2000001" } } },
        "renditions": [
          { "bitrate": null, "codec": "", "MIMEType": "", "fileSize": null, "height": null, "url": "" },
          { "bitrate": 192, "codec": "MPEG Audio", "MIMEType": "audio/mpeg", "fileSize": 41065013, "height": null,
            "url": "https://media.example.test/audio/02/nn/Z/hd.mp3" },
          { "bitrate": 0, "codec": "AAC", "MIMEType": "audio/aac", "fileSize": 118241877, "height": null,
            "url": "https://media.example.test/audio/02/no/Z/32.aac" },
          { "bitrate": 0, "codec": "AAC", "MIMEType": "application/vnd.apple.mpegurl", "fileSize": 118241877, "height": null,
            "url": "https://media.example.test/audio/02/no/Z/32.aac" }
        ]
      }
    }
  } }
}`

// TestABCListenEpisodePicksTheWholeFileNotTheLabel covers all three traps at
// once, because they arrive together on a real page.
func TestABCListenEpisodePicksTheWholeFileNotTheLabel(t *testing.T) {
	page, err := abcListenParse(abcListenHTML("00s Night - ABC listen", abcListenAACIsland))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	u, _ := url.Parse("https://www.abc.net.au/listen/programs/00s-night/00s-night/100000001")

	res, err := testABCListen(t).episode(u, page.props.Data.DocumentProps)
	if err != nil {
		t.Fatalf("episode: %v", err)
	}
	if len(res.Files) != 1 {
		t.Fatalf("got %d files, want the one episode", len(res.Files))
	}

	file := res.Files[0]
	if want := "https://media.example.test/audio/02/no/Z/32.aac"; file.URL != want {
		t.Errorf("url = %q, want the largest rendition %q (bitrate is 0 on every AAC, so it cannot rank them)",
			file.URL, want)
	}
	if strings.HasSuffix(file.Name, ".m3u8") || !strings.HasSuffix(file.Name, ".aac") {
		t.Errorf("name = %q, want the extension the URL states rather than the MIME type beside it", file.Name)
	}
	if file.Segments != nil {
		t.Error("the mislabelled rendition sent this down the playlist path")
	}
	// The programme is named the same as the episode, so saying it twice
	// would be noise.
	if want := "100000001 00s Night.aac"; file.Name != want {
		t.Errorf("name = %q, want %q", file.Name, want)
	}
	// A media-core length is the object's own, so it is exact and a file
	// already on disk at that length may be skipped without connecting.
	if file.Size != 118241877 || file.SizeApprox {
		t.Errorf("size = %d approx = %v, want the exact 118241877", file.Size, file.SizeApprox)
	}
	// Nothing about this file is restricted, so it must take the plain path.
	if file.Resolve != nil {
		t.Error("an ordinary episode was given a resolver, which costs a request per attempt")
	}
}

// abcListenLegacyIsland is an episode from the older library, whose published
// length is computed from the duration rather than measured. Every one
// checked against the CDN was short, by between one and eighty kilobytes, and
// the only tell at extraction time is the unregistered audio/mp3 type it
// carries where the current media core writes audio/mpeg.
const abcListenLegacyIsland = `{
  "props": { "pageProps": {
    "data": { "documentProps": {
      "id": "13739740",
      "title": "Controlling the chatter in your head",
      "docType": "audioepisode",
      "analytics": { "document": { "program": { "name": "All In The Mind" } } },
      "renditions": [
        { "bitrate": 192, "codec": "MPEG-1 Audio layer 3", "MIMEType": "audio/mp3", "fileSize": 41931072, "height": null,
          "url": "https://legacy.example.test/rn/podcast/2022/02/aim_20220206.mp3" }
      ]
    } }
  } }
}`

// TestABCListenDerivedLengthIsNotExact pins the one thing that must not be
// got wrong in the other direction: a length that is merely close would have
// the skip check compare against a number the file can never reach.
func TestABCListenDerivedLengthIsNotExact(t *testing.T) {
	page, err := abcListenParse(abcListenHTML("Controlling the chatter in your head - ABC listen", abcListenLegacyIsland))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	u, _ := url.Parse("https://www.abc.net.au/listen/programs/allinthemind/chatter/13739740")

	res, err := testABCListen(t).episode(u, page.props.Data.DocumentProps)
	if err != nil {
		t.Fatalf("episode: %v", err)
	}
	file := res.Files[0]
	if file.Size != 41931072 {
		t.Errorf("size = %d, want the published 41931072 — it is still worth totalling a job with", file.Size)
	}
	if !file.SizeApprox {
		t.Error("a length the site worked out was reported as exact")
	}
	// A lone file lands in the download directory with nothing around it, so
	// the name has to say which programme it belongs to.
	if want := "13739740 All In The Mind - Controlling the chatter in your head.mp3"; file.Name != want {
		t.Errorf("name = %q, want %q", file.Name, want)
	}
	if want := "All In The Mind - Controlling the chatter in your head"; res.Title != want {
		t.Errorf("title = %q, want %q", res.Title, want)
	}
}

// TestABCListenLengthIsTrustedOnlyWhereItWasChecked pins which way the rule
// fails. The media core's three types were measured against the CDN and
// matched; everything else — the older library's audio/mp3, the mislabelled
// twin, and any name nobody here has seen — costs a skip rather than risking
// a length the file can never reach.
func TestABCListenLengthIsTrustedOnlyWhereItWasChecked(t *testing.T) {
	tests := map[string]bool{
		"audio/mpeg":                    true,
		"audio/aac":                     true,
		"video/mp4":                     true,
		"AUDIO/MPEG ":                   true,
		"audio/mp3":                     false,
		"application/vnd.apple.mpegurl": false,
		"audio/ogg":                     false,
		"":                              false,
	}
	for mime, want := range tests {
		if got := (abcListenRendition{MIMEType: mime}).measured(); got != want {
			t.Errorf("measured(%q) = %v, want %v", mime, got, want)
		}
	}
}

// ------------------------------------------------------------- refusals

// abcListenAppOnlyIsland is a programme the ABC publishes through its app
// alone: the flag is set, and the page carries neither media nor a listing
// to fall back on. The audiobooks are the case in point.
const abcListenAppOnlyIsland = `{
  "props": { "pageProps": {
    "webDistributionRestricted": true,
    "data": null,
    "programCollectionPrepared": null,
    "latestEpisodePrepared": null
  } }
}`

func TestABCListenAppOnlyProgrammeSaysSo(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, abcListenHTML("An Audiobook - ABC listen", abcListenAppOnlyIsland))
	}))
	t.Cleanup(srv.Close)

	u, _ := url.Parse(srv.URL + "/listen/programs/an-audiobook")
	_, err := testABCListen(t).Extract(context.Background(), u, Options{})
	if err == nil {
		t.Fatal("an app-only programme resolved")
	}
	if !strings.Contains(err.Error(), "listen app") {
		t.Errorf("error = %q, want the app restriction rather than an empty listing", err)
	}
}

// abcListenPersonIsland is a presenter page. It carries a document like an
// episode does and no renditions at all, which is what makes it worth a
// message of its own rather than a decode failure.
const abcListenPersonIsland = `{
  "props": { "pageProps": {
    "data": { "documentProps": {
      "id": "11866382",
      "title": "A Presenter",
      "docType": "person",
      "renditions": []
    } }
  } }
}`

func TestABCListenPresenterPageIsNotAnEpisode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, abcListenHTML("A Presenter - ABC listen", abcListenPersonIsland))
	}))
	t.Cleanup(srv.Close)

	u, _ := url.Parse(srv.URL + "/listen/radionational/a-presenter/11866382")
	_, err := testABCListen(t).Extract(context.Background(), u, Options{})
	if err == nil {
		t.Fatal("a presenter page resolved to something downloadable")
	}
	if !strings.Contains(err.Error(), "no audio or video") {
		t.Errorf("error = %q, want it to say the page carries no media", err)
	}
}

func TestABCListenPageWithoutTheIslandFails(t *testing.T) {
	if _, err := abcListenParse(`<html><head><title>Nothing</title></head><body></body></html>`); err == nil {
		t.Fatal("a page with no JSON island parsed")
	}
}

// ------------------------------------------------------------ geo bucket

func TestABCListenGeoBucketIsReadOffThePath(t *testing.T) {
	tests := map[string]bool{
		"https://media.example.test/audio/02/nn/Z/hd.mp3":         false,
		"https://media.example.test/geo-001/audio/02/n5/Z/40.mp3": true,
		// A second numbered bucket must be recognised too, rather than
		// silently treated as the open catalogue.
		"https://media.example.test/geo-002/audio/02/n5/Z/40.mp3": true,
		"https://media.example.test/geography/clip.mp3":           false,
		"": false,
	}
	for raw, want := range tests {
		if got := abcListenGeoBucket(raw); got != want {
			t.Errorf("abcListenGeoBucket(%q) = %v, want %v", raw, got, want)
		}
	}
}

// TestABCListenGeoRefusalIsASentence covers the whole path: the bucket is
// spotted at extraction, the check runs when the item starts, and what comes
// back names the restriction instead of the status code. A 403 fails a
// transfer before File.Reject is ever consulted, so this is the only place
// the extractor gets to speak.
func TestABCListenGeoRefusalIsASentence(t *testing.T) {
	var (
		asked string
		page  string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/geo-001/") {
			// What the real bucket answers from outside Australia: a refusal
			// with a web page behind it and nothing that says why.
			asked = r.Header.Get(httpx.HeaderRange)
			w.Header().Set(httpx.HeaderContentType, "text/html")
			w.WriteHeader(http.StatusForbidden)
			return
		}
		_, _ = io.WriteString(w, page)
	}))
	t.Cleanup(srv.Close)
	page = abcListenHTML("A Restricted Episode - ABC listen",
		abcListenGeoIsland(srv.URL+"/geo-001/audio/02/n5/Z/40.mp3"))

	u, _ := url.Parse(srv.URL + "/listen/programs/abc-rewind/an-episode/106914352")
	res, err := testABCListen(t).Extract(context.Background(), u, Options{})
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	// Extraction must not refuse: whether the caller is in Australia is not
	// something the page can say, and someone there downloads this fine.
	file := res.Files[0]
	if file.Resolve == nil {
		t.Fatal("a file in a geo bucket was queued with no check, so its refusal would arrive as a bare 403")
	}

	target, err := file.Resolve(context.Background())
	if err == nil {
		t.Fatalf("the refusal resolved to %v", target)
	}
	if !strings.Contains(err.Error(), "Australia") {
		t.Errorf("error = %q, want it to name the restriction", err)
	}
	if asked != "bytes=0-0" {
		t.Errorf("the check asked for %q, want a single byte", asked)
	}
}

// TestABCListenGeoCheckStandsAsideWhenAllowed pins the other half: inside
// Australia the check must cost one small request and then get out of the
// way, handing back the same URL rather than a different one.
func TestABCListenGeoCheckStandsAsideWhenAllowed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(httpx.HeaderContentRange, "bytes 0-0/49249399")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write([]byte{0})
	}))
	t.Cleanup(srv.Close)

	link := srv.URL + "/geo-001/audio/02/n5/Z/40.mp3"
	headers := httpx.Referer(abcListenRoot + "/")
	target, err := testABCListen(t).geoCheck(link, headers)(context.Background())
	if err != nil {
		t.Fatalf("an allowed file was refused: %v", err)
	}
	if target == nil || target.URL != link {
		t.Errorf("target = %v, want the same URL back", target)
	}
	// The target must not carry a length: the item already has the exact one
	// the page published, and a Size here would only overwrite it.
	if target.Size != 0 {
		t.Errorf("target size = %d, want the item's own to stand", target.Size)
	}
}

// abcListenGeoIsland is an episode whose only rendition sits in the bucket
// the ABC serves inside Australia. Nothing else in the document says so:
// webDistributionRestricted is false and the download is offered like any
// other, which is exactly why the path has to be read.
func abcListenGeoIsland(link string) string {
	return fmt.Sprintf(`{
  "props": { "pageProps": {
    "webDistributionRestricted": false,
    "data": { "documentProps": {
      "id": "106914352",
      "title": "An Episode",
      "docType": "audioepisode",
      "webDistributionRestricted": false,
      "allowMediaDownload": "all",
      "analytics": { "document": { "program": { "name": "ABC Rewind" } } },
      "renditions": [
        { "bitrate": 192, "codec": "MPEG Audio", "MIMEType": "audio/mpeg", "fileSize": 49249399, "height": null,
          "url": %q }
      ]
    } }
  } }
}`, link)
}

// --------------------------------------------------------------- listings

// abcListenProgrammeIsland is a programme page. It renders only the first two
// of its four episodes and states the total, which is what sends the rest to
// the paging endpoint.
func abcListenProgrammeIsland() string {
	return `{
  "props": { "pageProps": {
    "webDistributionRestricted": false,
    "data": null,
    "programCollectionPrepared": {
      "id": "105470722",
      "headingPrepared": "More from A Programme",
      "programId": "2892006",
      "programTemplate": "audio",
      "loadMoreUrl": "/listen/programs/a-programme",
      "pagination": { "collectionLoaderLimit": 2, "offset": 0, "size": 2, "total": 4 },
      "items": [
        { "articleLink": "/listen/programs/a-programme/the-newest/100000004", "cardId": "100000004", "cardTitle": "A Programme", "docType": "audioepisode" },
        { "articleLink": "/listen/programs/a-programme/next-one-down/100000003", "cardId": "100000003", "cardTitle": "Something else entirely", "docType": "audioepisode" }
      ]
    }
  } }
}`
}

// abcListenMoreEpisodes is what the paging endpoint answers with. Its
// pagination writes the page size as a string where the page wrote a number,
// which is why nothing here decodes that field.
const abcListenMoreEpisodes = `{
  "collection": {
    "id": "105470722",
    "programId": null,
    "programTemplate": "audio",
    "pagination": { "collectionLoaderLimit": "50", "offset": 2, "size": 50, "total": 4 },
    "items": [
      { "articleLink": "/listen/programs/a-programme/older/100000002", "cardId": "100000002", "cardTitle": "A Programme", "docType": "audioepisode" },
      { "articleLink": "/listen/programs/a-programme/oldest/100000001", "cardId": "100000001", "cardTitle": "Text only", "docType": "audioepisode" }
    ]
  }
}`

// abcListenEpisodeIsland builds one episode of that programme. An empty
// rendition list stands for the episodes the catalogue lists but has nothing
// behind.
func abcListenEpisodeIsland(id, title string, size int64) string {
	renditions := "[]"
	if size > 0 {
		renditions = fmt.Sprintf(
			`[{ "bitrate": 192, "codec": "MPEG Audio", "MIMEType": "audio/mpeg", "fileSize": %d, "height": null,
			    "url": "https://media.example.test/audio/%s.mp3" }]`, size, id)
	}
	return fmt.Sprintf(`{
  "props": { "pageProps": {
    "webDistributionRestricted": false,
    "data": { "documentProps": {
      "id": %q,
      "title": %q,
      "docType": "audioepisode",
      "analytics": { "document": { "program": { "name": "A Programme" } } },
      "renditions": %s
    } }
  } }
}`, id, title, renditions)
}

// abcListenServer serves one programme, its paging endpoint and its four
// episode pages, and records what was asked for. The episode pages are
// fetched concurrently, so the record is guarded.
//
// episodesStatus lets a test break the paging endpoint alone.
func abcListenServer(t *testing.T, episodesStatus int) (*httptest.Server, func() []string) {
	t.Helper()
	var (
		mu    sync.Mutex
		asked []string
	)

	episodes := map[string]string{
		"/listen/programs/a-programme/the-newest/100000004":    abcListenEpisodeIsland("100000004", "A Programme", 4000000),
		"/listen/programs/a-programme/next-one-down/100000003": abcListenEpisodeIsland("100000003", "Something else entirely", 3000000),
		"/listen/programs/a-programme/older/100000002":         abcListenEpisodeIsland("100000002", "A Programme", 2000000),
		"/listen/programs/a-programme/oldest/100000001":        abcListenEpisodeIsland("100000001", "Text only", 0),
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		asked = append(asked, r.URL.RequestURI())
		mu.Unlock()

		switch {
		case strings.HasPrefix(r.URL.Path, abcListenEpisodesPath):
			if episodesStatus != http.StatusOK {
				w.WriteHeader(episodesStatus)
				return
			}
			_, _ = io.WriteString(w, abcListenMoreEpisodes)
		case r.URL.Path == "/listen/programs/a-programme":
			_, _ = io.WriteString(w, abcListenHTML("A Programme - ABC listen", abcListenProgrammeIsland()))
		default:
			island, ok := episodes[r.URL.Path]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_, _ = io.WriteString(w, abcListenHTML("An Episode - ABC listen", island))
		}
	}))
	t.Cleanup(srv.Close)
	return srv, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), asked...)
	}
}

// TestABCListenProgrammeExpands covers the listing end to end: the page's own
// episodes, the ones only the endpoint knows about, the order they arrive in,
// and the naming that keeps two episodes of the same title apart.
func TestABCListenProgrammeExpands(t *testing.T) {
	srv, asked := abcListenServer(t, http.StatusOK)

	u, _ := url.Parse(srv.URL + "/listen/programs/a-programme")
	res, err := testABCListen(t).Extract(context.Background(), u, Options{})
	if err != nil {
		t.Fatalf("extract: %v", err)
	}

	// The programme's own name, off the page title with the site's fixed
	// suffix trimmed. It names the folder every episode lands in.
	if res.Title != "A Programme" {
		t.Errorf("title = %q, want the page title without its site suffix", res.Title)
	}

	// Four listed, one of them with nothing behind it: a catalogue entry that
	// will not resolve must not sink the rest.
	want := []string{
		"100000004 A Programme.mp3",
		"100000003 Something else entirely.mp3",
		"100000002 A Programme.mp3",
	}
	if len(res.Files) != len(want) {
		t.Fatalf("got %d files, want %d: %v", len(res.Files), len(want), res.Files)
	}
	for i, name := range want {
		if res.Files[i].Name != name {
			t.Errorf("file %d is %q, want %q", i, res.Files[i].Name, name)
		}
	}
	// Two of those episodes are both called "A Programme". Without the id
	// they collide in one folder and the downloader falls back to "(2)",
	// which lands on a different episode the next run.
	if res.Files[0].Name == res.Files[2].Name {
		t.Error("two episodes of the same title were given the same filename")
	}
	// They go in a folder named after the programme, so repeating it in every
	// filename would say nothing.
	for _, file := range res.Files {
		if strings.Contains(file.Name, " - ") {
			t.Errorf("file %q carries the programme name, which its folder already says", file.Name)
		}
	}

	var listing string
	for _, uri := range asked() {
		if strings.HasPrefix(uri, abcListenEpisodesPath) {
			listing = uri
		}
	}
	// The path after the endpoint is decoration the server ignores; the
	// collection id is what selects, and the offset must continue after the
	// episodes the page already rendered rather than repeating them.
	for _, want := range []string{
		abcListenEpisodesPath + "/listen/programs/a-programme?",
		"collectionId=105470722", "offset=2", "size=50",
	} {
		if !strings.Contains(listing, want) {
			t.Errorf("listing request %q is missing %q", listing, want)
		}
	}
}

// TestABCListenProgrammeSurvivesAListingFailure pins why the page's own
// episodes are kept rather than being re-fetched from the endpoint: when the
// endpoint stops answering, a programme still resolves to the newest
// episodes instead of to nothing at all.
func TestABCListenProgrammeSurvivesAListingFailure(t *testing.T) {
	srv, _ := abcListenServer(t, http.StatusNotFound)

	u, _ := url.Parse(srv.URL + "/listen/programs/a-programme")
	res, err := testABCListen(t).Extract(context.Background(), u, Options{})
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if len(res.Files) != 2 {
		t.Fatalf("got %d files, want the 2 the page itself rendered: %v", len(res.Files), res.Files)
	}
	if res.Files[0].Name != "100000004 A Programme.mp3" {
		t.Errorf("first file is %q, want the newest episode", res.Files[0].Name)
	}
}

// --------------------------------------------------------------- matching

func TestABCListenMatch(t *testing.T) {
	tests := map[string]bool{
		"https://www.abc.net.au/listen":                                   true,
		"https://www.abc.net.au/listen/programs/a-programme":              true,
		"https://abc.net.au/listen/programs/a-programme/an-episode/12345": true,
		// The domain is the whole broadcaster. None of these is ours.
		"https://www.abc.net.au/news/2026-08-22/a-story/12345": false,
		"https://iview.abc.net.au/show/a-show":                 false,
		"https://www.abc.net.au/":                              false,
		"https://listen.example.test/listen/programs/a":        false,
	}
	ex := &ABCListen{}
	for raw, want := range tests {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		if got := ex.Match(u); got != want {
			t.Errorf("Match(%s) = %v, want %v", raw, got, want)
		}
	}
}

// TestABCListenNameKeepsTheId is the naming rule on its own, including what
// it does when half of it is missing.
func TestABCListenNameKeepsTheId(t *testing.T) {
	tests := []struct{ id, title, want string }{
		{"106964748", "Why should I spend time in nature?", "106964748 Why should I spend time in nature?"},
		{"106964748", "", "106964748"},
		{"", "An Episode", "An Episode"},
		{" 106964748 ", " An Episode ", "106964748 An Episode"},
	}
	for _, tc := range tests {
		if got := abcListenName(tc.id, tc.title); got != tc.want {
			t.Errorf("abcListenName(%q, %q) = %q, want %q", tc.id, tc.title, got, tc.want)
		}
	}
}
