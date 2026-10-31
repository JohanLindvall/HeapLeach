// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/config"
)

// TestResolveFilesKeepsOrderAndReasons pins the multi-file resolve: each
// item's files in the listing's order, whichever finished first, the
// refusals in order beside them, and the first other failure.
func TestResolveFilesKeepsOrderAndReasons(t *testing.T) {
	resolve := func(_ context.Context, item string) ([]File, error) {
		switch item {
		case "refused-1", "refused-2":
			return nil, &broadcastRefusal{"site", item, "withheld " + item}
		case "failed-1", "failed-2":
			return nil, errors.New("site: " + item)
		case "empty":
			return nil, nil
		}
		return []File{{Name: item + "-a"}, {Name: item + "-b"}}, nil
	}
	items := []string{"one", "failed-1", "refused-1", "empty", "two", "refused-2", "failed-2"}
	files, refusals, failed := resolveFiles(context.Background(), items, resolve)
	if got := fmt.Sprint(fileNames(files)); got != "[one-a one-b two-a two-b]" {
		t.Errorf("files = %s", got)
	}
	if got := fmt.Sprint(refusals); got != "[withheld refused-1 withheld refused-2]" {
		t.Errorf("refusals = %s", got)
	}
	if failed == nil || failed.Error() != "site: failed-1" {
		t.Errorf("failed = %v", failed)
	}
}

// TestNoneReadSaysWhy pins the error a listing gives when none of its
// items could be read: a refusal first, then a failure less the site's
// name, then that they held nothing.
func TestNoneReadSaysWhy(t *testing.T) {
	for _, tc := range []struct {
		refusals []string
		failed   error
		want     string
	}{
		{[]string{"it is private"}, errors.New("site: transport"), "site: none of the 3 releases on the link could be read: it is private"},
		{nil, errors.New("site: transport"), "site: none of the 3 releases on the link could be read: transport"},
		{nil, nil, "site: none of the 3 releases on the link could be read: they hold nothing to download"},
	} {
		if got := noneRead("site", 3, "releases", "the link", tc.refusals, tc.failed).Error(); got != tc.want {
			t.Errorf("got %q, want %q", got, tc.want)
		}
	}
}

// Both modes of the ordered map stop the same way: nothing starts once the
// context has ended, a cancellation during a callback stops the items
// after it, and the outcomes that did arrive keep the listing's order.
func TestResolveOutcomesStopsAtCancellation(t *testing.T) {
	items := []int{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19}
	for _, inTurn := range []bool{true, false} {
		t.Run(fmt.Sprintf("inTurn=%v", inTurn), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			var calls atomic.Int32
			got := resolveOutcomes(ctx, items, inTurn, func(context.Context, int) (int, error) {
				calls.Add(1)
				return 0, nil
			})
			if len(got) != 0 || calls.Load() != 0 {
				t.Fatalf("cancelled before work: %d outcomes, %d calls", len(got), calls.Load())
			}

			ctx, cancel = context.WithCancel(context.Background())
			defer cancel()
			calls.Store(0)
			got = resolveOutcomes(ctx, items, inTurn, func(ctx context.Context, i int) (int, error) {
				calls.Add(1)
				if i == 2 {
					cancel()
				}
				return i * 10, nil
			})
			if inTurn && (calls.Load() != 3 || len(got) != 3) {
				t.Errorf("serial callbacks after cancellation: %d calls, %d outcomes", calls.Load(), len(got))
			}
			if !inTurn && int(calls.Load()) > 2+config.PageFetchConcurrency {
				t.Errorf("concurrent map kept starting items: %d calls", calls.Load())
			}
			for k := 1; k < len(got); k++ {
				if got[k].index <= got[k-1].index || got[k].got != got[k].index*10 || items[got[k].index] != got[k].item {
					t.Fatalf("outcomes out of order: %+v", got)
				}
			}
		})
	}
}

// The concurrent map stays within config.PageFetchConcurrency.
func TestResolveOutcomesIsBounded(t *testing.T) {
	items := make([]int, 4*config.PageFetchConcurrency)
	var active, peak atomic.Int32
	resolveOutcomes(context.Background(), items, false, func(context.Context, int) (int, error) {
		n := active.Add(1)
		for p := peak.Load(); n > p && !peak.CompareAndSwap(p, n); p = peak.Load() {
		}
		time.Sleep(2 * time.Millisecond)
		active.Add(-1)
		return 0, nil
	})
	if peak.Load() > int32(config.PageFetchConcurrency) {
		t.Errorf("peak concurrency %d", peak.Load())
	}
}

// A listing cancelled after some of its items resolved returns the
// cancellation, never the partial listing as a success; one that is not
// cancelled keeps its files in order and says why when none resolved.
func TestListingFinishersPreferCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	files, _, err := readListing(ctx, "site", "posts", "a listing", []string{"a", "b"},
		func(_ context.Context, item string) ([]File, error) {
			cancel()
			return []File{{Name: item}}, nil
		})
	if !errors.Is(err, context.Canceled) || files != nil {
		t.Fatalf("cancelled listing: files=%v err=%v", files, err)
	}
	if err := noneResolved(ctx, []File{{Name: "a"}}, "site", 1, "posts", "x", nil, nil); !errors.Is(err, context.Canceled) {
		t.Errorf("noneResolved with files after cancellation = %v", err)
	}

	files, note, err := readListing(context.Background(), "site", "posts", "a listing", []string{"a", "b", "c"},
		func(_ context.Context, item string) ([]File, error) {
			if item == "b" {
				return nil, &broadcastRefusal{site: "site", reason: "it is gone"}
			}
			return []File{{Name: item}}, nil
		})
	if err != nil || len(files) != 2 || files[0].Name != "a" || files[1].Name != "c" || note == "" {
		t.Fatalf("mixed listing: files=%v note=%q err=%v", files, note, err)
	}
	_, _, err = readListing(context.Background(), "site", "posts", "a listing", []string{"a"},
		func(context.Context, string) ([]File, error) { return nil, errors.New("site: broken") })
	if err == nil || !strings.Contains(err.Error(), "broken") {
		t.Errorf("all failed: %v", err)
	}
}

// A series cancelled while its episodes resolve returns the cancellation,
// through expandSeries as through listSeries.
func TestExpandSeriesPrefersCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := series[string]{site: "site", noun: "episodes", link: "x", items: []string{"a", "b"}, inTurn: true,
		resolve: func(_ context.Context, item string) (*File, error) {
			cancel()
			return &File{Name: item}, nil
		}}
	if resolved, _, err := expandSeries(ctx, s); !errors.Is(err, context.Canceled) || resolved != nil {
		t.Fatalf("expandSeries: %v, %v", resolved, err)
	}
}

// A season read in turn is not asked after a cancellation, and seasons
// read concurrently keep their order.
func TestReadSeasonsStopsAtCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var calls atomic.Int32
	list, _ := readSeasons(ctx, []string{"1", "2", "3"}, true, func(_ context.Context, s string) (seasonList[string], error) {
		calls.Add(1)
		cancel()
		return seasonList[string]{items: []string{s}}, nil
	})
	if calls.Load() != 1 || len(list.items) != 1 {
		t.Fatalf("calls=%d items=%v", calls.Load(), list.items)
	}
	list, first := readSeasons(context.Background(), []string{"1", "2", "3"}, false, func(_ context.Context, s string) (seasonList[string], error) {
		if s == "1" {
			return seasonList[string]{}, errors.New("unreadable")
		}
		return seasonList[string]{items: []string{s, s + "b"}}, nil
	})
	if first == nil || !reflect.DeepEqual(list.items, []string{"2", "2b", "3", "3b"}) || list.note != unreadableSeasons {
		t.Fatalf("items=%v note=%q first=%v", list.items, list.note, first)
	}
}
