package extractor

import (
	"strings"
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
	base, err := ParseURL("https://coomerfans.com/p/1/2/onlyfans")
	if err != nil {
		t.Fatal(err)
	}
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
	base, err := ParseURL("https://coomerfans.com/p/1/2/onlyfans")
	if err != nil {
		t.Fatal(err)
	}
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
	base, err := ParseURL("https://coomerfans.com/p/1/2/onlyfans")
	if err != nil {
		t.Fatal(err)
	}
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
	base, err := ParseURL("https://coomerfans.com/u/onlyfans/212948/a-creator")
	if err != nil {
		t.Fatal(err)
	}
	root, err := parseHTML(`<div class="pagination">
		<a href="/u/onlyfans/212948/a-creator?page=1">1</a>
		<a class="next" href="/u/onlyfans/212948/a-creator?page=2">next</a>
	</div>`)
	if err != nil {
		t.Fatal(err)
	}
	const want = "https://coomerfans.com/u/onlyfans/212948/a-creator?page=2"
	if got := coomerFansNextPage(root, base); got != want {
		t.Errorf("got %q, want %q", got, want)
	}

	last, err := parseHTML(`<div class="pagination"><a href="?page=5">5</a></div>`)
	if err != nil {
		t.Fatal(err)
	}
	if got := coomerFansNextPage(last, base); got != "" {
		t.Errorf("the last page offered %q, want nothing to follow", got)
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
