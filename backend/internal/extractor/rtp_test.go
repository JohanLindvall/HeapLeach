package extractor

import (
	"path"
	"strings"
	"testing"

	"github.com/JohanLindvall/HeapLeach/internal/util"
)

// The fixtures below are synthetic, but every trap in them is one the real
// pages carry. RTP builds an episode page by emitting the player's own
// example — commented out, and pointing at a sample clip filed under p1525 —
// then a decoy assignment naming the same sample, and only then the
// programme. All three go through the same obfuscation, so a reader that
// takes the first, or that takes the last without checking whose it is, gets
// a real and entirely fetchable video of the wrong thing.
//
// Every programme and episode id below is a placeholder that names nothing on
// the real site, and every media host is a reserved domain.

// rtpDecoyBlobs is what sits above the programme on every page: the sample
// clip, offered as HLS and DASH and then again through the DRM hosts. It is
// the whole of what an unavailable episode's page carries.
const rtpDecoyBlobs = `
        /*
        var player1 = new RTPPlayer({
          id: "player_prog",
          file: {hls : atob( decodeURIComponent(["aHR0cHM","6Ly9zdH","JlYW1pb","mctb25k","ZW1hbmQ","uZXhhbX","BsZS50Z","XN0L25h","czIuc2h","hcmUvaD","I2NC81M","TJ4Mzg0","L3AxNTI","1L3AxNT","I1XzFfM","jAyMTA0","MjA1OTk","2L21hc3","RlcnMub","TN1OA%3","D%3D"].join(""))), dash : atob( decodeURIComponent(["aHR0cHM","6Ly9zdH","JlYW1pb","mctb25k","ZW1hbmQ","uZXhhbX","BsZS50Z","XN0L25h","czIuc2h","hcmUvaD","I2NC81M","TJ4Mzg0","L3AxNTI","1L3AxNT","I1XzFfM","jAyMTA0","MjA1OTk","2L21hc3","RlcnMub","XBk"].join(""))) },
        });
        */
        var f = {hls : atob( decodeURIComponent(["aHR0cHM","6Ly9zdH","JlYW1pb","mctdm9k","LmV4YW1","wbGUudG","VzdC9kc","m0tZnBz","L25hczI","uc2hhcm","UvaDI2N","C81MTJ4","Mzg0L3A","xNTI1L3","AxNTI1X","zFfMjAy","MTA1MTA","2MTE4Lm","1wNC9tY","XN0ZXIu","bTN1OA%","3D%3D"].join(""))), dash : atob( decodeURIComponent(["aHR0cHM","6Ly9zdH","JlYW1pb","mctdm9k","LmV4YW1","wbGUudG","VzdC9kc","m0tZGFz","aC9uYXM","yLnNoYX","JlL2gyN","jQvNTEy","eDM4NC9","wMTUyNS","9wMTUyN","V8xXzIw","MjEwNTE","wNjExOC","5tcDQvb","WFuaWZl","c3QubXB","k"].join(""))) };
`

// rtpClearEpisode is a DRM-free episode: the decoys, then the programme's own
// stream, then the player. Note "drm : false" with a space before the colon
// and the doubled space in the document title — the site writes both, and
// both are exactly how it writes them.
const rtpClearEpisode = `<html><head><title>A Programme, Episódio 3 - de 1 jan 2026 -  RTP Play</title></head>
<body>
  <div id="player_prog"></div>
  <script>` + rtpDecoyBlobs + `
        var f = {hls : atob( decodeURIComponent(["aHR0cHM","6Ly9zdH","JlYW1pb","mctdm9k","LmV4YW1","wbGUudG","VzdC9ob","HMvbmFz","Mi5zaGF","yZSwvaD","I2NC81M","TJ4Mzg0","L3AxL3A","xXzFfMj","AyNjAxM","DEwMDAw","MDBlMDA","xdDAwMD","FfbG8ub","XA0LC9o","MjY0LzU","xMngzOD","QvcDEvc","DFfMV8y","MDI2MDE","wMTAwMD","AwMGUwM","DF0MDAw","MS5tcDQ","sLnVybH","NldC9tY","XN0ZXIu","bTN1OA%","3D%3D"].join(""))) };

        var player1 = new RTPPlayer({
          id: "player_prog",
          fileKey: atob( decodeURIComponent(["L25hczI","uc2hhcm","UvaDI2N","C81MTJ4","Mzg0L3A","xL3AxXz","FfMjAyN","jAxMDEw","MDAwMDB","lMDAxdD","AwMDEub","XA0"].join(""))),
          file: f,
          streamType:"ondemand",
          drm : false,
          mediaType: "video",
        });
  </script>
  <article><a href="/play/p1/e11/a-programme" class="episode-item">3</a></article>
</body></html>`

// rtpProtectedEpisode is the other half of the catalogue. Two things are
// reproduced deliberately: the flag is written without a space here, as the
// site writes it on these pages, and the last assignment names its playlist
// "fps" rather than "hls" — so nothing may key on the field name.
const rtpProtectedEpisode = `<html><head><title>Another Programme Episódio 17 - de 24 abr 2025 - RTP Play</title></head>
<body>
  <script>` + rtpDecoyBlobs + `
        var f = {fps : atob( decodeURIComponent(["aHR0cHM","6Ly9zdH","JlYW1pb","mctdm9k","LmV4YW1","wbGUudG","VzdC9kc","m0tZnBz","L25hczI","uc2hhcm","UsL2gyN","jQvNTEy","eDM4NC9","wMi9wMl","8xXzIwM","jYwMTAx","MDAwMDA","wZTAwMX","QwMDAyX","2xvLm1w","NCwvaDI","2NC81MT","J4Mzg0L","3AyL3Ay","XzFfMjA","yNjAxMD","EwMDAwM","DBlMDAx","dDAwMDI","ubXA0LC","51cmxzZ","XQvbWFz","dGVyLm0","zdTg%3D"].join(""))) };

        var player1 = new RTPPlayer({
          id: "player_prog",
          drm: true,
          mediaType: "video",
        });
  </script>
</body></html>`

// rtpUnavailableEpisode is the page for an episode that is gone, and it is
// the reason the programme id is checked at all: it is served with a 200,
// carries the decoys and nothing else, and even says "drm : false". Only the
// paragraph where the player would have gone tells the truth.
const rtpUnavailableEpisode = `<html><head><title> Este episódio não se encontra disponível - RTP Play</title></head>
<body>
  <p class="vod-no-result">Este epis&oacute;dio n&atilde;o se encontra dispon&iacute;vel</p>
  <script>` + rtpDecoyBlobs + `
        var player1 = new RTPPlayer({ id: "player_prog", drm : false, mediaType: "video" });
  </script>
</body></html>`

// rtpRadioEpisode is RTP's radio half, which shares the site and the URL
// shape and none of the machinery: the decoys are still there, but the file
// is named outright, and there is no drm flag anywhere on the page.
const rtpRadioEpisode = `<html><head><title>A Radio Programme Episódio 133 - de 10 jul 2026 - RTP Play</title></head>
<body>
  <script>` + rtpDecoyBlobs + `
        var f = "https://cdn-ondemand.example.test/nas2.share/wavrss/at2/2607/PGM2600204213301_595465.mp3";

        var player1 = new RTPPlayer({ id: "player_prog", mediaType: "audio" });
  </script>
</body></html>`

// rtpSeasonPage is a programme page, which is one season: its title names the
// season, the selector names the others, and the episode list it renders is
// only the first twelve of however many the season has. The links to other
// programmes are what the collector has to leave alone.
const rtpSeasonPage = `<html><head><title>A Programme, temporada 18 -  RTP Play</title></head>
<body>
  <div class="select"><select onchange='if (this.value) window.location.href=this.value'>
    <option value="/play/p1/a-programme"  selected>Temporada 18</option>
    <option value="/play/p4/a-programme" >Temporada 17</option>
  </select></div>
  <div id="listProgramsContent">
    <article><a href="/play/p1/e13/a-programme" class="episode-item">3 jan</a></article>
    <article><a href="/play/p1/e12/a-programme" class="episode-item">2 jan</a></article>
    <article><a href="/play/p1/e12/a-programme" class="episode-item"><img></a></article>
    <article><a href="/play/p1/e11/a-programme" class="episode-item">1 jan</a></article>
    <div class='last_id sr-only'>11</div>
  </div>
  <aside>
    <a href="/play/p4/e41/a-programme">an episode of last season</a>
    <a href="/play/p2/e21/another-programme">an episode of something else</a>
    <a href="/play/p1/e/a-programme">the placeholder link the site emits</a>
    <a href="/play/p1/a-programme">the season itself</a>
  </aside>
</body></html>`

// rtpListingFragment is what the infinite-scroll endpoint answers with: the
// same articles, with no document around them.
const rtpListingFragment = `    <article class="col-xs-12 episode-article">
      <a href="/play/p1/e10/a-programme" class="episode-item"><div class="episode-date">31 dez. 2025</div></a>
    </article><article class="col-xs-12 episode-article">
      <a href="/play/p1/e9/a-programme" class="episode-item"><div class="episode-date">30 dez. 2025</div></a>
    </article><div class='last_id sr-only'>9</div>`

// rtpMasterPlaylist is RTP's own shape, and the claim the whole extractor
// rests on: nginx-vod-module's urlset, with no EXT-X-MEDIA line anywhere in
// it. The "-v1-a1" in each variant name is video track one and audio track
// one in the same MPEG-TS segments. The I-FRAME entries are trick-play and
// must not be mistaken for renditions.
const rtpMasterPlaylist = `#EXTM3U
#EXT-X-STREAM-INF:PROGRAM-ID=1,BANDWIDTH=1014360,RESOLUTION=1024x576,FRAME-RATE=25.000,CODECS="avc1.64001f,mp4a.40.2",VIDEO-RANGE=SDR
index-f1-v1-a1.m3u8
#EXT-X-STREAM-INF:PROGRAM-ID=1,BANDWIDTH=1899176,RESOLUTION=1280x720,FRAME-RATE=25.000,CODECS="avc1.64001f,mp4a.40.2",VIDEO-RANGE=SDR
index-f2-v1-a1.m3u8

#EXT-X-I-FRAME-STREAM-INF:BANDWIDTH=341230,RESOLUTION=1024x576,CODECS="avc1.64001f",URI="iframes-f1-v1-a1.m3u8",VIDEO-RANGE=SDR
#EXT-X-I-FRAME-STREAM-INF:BANDWIDTH=590847,RESOLUTION=1280x720,CODECS="avc1.64001f",URI="iframes-f2-v1-a1.m3u8",VIDEO-RANGE=SDR
`

// rtpMediaPlaylist is what the chosen variant answers with. It is plain
// MPEG-TS and, on the DRM-free half, carries no EXT-X-KEY.
const rtpMediaPlaylist = `#EXTM3U
#EXT-X-TARGETDURATION:7
#EXT-X-ALLOW-CACHE:YES
#EXT-X-PLAYLIST-TYPE:VOD
#EXT-X-VERSION:3
#EXT-X-MEDIA-SEQUENCE:1
#EXTINF:6.400,
seg-1-f2-v1-a1.ts
#EXTINF:5.880,
seg-2-f2-v1-a1.ts
#EXT-X-ENDLIST
`

// rtpProtectedMediaPlaylist is what sits below a drm-fps master, and the
// reason the refusal cannot wait for it: the master itself answers 200 and
// looks ordinary, and the FairPlay key only appears here, one request later.
const rtpProtectedMediaPlaylist = `#EXTM3U
#EXT-X-TARGETDURATION:10
#EXT-X-ALLOW-CACHE:YES
#EXT-X-PLAYLIST-TYPE:VOD
#EXT-X-KEY:METHOD=SAMPLE-AES,URI="skd://lic.example.test/license-server-fairplay?keyId=0000",KEYFORMAT="com.apple.streamingkeydelivery",KEYFORMATVERSIONS="1"
#EXTINF:10.000,
seg-1-f2-v1-a1.ts
`

const (
	rtpClearMaster = "https://streaming-vod.example.test/hls/nas2.share,/h264/512x384/p1/" +
		"p1_1_20260101000000e001t0001_lo.mp4,/h264/512x384/p1/p1_1_20260101000000e001t0001.mp4," +
		".urlset/master.m3u8"
	rtpProtectedMaster = "https://streaming-vod.example.test/drm-fps/nas2.share,/h264/512x384/p2/" +
		"p2_1_20260101000000e001t0002_lo.mp4,/h264/512x384/p2/p2_1_20260101000000e001t0002.mp4," +
		".urlset/master.m3u8"
	rtpSampleMaster = "https://streaming-vod.example.test/drm-fps/nas2.share/h264/512x384/p1525/" +
		"p1525_1_202105106118.mp4/master.m3u8"
)

// TestRTPManifestTakesTheLastBlobThatBelongsToTheProgramme is the one that
// matters most. Both halves of the rule are load-bearing: take the first
// blob and you get the player's sample clip, and take the last one blindly
// and an unavailable episode hands you the same sample under a name that
// says otherwise.
func TestRTPManifestTakesTheLastBlobThatBelongsToTheProgramme(t *testing.T) {
	if got := rtpManifest(rtpClearEpisode, "p1"); got != rtpClearMaster {
		t.Errorf("chose %q, want the programme's own stream", got)
	}
	if got := rtpManifest(rtpProtectedEpisode, "p2"); got != rtpProtectedMaster {
		t.Errorf("chose %q, want the programme's own stream", got)
	}
	// The decoys decode to a perfectly fetchable playlist, so "nothing found"
	// here is a decision and not a parse failure.
	if got := rtpManifest(rtpUnavailableEpisode, "p1"); got != "" {
		t.Errorf("chose %q from a page carrying only the player's sample clip", got)
	}
	if got := rtpManifest(rtpRadioEpisode, "p3"); got != "" {
		t.Errorf("chose %q from a radio page, whose media is not a playlist at all", got)
	}
	// And this is what "the last playlist on the page" would have handed back
	// instead: the player's sample clip, entirely real and entirely fetchable,
	// under whatever name the episode had.
	if got := rtpManifest(rtpUnavailableEpisode, "p1525"); got != rtpSampleMaster {
		t.Errorf("the fixture no longer carries the sample it exists to guard against: %q", got)
	}
}

// TestRTPManifestIgnoresTheDASHSibling covers the other reason a blob is
// skipped: every stream is published twice, and only one of the two is a
// playlist this can join.
func TestRTPManifestIgnoresTheDASHSibling(t *testing.T) {
	if strings.Contains(rtpManifest(rtpClearEpisode, "p1"), ".mpd") {
		t.Error("chose a DASH manifest")
	}
	// The file key sits in a blob of its own and is a path, not a URL.
	if got := rtpManifest(rtpClearEpisode, "p1"); !strings.HasPrefix(got, "https://") {
		t.Errorf("chose %q, which is not a URL", got)
	}
}

// TestRTPProtectedReadsBothSignals pins the belt-and-braces refusal. The flag
// is what the site believes and the path is what the CDN acts on; either
// alone is enough to refuse.
func TestRTPProtectedReadsBothSignals(t *testing.T) {
	tests := []struct {
		name     string
		doc      string
		manifest string
		want     bool
	}{
		{"clear on both counts", rtpClearEpisode, rtpClearMaster, false},
		{"flagged and served from the DRM host", rtpProtectedEpisode, rtpProtectedMaster, true},
		{"flagged only", `var f = {}; drm: true,`, rtpClearMaster, true},
		{"served from the FairPlay host only", `drm : false,`, rtpProtectedMaster, true},
		{"served from the Widevine host only", `drm : false,`,
			"https://streaming-vod.example.test/drm-dash/nas2.share/x.mp4/manifest.mpd", true},
		// Radio pages carry no flag at all, and an absent flag is not a
		// "false" that could be read as an assertion either way.
		{"no flag anywhere", rtpRadioEpisode, "https://cdn.example.test/a.mp3", false},
		// "drm-fps" has to be the first segment, not merely present: a
		// programme slug could contain anything.
		{"the word appears deeper in the path", ``,
			"https://streaming-vod.example.test/hls/nas2.share/drm-fps/x.mp4/master.m3u8", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := rtpProtected(tc.doc, tc.manifest); got != tc.want {
				t.Errorf("rtpProtected = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestRTPProtectedTakesTheLastFlag guards the same ordering rule the manifest
// follows. A page that flagged the decoy and the programme differently must
// be read the way the browser reads it, where the last assignment wins.
func TestRTPProtectedTakesTheLastFlag(t *testing.T) {
	doc := `var f = {}; drm : false, ... var f = {}; drm: true,`
	if !rtpProtected(doc, rtpClearMaster) {
		t.Error("an earlier drm:false outvoted the later drm:true")
	}
}

// TestRTPMasterPlaylistIsSelfContained is the claim that lets this host use
// the segment path at all. RTP declares no audio group, so every variant
// carries its own audio and concatenation yields something that plays.
func TestRTPMasterPlaylistIsSelfContained(t *testing.T) {
	base, err := ParseURL(rtpClearMaster)
	if err != nil {
		t.Fatal(err)
	}

	variants := parseMasterPlaylist(rtpMasterPlaylist, base)
	if len(variants) != 2 {
		t.Fatalf("parsed %d variants, want 2 (the I-FRAME entries are not renditions)", len(variants))
	}
	for _, v := range variants {
		if !v.muxed() {
			t.Errorf("%s reads as video only, but RTP declares no audio group at all", v.Resolution)
		}
	}

	best, ok := bestVariant(variants)
	if !ok {
		t.Fatal("no variant was chosen")
	}
	if best.Resolution != "1280x720" {
		t.Errorf("chose %s, want the largest rendition", best.Resolution)
	}
}

// TestRTPSegmentExtensionIgnoresTheUrlsetPath is why playlistExtension is not
// used here. The manifest path spells out the source MP4s — twice — so the
// general helper calls an MPEG-TS stream ".mp4" and leaves a file nothing
// opens. The segments say what the container really is.
func TestRTPSegmentExtensionIgnoresTheUrlsetPath(t *testing.T) {
	base, err := ParseURL(rtpClearMaster)
	if err != nil {
		t.Fatal(err)
	}
	best, ok := bestVariant(parseMasterPlaylist(rtpMasterPlaylist, base))
	if !ok {
		t.Fatal("no variant was chosen")
	}
	if got := playlistExtension(best); got != ".mp4" {
		t.Fatalf("playlistExtension = %q; this test exists because it says .mp4 here", got)
	}

	variantBase, err := ParseURL(best.URL)
	if err != nil {
		t.Fatal(err)
	}
	segments := parseMediaPlaylist(rtpMediaPlaylist, variantBase)
	if len(segments) != 2 {
		t.Fatalf("parsed %d segments, want 2", len(segments))
	}
	if got := segmentsExtension(segments, best); got != ".ts" {
		t.Errorf("extension %q, want .ts", got)
	}
}

// TestRTPSegmentExtensionFallsBack covers the shapes RTP does not serve
// today, where the general helper is still the best answer available.
func TestRTPSegmentExtensionFallsBack(t *testing.T) {
	fragmented := hlsVariant{URL: "https://cdn.example.test/cmaf/index.m3u8"}
	if got := segmentsExtension([]string{"https://cdn.example.test/cmaf/seg-1.m4s"}, fragmented); got != ".mp4" {
		t.Errorf("fMP4 segments gave %q, want .mp4", got)
	}
	if got := segmentsExtension(nil, fragmented); got != ".mp4" {
		t.Errorf("with no segments to read, got %q, want playlistExtension's answer", got)
	}
	unknown := hlsVariant{URL: "https://cdn.example.test/a/index.m3u8"}
	if got := segmentsExtension([]string{"https://cdn.example.test/a/seg-1"}, unknown); got != ".ts" {
		t.Errorf("nameless segments gave %q, want playlistExtension's answer", got)
	}
}

// TestRTPProtectedMediaPlaylistCarriesTheKey documents why the refusal has to
// happen from the page. Nothing about a protected master is wrong; the
// FairPlay key is one request further down, and by then a job has been
// created and the caller told it was fine.
func TestRTPProtectedMediaPlaylistCarriesTheKey(t *testing.T) {
	if !strings.Contains(rtpProtectedMediaPlaylist, "#EXT-X-KEY") {
		t.Fatal("the fixture lost the trap it exists for")
	}
	base, err := ParseURL(rtpProtectedMaster)
	if err != nil {
		t.Fatal(err)
	}
	// It parses perfectly well, which is precisely the problem.
	if got := len(parseMediaPlaylist(rtpProtectedMediaPlaylist, base)); got != 1 {
		t.Errorf("parsed %d segments; a DRM playlist looks entirely ordinary", got)
	}
	if !rtpProtected(rtpProtectedEpisode, rtpProtectedMaster) {
		t.Error("the page was not refused, so this playlist would have been fetched")
	}
}

// TestRTPPlainMediaIsPreferredForRadio covers the other half of the site.
// Those pages carry the same decoys, so the plain assignment has to win — and
// the manifest rule has to decline the decoys rather than treating the
// programme as a video.
func TestRTPPlainMediaIsPreferredForRadio(t *testing.T) {
	want := "https://cdn-ondemand.example.test/nas2.share/wavrss/at2/2607/PGM2600204213301_595465.mp3"
	if got := rtpPlainMediaURL(rtpRadioEpisode); got != want {
		t.Errorf("found %q, want %q", got, want)
	}
	if got := strings.ToLower(path.Ext(util.NameFromURL(want))); got != ".mp3" {
		t.Errorf("extension %q, want .mp3", got)
	}
	// A video page must not be read this way: its `var f` is an object.
	if got := rtpPlainMediaURL(rtpClearEpisode); got != "" {
		t.Errorf("a video page yielded the plain file %q", got)
	}
	// Nor may a manifest assigned this way become a URL to fetch: it would
	// download in full, as text, and record as a finished file.
	if got := rtpPlainMediaURL(`var f = "https://cdn.example.test/hls/master.m3u8";`); got != "" {
		t.Errorf("a playlist was taken as a plain file: %q", got)
	}
	if got := rtpPlainMediaURL(`var f = "https://cdn.example.test/dash/manifest.mpd";`); got != "" {
		t.Errorf("a manifest was taken as a plain file: %q", got)
	}
}

// TestRTPDecodeKeepsAPlusSign pins the choice of unescaping. Base64's
// alphabet includes "+", and a query unescape turns every one of them into a
// space — which decodeURIComponent, the thing being reversed, does not do.
func TestRTPDecodeKeepsAPlusSign(t *testing.T) {
	const array = `"aHR0cHM","6Ly9jZG","4tb25kZ","W1hbmQu","ZXhhbXB","sZS50ZX","N0L25hc","zIuc2hh",` +
		`"cmUvd2F","2cnNzL2","F0Mi8yN","jAwNy9Q","R01+MjY","wMDIwND","IxMzMwM","V81OTU0","NjUubXA","z"`
	const want = "https://cdn-ondemand.example.test/nas2.share/wavrss/at2/26007/PGM~2600204213301_595465.mp3"
	if got := rtpDecode(array); got != want {
		t.Errorf("rtpDecode = %q, want %q", got, want)
	}
}

// TestRTPDecodeRejectsRubbish covers a page whose obfuscation this does not
// understand. Nothing is better than something here: an unreadable blob is
// one candidate fewer, and the caller reports the page as offering no stream.
func TestRTPDecodeRejectsRubbish(t *testing.T) {
	for _, array := range []string{``, `""`, `"%zz"`, `"!!!!"`} {
		if got := rtpDecode(array); got != "" {
			t.Errorf("rtpDecode(%s) = %q, want nothing", array, got)
		}
	}
}

// TestRTPUnavailableIsPassedThroughVerbatim is why the paragraph is read at
// all: the page has no other way of saying no, and RTP's own sentence is
// more use than anything concluded from a missing stream. The entities in it
// are the parser's job, not a caller's.
func TestRTPUnavailableIsPassedThroughVerbatim(t *testing.T) {
	root, err := parseHTML(rtpUnavailableEpisode)
	if err != nil {
		t.Fatal(err)
	}
	if got := rtpUnavailable(root); got != "Este episódio não se encontra disponível" {
		t.Errorf("rtpUnavailable = %q, want RTP's own words", got)
	}

	// An ordinary episode page carries no such paragraph, so nothing here can
	// refuse a perfectly good download.
	for name, doc := range map[string]string{"clear": rtpClearEpisode, "radio": rtpRadioEpisode} {
		root, err := parseHTML(doc)
		if err != nil {
			t.Fatal(err)
		}
		if got := rtpUnavailable(root); got != "" {
			t.Errorf("%s page reported %q", name, got)
		}
	}
}

// TestRTPTitleTrimsTheSuffixAsOneString covers the naming rule. Cutting at
// the first " - ", the way most of these hosts are handled, beheads the
// ordinary RTP episode title, which carries one of its own.
func TestRTPTitleTrimsTheSuffixAsOneString(t *testing.T) {
	tests := []struct{ name, doc, want string }{
		{"an episode", rtpClearEpisode, "A Programme, Episódio 3 - de 1 jan 2026"},
		{"a protected episode", rtpProtectedEpisode, "Another Programme Episódio 17 - de 24 abr 2025"},
		{"a season", rtpSeasonPage, "A Programme, temporada 18"},
		// The site writes one space before its own name on some pages and two
		// on others; the parser collapses the run before the suffix is cut.
		{"a doubled space", `<html><head><title>A Programme -  RTP Play</title></head></html>`, "A Programme"},
		{"no suffix at all", `<html><head><title>A Programme</title></head></html>`, "A Programme"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root, err := parseHTML(tc.doc)
			if err != nil {
				t.Fatal(err)
			}
			if got := rtpTitle(root); got != tc.want {
				t.Errorf("rtpTitle = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestRTPEpisodeLinksKeepTheSeasonsOwn covers the listing collector. The page
// links into other seasons and other programmes, and an episode id is only
// unique beneath its own programme.
func TestRTPEpisodeLinksKeepTheSeasonsOwn(t *testing.T) {
	ref := rtpRef{program: "p1", page: "https://www.rtp.pt/play/p1/a-programme"}
	seen := make(map[string]bool)

	got := rtpEpisodeLinks(rtpSeasonPage, ref, seen)
	want := []rtpEpisodeRef{
		{id: "e13", page: "https://www.rtp.pt/play/p1/e13/a-programme"},
		{id: "e12", page: "https://www.rtp.pt/play/p1/e12/a-programme"},
		{id: "e11", page: "https://www.rtp.pt/play/p1/e11/a-programme"},
	}
	if len(got) != len(want) {
		t.Fatalf("collected %+v, want %+v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("episode %d = %+v, want %+v", i, got[i], want[i])
		}
	}

	// The endpoint answers with a bare fragment; the same collector reads it,
	// and the seen set is what stops the walk repeating itself.
	more := rtpEpisodeLinks(rtpListingFragment, ref, seen)
	if len(more) != 2 || more[0].id != "e10" || more[1].id != "e9" {
		t.Errorf("listing fragment gave %+v", more)
	}
	if again := rtpEpisodeLinks(rtpListingFragment, ref, seen); len(again) != 0 {
		t.Errorf("the same page contributed %d episodes twice", len(again))
	}
}

// TestRTPTarget covers the two URL shapes, and the several that look like
// them. There is no third: RTP gives every season a programme id of its own,
// so a programme URL is always one season.
func TestRTPTarget(t *testing.T) {
	tests := map[string]rtpRef{
		"https://www.rtp.pt/play/p1/a-programme":      {program: "p1"},
		"https://www.rtp.pt/play/p1/e11/a-programme":  {program: "p1", episode: "e11"},
		"https://www.rtp.pt/play/p1/e11/a-programme/": {program: "p1", episode: "e11"},
		"https://rtp.pt/play/p1/a-programme":          {program: "p1"},
		"https://www.rtp.pt/play/p1":                  {program: "p1"},
		// The site emits this one itself, for an episode it has no id for.
		"https://www.rtp.pt/play/p1/e/a-programme": {program: "p1"},
		"https://www.rtp.pt/play/":                 {},
		"https://www.rtp.pt/play/perfil":           {},
		"https://www.rtp.pt/noticias/p1/a-story":   {},
	}
	for raw, want := range tests {
		u, err := ParseURL(raw)
		if err != nil {
			t.Fatal(err)
		}
		want.page = u.String()
		if want.program == "" {
			want.page = ""
		}
		if got := rtpTarget(u); got != want {
			t.Errorf("rtpTarget(%s) = %+v, want %+v", raw, got, want)
		}
	}
}

// TestRTPMatch pins the host rule. RTP's news site and its radio station
// pages share the domain and nothing else, so only /play is claimed.
func TestRTPMatch(t *testing.T) {
	r := NewRTPPlay(nil)
	for _, raw := range []string{
		"https://www.rtp.pt/play/p1/a-programme",
		"https://rtp.pt/play/p1/e11/a-programme",
		"https://WWW.RTP.PT/play/p1/a-programme",
		"https://www.rtp.pt/play/",
	} {
		u, err := ParseURL(raw)
		if err != nil {
			t.Fatal(err)
		}
		if !r.Match(u) {
			t.Errorf("%s was not matched", raw)
		}
	}
	for _, raw := range []string{
		"https://www.rtp.pt/noticias/pais/a-story_v1",
		"https://www.rtp.pt/antena1/programas/a-show",
		"https://www.rtp.pt/",
		"https://notrtp.pt.example.test/play/p1/a-programme",
	} {
		u, err := ParseURL(raw)
		if err != nil {
			t.Fatal(err)
		}
		if r.Match(u) {
			t.Errorf("%s was matched", raw)
		}
	}
}

// TestRTPRefusedCarriesTheReason covers what the season expansion leans on:
// a whole season refused for one reason should read as that one reason.
func TestRTPRefusedCarriesTheReason(t *testing.T) {
	err := &rtpRefused{page: "https://www.rtp.pt/play/p2/e21/x", reason: "DRM protected"}
	if got := err.Error(); got != "rtpplay: https://www.rtp.pt/play/p2/e21/x: DRM protected" {
		t.Errorf("Error() = %q", got)
	}
}

// TestRTPProgramIDShapes guards the two id patterns against the slug, which
// sits in the same position and can begin with either letter.
func TestRTPProgramIDShapes(t *testing.T) {
	for _, id := range []string{"p1", "p12345"} {
		if !rtpProgramID.MatchString(id) {
			t.Errorf("%q was not read as a programme id", id)
		}
	}
	for _, id := range []string{"p", "praia-e-mar", "e11", "12345", "p12345a"} {
		if rtpProgramID.MatchString(id) {
			t.Errorf("%q was read as a programme id", id)
		}
	}
	for _, id := range []string{"e1", "e67890"} {
		if !rtpEpisodeID.MatchString(id) {
			t.Errorf("%q was not read as an episode id", id)
		}
	}
	for _, id := range []string{"e", "estreias", "p1", "67890"} {
		if rtpEpisodeID.MatchString(id) {
			t.Errorf("%q was read as an episode id", id)
		}
	}
}
