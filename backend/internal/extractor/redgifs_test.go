package extractor

import "testing"

func TestRedgifsItemFile(t *testing.T) {
	item := redgifsItem{ID: "SyntheticClip"}
	item.URLs.HD = "https://media.example.test/SyntheticClip.mp4"
	item.URLs.SD = "https://media.example.test/SyntheticClip-mobile.mp4"

	file, ok := item.file()
	if !ok {
		t.Fatal("an item with both renditions yielded nothing")
	}
	if file.URL != item.URLs.HD {
		t.Errorf("url = %q, want the HD rendition preferred", file.URL)
	}
	if file.Name != "SyntheticClip.mp4" {
		t.Errorf("name = %q, want the id with the link's extension", file.Name)
	}

	// SD only: still worth having.
	item.URLs.HD = ""
	if file, ok := item.file(); !ok || file.URL != item.URLs.SD {
		t.Errorf("SD fallback = %q ok=%v, want the SD rendition", file.URL, ok)
	}

	// No renditions at all: nothing to queue.
	item.URLs.SD = ""
	if _, ok := item.file(); ok {
		t.Error("an item with no renditions yielded a file")
	}
}
