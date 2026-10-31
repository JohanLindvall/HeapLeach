// SPDX-License-Identifier: MIT

package extractor

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

func TestDoodLandingSiteMetadata(t *testing.T) {
	const frame = `<iframe src="/e/synthetic"></iframe>`
	for _, tc := range []struct {
		name, path, head, body string
		want                   bool
	}{
		{"site-meta", "/d/synthetic", `<meta name="og:sitename" content="DoodStream.com">`, frame, true},
		{"site-meta-property", "/d/synthetic", `<meta property="og:sitename" content=" doodstream.COM ">`, frame, true},
		{"standard-meta", "/d/synthetic", `<meta property="og:site_name" content="DoodStream.com">`, frame, true},
		{"title", "/d/synthetic", `<title>Invented Clip - DoodStream</title>`, frame, true},
		{"unbranded", "/d/synthetic", `<title>Invented Clip</title>`, frame, false},
		{"other-site", "/d/synthetic", `<meta name="og:sitename" content="Other Player">`, frame, false},
		{"name-in-content", "/d/synthetic", `<meta name="og:sitename" content="About DoodStream.com">`, frame, false},
		{"no-player", "/d/synthetic", `<meta name="og:sitename" content="DoodStream.com">`, "", false},
		{"wrong-frame", "/d/synthetic", `<meta name="og:sitename" content="DoodStream.com">`, `<iframe src="/advert"></iframe>`, false},
		{"wrong-page", "/help", `<meta name="og:sitename" content="DoodStream.com">`, frame, false},
		{"embed-without-player", "/e/synthetic", `<meta name="og:sitename" content="DoodStream.com">`, frame, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			page := htmlPage(t, "https://alias.example.test"+tc.path,
				"<html><head>"+tc.head+"</head><body>"+tc.body+"</body></html>")
			if got := NewDoodStream(nil).Recognise(t.Context(), page); got != tc.want {
				t.Fatalf("recognised=%v, want %v", got, tc.want)
			}
		})
	}
}

// Recognition must reach the player's refusal handling during a routed
// refresh too. The URL form and the exact bare refusal identify that answer;
// an arbitrary sentence or an HTML page quoting it does not.
func TestDoodRecognisesAddressRefusal(t *testing.T) {
	const refusal = "The resource you are looking for has been removed or is temporarily unavailable."
	for _, tc := range []struct {
		name, path, body string
		want             bool
	}{
		{"landing", "/d/synthetic", refusal, true},
		{"player", "/e/synthetic", refusal, true},
		{"whitespace", "/d/synthetic", "\n " + refusal + "\r\n", true},
		{"other-path", "/help", refusal, false},
		{"other-sentence", "/d/synthetic", "This page is temporarily unavailable.", false},
		{"quoted-in-html", "/d/synthetic", "<html><p>" + refusal + "</p></html>", false},
		{"removed-video", "/d/synthetic", "<html><title>Video not found</title></html>", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			page := htmlPage(t, "https://alias.example.test"+tc.path, tc.body)
			if got := NewDoodStream(nil).Recognise(t.Context(), page); got != tc.want {
				t.Fatalf("recognised=%v, want %v", got, tc.want)
			}
		})
	}
}

// Followed sources use strict recognition. A landing page whose site is named
// only in metadata must reach its player, including the player's challenge
// handling, rather than being discarded as an unrecognised navigation link.
func TestDoodLandingMetadataReachesPlayer(t *testing.T) {
	for _, gate := range []bool{false, true} {
		t.Run(fmt.Sprintf("gate=%v", gate), func(t *testing.T) {
			var mu sync.Mutex
			requests := make(map[string]int)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				requests[r.URL.Path]++
				mu.Unlock()
				w.Header().Set(httpx.HeaderContentType, "text/html")
				switch r.URL.Path {
				case "/d/synthetic":
					fmt.Fprint(w, `<html><head><title>Invented Clip</title><meta name="og:sitename" content="DoodStream.com"></head><body><iframe src="/e/synthetic"></iframe></body></html>`)
				case "/e/synthetic":
					if gate {
						fmt.Fprint(w, doodGateFixture)
					} else {
						fmt.Fprint(w, `<html><title>Invented Clip</title><script>fetch('/pass_md5/session/synthetic')</script></html>`)
					}
				case "/pass_md5/session/synthetic":
					fmt.Fprint(w, "https://cdn.example.test/media/")
				default:
					t.Errorf("unexpected request: %s", r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer srv.Close()
			client := testClient()
			defer client.CloseIdleConnections()
			player := NewDoodStream(client)
			reg := &Registry{client: client, extractors: []Extractor{player}, fallback: NewDirect(client)}
			res, ex, err := reg.follow(t.Context(), srv.URL+"/d/synthetic", Options{})
			if ex != player || errors.Is(err, errUnrecognised) {
				t.Fatalf("extractor=%v err=%v", ex, err)
			}
			if gate {
				if _, ok := errors.AsType[*httpx.ChallengeError](err); !ok {
					t.Fatalf("expected the player's challenge, got %v", err)
				}
			} else {
				if err != nil || len(res.Files) != 1 {
					t.Fatalf("result=%+v err=%v", res, err)
				}
				file := res.Files[0]
				if file.Name != "Invented Clip.mp4" || file.Resolve == nil ||
					!strings.HasPrefix(file.URL, "https://cdn.example.test/media/") ||
					file.Headers[httpx.HeaderReferer] != srv.URL+"/e/synthetic" {
					t.Fatalf("file=%+v", file)
				}
			}
			tokenRequests := 1
			if gate {
				tokenRequests = 0
			}
			mu.Lock()
			defer mu.Unlock()
			if requests["/d/synthetic"] != 1 || requests["/e/synthetic"] != 1 || requests["/pass_md5/session/synthetic"] != tokenRequests {
				t.Fatalf("requests=%v", requests)
			}
		})
	}
}
