// SPDX-License-Identifier: MIT

package extractor

import "testing"

// TestLinkExt pins the extension read off a link: lowercased, the query
// ignored, a trailing slash stepped over to the name before it, and the
// fallback where the name carries no container.
func TestLinkExt(t *testing.T) {
	tests := []struct{ link, fallback, want string }{
		{"https://cdn.example.test/a/b.MP3?token=1", ".mp3", ".mp3"},
		{"https://cdn.example.test/a/b.m4a", ".mp3", ".m4a"},
		{"https://cdn.example.test/a/b", ".mp4", ".mp4"},
		{"https://cdn.example.test/a/b.toolong", ".mp3", ".mp3"},
		{"https://cdn.example.test/a.v2/b", "", ""},
		{"https://cdn.example.test/a/b.", ".mp3", ".mp3"},
		{"https://cdn.example.test/get_file/1/abc/1000/1000.mp4/", ".flv", ".mp4"},
		{"https://cdn.example.test/", ".mp3", ".mp3"},
		{"not a link\x7f", ".mp3", ".mp3"},
	}
	for _, tc := range tests {
		if got := linkExt(tc.link, tc.fallback); got != tc.want {
			t.Errorf("linkExt(%q, %q) = %q, want %q", tc.link, tc.fallback, got, tc.want)
		}
	}
}
