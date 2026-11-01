// SPDX-License-Identifier: MIT

package extractor

import (
	"testing"

	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

func TestRajceStorageFilenames(t *testing.T) {
	for _, tc := range []struct {
		name, path, want string
	}{
		{"extensionless video", "/storage/video/synthetic", "synthetic.mp4"},
		{"existing extension", "/storage/video/scene.mp4", "scene.mp4"},
		{"uppercase extension", "/storage/video/scene.MP4", "scene.mp4"},
		{"poster extension", "/storage/video/scene.clip.jpg", "scene.clip.mp4"},
		{"query filename ignored", "/storage/video/synthetic?filename=unrelated.jpg&download=1", "synthetic.mp4"},
		{"percent encoded name", "/storage/video/Quiet%20scene%2Ejpg?download=1#selected", "Quiet scene.mp4"},
		{"image with video query", "/storage/images/photo.jpg?next=/video/synthetic", "photo.jpg"},
		{"extensionless image", "/storage/images/synthetic", "synthetic"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := "https://storage.example.test" + tc.path
			// A storage link is already the media. With no HTTP client, the
			// extractor must produce its file without requesting any bytes.
			res, err := NewRajce(nil).Extract(t.Context(), mustParse(t, raw), Options{})
			if err != nil {
				t.Fatal(err)
			}
			if len(res.Files) != 1 {
				t.Fatalf("got %d files, want the one storage link", len(res.Files))
			}
			file := res.Files[0]
			if res.Title != tc.want || file.Name != tc.want {
				t.Errorf("title = %q, filename = %q; want %q for both", res.Title, file.Name, tc.want)
			}
			if file.URL != raw || file.Headers[httpx.HeaderReferer] != rajceRoot+"/" {
				t.Errorf("URL = %q, Referer = %q; want the unchanged URL and site Referer", file.URL, file.Headers[httpx.HeaderReferer])
			}
		})
	}
}
