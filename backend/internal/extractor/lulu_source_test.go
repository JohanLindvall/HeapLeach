// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// packedLuluSetup keeps the synthetic script intact through the real packer
// decoder: with no dictionary entries, its payload needs no substitution.
func packedLuluSetup(setup string) string {
	setup = strings.ReplaceAll(strings.ReplaceAll(setup, `\`, `\\`), `'`, `\'`)
	return `eval(function(p,a,c,k,e,d){}('` + setup + `',36,0,''.split('|'),0,{}))`
}

func TestLuluSourceSingleFileSetup(t *testing.T) {
	base, _ := url.Parse("https://lulu.example.test/abcd1234wxyz")
	for _, tc := range []struct {
		name, setup, want string
	}{
		{"direct", `jwplayer("player").setup({file:"https://cdn.example.test/first.m3u8?token=synthetic"})`, "https://cdn.example.test/first.m3u8?token=synthetic"},
		{"named player and quoted key", `const player = jwplayer('player'); player.setup({'file':'/first.m3u8'});`, "https://lulu.example.test/first.m3u8"},
		{"nested decoys before media", `jwplayer("player").setup({tracks:[{file:"/preview.m3u8",kind:"thumbnails"}],preview:{file:"/preview.m3u8"},description:"a } and file: '/decoy.m3u8'",file:"/first.m3u8"})`, "https://lulu.example.test/first.m3u8"},
		{"nested decoys after media", `jwplayer("player").setup({file:"/first.m3u8",preview:{file:"/preview.m3u8"},tracks:[{file:"/sprites.jpg"}]})`, "https://lulu.example.test/first.m3u8"},
		{"comments", "// jwplayer(\"other\").setup({file:\"/decoy.m3u8\"})\njwplayer(\"player\").setup({/* file: '/decoy.m3u8', */ file: /* choose */ '/first.m3u8'})", "https://lulu.example.test/first.m3u8"},
		{"unrelated source declarations", `var example = 'sources:[]'; /* sources:[] */ var preview = {sources:[]}; jwplayer("player").setup({description:"sources:[]",file:"/first.m3u8"})`, "https://lulu.example.test/first.m3u8"},
		{"unrelated scalar shorthand", `jwplayer("player").setup({file:"/first.m3u8",controls})`, "https://lulu.example.test/first.m3u8"},
		{"source list keeps precedence", `jwplayer("player").setup({file:"/preview.m3u8",sources:[{file:"/first.mpd"},{file:"/first.m3u8"}]})`, "https://lulu.example.test/first.m3u8"},
	} {
		for _, packed := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/packed=%t", tc.name, packed), func(t *testing.T) {
				setup := tc.setup
				if packed {
					setup = packedLuluSetup(setup)
				}
				got, err := luluSource("<script>"+setup+"</script>", base)
				if err != nil || got != tc.want {
					t.Fatalf("source = %q, %v; want %q", got, err, tc.want)
				}
			})
		}
	}
}

func TestLuluSourceSharesNamedPlayerAcrossScripts(t *testing.T) {
	base, _ := url.Parse("https://lulu.example.test/abcd1234wxyz")
	for _, packed := range []bool{false, true} {
		t.Run(fmt.Sprintf("packed=%t", packed), func(t *testing.T) {
			setup := `player.setup({file:"/first.m3u8",tracks:[{file:"/preview.m3u8"}]});`
			if packed {
				setup = packedLuluSetup(setup)
			}
			// This comment ends with its script element, not at a newline.
			doc := `<script>var player = jwplayer('player'); // initialise</script><script>` + setup + `</script>`
			got, err := luluSource(doc, base)
			if err != nil || got != "https://lulu.example.test/first.m3u8" {
				t.Fatalf("source = %q, %v; want the later script's HLS file", got, err)
			}
		})
	}
}

func TestLuluSourceRejectsPreviewFiles(t *testing.T) {
	base, _ := url.Parse("https://lulu.example.test/abcd1234wxyz")
	for _, tc := range []struct{ name, setup string }{
		{"thumbnail", `jwplayer("player").setup({file:"/sprites.jpg"})`},
		{"nested HLS preview", `jwplayer("player").setup({preview:{file:"/preview.m3u8"},tracks:[{file:"/preview.m3u8"}]})`},
		{"outside player", `var preview = {file:"/preview.m3u8"}; jwplayer("player").setup({image:"/cover.jpg"})`},
		{"unrelated setup", `preview.setup({file:"/preview.m3u8"})`},
		{"quoted fake player", `var example = 'jwplayer("player").setup({file:"/preview.m3u8"})';`},
		{"commented fake player", `/* jwplayer("player").setup({file:"/preview.m3u8"}) */`},
		{"query-only HLS", `jwplayer("player").setup({file:"/sprites.jpg?next=first.m3u8"})`},
		{"file expression", `jwplayer("player").setup({file:"/preview.m3u8" + ".jpg"})`},
		{"unfinished object", `jwplayer("player").setup({file:"/preview.m3u8"`},
		{"mismatched containers", `jwplayer("player").setup({file:"/preview.m3u8", tracks:[}})`},
		{"spread overrides file", `jwplayer("player").setup({file:"/preview.m3u8",...options})`},
		{"computed file override", `jwplayer("player").setup({file:"/preview.m3u8",['file']:actualFile})`},
		{"computed source override", `jwplayer("player").setup({file:"/preview.m3u8",[mediaKey]:actualSources})`},
		{"shorthand file override", `jwplayer("player").setup({file:"/preview.m3u8",file})`},
		{"shorthand sources override", `jwplayer("player").setup({file:"/preview.m3u8",sources})`},
		{"method file override", `jwplayer("player").setup({file:"/preview.m3u8",file(){return actualFile}})`},
		{"method sources override", `jwplayer("player").setup({file:"/preview.m3u8",sources(){return actualSources}})`},
		{"getter file override", `jwplayer("player").setup({file:"/preview.m3u8",get file(){return actualFile}})`},
		{"getter sources override", `jwplayer("player").setup({file:"/preview.m3u8",get sources(){return actualSources}})`},
		{"DASH source list", `jwplayer("player").setup({file:"/preview.m3u8",sources:[{file:"/first.mpd"}]})`},
	} {
		for _, packed := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/packed=%t", tc.name, packed), func(t *testing.T) {
				setup := tc.setup
				if packed {
					setup = packedLuluSetup(setup)
				}
				got, err := luluSource("<script>"+setup+"</script>", base)
				if err == nil || got != "" {
					t.Fatalf("decoy resolved to %q, %v", got, err)
				}
				if tc.name == "DASH source list" && !strings.Contains(err.Error(), "DASH") {
					t.Fatalf("lost DASH refusal: %v", err)
				}
			})
		}
	}
}

// Recognition and extraction must accept the same single-file form, even
// on an unlisted host; the player engine still resolves the HLS natively.
func TestLuluSingleFileThroughRegistry(t *testing.T) {
	for _, packed := range []bool{false, true} {
		t.Run(fmt.Sprintf("packed=%t", packed), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/abcd1234wxyz":
					w.Header().Set("Content-Type", "text/html")
					setup := `jwplayer("player").setup({tracks:[{file:"/preview.m3u8"}],file:"/hls/first.m3u8"});`
					if packed {
						setup = packedLuluSetup(setup)
					}
					fmt.Fprintf(w, `<html><title>First Clip - LuluStream</title><h1>First Clip</h1><p>Today's synthetic clip.</p><script>%s</script></html>`, setup)
				case "/hls/first.m3u8":
					w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
					io.WriteString(w, "#EXTM3U\n#EXT-X-TARGETDURATION:10\n#EXTINF:10,\nfirst-segment.ts\n#EXT-X-ENDLIST\n")
				default:
					t.Errorf("unexpected request: %s", r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer srv.Close()
			res, ex, err := probeRegistry().Extract(context.Background(), srv.URL+"/abcd1234wxyz", Options{})
			if err != nil {
				t.Fatal(err)
			}
			if ex.Name() != "lulustream" || len(res.Files) != 1 {
				t.Fatalf("got %+v from %s", res, ex.Name())
			}
			file := res.Files[0]
			if file.Name != "First Clip.ts" || len(file.Segments) != 1 || file.Segments[0] != srv.URL+"/hls/first-segment.ts" {
				t.Fatalf("got %+v", file)
			}
		})
	}
}
