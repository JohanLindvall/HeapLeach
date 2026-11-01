// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"strconv"
	"testing"

	"github.com/JohanLindvall/HeapLeach/internal/config"
)

// A paced wiki's category walk keeps its own lower bound under the default
// source limit, and a lower source limit still holds.
func TestMediaWikiCategoryLimit(t *testing.T) {
	if got := mediaWikiCategoryLimit(Options{}); got != config.MaxMediaWikiCategories {
		t.Errorf("default: %d, want %d", got, config.MaxMediaWikiCategories)
	}
	if got := mediaWikiCategoryLimit(Options{limits: Limits{Sources: 7}}); got != 7 {
		t.Errorf("lower source limit: %d, want 7", got)
	}
}

// A broadcaster that bears fewer requests resolves no more episodes than
// its own budget, and the note says how many of the listing were kept.
func TestExpandSeriesHoldsAHostBudget(t *testing.T) {
	items := make([]int, config.MaxResolvedEpisodesVRT+50)
	for i := range items {
		items[i] = i
	}
	asked := 0
	got, note, err := expandSeries(context.Background(), series[int]{
		site: "example", noun: "episodes", link: "https://example.test/show",
		items: items, inTurn: true, budget: config.MaxResolvedEpisodesVRT,
		resolve: func(_ context.Context, i int) (*File, error) {
			asked++
			return &File{Name: strconv.Itoa(i) + ".mp4", URL: "https://example.test/" + strconv.Itoa(i), Size: -1}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != config.MaxResolvedEpisodesVRT || asked != config.MaxResolvedEpisodesVRT {
		t.Fatalf("resolved %d, asked %d; want %d", len(got), asked, config.MaxResolvedEpisodesVRT)
	}
	if want := cappedNote(config.MaxResolvedEpisodesVRT, len(items), "episodes"); note != want {
		t.Fatalf("note %q, want %q", note, want)
	}
	if config.MaxResolvedEpisodesVRT >= config.MaxResolvedEpisodes {
		t.Fatal("the lower budget is no lower than the shared one")
	}
}
