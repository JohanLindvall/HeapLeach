package extractor

import (
	"net/url"
	"strings"
	"testing"

	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

func TestKemonoRootCoversBothServices(t *testing.T) {
	cases := map[string]string{
		"https://kemono.cr/patreon/user/1":  "https://kemono.cr",
		"https://coomer.st/onlyfans/user/2": "https://coomer.st",
		"https://example.test/x/user/3":     "",
	}
	for raw, want := range cases {
		u, _ := url.Parse(raw)
		if got := kemonoRoot(u); got != want {
			t.Errorf("kemonoRoot(%s) = %q, want %q", raw, got, want)
		}
	}
}

func TestKemonoFilesOf(t *testing.T) {
	posts := []kemonoPost{
		{
			ID:    "111",
			Title: "First Post",
			File:  kemonoAttachment{Name: "cover.jpg", Path: "/aa/bb/cover"},
			Attachments: []kemonoAttachment{
				{Name: "one.mp4", Path: "/cc/dd/one"},
				// The primary file listed again among the attachments, which
				// the sites do; it must not queue twice.
				{Name: "cover.jpg", Path: "/aa/bb/cover"},
			},
		},
		{
			ID: "112",
			// No title: the folder falls back to the post id.
			File: kemonoAttachment{Path: "/ee/ff/plain.png"},
		},
		{
			ID: "113", Title: "Text only",
			// No file and no attachments: contributes nothing.
		},
	}

	k := NewKemono(nil)
	files := k.filesOf("https://kemono.cr", posts, true)
	if len(files) != 3 {
		t.Fatalf("got %d files, want 3 (the repeat folded, the text post empty)", len(files))
	}

	first := files[0]
	if first.Name != "cover.jpg" || first.Dir != "First Post" {
		t.Errorf("first = %q in %q, want cover.jpg in the post's folder", first.Name, first.Dir)
	}
	if want := "https://kemono.cr/data/aa/bb/cover"; first.URL != want {
		t.Errorf("url = %q, want %q", first.URL, want)
	}
	// The sites answer an ordinary browser request with 403 and a message
	// naming the header they want instead; every file must carry it.
	if first.Headers[httpx.HeaderAccept] != kemonoScrapeAccept {
		t.Errorf("accept = %q, want %q", first.Headers[httpx.HeaderAccept], kemonoScrapeAccept)
	}

	// An attachment with no name is saved under the path's own base.
	if files[2].Name != "plain.png" || files[2].Dir != "112" {
		t.Errorf("nameless attachment = %q in %q, want plain.png in the id folder",
			files[2].Name, files[2].Dir)
	}

	// A single post's files go unfoldered: the job title already names it.
	flat := k.filesOf("https://kemono.cr", posts[:1], false)
	for _, f := range flat {
		if f.Dir != "" {
			t.Errorf("%s filed under %q, want no folder for a single post", f.Name, f.Dir)
		}
	}
}

func TestFolderNameTrimsRamblingTitles(t *testing.T) {
	if got := folderName("  a tidy title  "); got != "a tidy title" {
		t.Errorf("got %q", got)
	}
	long := strings.Repeat("x", 200)
	if got := folderName(long); len([]rune(got)) != 80 {
		t.Errorf("a %d-rune title survived as %d runes, want 80", len(long), len([]rune(got)))
	}
}
