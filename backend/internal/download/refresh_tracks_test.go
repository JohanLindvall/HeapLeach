// SPDX-License-Identifier: MIT

package download

import (
	"context"
	"reflect"
	"testing"

	"github.com/JohanLindvall/HeapLeach/internal/extractor"
)

// A playlist read again replaces its key and tracks, and a reading without
// tracks clears the ones an earlier reading had, so a joined file never
// mixes a stale soundtrack with fresh video.
func TestResolvedPlaylistReplacesKeyAndTracks(t *testing.T) {
	stale := []extractor.Track{{Segments: []string{"https://media.example.test/stale/a.aac"}}}
	fresh := []extractor.Track{{Segments: []string{"https://media.example.test/fresh/a.aac"}, Language: "en"}}
	key := &extractor.SegmentKey{URI: "https://media.example.test/fresh/key"}
	for _, tc := range []struct {
		name   string
		tracks []extractor.Track
	}{{"fresh tracks", fresh}, {"none", nil}} {
		t.Run(tc.name, func(t *testing.T) {
			it := &Item{Name: "Clip.mp4", Segments: []string{"stale"}, Audio: stale, Subtitles: stale,
				SegmentKey: &extractor.SegmentKey{URI: "https://media.example.test/stale/key"},
				resolve: func(context.Context) (*extractor.Target, error) {
					return &extractor.Target{URL: "https://media.example.test/fresh/0.ts", Size: -1,
						Segments: []string{"https://media.example.test/fresh/0.ts"}, SegmentKey: key,
						Audio: tc.tracks, Subtitles: tc.tracks}, nil
				}}
			if err := resolveRetryManager(0).resolveTarget(t.Context(), it); err != nil {
				t.Fatal(err)
			}
			if it.SegmentKey != key || !reflect.DeepEqual(it.Audio, tc.tracks) || !reflect.DeepEqual(it.Subtitles, tc.tracks) {
				t.Errorf("key=%+v audio=%+v subtitles=%+v", it.SegmentKey, it.Audio, it.Subtitles)
			}
		})
	}
}
