// SPDX-License-Identifier: MIT

package extractor

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/JohanLindvall/HeapLeach/internal/config"
)

const pornzogPage = `<title>Sample clip - PornZog</title>
<iframe allowfullscreen src="https://ads.example.test/banner"></iframe>
<div class="player"><div class="fluid-width-video-wrapper">
<iframe src="https://tube.example.test/embed/123456/?promo=example&amp;source=" allowfullscreen></iframe>
</div></div>
<textarea><iframe allowfullscreen src="https://ads.example.test/example"></iframe></textarea>`

func TestPornzogPointer(t *testing.T) {
	const page = "https://index.example.test/video/123456/sample-clip/"
	p := pointerNamed(t, "pornzog")
	for _, tc := range []struct {
		name, body string
		want       bool
	}{
		{"player", pornzogPage, true},
		{"name only", `<title>PornZog</title>`, false},
		{"no name", strings.ReplaceAll(pornzogPage, "PornZog", "Other"), false},
		{"advert outside player", strings.ReplaceAll(pornzogPage, `class="player"`, `class="advert"`), false},
		{"not fullscreen", strings.ReplaceAll(pornzogPage, "allowfullscreen", ""), false},
		{"home alias", strings.ReplaceAll(pornzogPage, "tube.example.test", "www.pornzog.com"), false},
		{"not web", strings.ReplaceAll(pornzogPage, "https://tube.example.test", "javascript:"), false},
	} {
		t.Run(tc.name, func(t *testing.T) { expectRecognised(t, p, htmlPage(t, page, tc.body), tc.want) })
	}
	expectShape(t, p, page, true)
	for _, path := range []string{"/categories/", "/video/not-an-id/sample/", "/video/123456/", "/user/history/"} {
		expectShape(t, p, "https://index.example.test"+path, false)
		expectRecognised(t, p, htmlPage(t, "https://index.example.test"+path, pornzogPage), false)
	}
	got := pornzogFrames(mustParseDoc(t, pornzogPage+pornzogPage), mustParse(t, page))
	if !slices.Equal(got, []string{"https://tube.example.test/embed/123456/?promo=example&source="}) {
		t.Fatalf("player links = %v", got)
	}
	if got := pornzogFrames(mustParseDoc(t, strings.ReplaceAll(pornzogPage, "tube.example.test", "index.example.test")), mustParse(t, page)); len(got) != 0 {
		t.Fatalf("same-site frame = %v", got)
	}
}

func TestPornzogRegistryFollowsOnlyPlayer(t *testing.T) {
	for _, removed := range []bool{false, true} {
		t.Run(fmt.Sprint(removed), func(t *testing.T) {
			body := pornzogPage
			if removed {
				body = `<title>PornZog</title><iframe allowfullscreen src="https://tube.example.test/embed/123456/"></iframe>`
			}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, body) }))
			t.Cleanup(srv.Close)
			reg := NewRegistry(&config.Config{}, testClient())
			aliasClaims(reg, "pornzog", "127.0.0.1")
			reg.extractors = append(reg.extractors, stubExtractor{hostClaim: "tube.example.test", title: "Sample clip"})
			res, ex, err := reg.Extract(t.Context(), srv.URL+"/video/123456/sample-clip/", Options{})
			if ex == nil || ex.Name() != "pornzog" {
				t.Fatalf("extractor = %v: %v", ex, err)
			}
			if removed {
				if err == nil || !strings.Contains(err.Error(), "embeds no player") {
					t.Fatalf("removed: %v", err)
				}
			} else if err != nil || res.Title != "Sample clip" {
				t.Fatalf("result = %+v: %v", res, err)
			}
		})
	}
}
