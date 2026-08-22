package extractor

import (
	"net/url"
	"strings"
	"testing"
)

// The scrambled/unscrambled pairs below are the ones pinned in kvs_test.go,
// on a reserved domain. What is under test here is which rendition gets
// picked and how it is named, neither of which cares whose host it is.
const (
	kvsLicense = "$112233445566778"

	kvsLowScrambled = "function/0/https://example.test/get_file/5/" +
		"fedcba9876543210fedcba9876543210dd33ee44ff/10000000/10000001/clip_240p.mp4/"
	kvsLowDirect = "https://example.test/get_file/5/" +
		"089d1c2987defb604ecbfa3a65123475dd33ee44ff/10000000/10000001/clip_240p.mp4/"

	kvsHighScrambled = "function/0/https://example.test/get_file/5/" +
		"0123456789abcdef0123456789abcdefaa11bb22cc/10000000/10000001/clip.mp4/"
	kvsHighDirect = "https://example.test/get_file/5/" +
		"f762e3d67821049fb13405c59aedcb8aaa11bb22cc/10000000/10000001/clip.mp4/"
)

// kvsPage builds a player page in the shape every install serves.
func kvsPage() string {
	return `<html><head><title>A Synthetic Clip - Example Tube</title></head><body>
<script type="text/javascript">
	var flashvars = {
		video_id: '12345',
		video_title: 'A Synthetic Clip',
		license_code: '` + kvsLicense + `',
		postfix: '.mp4',
		video_url: '` + kvsLowScrambled + `',
		video_url_text: '240p',
		video_alt_url: '` + kvsHighScrambled + `',
		video_alt_url_text: '1080p'
	}
</script>
</body></html>`
}

// TestKVSResultPicksTheLargestRendition matters because the order the page
// lists renditions in is not the order of their quality on every install:
// video_url is usually the best and sometimes is not, so the labels decide.
func TestKVSResultPicksTheLargestRendition(t *testing.T) {
	u, err := ParseURL("https://example.test/videos/12345/a-synthetic-clip/")
	if err != nil {
		t.Fatal(err)
	}
	res, err := kvsResult(kvsPage(), u, "example")
	if err != nil {
		t.Fatalf("kvsResult: %v", err)
	}

	if res.Title != "A Synthetic Clip" {
		t.Errorf("title = %q, want the player's own", res.Title)
	}
	if len(res.Files) != 1 {
		t.Fatalf("got %d files, want 1", len(res.Files))
	}
	file := res.Files[0]
	if file.URL != kvsHighDirect {
		t.Errorf("chose\n  %s\nwant the 1080p rendition\n  %s", file.URL, kvsHighDirect)
	}
	if file.Name != "A Synthetic Clip.mp4" {
		t.Errorf("name = %q, want the title plus the advertised extension", file.Name)
	}
}

// TestKVSResultFallsBackToTheOnlyRendition covers the plainer installs,
// which list one URL and no labels.
func TestKVSResultFallsBackToTheOnlyRendition(t *testing.T) {
	page := `<html><head><title>Only One | Example Tube</title></head><body>
<script>
	var flashvars = {
		license_code: '` + kvsLicense + `',
		video_url: '` + kvsLowScrambled + `'
	}
</script></body></html>`

	u, err := ParseURL("https://example.test/videos/9/only-one/")
	if err != nil {
		t.Fatal(err)
	}
	res, err := kvsResult(page, u, "example")
	if err != nil {
		t.Fatalf("kvsResult: %v", err)
	}
	if res.Files[0].URL != kvsLowDirect {
		t.Errorf("got %s, want %s", res.Files[0].URL, kvsLowDirect)
	}
	// No video_title, so the document title stands in, minus the site name.
	if res.Title != "Only One" {
		t.Errorf("title = %q, want it taken from the document title", res.Title)
	}
}

func TestKVSResultReportsAPageWithNoPlayer(t *testing.T) {
	u, err := ParseURL("https://example.test/videos/9/gone/")
	if err != nil {
		t.Fatal(err)
	}
	_, err = kvsResult("<html><body>This video has been removed.</body></html>", u, "example")
	if err == nil {
		t.Fatal("a page with no player configuration was accepted")
	}
	if !strings.Contains(err.Error(), "example") {
		t.Errorf("error %q does not name the host", err)
	}
}

// TestKVSVideoPath guards the sniff's trigger. It decides whether an
// otherwise unknown URL is worth one extra request, so it has to recognise
// the page shape without firing on plain file links.
func TestKVSVideoPath(t *testing.T) {
	tests := []struct {
		raw  string
		want bool
	}{
		{raw: "https://example.test/videos/12345/a-clip/", want: true},
		{raw: "https://example.test/videos/12345", want: true},
		{raw: "https://example.test/video/12345/a-clip/", want: true},
		{raw: "https://example.test/embed/12345", want: false},
		{raw: "https://example.test/videos/12345/a-clip.mp4", want: false},
		{raw: "https://example.test/videos", want: false},
		{raw: "https://example.test/", want: false},
		{raw: "https://example.test/files/archive.zip", want: false},
	}
	for _, tc := range tests {
		u, err := ParseURL(tc.raw)
		if err != nil {
			t.Fatal(err)
		}
		if got := kvsVideoPath(u); got != tc.want {
			t.Errorf("kvsVideoPath(%s) = %v, want %v", tc.raw, got, tc.want)
		}
	}
}

func TestKVSSitesNameAndMatchTheirHosts(t *testing.T) {
	sites := NewKVSSites(nil, nil)
	if len(sites) != len(kvsKnownHosts) {
		t.Fatalf("got %d extractors for %d hosts", len(sites), len(kvsKnownHosts))
	}
	for i, host := range kvsKnownHosts {
		name, _, _ := strings.Cut(host, ".")
		if got := sites[i].Name(); got != name {
			t.Errorf("%s is named %q, want %q", host, got, name)
		}
		u := &url.URL{Scheme: "https", Host: "www." + host, Path: "/videos/1/x/"}
		if !sites[i].Match(u) {
			t.Errorf("%s did not match its own host", host)
		}
		if sites[i].Match(&url.URL{Scheme: "https", Host: "unrelated.example.test"}) {
			t.Errorf("%s matched an unrelated host", host)
		}
	}
}
