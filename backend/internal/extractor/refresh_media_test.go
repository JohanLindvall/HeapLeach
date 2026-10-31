// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"net/url"
	"reflect"
	"testing"
)

// playlistReader answers each read with the playlist of its next
// generation: signed segments, a key, and whatever tracks that generation
// lists.
type playlistReader struct {
	hostClaim
	generations []File
	reads       int
}

func (p *playlistReader) Name() string { return "playlist" }

func (p *playlistReader) Extract(context.Context, *url.URL, Options) (*Result, error) {
	f := p.generations[min(p.reads, len(p.generations)-1)]
	p.reads++
	return &Result{Title: "Clip", Files: []File{f}}, nil
}

func syntheticPlaylist(gen string, tracks bool) File {
	f := File{
		Name: "Clip.mp4", Size: 1000, SizeApprox: true,
		Segments:   []string{"https://media.example.test/" + gen + "/0.ts", "https://media.example.test/" + gen + "/1.ts"},
		SegmentKey: &SegmentKey{URI: "https://media.example.test/" + gen + "/key", Sequence: 7},
	}
	if tracks {
		f.Audio = []Track{{Segments: []string{"https://media.example.test/" + gen + "/a.aac"}, Language: "en", Default: true}}
		f.Subtitles = []Track{{Segments: []string{"https://media.example.test/" + gen + "/s.vtt"}, Language: "sv", Forced: true}}
	}
	return f
}

// Challenge recovery reads a playlist's source again on a new route. The
// fresh key and tracks replace the stale ones, and a reading without tracks
// clears them, which the downloader relies on (resolveTarget).
func TestRefreshCarriesEveryPlaylistField(t *testing.T) {
	for _, tc := range []struct {
		name  string
		fresh File
	}{
		{"tracks", syntheticPlaylist("fresh", true)},
		{"no tracks", syntheticPlaylist("fresh", false)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reader := &playlistReader{hostClaim: "tube.example.test",
				generations: []File{syntheticPlaylist("stale", true), tc.fresh}}
			reg := &Registry{fallback: NewDirect(nil), extractors: []Extractor{reader}}
			res, _, err := reg.Extract(context.Background(), "https://tube.example.test/v/1", Options{})
			if err != nil || len(res.Files) != 1 || res.Files[0].Refresh == nil {
				t.Fatalf("res=%+v err=%v", res, err)
			}
			target, err := res.Files[0].Refresh(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			want := tc.fresh
			if target.URL != want.Segments[0] || target.Name != "Clip.mp4" || target.Size != -1 {
				t.Errorf("url=%q name=%q size=%d", target.URL, target.Name, target.Size)
			}
			if !reflect.DeepEqual(target.Segments, want.Segments) || !reflect.DeepEqual(target.SegmentKey, want.SegmentKey) {
				t.Errorf("segments=%v key=%+v", target.Segments, target.SegmentKey)
			}
			if !reflect.DeepEqual(target.Audio, want.Audio) || !reflect.DeepEqual(target.Subtitles, want.Subtitles) {
				t.Errorf("audio=%+v subtitles=%+v", target.Audio, target.Subtitles)
			}
		})
	}
}

// An eagerly listed attachment answering as a resolver keeps its exact
// size, address and headers; an approximate one becomes unknown.
func TestFileTargetSizes(t *testing.T) {
	exact := File{Name: "a.jpg", URL: "https://media.example.test/a.jpg", Size: 42}
	if got := exact.target(); got.URL != exact.URL || got.Size != 42 || got.Name != "a.jpg" {
		t.Errorf("exact = %+v", got)
	}
	approx := exact
	approx.SizeApprox = true
	if got := approx.target(); got.Size != -1 {
		t.Errorf("approximate size kept: %d", got.Size)
	}
}
