package extractor

import (
	"net/url"
	"testing"
)

// The configuration lists every rendition the site knows of, present or
// not: an absent one is an empty string or a JSON null, and null decodes to
// the same empty string.
func TestDrTuberRenditionsSkipTheAbsentOnes(t *testing.T) {
	got := drtuberRenditions(map[string]string{
		"lq": "https://cdn.example.test/mp4_lq/synthetic.mp4?expire=1",
		"hq": "",
		"4k": "",
	})
	if len(got) != 1 {
		t.Fatalf("read %d renditions, want 1", len(got))
	}
	if got[0].Quality != 320 {
		t.Errorf("the lq rendition ranks as %d, want 320", got[0].Quality)
	}
}

// The rendition names are names, not resolutions, so sorting them as text
// would put "lq" above "4k".
func TestDrTuberRenditionsRankByQualityNotName(t *testing.T) {
	best, ok := bestCandidate(drtuberRenditions(map[string]string{
		"lq": "https://cdn.example.test/mp4_lq/synthetic.mp4",
		"hq": "https://cdn.example.test/mp4_hq/synthetic.mp4",
		"4k": "https://cdn.example.test/mp4_4k/synthetic.mp4",
	}))
	if !ok {
		t.Fatal("no rendition was chosen")
	}
	if want := "https://cdn.example.test/mp4_4k/synthetic.mp4"; best.URL != want {
		t.Errorf("chose %q, want the 4k rendition", best.URL)
	}
}

// A name the map does not know still has to rank, or it would sort below
// every named one and never be chosen.
func TestDrTuberRenditionsReadAnUnknownName(t *testing.T) {
	best, ok := bestCandidate(drtuberRenditions(map[string]string{
		"lq":    "https://cdn.example.test/mp4_lq/synthetic.mp4",
		"1080p": "https://cdn.example.test/mp4_1080p/synthetic.mp4",
	}))
	if !ok {
		t.Fatal("no rendition was chosen")
	}
	if want := "https://cdn.example.test/mp4_1080p/synthetic.mp4"; best.URL != want {
		t.Errorf("chose %q, want the 1080p rendition", best.URL)
	}
}

// Equal-quality renditions must be chosen the same way every time: the
// resolve at queue time and the one at download time have to agree.
func TestDrTuberRenditionsAreOrderedDeterministically(t *testing.T) {
	files := map[string]string{
		"a": "https://cdn.example.test/a/synthetic.mp4",
		"b": "https://cdn.example.test/b/synthetic.mp4",
		"c": "https://cdn.example.test/c/synthetic.mp4",
	}
	first := drtuberRenditions(files)
	for i := 0; i < 20; i++ {
		got := drtuberRenditions(files)
		for j := range got {
			if got[j].URL != first[j].URL {
				t.Fatalf("rendition %d came back as %q, first read %q", j, got[j].URL, first[j].URL)
			}
		}
	}
}

func TestDrTuberRenditionsOnAVideoWithNoFiles(t *testing.T) {
	if got := drtuberRenditions(nil); len(got) != 0 {
		t.Errorf("found %d renditions in an empty list", len(got))
	}
	if _, ok := bestCandidate(drtuberRenditions(map[string]string{"lq": "", "hq": ""})); ok {
		t.Error("a video with no rendition was accepted")
	}
}

// Match covers the watch page and the player the embeds point at, and the
// id pattern is what separates a video from the rest of the site.
func TestDrTuberMatchAndID(t *testing.T) {
	d := NewDrTuber(nil)
	for _, host := range []string{"drtuber.com", "www.drtuber.com", "m.drtuber.com"} {
		u := &url.URL{Scheme: "https", Host: host, Path: "/video/1234567/a-synthetic-clip"}
		if !d.Match(u) {
			t.Errorf("%s was not matched", host)
		}
	}
	if d.Match(&url.URL{Scheme: "https", Host: "notdrtuber.com", Path: "/video/1234567/x"}) {
		t.Error("notdrtuber.com was matched")
	}

	ids := map[string]string{
		"/video/1234567/a-synthetic-clip": "1234567",
		"/video/1234567":                  "1234567",
		"/embed/1234567":                  "1234567",
		"/videos/1234567/x":               "",
		"/categories/something":           "",
		"/":                               "",
	}
	for path, want := range ids {
		var got string
		if m := drtuberID.FindStringSubmatch(path); m != nil {
			got = m[1]
		}
		if got != want {
			t.Errorf("drtuberID(%q) = %q, want %q", path, got, want)
		}
	}
}
