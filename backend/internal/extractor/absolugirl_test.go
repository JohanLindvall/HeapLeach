// SPDX-License-Identifier: MIT

package extractor

import (
	"strings"
	"testing"

	"github.com/JohanLindvall/HeapLeach/internal/config"
)

const encodedPlayerFixture = `<title>A sample clip</title><script>
servervideo = 'https://cdn.example.test/';
path = 'media/';
repp = codage('sample');
filee = '/clip.mp4';
flowplayer("player", {src:'player.swf'}, {
 clip: {url: servervideo+path+repp+filee, framesURL: '/preview/[index].jpg'},
 plugins: {controls: {url: '/controls.swf'}}
});</script>`

func TestEncodedPlayer(t *testing.T) {
	const want = "https://cdn.example.test/media/5e8ff9bf55ba3508199d22e984129be6/clip.mp4"
	for _, tc := range []struct {
		name, doc string
		want      bool
	}{
		{"player", encodedPlayerFixture, true},
		{"HTML5", strings.ReplaceAll(encodedPlayerFixture, "clip: {url: servervideo+path+repp+filee, framesURL: '/preview/[index].jpg'}", `clip: {sources: [{type: "video/mp4", src: servervideo+path+repp+filee}]}`), true},
		{"no player", strings.ReplaceAll(encodedPlayerFixture, "flowplayer(", "other("), false},
		{"no clip", strings.ReplaceAll(encodedPlayerFixture, "clip:", "preview:"), false},
		{"missing field", strings.ReplaceAll(encodedPlayerFixture, "path = 'media/';", ""), false},
		{"expression", strings.ReplaceAll(encodedPlayerFixture, "path = 'media/';", "path = 'media/' + unknown;"), false},
		{"nonmedia", strings.ReplaceAll(encodedPlayerFixture, "clip.mp4", "poster.jpg"), false},
		{"duplicate assignment", strings.ReplaceAll(encodedPlayerFixture, "</script>", "path = 'other/';</script>"), false},
		{"comment", "<script>/*" + strings.TrimSuffix(strings.TrimPrefix(encodedPlayerFixture, "<title>A sample clip</title><script>"), "</script>") + "*/</script>", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			expectRecognised(t, NewPlayer(nil, &absolugirlPlayer), htmlPage(t, "https://example.test/video-test123.html", tc.doc), tc.want)
			got := absolugirlMedia(mustParseDoc(t, tc.doc), mustParse(t, "https://example.test/video-test123.html"))
			if tc.want && got != want || !tc.want && got != "" {
				t.Fatalf("got %q", got)
			}
		})
	}
	srv, _ := mediaPageServer(t, map[string]sitePage{"/video-test123.html": {kind: "text/html", body: encodedPlayerFixture}})
	client := testClient()
	reg := NewRegistry(&config.Config{}, client)
	result, ex, err := reg.Extract(t.Context(), srv.URL+"/video-test123.html", Options{})
	if err != nil {
		t.Fatal(err)
	}
	if ex.Name() != "absolugirl" {
		t.Fatalf("wrong extractor: %s", ex.Name())
	}
	if len(result.Files) != 1 || result.Files[0].URL != want {
		t.Fatalf("got %+v", result)
	}
}
