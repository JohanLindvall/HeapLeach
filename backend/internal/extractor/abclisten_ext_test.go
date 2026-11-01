// SPDX-License-Identifier: MIT

package extractor

import "testing"

// An episode's extension is kept as its link spells it, case and length
// alike: the episodes already downloaded are filed under that spelling.
func TestABCListenKeepsTheLinksExtension(t *testing.T) {
	a := NewABCListen(nil)
	for link, want := range map[string]string{
		"https://media.example.test/a/an-episode.MP3":  ".MP3",
		"https://media.example.test/a/an-episode.flac": ".flac",
		"https://media.example.test/a/an-episode.m4a":  ".m4a",
	} {
		file, ok := a.file(abcListenDoc{Renditions: []abcListenRendition{{URL: link, MIMEType: "audio/mpeg", Bitrate: 128}}})
		if !ok || file.Name != want {
			t.Errorf("%s: name %q, ok %v; want %q", link, file.Name, ok, want)
		}
	}
}
