package extractor

import (
	"net/url"
	"strings"
	"testing"
)

// nprListingPage is a page in the shape NPR serves one, with everything that
// has to be got right on it at once. The markup is trimmed to the attribute
// that matters; every other property is real.
//
//   - The first block appears twice. NPR renders the segment featured at the
//     head of a rundown again in the list below it, from the same player
//     module and with identical metadata, so a page always lists some of its
//     audio twice.
//   - Two delivery paths are represented, because the length stated in the
//     URL is only true on one of them. The store's figure is the file's own;
//     a podcast's is what the episode measured before advertising was
//     stitched in, and the file that arrives is megabytes longer.
//   - The bonus episode has available:true and no audio at all. It is an NPR+
//     subscriber episode, and podcastEpisodeDerivedPlusType is the only field
//     that says so.
//   - The ampersands in the query strings are unencoded, exactly as the page
//     writes them. Reading the attribute with a tokeniser rather than a
//     pattern is what keeps them out of the character-reference table.
//   - Titles arrive with stray whitespace, with a slash in them, and with
//     their apostrophes escaped as JSON rather than as HTML.
const nprListingPage = `<!doctype html><html><head>
<title>A Programme : NPR</title>
<meta property="og:title" content="A Programme"/>
</head><body>

<div class="audio-module-controls-wrap" data-audio='{"uid":"nx-s1-1000001:nx-s1-2000001","available":true,"duration":310,"title":"A segment","audioUrl":"https:\/\/ondemand.npr.org\/anon.npr-mp3\/npr\/example\/00000000_ex_one.mp3?t=progseg&e=nx-s1-3000001&p=2&seg=0&d=310&size=4965817&sc=siteplayer","program":"A Programme","type":"segment","isStreamAudioType":false,"podcastEpisodeDerivedPlusType":""}'></div>

<div class="audio-module-listen" data-audio='{"uid":"nx-s1-1000001:nx-s1-2000001","available":true,"duration":310,"title":"A segment","audioUrl":"https:\/\/ondemand.npr.org\/anon.npr-mp3\/npr\/example\/00000000_ex_one.mp3?t=progseg&e=nx-s1-3000001&p=2&seg=0&d=310&size=4965817&sc=siteplayer","program":"A Programme","type":"segment","isStreamAudioType":false,"podcastEpisodeDerivedPlusType":""}'></div>

<div data-audio='{"uid":"nx-s1-1000002:nx-s1-2000002","available":true,"duration":250,"title":"\n  Nairobi \/ Lagos  ","audioUrl":"https:\/\/ondemand.npr.org\/anon.npr-mp3\/npr\/example\/00000000_ex_two.mp3?t=progseg&e=nx-s1-3000001&p=2&seg=1&d=250&sc=siteplayer","program":"A Programme","type":"segment","isStreamAudioType":false,"podcastEpisodeDerivedPlusType":""}'></div>

<div data-audio='{"uid":"nx-s1-1000003:nx-s1-2000003","available":true,"duration":211,"title":"A segment","audioUrl":"https:\/\/ondemand.npr.org\/anon.npr-mp3\/npr\/example\/00000000_ex_three.mp3?t=progseg&e=nx-s1-3000001&p=2&seg=2&d=211&size=3377991&sc=siteplayer","program":"A Programme","type":"segment","isStreamAudioType":false,"podcastEpisodeDerivedPlusType":""}'></div>

<div data-audio='{"uid":"nx-s1-1000004:nx-s1-mx-1000004-1","available":true,"duration":2278,"title":"An episode\n","audioUrl":"https:\/\/tracking.swap.fm\/track\/AAAAAAAAAAAAAAAAAAAA\/prfx.byspotify.com\/e\/play.podtrac.com\/npr-000000\/npr.simplecastaudio.com\/00000000-0000-0000-0000-000000000000\/episodes\/11111111-1111-1111-1111-111111111111\/audio\/128\/default.mp3?awCollectionId=00000000-0000-0000-0000-000000000000&t=podcast&e=nx-s1-1000004&p=000000&d=2278&size=36461592&sc=siteplayer&aw_0_1st.playerid=siteplayer","program":"A Podcast","type":"segment","isStreamAudioType":false,"podcastEpisodeRawType":"FULL","podcastEpisodeDerivedPlusType":""}'></div>

<div data-audio='{"uid":"nx-s1-1000005:nx-s1-mx-1000005-1","available":true,"duration":1080,"title":"It\u0027s another episode","audioUrl":"https:\/\/npr.simplecastaudio.com\/00000000-0000-0000-0000-000000000000\/episodes\/22222222-2222-2222-2222-222222222222\/audio\/128\/default.mp3?awCollectionId=00000000-0000-0000-0000-000000000000&sc=siteplayer","program":"A Podcast","type":"segment","isStreamAudioType":false,"podcastEpisodeDerivedPlusType":""}'></div>

<div data-audio='{"uid":"nx-s1-1000006:nx-s1-mx-1000006-3","available":true,"duration":3164,"title":"A Podcast-08.03.2026","audioUrl":"","program":"A Podcast","type":"segment","isStreamAudioType":false,"skipSponsorship":true,"podcastEpisodeRawType":"BONUS","podcastEpisodeDerivedPlusType":"PLUS_EXCLUSIVE_BONUS"}'></div>

<div data-audio='{"uid":"nx-s1-1000007:nx-s1-2000007","available":false,"duration":180,"title":"A withdrawn segment","audioUrl":"https:\/\/ondemand.npr.org\/anon.npr-mp3\/npr\/example\/00000000_ex_gone.mp3?t=progseg&e=nx-s1-3000001&p=2&seg=3&d=180&size=2884610&sc=siteplayer","program":"A Programme","type":"segment","isStreamAudioType":false,"podcastEpisodeDerivedPlusType":""}'></div>

<div data-audio='{"uid":"nx-s1-1000008:nx-s1-2000008","available":true,"duration":0,"title":"The live stream","audioUrl":"https:\/\/ondemand.npr.org\/anon.npr-mp3\/npr\/example\/live.mp3?t=live&sc=siteplayer","program":"","type":"stream","isStreamAudioType":true,"podcastEpisodeDerivedPlusType":""}'></div>

</body></html>`

// nprSingleProgrammePage is a programme's own page: every piece of audio on
// it belongs to the same strand, which is what decides that nothing is filed
// into a folder.
const nprSingleProgrammePage = `<!doctype html><html><head>
<title>A Programme : NPR</title>
<meta property="og:title" content="A Programme"/>
</head><body>
<div data-audio='{"uid":"a:1","available":true,"title":"First","audioUrl":"https:\/\/ondemand.npr.org\/anon.npr-mp3\/npr\/example\/00000000_ex_one.mp3?t=progseg&size=100","program":"A Programme"}'></div>
<div data-audio='{"uid":"a:2","available":true,"title":"Second","audioUrl":"https:\/\/ondemand.npr.org\/anon.npr-mp3\/npr\/example\/00000000_ex_two.mp3?t=progseg&size=200","program":"A Programme"}'></div>
</body></html>`

// nprWithheldPage is a podcast page whose episodes are all NPR+ bonus
// material: listed like any other, available, and with nothing to fetch.
const nprWithheldPage = `<!doctype html><html><head>
<title>A Podcast : NPR</title>
</head><body>
<div data-audio='{"uid":"b:1","available":true,"title":"Bonus one","audioUrl":"","program":"A Podcast","podcastEpisodeRawType":"BONUS","podcastEpisodeDerivedPlusType":"PLUS_EXCLUSIVE_BONUS"}'></div>
<div data-audio='{"uid":"b:2","available":true,"title":"Bonus two","audioUrl":"","program":"A Podcast","podcastEpisodeRawType":"BONUS","podcastEpisodeDerivedPlusType":"PLUS_EXCLUSIVE_BONUS"}'></div>
<div data-audio='{"uid":"b:3","available":false,"title":"Withdrawn","audioUrl":"","program":"A Podcast","podcastEpisodeDerivedPlusType":""}'></div>
</body></html>`

func nprTestURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := ParseURL(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// TestNPRPageListsEveryPieceOfAudioOnce covers the fold. A page repeats a
// block for every control it draws for the same audio, and each repeat would
// otherwise be a second copy of the same download.
func TestNPRPageListsEveryPieceOfAudioOnce(t *testing.T) {
	root, err := parseHTML(nprListingPage)
	if err != nil {
		t.Fatal(err)
	}

	entries := nprAudioFrom(root)
	if len(entries) != 8 {
		t.Fatalf("got %d entries, want the 8 distinct blocks of the 9 on the page", len(entries))
	}

	want := []string{
		"nx-s1-1000001:nx-s1-2000001",
		"nx-s1-1000002:nx-s1-2000002",
		"nx-s1-1000003:nx-s1-2000003",
		"nx-s1-1000004:nx-s1-mx-1000004-1",
		"nx-s1-1000005:nx-s1-mx-1000005-1",
		"nx-s1-1000006:nx-s1-mx-1000006-3",
		"nx-s1-1000007:nx-s1-2000007",
		"nx-s1-1000008:nx-s1-2000008",
	}
	for i, uid := range want {
		if entries[i].UID != uid {
			t.Errorf("entry %d is %q, want %q — the page's own order is the download order", i, entries[i].UID, uid)
		}
	}
}

// TestNPRAudioURLSurvivesTheParser is why the attribute is read with the HTML
// tokeniser rather than matched out of the text. These values are query
// strings with unencoded ampersands, and half of what follows one reads as
// the opening of a character reference.
func TestNPRAudioURLSurvivesTheParser(t *testing.T) {
	root, err := parseHTML(nprListingPage)
	if err != nil {
		t.Fatal(err)
	}

	entries := nprAudioFrom(root)
	const want = "https://ondemand.npr.org/anon.npr-mp3/npr/example/00000000_ex_one.mp3" +
		"?t=progseg&e=nx-s1-3000001&p=2&seg=0&d=310&size=4965817&sc=siteplayer"
	if entries[0].AudioURL != want {
		t.Errorf("audio URL = %q, want %q", entries[0].AudioURL, want)
	}
	// The apostrophe is escaped in the JSON rather than in the HTML, which is
	// what lets the attribute be delimited by single quotes at all.
	if entries[4].Title != "It's another episode" {
		t.Errorf("title = %q, want the apostrophe decoded", entries[4].Title)
	}
}

// TestNPRSizeIsTrustedOnlyFromNPRsOwnStore is the arithmetic trap, and the
// one place where believing the page costs something. An exact Size is what
// makes the downloader skip a file it already has without connecting, so a
// figure that is merely close is worse than none at all.
func TestNPRSizeIsTrustedOnlyFromNPRsOwnStore(t *testing.T) {
	tests := []struct {
		name string
		url  string
		want int64
	}{
		{
			name: "the on-demand store states the file's own length",
			url:  "https://ondemand.npr.org/anon.npr-mp3/npr/example/00000000_ex_one.mp3?t=progseg&d=310&size=4965817&sc=siteplayer",
			want: 4965817,
		},
		{
			// Measured: 36,461,592 advertised, 39,777,666 delivered. The
			// difference is the advertising stitched in per request.
			name: "a podcast's redirect chain states what it measured before the adverts",
			url:  "https://tracking.swap.fm/track/AAAAAAAAAAAAAAAAAAAA/prfx.byspotify.com/e/play.podtrac.com/npr-000000/npr.simplecastaudio.com/x/episodes/y/audio/128/default.mp3?t=podcast&d=2278&size=36461592&sc=siteplayer",
			want: -1,
		},
		{
			name: "the store with no length to state",
			url:  "https://ondemand.npr.org/anon.npr-mp3/npr/example/00000000_ex_two.mp3?t=progseg&d=250&sc=siteplayer",
			want: -1,
		},
		{
			name: "a delivery host that states none either",
			url:  "https://npr.simplecastaudio.com/x/episodes/y/audio/128/default.mp3?sc=siteplayer",
			want: -1,
		},
		{
			name: "a length that is not a number",
			url:  "https://ondemand.npr.org/anon.npr-mp3/npr/example/00000000_ex_one.mp3?size=unknown",
			want: -1,
		},
		{
			name: "a length of zero, which is the host saying it does not know",
			url:  "https://ondemand.npr.org/anon.npr-mp3/npr/example/00000000_ex_one.mp3?size=0",
			want: -1,
		},
		{
			// The store's name must be matched as a host, not as text: a
			// redirector is free to carry it in its own path, and one does.
			name: "a redirector whose path names the store",
			url:  "https://tracker.example.test/e/ondemand.npr.org/anon.npr-mp3/npr/example/one.mp3?size=4965817",
			want: -1,
		},
		{name: "no URL at all", url: "", want: -1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := nprSize(tc.url); got != tc.want {
				t.Errorf("nprSize = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestNPRRefusalReadsTheBlockRatherThanItsAvailability pins the order of the
// checks. "available" is the field that looks like the answer and is not one.
func TestNPRRefusalReadsTheBlockRatherThanItsAvailability(t *testing.T) {
	tests := []struct {
		name  string
		audio nprAudio
		want  nprRefusal
	}{
		{
			name:  "an ordinary segment",
			audio: nprAudio{Available: true, AudioURL: "https://ondemand.npr.org/a.mp3"},
			want:  nprPlayable,
		},
		{
			name:  "an NPR+ bonus episode, available and with nothing behind it",
			audio: nprAudio{Available: true, PlusType: "PLUS_EXCLUSIVE_BONUS"},
			want:  nprSubscriberOnly,
		},
		{
			// NPR+ also classifies episodes that are perfectly fetchable — the
			// sponsor-free cut of an ordinary one — so the tier decides
			// nothing where there is audio to take.
			name:  "an NPR+ episode that does carry its audio",
			audio: nprAudio{Available: true, AudioURL: "https://npr.simplecastaudio.com/a.mp3", PlusType: "PLUS_SPONSOR_FREE"},
			want:  nprPlayable,
		},
		{
			name:  "withdrawn, whatever else the block says",
			audio: nprAudio{Available: false, AudioURL: "https://ondemand.npr.org/a.mp3"},
			want:  nprUnavailable,
		},
		{
			name:  "the continuous live stream, which has no end to download to",
			audio: nprAudio{Available: true, AudioURL: "https://ondemand.npr.org/live.mp3", IsStreamAudio: true},
			want:  nprLiveStream,
		},
		{
			name:  "listed with no audio and no reason given",
			audio: nprAudio{Available: true},
			want:  nprNoAudio,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.audio.refusal(); got != tc.want {
				t.Errorf("refusal = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestNPRListingPageResolves is the whole parse end to end: which blocks are
// taken, what each is called, how long each is said to be, and where it is
// filed.
func TestNPRListingPageResolves(t *testing.T) {
	res, err := nprResult(nprListingPage, nprTestURL(t, "https://www.npr.org/programs/a-programme/"))
	if err != nil {
		t.Fatal(err)
	}
	if res.Title != "A Programme" {
		t.Errorf("title = %q", res.Title)
	}

	want := []File{
		{Name: "A segment.mp3", Size: 4965817, Dir: "A Programme"},
		// A slash in a title is prose here, not a path separator: SafeName
		// would keep only what follows it and the file would land as "Lagos".
		{Name: "Nairobi - Lagos.mp3", Size: -1, Dir: "A Programme"},
		// The same title twice on one page, which is what the numbering is
		// for — two segments must not write to one file.
		{Name: "A segment (2).mp3", Size: 3377991, Dir: "A Programme"},
		{Name: "An episode.mp3", Size: -1, Dir: "A Podcast"},
		{Name: "It's another episode.mp3", Size: -1, Dir: "A Podcast"},
	}
	if len(res.Files) != len(want) {
		t.Fatalf("got %d files, want %d: %+v", len(res.Files), len(want), res.Files)
	}
	for i, f := range res.Files {
		if f.Name != want[i].Name || f.Size != want[i].Size || f.Dir != want[i].Dir {
			t.Errorf("file %d = {%q %d %q}, want {%q %d %q}",
				i, f.Name, f.Size, f.Dir, want[i].Name, want[i].Size, want[i].Dir)
		}
		if f.URL == "" || len(f.Segments) > 0 {
			t.Errorf("file %d is not a plain rangeable file: %+v", i, f)
		}
		if f.SizeApprox {
			t.Errorf("file %d carries an approximate size; NPR states exact ones or none", i)
		}
	}
}

// TestNPRFilesByProgrammeOnlyWhenThereAreSeveral covers the folder rule. A
// programme's own page is one strand throughout and a folder named after it
// would only repeat the job's own name.
func TestNPRFilesByProgrammeOnlyWhenThereAreSeveral(t *testing.T) {
	res, err := nprResult(nprSingleProgrammePage, nprTestURL(t, "https://www.npr.org/programs/a-programme/"))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Files) != 2 {
		t.Fatalf("got %d files, want 2", len(res.Files))
	}
	for _, f := range res.Files {
		if f.Dir != "" {
			t.Errorf("%q was filed under %q, but the page is one programme throughout", f.Name, f.Dir)
		}
	}
}

// TestNPRWithheldPageNamesTheSubscription is the refusal that matters here.
// A page of bonus episodes reports itself as available and offers nothing,
// and "no downloadable files found" would leave the caller guessing at a fact
// NPR stated in a field of its own.
func TestNPRWithheldPageNamesTheSubscription(t *testing.T) {
	_, err := nprResult(nprWithheldPage, nprTestURL(t, "https://www.npr.org/podcasts/000000/a-podcast"))
	if err == nil {
		t.Fatal("a page with nothing fetchable on it resolved")
	}
	for _, want := range []string{"NPR+", "2 behind an NPR+ subscription", "1 marked unavailable"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

// TestNPRRefusalsRead covers the counting, since it is the whole of what a
// failed page has to say for itself.
func TestNPRRefusalsRead(t *testing.T) {
	var r nprRefusals
	if got := r.String(); !strings.Contains(got, "names nothing to fetch") {
		t.Errorf("an empty tally reads %q", got)
	}

	r.add(nprSubscriberOnly)
	r.add(nprUnavailable)
	r.add(nprUnavailable)
	r.add(nprLiveStream)
	r.add(nprNoAudio)
	const want = "1 behind an NPR+ subscription, 2 marked unavailable, " +
		"1 listed as a live stream, which has no end to download to, 1 listed with no audio at all"
	if got := r.String(); got != want {
		t.Errorf("tally = %q, want %q", got, want)
	}
}

// TestNPRPageWithNoAudioSaysSo covers the ordinary miss. Much of npr.org is
// text or video, and a page of either carries no block at all.
func TestNPRPageWithNoAudioSaysSo(t *testing.T) {
	doc := `<html><head><title>A written story : NPR</title></head><body><p>Words.</p></body></html>`
	_, err := nprResult(doc, nprTestURL(t, "https://www.npr.org/sections/news/"))
	if err == nil {
		t.Fatal("a page with no audio resolved")
	}
	if !strings.Contains(err.Error(), "no audio") {
		t.Errorf("error = %q", err)
	}
}

// TestNPRSeenIgnoresAnEmptyKey guards the fold. A block with no uid has no
// identity, and treating the empty string as one would collapse every such
// block into the first.
func TestNPRSeenIgnoresAnEmptyKey(t *testing.T) {
	seen := make(map[string]bool)
	if nprSeen(seen, "") {
		t.Error("an empty key was taken for an identity")
	}
	// Asked again: the first ask must not have recorded it either.
	if nprSeen(seen, "") {
		t.Error("an empty key was remembered by the first ask")
	}
	if nprSeen(seen, "a") {
		t.Error("a fresh key read as already seen")
	}
	if !nprSeen(seen, "a") {
		t.Error("a repeated key was not recognised")
	}
}

// TestNPRPageTitle covers where a job's name comes from. The document title
// carries the site's own suffix and the social metadata does not, which is
// why the latter is preferred; the suffix is trimmed as one fixed string
// because NPR writes headlines with a colon in them.
func TestNPRPageTitle(t *testing.T) {
	tests := map[string]string{
		`<html><head><title>All Things Considered : NPR</title>
		 <meta property="og:title" content="All Things Considered"/></head><body></body></html>`: "All Things Considered",
		`<html><head><title>All Things Considered : NPR</title></head><body></body></html>`:    "All Things Considered",
		`<html><head><title>Elbridge who : a founder : NPR</title></head><body></body></html>`: "Elbridge who : a founder",
		`<html><head><title>A page that names no site</title></head><body></body></html>`:      "A page that names no site",
		`<html><head></head><body></body></html>`:                                              "",
	}
	for doc, want := range tests {
		root, err := parseHTML(doc)
		if err != nil {
			t.Fatal(err)
		}
		if got := nprPageTitle(root); got != want {
			t.Errorf("nprPageTitle = %q, want %q", got, want)
		}
	}
}

// TestNPRMatch pins the host list. It is exact rather than the usual
// subdomain match because every npr.org subdomain that matters is somebody
// else's job: the on-demand store serves a plain MP3 the fallback extractor
// downloads as it stands, and the feed host serves RSS that the feed
// extractor reads.
func TestNPRMatch(t *testing.T) {
	n := NewNPR(nil)
	for _, host := range []string{"npr.org", "www.npr.org", "WWW.NPR.ORG", "www.npr.org."} {
		if !n.Match(&url.URL{Scheme: "https", Host: host, Path: "/programs/a-programme/"}) {
			t.Errorf("%s was not matched", host)
		}
	}
	for _, host := range []string{
		"ondemand.npr.org", "feeds.npr.org", "media.npr.org",
		"npr.simplecastaudio.com", "npr.org.example.test",
	} {
		if n.Match(&url.URL{Scheme: "https", Host: host, Path: "/a/b.mp3"}) {
			t.Errorf("%s was matched", host)
		}
	}
}
