// SPDX-License-Identifier: MIT

package extractor

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

const bebasbokepSetupFixture = `jwplayer("jw_player").setup({playlist:[{sources:(function(){let rawSources=[
{'label':'360p','type':'video\/mp4','file':'/media/Low.mp4'},
{'label':'1080p','type':'video\/mp4','file':'/media/First%20Clip.mp4?token=a%2Bb&filename=Original%20Clip.mp4'},
{'label':'2160p','type':'image/jpeg','file':'/media/preview.jpg'}
];if(Array.isArray(rawSources)){return rawSources.map(source=>{if(source.file){}return source})}return rawSources})(),
image:'/poster.jpg',tracks:[{file:'/thumbnails.vtt'}]}],logo:{file:'/logo.svg'}});`

func bebasbokepFixture(script string, packed bool) string {
	if packed {
		script = strings.ReplaceAll(script, "jwplayer", "0")
		script = strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(script)
		script = `eval(function(p,a,c,k,e,d){return p;}('` + script + `',62,1,'jwplayer'.split('|'),0,{}))`
	}
	return `<title>A sample clip</title><div id="jw_player"></div><script>` + script + `</script>`
}

func TestBebasBokepRecognition(t *testing.T) {
	player := NewPlayer(nil, &bebasbokepPlayer)
	for _, tc := range []struct {
		name, script string
		want         bool
	}{
		{"sources", bebasbokepSetupFixture, true},
		{"name alone", `var title="BebasBokep";`, false},
		{"comment", "/*" + bebasbokepSetupFixture + "*/", false},
		{"quoted example", "`" + bebasbokepSetupFixture + "`", false},
		{"different player", strings.ReplaceAll(bebasbokepSetupFixture, `"jw_player"`, `"advertisement"`), false},
		{"different array", strings.ReplaceAll(bebasbokepSetupFixture, "rawSources", "advertisements"), false},
		{"expression", strings.ReplaceAll(bebasbokepSetupFixture, "'/media/Low.mp4'", "getSource()"), false},
		{"concatenation", strings.ReplaceAll(bebasbokepSetupFixture, "'/media/Low.mp4'", "'/media/Low.mp4' + token"), false},
		{"duplicate property", strings.ReplaceAll(bebasbokepSetupFixture, "'label':'360p'", "'label':'360p','file':'/other.mp4'"), false},
		{"broken array", strings.ReplaceAll(bebasbokepSetupFixture, "];if", ";if"), false},
		{"array expression", strings.ReplaceAll(bebasbokepSetupFixture, "];if", "].concat(extra);if"), false},
		{"unsafe links", strings.ReplaceAll(bebasbokepSetupFixture, "'/media/", "'javascript:/media/"), false},
		{"only images", strings.ReplaceAll(bebasbokepSetupFixture, ".mp4", ".jpg"), false},
	} {
		for _, packed := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/packed=%t", tc.name, packed), func(t *testing.T) {
				expectRecognised(t, player, htmlPage(t, "https://example.test/video/AbC123", bebasbokepFixture(tc.script, packed)), tc.want)
			})
		}
	}
	expectShape(t, player, "https://example.test/video/AbC123", true)
	expectShape(t, player, "https://example.test/video/AbC123/extra", false)
	expectRecognised(t, player, htmlPage(t, "https://example.test/category/clips", bebasbokepFixture(bebasbokepSetupFixture, true)), false)
	expectRecognised(t, player, htmlPage(t, "https://example.test/video/AbC123", strings.ReplaceAll(bebasbokepFixture(bebasbokepSetupFixture, true), `id="jw_player"`, `id="removed"`)), false)
}

func TestBebasBokepEmbeddedPlayer(t *testing.T) {
	var reads atomic.Int32
	player := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/video/Old123":
			http.Redirect(w, r, "/video/New456", http.StatusFound)
		case "/video/New456":
			reads.Add(1)
			w.Header().Set(httpx.HeaderContentType, "text/html")
			fmt.Fprint(w, bebasbokepFixture(bebasbokepSetupFixture, true))
		default:
			t.Errorf("unexpected player/media request: %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(player.Close)
	// This theme places the main player outside its empty entry-content.
	post := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(httpx.HeaderContentType, "text/html")
		fmt.Fprintf(w, `<html><head><meta name="generator" content="WordPress"><title>Example post</title></head>
<body class="single"><main><div class="video-player"><iframe src="%s/video/Old123"></iframe></div>
<div class="entry-content"></div></main></body></html>`, player.URL)
	}))
	t.Cleanup(post.Close)
	for _, source := range []string{player.URL + "/video/Old123", post.URL + "/clips/sample/"} {
		reg := NewRegistry(&config.Config{}, testClient())
		reg.AliasHosts("bebasbokep", "127.0.0.1")
		before := reads.Load()
		res, _, err := reg.Extract(t.Context(), source, Options{})
		if err != nil || res == nil || len(res.Files) != 1 {
			t.Fatalf("result=%+v err=%v", res, err)
		}
		f := res.Files[0]
		if f.URL != player.URL+"/media/First%20Clip.mp4?token=a%2Bb&filename=Original%20Clip.mp4" ||
			f.Name != "Original Clip.mp4" || f.Headers[httpx.HeaderReferer] != player.URL+"/video/New456" || reads.Load()-before != 1 || f.External != "" {
			t.Fatalf("file=%+v reads=%d", f, reads.Load()-before)
		}
	}
}
