package extractor

import (
	"net/url"
	"testing"
)

// okruEmbedPage is an embed page in the shape the site serves: the player's
// options are JSON in an attribute, and one of that JSON's fields is another
// JSON document carried as a string. Hosts are reserved domains.
const okruEmbedPage = `<html><body>
<div id="video-player" data-module="OKVideo" data-options='{"playerId":"1","flashvars":{"metadata":"{\"movie\":{\"id\":\"1234\",\"title\":\"A Synthetic Clip\",\"status\":\"OK\",\"statusText\":\"OK\"},\"videos\":[{\"name\":\"sd\",\"url\":\"https://vd1.example.test/sd?expires=1\"},{\"name\":\"full\",\"url\":\"https://vd1.example.test/full?expires=1\"},{\"name\":\"mobile\",\"url\":\"https://vd1.example.test/mobile?expires=1\"}],\"hlsManifestUrl\":\"https://vd1.example.test/video.m3u8\"}"}}'></div>
</body></html>`

func TestOKruMetadataFrom(t *testing.T) {
	meta, err := okruMetadataFrom(okruEmbedPage)
	if err != nil {
		t.Fatalf("okruMetadataFrom: %v", err)
	}
	if meta.Movie.Title != "A Synthetic Clip" {
		t.Errorf("title = %q", meta.Movie.Title)
	}
	if len(meta.Videos) != 3 {
		t.Fatalf("got %d renditions, want 3", len(meta.Videos))
	}
	if meta.HLSManifestURL == "" {
		t.Error("the playlist fallback was not read")
	}
}

// TestOKruBestPrefersTheLargestName is the point of the rank table: the site
// lists renditions in no useful order, so the name is all there is to go on.
func TestOKruBestPrefersTheLargestName(t *testing.T) {
	meta, err := okruMetadataFrom(okruEmbedPage)
	if err != nil {
		t.Fatal(err)
	}
	best, ok := okruBest(meta.Videos)
	if !ok {
		t.Fatal("no rendition was chosen")
	}
	if best != "https://vd1.example.test/full?expires=1" {
		t.Errorf("chose %q, want the full-quality rendition", best)
	}
}

func TestOKruBestHandlesAwkwardLists(t *testing.T) {
	tests := []struct {
		name   string
		videos []okruVideo
		want   string
		wantOK bool
	}{
		{
			name:   "nothing at all",
			videos: nil,
		},
		{
			name:   "named but empty",
			videos: []okruVideo{{Name: "hd"}, {Name: "sd"}},
		},
		{
			// A label this does not know is still better than no download.
			name:   "only an unknown label",
			videos: []okruVideo{{Name: "superduper", URL: "https://vd1.example.test/x"}},
			want:   "https://vd1.example.test/x",
			wantOK: true,
		},
		{
			name: "a known label outranks an unknown one",
			videos: []okruVideo{
				{Name: "superduper", URL: "https://vd1.example.test/x"},
				{Name: "sd", URL: "https://vd1.example.test/sd"},
			},
			want:   "https://vd1.example.test/sd",
			wantOK: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := okruBest(tc.videos)
			if ok != tc.wantOK || got != tc.want {
				t.Errorf("okruBest = %q, %v; want %q, %v", got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

func TestOKruMetadataFromRejectsAPageWithoutAPlayer(t *testing.T) {
	for _, doc := range []string{
		`<html><body>Video not found</body></html>`,
		`<html><body><div data-options='not json'></div></body></html>`,
		`<html><body><div data-options='{"flashvars":{}}'></div></body></html>`,
	} {
		if _, err := okruMetadataFrom(doc); err == nil {
			t.Errorf("a page with no usable player configuration was accepted: %.40s", doc)
		}
	}
}

func TestOKruVideoID(t *testing.T) {
	tests := map[string]string{
		"https://ok.ru/video/1234567890":            "1234567890",
		"https://ok.ru/video/1234567890?fromTime=9": "1234567890",
		"https://ok.ru/videoembed/1234567890":       "1234567890",
		"https://ok.ru/live/1234567890":             "1234567890",
		"https://ok.ru/dk?st.mvId=1234567890":       "1234567890",
		"https://ok.ru/video":                       "",
		"https://ok.ru/":                            "",
	}
	for raw, want := range tests {
		u, err := ParseURL(raw)
		if err != nil {
			t.Fatal(err)
		}
		if got := okruVideoID(u); got != want {
			t.Errorf("okruVideoID(%s) = %q, want %q", raw, got, want)
		}
	}
}

func TestOKruMatch(t *testing.T) {
	o := NewOKru(nil)
	for _, host := range []string{"ok.ru", "www.ok.ru", "m.ok.ru", "odnoklassniki.ru"} {
		if !o.Match(&url.URL{Scheme: "https", Host: host, Path: "/video/1"}) {
			t.Errorf("%s was not matched", host)
		}
	}
	if o.Match(&url.URL{Scheme: "https", Host: "notok.ru"}) {
		t.Error("a lookalike host was matched")
	}
}
