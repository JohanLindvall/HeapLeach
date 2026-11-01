// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
)

// A series held to the user's file cap resolves no episode past it, yet an
// episode refused before the cap does not cost a playable one: the missing
// one is asked for in a further round, and the note counts what the job
// holds of the series.
func TestExpandSeriesHoldsToTheFileCap(t *testing.T) {
	items := []string{"a", "b", "c", "d", "e"}
	var asked atomic.Int32
	resolve := func(_ context.Context, id string) (*File, error) {
		asked.Add(1)
		if id == "a" {
			return nil, &itemRefusal{site: "site", title: id, reason: "gone"}
		}
		return &File{Name: id + ".mp4"}, nil
	}
	resolved, note, err := expandSeries(context.Background(), series[string]{
		site: "site", noun: "episodes", link: "the link", items: items, files: 2, resolve: resolve,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprint(filePaths(listedFiles(resolved))); got != "[b.mp4 c.mp4]" || note != "2 of 5 episodes" {
		t.Errorf("files = %s, note %q", got, note)
	}
	if n := asked.Load(); n != 3 {
		t.Errorf("resolved %d episodes, want the 3 up to the cap", n)
	}
}
