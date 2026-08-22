package extractor

import (
	"net/url"
	"testing"
)

// A listing page shaped like the platform's, carrying the three things the
// walk depends on and the trap it has to survive: the block id that names the
// next page, the stated total, video tiles — and the site's own furniture,
// which is linked exactly like the tiles are and must not be mistaken for
// them. One tile is repeated, because the platform prints a video twice when
// it is both recent and popular.
const kvsMemberListing = `<!DOCTYPE html><html><head>
<title>Someone's Profile | Example Tube</title>
</head><body>
<nav>
  <a href="/">Home</a>
  <a href="/categories/">Categories</a>
  <a href="/members/4242/favourite_videos/">Favourites</a>
  <a href="https://other.example.test/videos/1/elsewhere/">A video on another site</a>
</nav>
<div class="headline"><h2>Someone's Public Videos</h2>
  <span>Showing 1 - 3 of 3 videos</span>
</div>
<div class="thumbs-items" id="list_videos_public_videos_items">
  <a href="https://tube.example.test/videos/11/first-clip/" class="tumbpu"><span>First</span></a>
  <a href="https://tube.example.test/videos/12/second-clip/" class="tumbpu"><span>Second</span></a>
  <a href="https://tube.example.test/videos/11/first-clip/" class="tumbpu"><span>First again</span></a>
  <a href="https://tube.example.test/videos/13/third-clip/" class="tumbpu"><span>Third</span></a>
</div>
</body></html>`

func TestKVSMemberPathAcceptsAnySection(t *testing.T) {
	want := "https://tube.example.test/members/4242/public_videos/"
	for _, raw := range []string{
		"https://tube.example.test/members/4242/",
		"https://tube.example.test/members/4242/public_videos/",
		// Someone pasting the wall or the favourites means "this person's
		// videos", so every section resolves to the one that is theirs.
		"https://tube.example.test/members/4242/wall/",
		"https://tube.example.test/members/4242/favourite_videos/",
	} {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		got, ok := kvsMemberPath(u)
		if !ok || got != want {
			t.Errorf("kvsMemberPath(%q) = %q, %v; want %q, true", raw, got, ok, want)
		}
	}
}

func TestKVSMemberPathRejectsEverythingElse(t *testing.T) {
	for _, raw := range []string{
		"https://tube.example.test/videos/11/a-clip/",
		"https://tube.example.test/members/",
		// A member id is numeric on this platform; a name is some other
		// site's idea of a profile and not ours to guess at.
		"https://tube.example.test/members/someone/",
		"https://tube.example.test/",
	} {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := kvsMemberPath(u); ok {
			t.Errorf("kvsMemberPath(%q) claimed a profile", raw)
		}
	}
}

// The listing is full of links that are not videos, and one that is a video
// on a different site entirely. Only the tiles count.
func TestKVSListingVideosIgnoresTheFurniture(t *testing.T) {
	root, err := parseHTML(kvsMemberListing)
	if err != nil {
		t.Fatal(err)
	}
	got := kvsListingVideos(root, "https://tube.example.test/members/4242/public_videos/")

	want := []string{
		"https://tube.example.test/videos/11/first-clip/",
		"https://tube.example.test/videos/12/second-clip/",
		"https://tube.example.test/videos/13/third-clip/",
	}
	if len(got) != len(want) {
		t.Fatalf("read %d videos, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("video %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestKVSMemberTitleIsTheMemberNotTheSection(t *testing.T) {
	root, err := parseHTML(kvsMemberListing)
	if err != nil {
		t.Fatal(err)
	}
	// "Someone's Public Videos" names a section as much as a person, and it
	// is the person a folder should be called after.
	if got := kvsMemberTitle(root, kvsMemberListing); got != "Someone" {
		t.Errorf("title = %q, want %q", got, "Someone")
	}
}

func TestKVSMemberTitleFallsBackToTheHeading(t *testing.T) {
	doc := `<html><head><title>A Tube</title></head><body><h2>Latest Videos</h2></body></html>`
	root, err := parseHTML(doc)
	if err != nil {
		t.Fatal(err)
	}
	if got := kvsMemberTitle(root, doc); got != "Latest Videos" {
		t.Errorf("title = %q, want the heading as it stands", got)
	}
}

// The two values the walk reads out of the first page to know how to ask for
// the second, and when to stop asking.
func TestKVSListingBlockAndTotal(t *testing.T) {
	m := kvsListingBlock.FindStringSubmatch(kvsMemberListing)
	if m == nil || m[1] != "list_videos_public_videos" {
		t.Errorf("block id = %v, want list_videos_public_videos", m)
	}
	n := kvsShowingTotal.FindStringSubmatch(kvsMemberListing)
	if n == nil || n[1] != "3" {
		t.Errorf("total = %v, want 3", n)
	}
	// A listing with neither is still walkable — it simply stops after the
	// page it could read.
	if kvsListingBlock.MatchString(`<div class="thumbs-items">`) {
		t.Error("matched a container with no block id")
	}
}
