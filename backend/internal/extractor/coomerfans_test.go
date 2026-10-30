// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// coomerFansPostPage is a post carrying one image and one video, in the shape
// the site serves. Both are on a reserved domain: what is under test is which
// elements count as media and which of them expire, neither of which cares
// whose host they are on.
const coomerFansPostPage = `<html><head><title>a-creator / A post with two things</title></head><body>
<article class="text-block model-info"><span class="model-name">a-creator</span></article>
<div class="post-wrap">
  <h1>A post with two things</h1>
  <div class="post-body">
    <img src="https://img5.example.test/storage/2/yh/nl/aaaa-1111.jpg" alt="">
    <div class="player-wrap">
      <div class="player">
        <video id="my-video" class="video-js" controls>
          <source src="https://img2.example.test/storage/6/pc/of/bbbb-2222.m4v?e=1787382453&amp;hash=SIGNATURE" alt="">
        </video>
      </div>
    </div>
  </div>
</div>
<div class="thumbs-list">
  <div class="thumb"><a href="/u/onlyfans/999/someone-else"><img src="https://example.test/istorage/999.jpg"></a></div>
</div>
</body></html>`

func TestCoomerFansPostSeparatesVideoFromImage(t *testing.T) {
	base := mustParse(t, "https://coomerfans.com/p/1/2/onlyfans")
	title, media, err := coomerFansPost(coomerFansPostPage, base)
	if err != nil {
		t.Fatalf("coomerFansPost: %v", err)
	}

	if title != "A post with two things" {
		t.Errorf("title = %q, want the post's own heading", title)
	}
	if len(media) != 2 {
		t.Fatalf("found %d media, want 2: %+v", len(media), media)
	}
	if media[0].Video {
		t.Error("the image was taken for a video")
	}
	if !media[1].Video {
		t.Error("the video was taken for an image")
	}
	if !strings.HasSuffix(media[0].URL, "aaaa-1111.jpg") {
		t.Errorf("image URL = %q", media[0].URL)
	}
	if !strings.Contains(media[1].URL, "hash=SIGNATURE") {
		t.Errorf("video URL lost its signature: %q", media[1].URL)
	}
}

// TestCoomerFansPostIgnoresTheRestOfThePage matters because these pages carry
// a whole gallery of other creators below the post: only what is inside the
// post body belongs to this job.
func TestCoomerFansPostIgnoresTheRestOfThePage(t *testing.T) {
	base := mustParse(t, "https://coomerfans.com/p/1/2/onlyfans")
	_, media, err := coomerFansPost(coomerFansPostPage, base)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range media {
		if strings.Contains(item.URL, "istorage") {
			t.Errorf("a recommendation thumbnail was collected: %s", item.URL)
		}
	}
}

func TestCoomerFansPostReportsAMissingBody(t *testing.T) {
	base := mustParse(t, "https://coomerfans.com/p/1/2/onlyfans")
	if _, _, err := coomerFansPost("<html><body>Not found</body></html>", base); err == nil {
		t.Fatal("a page with no post body was accepted")
	}
}

// TestCoomerFansMediaPathIgnoresTheSignature is what lets a video be found
// again on a later visit: the signature changes every time, the path does not.
func TestCoomerFansMediaPathIgnoresTheSignature(t *testing.T) {
	first := coomerFansMediaPath("https://img2.example.test/storage/6/pc/of/bbbb-2222.m4v?e=1&hash=AAA")
	second := coomerFansMediaPath("https://img2.example.test/storage/6/pc/of/bbbb-2222.m4v?e=2&hash=BBB")
	if first != second {
		t.Errorf("%q and %q are the same file but did not match", first, second)
	}
	other := coomerFansMediaPath("https://img2.example.test/storage/6/pc/of/cccc-3333.m4v?e=1&hash=AAA")
	if first == other {
		t.Error("two different files matched")
	}
}

func TestCoomerFansIsPost(t *testing.T) {
	tests := map[string]bool{
		"https://coomerfans.com/p/13423866/212948/onlyfans": true,
		"https://coomerfans.com/p/1/2":                      true,
		"https://coomerfans.com/u/onlyfans/212948/someone":  false,
		"https://coomerfans.com/":                           false,
		"https://elsewhere.example.test/p/1/2/onlyfans":     false,
		"not a url at all":                                  false,
	}
	for link, want := range tests {
		if got := coomerFansIsPost(link); got != want {
			t.Errorf("coomerFansIsPost(%q) = %v, want %v", link, got, want)
		}
	}
}

func TestCoomerFansNextPage(t *testing.T) {
	base := mustParse(t, "https://coomerfans.com/u/onlyfans/212948/a-creator")
	root := mustParseDoc(t, `<div class="pagination">
		<a href="/u/onlyfans/212948/a-creator?page=1">1</a>
		<a class="next" href="/u/onlyfans/212948/a-creator?page=2">next</a>
	</div>`)
	const want = "https://coomerfans.com/u/onlyfans/212948/a-creator?page=2"
	if got := coomerFansNextPage(root, base); got != want {
		t.Errorf("got %q, want %q", got, want)
	}

	last := mustParseDoc(t, `<div class="pagination"><a href="?page=5">5</a></div>`)
	if got := coomerFansNextPage(last, base); got != "" {
		t.Errorf("the last page offered %q, want nothing to follow", got)
	}
}

// A creator's pages are walked through their next links, and the listing
// ends quietly where it does, at a page that is not there.
func TestCoomerFansPostLinksWalksToTheEnd(t *testing.T) {
	page := func(next string, posts ...string) string {
		var b strings.Builder
		b.WriteString(`<h1>a-creator</h1><a href="/u/onlyfans/1/someone-else">Other</a>`)
		for _, post := range posts {
			b.WriteString(`<a href="https://coomerfans.com/p/` + post + `/1/onlyfans">` + post + `</a>`)
		}
		if next != "" {
			b.WriteString(`<a class="next" href="` + next + `">next</a>`)
		}
		return b.String()
	}
	for _, tc := range []struct {
		name  string
		pages map[string]string
		want  []string
		never string
		opts  Options
		note  string
	}{
		{"missing page", map[string]string{
			"":  page("?page=2", "11", "12"),
			"2": page("?page=3", "12", "13"),
		}, []string{"11", "12", "13"}, "", Options{}, ""},
		// Every post is a page to read, so the listing cap stops the walk,
		// and says so.
		{"capped", map[string]string{
			"":  page("?page=2", "11", "12"),
			"2": page("?page=3", "13", "14"),
			"3": page("", "15"),
		}, []string{"11", "12", "13"}, "3", Options{limits: Limits{Sources: 3}}, "partial — source limit"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			asked := map[string]int{}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				n := r.URL.Query().Get("page")
				mu.Lock()
				asked[n]++
				mu.Unlock()
				if doc, ok := tc.pages[n]; ok {
					_, _ = w.Write([]byte(doc))
					return
				}
				http.NotFound(w, r)
			}))
			defer srv.Close()
			c := NewCoomerFans(testClient())
			links, title, note, err := c.postLinks(context.Background(), mustParse(t, srv.URL+"/u/onlyfans/1/a-creator"), tc.opts)
			if err != nil || title != "a-creator" || len(links) != len(tc.want) || note != tc.note {
				t.Fatalf("links=%v title=%q note=%q err=%v", links, title, note, err)
			}
			for i, post := range tc.want {
				if links[i] != "https://coomerfans.com/p/"+post+"/1/onlyfans" {
					t.Errorf("link %d = %s, want post %s", i, links[i], post)
				}
			}
			mu.Lock()
			defer mu.Unlock()
			if tc.never != "" && asked[tc.never] != 0 {
				t.Errorf("asked past the end: %v", asked)
			}
		})
	}
}

// TestCoomerFansTitle covers the dangling separator a post with no text of
// its own leaves behind, which would otherwise name a folder.
func TestCoomerFansTitle(t *testing.T) {
	tests := map[string]string{
		"a-creator / ":         "a-creator",
		"a-creator /":          "a-creator",
		"  a-creator / A post": "a-creator / A post",
		"a-creator":            "a-creator",
	}
	for in, want := range tests {
		if got := coomerFansTitle(in); got != want {
			t.Errorf("coomerFansTitle(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCoomerFansRecognise(t *testing.T) {
	c := NewCoomerFans(nil)
	post := `<html><head><title>a-creator / A post</title></head><body><div class="post-body"><img src="/i/1.jpg"></div></body></html>`
	profile := `<html><head><title>a-creator posts</title></head><body>` +
		`<a href="/p/101/212948/onlyfans">A post</a><a class="next" href="?page=2">Next</a></body></html>`
	expectRecognised(t, c, htmlPage(t, "https://fans.example.test/p/101/212948/onlyfans", post), true)
	expectRecognised(t, c, htmlPage(t, "https://fans.example.test/u/onlyfans/212948/a-creator", profile), true)
	// The right form, but no post body, or posts of another creator only.
	expectRecognised(t, c, htmlPage(t, "https://fans.example.test/p/101/212948/onlyfans", profile), false)
	expectRecognised(t, c, htmlPage(t, "https://fans.example.test/u/onlyfans/999/someone-else", profile), false)
	// A post body on a page of another form.
	expectRecognised(t, c, htmlPage(t, "https://fans.example.test/random-post", post), false)

	expectShape(t, c, "https://fans.example.test/p/101/212948/onlyfans", true)
	expectShape(t, c, "https://fans.example.test/u/onlyfans/212948/a-creator", true)
	expectShape(t, c, "https://fans.example.test/u/a-creator", false)
	expectShape(t, c, "https://fans.example.test/p/101", false)
}
