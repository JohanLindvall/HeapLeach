// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

// sendvidSite serves watch pages shaped like the site's: the title in the
// page metadata, og:video naming a link that is not the media, and the
// player's <source> signed afresh on every visit.
func sendvidSite(t *testing.T, titles map[string]string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var visits atomic.Int32
	mux := http.NewServeMux()
	for id, title := range titles {
		mux.HandleFunc("/"+id, func(w http.ResponseWriter, _ *http.Request) {
			n := visits.Add(1)
			meta := ""
			if title != "" {
				meta = fmt.Sprintf(`<meta name="twitter:title" content="%s"/><meta property="og:title" content="%s"/>`, title, title)
			}
			_, _ = fmt.Fprintf(w, `<!DOCTYPE html><html><head><title>%s - Sendvid</title>%s
<meta property="og:video" content="https://sendvid.example.test/%s.mp4"/>
<meta property="og:video:type" content="video/mp4"/></head><body>
<video id="video-js-video" class="video-js" controls preload="none">
<source src="https://videos9.media.example.test/ab/cd/%s.mp4?validfrom=1000&amp;validto=15400&amp;rate=250k&amp;hash=visit%d" type="video/mp4" id="video_source"/>
</video></body></html>`, title, meta, id, id, n)
		})
	}
	mux.HandleFunc("/gone0000", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "<html><title>Upload and Share Videos - Sendvid</title>Not Found</html>", http.StatusNotFound)
	})
	mux.HandleFunc("/empty000", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `<html><head><meta property="og:title" content="Nothing Here"/></head><body></body></html>`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &visits
}

func sendvidExtract(t *testing.T, raw string) (*Result, error) {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return NewSendvid(httpx.New("test-agent", "en-US", 0, 5*time.Second)).Extract(context.Background(), u, Options{})
}

func TestSendvidTakesThePlayersSignedSourceAndSignsItAgainLater(t *testing.T) {
	srv, visits := sendvidSite(t, map[string]string{"abcd1234": "Harbour Walk"})
	res, err := sendvidExtract(t, srv.URL+"/abcd1234")
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	f := res.Files[0]
	if res.Title != "Harbour Walk" || f.Name != "Harbour Walk.mp4" {
		t.Errorf("title %q, name %q", res.Title, f.Name)
	}
	want := "https://videos9.media.example.test/ab/cd/abcd1234.mp4?validfrom=1000&validto=15400&rate=250k&hash=visit1"
	if f.URL != want {
		t.Errorf("url = %q, want the player's source with its signature intact", f.URL)
	}
	if f.Headers["Referer"] != srv.URL+"/abcd1234" {
		t.Errorf("headers = %v, want the page as referer", f.Headers)
	}
	if f.Resolve == nil {
		t.Fatal("no resolver: a link signed for hours would expire in a long queue")
	}
	target, err := f.Resolve(context.Background())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !strings.HasSuffix(target.URL, "hash=visit2") || visits.Load() != 2 {
		t.Errorf("resolved %q after %d visits, want a link signed on a second visit", target.URL, visits.Load())
	}
}

// The embed page and the og:video link name the same video as the watch
// page, which is the one read.
func TestSendvidReadsEveryLinkShapeAsTheWatchPage(t *testing.T) {
	srv, _ := sendvidSite(t, map[string]string{"abcd1234": "Harbour Walk"})
	for _, raw := range []string{srv.URL + "/embed/abcd1234", srv.URL + "/abcd1234.mp4"} {
		res, err := sendvidExtract(t, raw)
		if err != nil {
			t.Fatalf("%s: %v", raw, err)
		}
		if res.Files[0].Name != "Harbour Walk.mp4" || !strings.Contains(res.Files[0].URL, "videos9.media.example.test") {
			t.Errorf("%s: %q from %q", raw, res.Files[0].Name, res.Files[0].URL)
		}
	}
}

func TestSendvidNamesAnUntitledVideoByItsID(t *testing.T) {
	srv, _ := sendvidSite(t, map[string]string{"untitled9": ""})
	res, err := sendvidExtract(t, srv.URL+"/untitled9")
	if err != nil {
		t.Fatal(err)
	}
	if res.Files[0].Name != "untitled9.mp4" {
		t.Errorf("name %q, want the id", res.Files[0].Name)
	}
}

func TestSendvidSaysWhenAVideoIsGone(t *testing.T) {
	srv, _ := sendvidSite(t, nil)
	_, err := sendvidExtract(t, srv.URL+"/gone0000")
	if err == nil || !strings.Contains(err.Error(), "not found or has been removed") {
		t.Errorf("err = %v, want the video reported gone", err)
	}
	_, err = sendvidExtract(t, srv.URL+"/empty000")
	if err == nil || !strings.Contains(err.Error(), "no video on") {
		t.Errorf("err = %v, want a page without a player refused", err)
	}
}

func TestSendvidID(t *testing.T) {
	for raw, want := range map[string]string{
		"https://sendvid.com/abcd1234":          "abcd1234",
		"https://sendvid.com/abcd1234.mp4":      "abcd1234",
		"https://www.sendvid.com/embed/Ab12Cd":  "Ab12Cd",
		"https://sendvid.com/embed/abcd1234?x=": "abcd1234",
		"https://sendvid.com/":                  "",
		"https://sendvid.com/help/faq":          "",
		"https://sendvid.com/users/sign_in":     "",
		"https://sendvid.com/not-an-id":         "",
	} {
		u, _ := url.Parse(raw)
		if got := sendvidID(u); got != want {
			t.Errorf("sendvidID(%s) = %q, want %q", raw, got, want)
		}
	}
}
