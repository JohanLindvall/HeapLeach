package extractor

import "testing"

// The player lists its largest file first and marks it 720p, while the
// file's own name says 1920x1080. That disagreement is the reason the
// filename decides, and the reason this fixture exists.
const pornonePlayerPage = `<html><body>
<h1>a synthetic clip</h1>
<video id="pornone-video-player" class="video-js" controls preload="none">
  <source src="https://s1.example.test/vid2/TOKEN/1787378460/21/1/1_1920x1080_4000k.mp4?lang=en"
          type="video/mp4" label="720p" res="720"/>
  <source src="https://s2.example.test/vid2/TOKEN/1787378460/21/1/1_720x406_500k.mp4?lang=en"
          type="video/mp4" label="480p" res="480"/>
</video>
</body></html>`

func TestPornOneRenditionsTrustTheFilename(t *testing.T) {
	renditions := pornoneRenditions(pornonePlayerPage)
	if len(renditions) != 2 {
		t.Fatalf("read %d renditions, want 2", len(renditions))
	}
	if renditions[0].Quality != 1080 {
		t.Errorf("the 1920x1080 file ranks as %d, but the player labels it 720p",
			renditions[0].Quality)
	}
	if renditions[1].Quality != 406 {
		t.Errorf("the 720x406 file ranks as %d", renditions[1].Quality)
	}

	best, ok := bestCandidate(renditions)
	if !ok {
		t.Fatal("no rendition was chosen")
	}
	if best.Quality != 1080 {
		t.Errorf("chose the %d rendition, want the largest", best.Quality)
	}
}

// TestPornOneRenditionsFallBackToTheAttribute covers a page whose filenames
// carry no dimensions, where the player's own attribute is all there is.
func TestPornOneRenditionsFallBackToTheAttribute(t *testing.T) {
	const page = `<html><body><video>
  <source src="https://s1.example.test/vid2/TOKEN/1/1/low.mp4" label="480p" res="480"/>
  <source src="https://s1.example.test/vid2/TOKEN/1/1/high.mp4" label="1080p" res="1080"/>
</video></body></html>`

	best, ok := bestCandidate(pornoneRenditions(page))
	if !ok {
		t.Fatal("no rendition was chosen")
	}
	if best.Quality != 1080 {
		t.Errorf("chose the %d rendition, want the one the player calls largest", best.Quality)
	}
}

func TestPornOneRenditionsOnAPageWithNoPlayer(t *testing.T) {
	if got := pornoneRenditions(`<html><body>This video has been removed.</body></html>`); len(got) != 0 {
		t.Errorf("found %d renditions on a page with no player", len(got))
	}
}

func TestFirstHeading(t *testing.T) {
	if got := firstHeading(pornonePlayerPage); got != "a synthetic clip" {
		t.Errorf("firstHeading = %q", got)
	}
	if got := firstHeading(`<html><body><p>no heading</p></body></html>`); got != "" {
		t.Errorf("firstHeading = %q, want empty", got)
	}
}
