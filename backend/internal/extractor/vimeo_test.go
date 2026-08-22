package extractor

import (
	"net/url"
	"testing"
)

func TestVimeoVideoID(t *testing.T) {
	tests := []struct {
		raw      string
		wantID   string
		wantHash string
	}{
		{raw: "https://vimeo.com/123456789", wantID: "123456789"},
		{raw: "https://vimeo.com/123456789?foo=bar", wantID: "123456789"},
		// The second segment of an unlisted link is its privacy hash.
		{raw: "https://vimeo.com/123456789/abcdef1234", wantID: "123456789", wantHash: "abcdef1234"},
		{raw: "https://player.vimeo.com/video/123456789", wantID: "123456789"},
		{raw: "https://player.vimeo.com/video/123456789?h=abcdef1234", wantID: "123456789", wantHash: "abcdef1234"},
		{raw: "https://vimeo.com/channels/staffpicks/123456789", wantID: "123456789"},
		{raw: "https://vimeo.com/groups/agroup/videos/123456789", wantID: "123456789"},
		{raw: "https://vimeo.com/album/987654/video/123456789", wantID: "123456789"},
		// Nothing numeric to take: a channel or a profile, not a video.
		{raw: "https://vimeo.com/channels/staffpicks"},
		{raw: "https://vimeo.com/someuser"},
		{raw: "https://vimeo.com/"},
	}
	for _, tc := range tests {
		u, err := ParseURL(tc.raw)
		if err != nil {
			t.Fatal(err)
		}
		id, hash := vimeoVideoID(u)
		if id != tc.wantID || hash != tc.wantHash {
			t.Errorf("vimeoVideoID(%s) = %q, %q; want %q, %q", tc.raw, id, hash, tc.wantID, tc.wantHash)
		}
	}
}

// TestVimeoPlayerURL pins the one thing that makes this host work at all:
// the request goes to the embed player, not to the watch page.
func TestVimeoPlayerURL(t *testing.T) {
	if got, want := vimeoPlayerURL("123456789", ""), "https://player.vimeo.com/video/123456789"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if got, want := vimeoPlayerURL("123456789", "abcdef1234"),
		"https://player.vimeo.com/video/123456789?h=abcdef1234"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestVimeoMatch(t *testing.T) {
	v := NewVimeo()
	for _, host := range []string{"vimeo.com", "www.vimeo.com", "player.vimeo.com"} {
		if !v.Match(&url.URL{Scheme: "https", Host: host, Path: "/1"}) {
			t.Errorf("%s was not matched", host)
		}
	}
	if v.Match(&url.URL{Scheme: "https", Host: "notvimeo.com"}) {
		t.Error("a lookalike host was matched")
	}
}
