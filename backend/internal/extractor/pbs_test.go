package extractor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

// pbsMasterPlaylist is PBS's own manifest, reduced to two variants and
// written on a reserved domain. It is the fixture the whole extractor turns
// on, and everything in it that matters is a trap.
//
// Every variant names AUDIO="multiple_audio_tracks", and that group's
// EXT-X-MEDIA entry has a URI of its own — so the variants carry video and
// nothing else. CODECS says "avc1.640028,mp4a.40.2" on both anyway, exactly
// the way vimeo's does, which is why believing it is not an option.
const pbsMasterPlaylist = `#EXTM3U
#EXT-X-INDEPENDENT-SEGMENTS
#EXT-X-VERSION:4
#EXT-X-MEDIA:URI="a-programme-AABR-AVC_eng_0_VBR_48_265342.m3u8",TYPE=AUDIO,GROUP-ID="multiple_audio_tracks",LANGUAGE="eng",NAME="English",DEFAULT=YES,AUTOSELECT=YES,CHANNELS="2"
#EXT-X-MEDIA:URI="a-programme_en-captions_834.m3u8",TYPE=SUBTITLES,GROUP-ID="subs",LANGUAGE="en",NAME="English",DEFAULT=NO,AUTOSELECT=YES,FORCED=NO
#EXT-X-STREAM-INF:BANDWIDTH=9735348,AVERAGE-BANDWIDTH=2322886,RESOLUTION=1920x1080,FRAME-RATE=23.976,CODECS="avc1.640028,mp4a.40.2",VIDEO-RANGE=SDR,AUDIO="multiple_audio_tracks",SUBTITLES="subs"
a-programme-AABR-AVC-1080p_13000k_1.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=3204763,AVERAGE-BANDWIDTH=917657,RESOLUTION=1280x720,FRAME-RATE=23.976,CODECS="avc1.64001f,mp4a.40.2",VIDEO-RANGE=SDR,AUDIO="multiple_audio_tracks",SUBTITLES="subs"
a-programme-AABR-AVC-720p_2794k_2.m3u8
`

// pbsPlayerPage is the shape player.pbs.org answers with: the whole player
// state assigned to a script variable rather than served as a document.
//
// The title carries a "};" on purpose. That is the sequence a regex would cut
// the value at, and a real synopsis or title is free to contain one, so the
// fixture makes the difference between reading this with a pattern and
// reading it with a JSON decoder visible.
const pbsPlayerPage = `<!doctype html>
<html lang="en-US">
<head><title>A Programme | Watch Online | PBS</title></head>
<body>
<div id="partnerPlayer"></div>
<script>
  window.videoBridge = {"availability": "available", "encodings": ["https://urs.example.test/redirect/1111/", "https://urs.example.test/redirect/2222/"], "has_hls_encodings": true, "has_mp4_encodings": true, "id": "3100000001", "slug": "a-video-slug", "title": "Punctuation }; and Other Hazards", "series_info": "Season 41 Episode 31", "program": {"title": "A Programme", "slug": "a-programme", "producer": "PBS"}, "duration": 3211, "is_mvod": false, "is_playable": true, "has_drm": false};
  window.somethingElse = {"unrelated": true};
</script>
</body>
</html>`

// pbsErrorPage is what an expired or unknown title answers with: HTTP 200,
// no player state at all, and two sentences of apology. Reproduced with the
// whitespace PBS actually ships, since the message has to survive being
// collapsed out of it.
const pbsErrorPage = `<!doctype html>
<html class="video-error" lang="en-US">
<head><title>Video: | Watch Online | PBS</title></head>
<body>
<div class="page-wrap">
	<div class="l-error-wrap">
		<p class="error-message">


				We're sorry, but this video is not available.


		</p>
	</div>
</div>
</body>
</html>`

// pbsShowPage is a programme page reduced to its links, carrying the trap
// that decided how they are read: an "also available from" link whose path
// is /gp/video/detail/<id> on somebody else's host. A pattern for
// /video/<slug>/ run over the document takes that as an episode.
const pbsShowPage = `<!doctype html>
<html lang="en-US">
<head><title>A Programme | PBS</title></head>
<body>
<ul>
  <li><a href="/video/first-episode-aaa111/">First Episode</a></li>
  <li><a href="https://www.pbs.org/video/second-episode-bbb222/">Second Episode</a></li>
  <li><a href="/video/first-episode-aaa111/">First Episode (again, from the carousel)</a></li>
  <li><a href="https://www.amazon.example.test/gp/video/detail/B0FXZ9VHQZ?sr=1-1">Buy on Amazon</a></li>
  <li><a href="/show/a-programme/">The show itself</a></li>
  <li><a href="/video/">Every video</a></li>
  <li><a href="/video/third-episode-ccc333/">Third Episode</a></li>
</ul>
</body>
</html>`

// TestPBSMasterPlaylistIsDemuxed pins the fact this extractor exists to
// route around. Should PBS's manifest ever be reached, every variant of it
// reads as video only, and a variant joined without its audio group is a
// full-length silent file that the downloader records as complete.
func TestPBSMasterPlaylistIsDemuxed(t *testing.T) {
	base, err := ParseURL("https://cdn.example.test/videos/a-programme/master.m3u8")
	if err != nil {
		t.Fatal(err)
	}

	variants := parseMasterPlaylist(pbsMasterPlaylist, base)
	if len(variants) != 2 {
		t.Fatalf("parsed %d variants, want 2", len(variants))
	}
	for _, v := range variants {
		if v.muxed() {
			t.Errorf("%s reads as self-contained, but its audio group has a playlist of its own; "+
				"joining it would write a silent video", v.Resolution)
		}
		if !strings.Contains(strings.ToLower(v.Codecs), "mp4a") {
			t.Errorf("%s no longer advertises audio it does not carry, so the fixture has "+
				"stopped reproducing the trap", v.Resolution)
		}
	}
}

// TestPBSBridgeSurvivesPunctuationInATitle is the regression guard for how
// the player state is read. Cutting the assignment at the first "};" is the
// obvious thing to do and is wrong: a title is free to contain one.
func TestPBSBridgeSurvivesPunctuationInATitle(t *testing.T) {
	bridge, err := pbsBridgeOf(pbsPlayerPage)
	if err != nil {
		t.Fatalf("pbsBridgeOf: %v", err)
	}
	if want := "Punctuation }; and Other Hazards"; bridge.Title != want {
		t.Errorf("title %q, want %q", bridge.Title, want)
	}
	if len(bridge.Encodings) != 2 {
		t.Fatalf("read %d encodings, want 2 — the value was cut short", len(bridge.Encodings))
	}
	if bridge.Program.Title != "A Programme" {
		t.Errorf("programme %q, want %q", bridge.Program.Title, "A Programme")
	}
	if err := bridge.playable(); err != nil {
		t.Errorf("a title PBS calls available was refused: %v", err)
	}
}

// TestPBSPlayableRefusesWhatPBSRefuses covers the three published fields the
// extractor gates on. Each is a statement rather than an inference, which is
// what lets a restriction be reported before anything is fetched instead of
// arriving as a bare 403.
func TestPBSPlayableRefusesWhatPBSRefuses(t *testing.T) {
	tests := []struct {
		name   string
		bridge pbsBridge
		want   string
	}{
		{
			name:   "available",
			bridge: pbsBridge{Availability: "available", IsPlayable: true},
		},
		{
			name:   "protected",
			bridge: pbsBridge{Availability: "available", IsPlayable: true, HasDRM: true},
			want:   "DRM",
		},
		{
			name:   "not playable",
			bridge: pbsBridge{Availability: "available"},
			want:   "not playable",
		},
		{
			name:   "withdrawn",
			bridge: pbsBridge{Availability: "unavailable", IsPlayable: true},
			want:   `"unavailable"`,
		},
		{
			name:   "passport",
			bridge: pbsBridge{Availability: "all_members", IsPlayable: true, IsMVOD: true},
			want:   "Passport",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.bridge.playable()
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("refused a playable title: %v", err)
			case tc.want == "":
				return
			case err == nil:
				t.Fatal("accepted a title PBS will not serve")
			case !strings.Contains(err.Error(), tc.want):
				t.Errorf("refusal %q does not mention %q", err, tc.want)
			}
		})
	}
}

// TestPBSErrorPageIsReportedInPBSsOwnWords covers the state a good share of
// any back catalogue is in. The page is an ordinary 200 with no player state,
// and its sentence is the only thing separating "this expired" from "no such
// video".
func TestPBSErrorPageIsReportedInPBSsOwnWords(t *testing.T) {
	_, err := pbsBridgeOf(pbsErrorPage)
	if err == nil {
		t.Fatal("an error page was read as a playable video")
	}
	if want := "We're sorry, but this video is not available."; !strings.Contains(err.Error(), want) {
		t.Errorf("reported %q, want PBS's own sentence %q", err, want)
	}
}

// TestPBSVideoSlugsStayOnPBS guards how a show page is read. The links are
// anchors resolved against the page rather than a pattern over the document,
// because the page carries a shop link whose own path is /gp/video/detail/.
func TestPBSVideoSlugsStayOnPBS(t *testing.T) {
	base, err := ParseURL("https://www.pbs.org/show/a-programme/")
	if err != nil {
		t.Fatal(err)
	}

	got := pbsVideoSlugs(pbsShowPage, base)
	want := []string{"first-episode-aaa111", "second-episode-bbb222", "third-episode-ccc333"}
	if len(got) != len(want) {
		t.Fatalf("collected %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("collected %v, want %v (the page's own order, deduped)", got, want)
		}
	}
}

// TestPBSTargetOf pins which shapes on the site are claimed. The absent ones
// matter as much as the present: the redirector and the CDN under pbs.org
// serve plain rangeable files, which is exactly what the direct extractor
// downloads, so claiming them here could only refuse them.
func TestPBSTargetOf(t *testing.T) {
	tests := map[string]pbsTarget{
		"https://www.pbs.org/video/a-title-slug/":                 {slug: "a-title-slug"},
		"https://pbs.org/video/a-title-slug/":                     {slug: "a-title-slug"},
		"https://www.pbs.org/video/a-title-slug/?foo=bar":         {slug: "a-title-slug"},
		"https://www.pbs.org/show/a-programme/":                   {show: "a-programme"},
		"https://player.pbs.org/portalplayer/3100000001/":         {id: "3100000001"},
		"https://player.pbs.org/viralplayer/3100000001/":          {id: "3100000001"},
		"https://player.pbs.org/widget/partnerplayer/3100000001/": {id: "3100000001"},

		// Claimed by nobody here.
		"https://www.pbs.org/":                        {},
		"https://www.pbs.org/video/":                  {},
		"https://www.pbs.org/newshour/":               {},
		"https://urs.pbs.org/redirect/abc123/":        {},
		"https://ga.pbs-video.pbs.org/p/-/sid/a.mp4":  {},
		"https://player.pbs.org/portalplayer/":        {},
		"https://not-pbs.example.test/video/a-slug/":  {},
		"https://www.pbs.org.example.test/video/a-b/": {},
	}
	for raw, want := range tests {
		u, err := ParseURL(raw)
		if err != nil {
			t.Fatal(err)
		}
		got := pbsTargetOf(u)
		if want == (pbsTarget{}) {
			if got != nil {
				t.Errorf("%s was claimed as %+v, want left to the fallback", raw, *got)
			}
			continue
		}
		if got == nil {
			t.Errorf("%s was not claimed, want %+v", raw, want)
			continue
		}
		if *got != want {
			t.Errorf("%s read as %+v, want %+v", raw, *got, want)
		}
	}
}

// TestPBSSeasonOf covers the two forms series_info takes. It is written for
// a viewer, so it is parsed rather than trusted: a special belongs to no
// season and must not be filed under one.
func TestPBSSeasonOf(t *testing.T) {
	tests := map[string]string{
		"Season 41 Episode 31": "Season 41",
		"season 3 episode 1":   "Season 3",
		"Season 7":             "Season 7",
		"Special":              "",
		"":                     "",
		"Episode 12":           "",
	}
	for info, want := range tests {
		if got := pbsSeasonOf(info); got != want {
			t.Errorf("pbsSeasonOf(%q) = %q, want %q", info, got, want)
		}
	}
}

// TestPBSJoin covers naming a lone file, which carries the programme because
// nothing else around it will say what it belongs to.
func TestPBSJoin(t *testing.T) {
	tests := []struct{ program, title, want string }{
		{"A Programme", "An Episode Of It", "A Programme - An Episode Of It"},
		{"A Programme", "A Programme", "A Programme"},
		{"a programme", "A Programme", "A Programme"},
		{"", "A Lone Title", "A Lone Title"},
		{"A Programme", "", "A Programme"},
		{"", "", ""},
	}
	for _, tc := range tests {
		if got := pbsJoin(tc.program, tc.title); got != tc.want {
			t.Errorf("pbsJoin(%q, %q) = %q, want %q", tc.program, tc.title, got, tc.want)
		}
	}
}

// pbsMP4Size is the length the fixture CDN reports, which the extractor has
// to carry through exactly: an approximate size would make the downloader's
// pre-connect skip compare against a length the file can never reach.
const pbsMP4Size = 806238694

// pbsEncodings stands in for the pair PBS publishes. Neither is labelled and
// their order is not something to rely on, so the server answers both and the
// test runs them in each order.
func pbsEncodings(t *testing.T) (root, hls, mp4 string) {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/redirect/hls/", func(w http.ResponseWriter, r *http.Request) {
		// A fresh session id per request, which is exactly why the extractor
		// keeps the redirect rather than what it resolved to.
		http.Redirect(w, r, "/p/-/sid/"+r.URL.Query().Get("n")+"/a-programme.m3u8", http.StatusFound)
	})
	mux.HandleFunc("/redirect/mp4/", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/p/-/sid/"+r.URL.Query().Get("n")+"/a-programme-mp4-720p-3000k.mp4", http.StatusFound)
	})
	mux.HandleFunc("/p/-/", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".mp4") {
			w.Header().Set(httpx.HeaderContentType, "video/mp4")
			w.Header().Set("Content-Length", strconv.Itoa(pbsMP4Size))
			return
		}
		w.Header().Set(httpx.HeaderContentType, "application/x-mpegURL")
		w.Header().Set("Content-Length", "2262")
	})

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server.URL, server.URL + "/redirect/hls/", server.URL + "/redirect/mp4/"
}

func newTestPBS() *PBS {
	return &PBS{client: httpx.New("test-agent", "en-US", 0, 5*time.Second)}
}

// TestPBSProgressiveProbesRatherThanIndexes covers the encoding walk. Which
// of the two links is the file is not stated anywhere and is not positional,
// so each is asked where it leads; what comes back must be the stable
// redirect and the exact length behind it.
func TestPBSProgressiveProbesRatherThanIndexes(t *testing.T) {
	_, hls, mp4 := pbsEncodings(t)

	for _, order := range [][]string{{hls, mp4}, {mp4, hls}} {
		link, size, err := newTestPBS().progressive(context.Background(), order)
		if err != nil {
			t.Fatalf("progressive(%v): %v", order, err)
		}
		if link != mp4 {
			t.Errorf("took %s, want the encoding that leads to the file", link)
		}
		if !strings.Contains(link, "/redirect/") {
			t.Errorf("took the resolved location %s rather than the stable redirect; "+
				"a signed link would be stale by the time the item's turn came", link)
		}
		if size != pbsMP4Size {
			t.Errorf("size %d, want the exact %d the CDN reports", size, pbsMP4Size)
		}
	}
}

// TestPBSProgressiveRefusesAStreamOnlyTitle is the single regression this
// extractor most needs guarded. With no progressive file on offer the only
// thing left is the demuxed manifest, and falling back to it would write a
// silent video at full length that finishes cleanly.
func TestPBSProgressiveRefusesAStreamOnlyTitle(t *testing.T) {
	_, hls, _ := pbsEncodings(t)

	link, _, err := newTestPBS().progressive(context.Background(), []string{hls})
	if err == nil {
		t.Fatalf("accepted %s, which resolves to a playlist whose variants carry no audio", link)
	}
	if !strings.Contains(err.Error(), "silent") {
		t.Errorf("refusal %q does not say what taking the stream would produce", err)
	}
}

// TestPBSProgressiveSurvivesADeadEncoding covers the ordinary case of one of
// the two links having gone. The remaining one still has to be found, so a
// failed probe moves on rather than ending the walk.
func TestPBSProgressiveSurvivesADeadEncoding(t *testing.T) {
	root, _, mp4 := pbsEncodings(t)

	dead := root + "/redirect/gone/"
	link, size, err := newTestPBS().progressive(context.Background(), []string{dead, mp4})
	if err != nil {
		t.Fatalf("a dead first encoding ended the walk: %v", err)
	}
	if link != mp4 || size != pbsMP4Size {
		t.Errorf("took %s at %d bytes, want %s at %d", link, size, mp4, pbsMP4Size)
	}
}
